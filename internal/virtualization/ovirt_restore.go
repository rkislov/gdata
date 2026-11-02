// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"io"
	"net/url"
	"slices"
)

func (c *OVirt) own(j *Journal) string { return "srk_run=" + j.ID + ";srk_proxy=" + j.Owner }
func (c *OVirt) Restore(ctx context.Context, j *Journal, meta model.VMBackup, request model.VMRestoreRequest, open func(model.VMDisk) (io.ReadCloser, error)) (model.VMRestoreResult, error) {
	result := model.VMRestoreResult{Name: request.Name}
	j.SourceServer = meta.ServerID
	if e := j.Save(); e != nil {
		return result, e
	}
	if e := request.Validate(); e != nil {
		return result, e
	}
	if len(request.SecurityGroups) > 0 || request.KeyName != "" || request.AvailabilityZone != "" {
		return result, errors.New("OpenStack-only restore options cannot be applied to oVirt")
	}
	if !model.CloudUUID(request.FlavorID) || !model.CloudUUID(request.StorageDomainID) {
		return result, errors.New("oVirt restore requires target cluster and storage domain UUIDs")
	}
	for _, path := range []string{"/clusters/" + request.FlavorID, "/storagedomains/" + request.StorageDomainID} {
		if e := c.api(ctx, "GET", path, "", new(ovRef)); e != nil {
			return result, e
		}
	}
	for _, id := range request.Networks {
		if e := c.api(ctx, "GET", "/vnicprofiles/"+id, "", new(ovRef)); e != nil {
			return result, e
		}
	}
	var diskIDs []string
	for i, disk := range meta.Disks {
		name := j.Name(fmt.Sprintf("disk-%02d", i))
		slot, e := j.Intent("ovirt-disk", name, true)
		if e != nil {
			return result, e
		}
		var created ovDisk
		body := `<disk><alias>` + xmlText(name) + `</alias><description>` + xmlText(c.own(j)) + `</description><format>raw</format><sparse>true</sparse><provisioned_size>` + fmt.Sprint(disk.Bytes) + `</provisioned_size><storage_domains><storage_domain id="` + request.StorageDomainID + `"/></storage_domains></disk>`
		if e = c.api(ctx, "POST", "/disks", body, &created); e != nil {
			return result, e
		}
		if e = j.Record(slot, created.ID); e != nil {
			return result, e
		}
		if e = c.wait(ctx, func() (bool, error) {
			e := c.api(ctx, "GET", "/disks/"+created.ID, "", &created)
			return created.Status == "ok", e
		}); e != nil {
			return result, e
		}
		transfer, e := c.transfer(ctx, j, created.ID, "", "upload")
		if e != nil {
			return result, e
		}
		reader, e := open(disk)
		if e != nil {
			return result, e
		}
		response, e := c.imageIO(ctx, transfer, "PUT", reader, disk.Bytes)
		closeErr := reader.Close()
		if e != nil {
			return result, e
		}
		response.Close()
		if closeErr != nil {
			return result, closeErr
		}
		if e = c.finishTransfer(ctx, j, transfer.ID); e != nil {
			return result, e
		}
		// Verify a separate readback, after storage has flushed the uploaded disk.
		check, e := c.transfer(ctx, j, created.ID, "", "download")
		if e != nil {
			return result, e
		}
		input, e := c.imageIO(ctx, check, "GET", nil, 0)
		if e != nil {
			return result, e
		}
		h := sha256.New()
		n, e := io.Copy(h, io.LimitReader(input, disk.Bytes+1))
		input.Close()
		if e != nil {
			return result, e
		}
		if n != disk.Bytes || hex.EncodeToString(h.Sum(nil)) != disk.SHA256 {
			return result, errors.New("oVirt restored disk readback checksum mismatch")
		}
		if e = c.finishTransfer(ctx, j, check.ID); e != nil {
			return result, e
		}
		diskIDs = append(diskIDs, created.ID)
	}
	name := j.Name("vm")
	slot, e := j.Intent("ovirt-vm", name, true)
	if e != nil {
		return result, e
	}
	var vm ovVM
	body := `<vm><name>` + xmlText(name) + `</name><description>` + xmlText(c.own(j)) + `</description><cluster id="` + request.FlavorID + `"/><template><name>Blank</name></template><memory>` + fmt.Sprint(int64(meta.Flavor.RAM)<<20) + `</memory><cpu><architecture>` + xmlText(meta.Metadata["architecture"]) + `</architecture><topology cores="` + fmt.Sprint(meta.Flavor.VCPUs) + `" sockets="1" threads="1"/></cpu><os><type>` + xmlText(meta.Metadata["os"]) + `</type><boot><devices><device>hd</device></devices></boot></os><bios><type>` + xmlText(meta.Metadata["bios"]) + `</type></bios></vm>`
	if e = c.api(ctx, "POST", "/vms", body, &vm); e != nil {
		return result, e
	}
	if vm.ID == meta.ServerID {
		return result, errors.New("oVirt returned source identity for a new VM")
	}
	if e = j.Record(slot, vm.ID); e != nil {
		return result, e
	}
	for i, id := range diskIDs {
		d := meta.Disks[i]
		body := `<disk_attachment><active>true</active><bootable>` + fmt.Sprint(d.BootIndex == 0) + `</bootable><interface>` + xmlText(d.Device) + `</interface><disk id="` + id + `"/></disk_attachment>`
		if e = c.api(ctx, "POST", "/vms/"+vm.ID+"/diskattachments", body, nil); e != nil {
			return result, e
		}
	}
	for i, id := range request.Networks {
		body := `<nic><name>nic` + fmt.Sprint(i) + `</name><interface>virtio</interface><linked>true</linked><vnic_profile id="` + id + `"/></nic>`
		if e = c.api(ctx, "POST", "/vms/"+vm.ID+"/nics", body, nil); e != nil {
			return result, e
		}
	}
	if e = c.api(ctx, "PUT", "/vms/"+vm.ID, `<vm><name>`+xmlText(request.Name)+`</name></vm>`, nil); e != nil {
		return result, e
	}
	if e = c.api(ctx, "POST", "/vms/"+vm.ID+"/start", "<action/>", nil); e != nil {
		return result, e
	}
	if e = c.wait(ctx, func() (bool, error) {
		e := c.api(ctx, "GET", "/vms/"+vm.ID, "", &vm)
		if vm.Status == "not_responding" || vm.Status == "unknown" {
			return false, errors.New("new oVirt VM did not start")
		}
		return vm.Status == "up", e
	}); e != nil {
		return result, e
	}
	restored, _, e := c.Preflight(ctx, vm.ID, meta.Profile, "crash-consistent")
	if e != nil {
		return result, e
	}
	got := []string{}
	for _, d := range restored.Disks {
		got = append(got, d.VolumeID)
	}
	slices.Sort(got)
	expected := slices.Clone(diskIDs)
	slices.Sort(expected)
	networks := []string{}
	for _, n := range restored.Networks {
		networks = append(networks, n.ID)
	}
	if !slices.Equal(got, expected) || !slices.Equal(networks, request.Networks) || restored.ProjectID != request.FlavorID || restored.Flavor.RAM != meta.Flavor.RAM || restored.Flavor.VCPUs != meta.Flavor.VCPUs {
		return result, errors.New("restored oVirt VM configuration differs")
	}
	result.ServerID = vm.ID
	result.Status = "up"
	result.Volumes = diskIDs
	result.ControlPlaneVerified = true
	j.Restore = &result
	return result, j.Save()
}
func (c *OVirt) findResource(ctx context.Context, j *Journal, r Resource) (string, error) {
	var ids []string
	switch r.Kind {
	case "ovirt-vm":
		var list struct {
			Items []ovVM `xml:"vm"`
		}
		if e := c.api(ctx, "GET", "/vms?search="+url.QueryEscape("name="+r.Name), "", &list); e != nil {
			return "", e
		}
		for _, v := range list.Items {
			if v.Name == r.Name && v.Description == c.own(j) {
				ids = append(ids, v.ID)
			}
		}
	case "ovirt-disk":
		var list struct {
			Items []ovDisk `xml:"disk"`
		}
		if e := c.api(ctx, "GET", "/disks?search="+url.QueryEscape("alias="+r.Name), "", &list); e != nil {
			return "", e
		}
		for _, v := range list.Items {
			if v.Alias == r.Name && v.Description == c.own(j) {
				ids = append(ids, v.ID)
			}
		}
	case "ovirt-transfer":
		var list struct {
			Items []ovTransfer `xml:"image_transfer"`
		}
		if e := c.api(ctx, "GET", "/imagetransfers", "", &list); e != nil {
			return "", e
		}
		for _, v := range list.Items {
			if v.Disk.ID == r.Name {
				if j.Kind == "backup" && v.Backup.ID == cloudUUIDFromRun(j.ID) || j.Kind == "restore" {
					ids = append(ids, v.ID)
				}
			}
		}
	default:
		return "", errors.New("unknown oVirt resource intent")
	}
	if len(ids) != 1 || !model.CloudUUID(ids[0]) {
		return "", errors.New("ambiguous or unresolved oVirt create intent; cleanup remains pending")
	}
	return ids[0], nil
}
func (c *OVirt) Cleanup(ctx context.Context, j *Journal, log func(string)) error {
	for i := len(j.Resources) - 1; i >= 0; i-- {
		r := &j.Resources[i]
		if r.Deleted || j.Success && r.Keep {
			continue
		}
		if r.ID == "" {
			id, e := c.findResource(ctx, j, *r)
			if e != nil {
				j.LastError = e.Error()
				_ = j.Save()
				return e
			}
			if e = j.Record(i, id); e != nil {
				return e
			}
		}
		var e error
		switch r.Kind {
		case "ovirt-transfer":
			var transfer ovTransfer
			e = c.api(ctx, "GET", "/imagetransfers/"+r.ID, "", &transfer)
			if isMissing(e) {
				e = nil
				break
			}
			if e != nil {
				break
			}
			if transfer.Disk.ID != r.Name {
				return errors.New("foreign oVirt image transfer; cleanup refused")
			}
			e = c.api(ctx, "POST", "/imagetransfers/"+r.ID+"/cancel", "<action/>", nil)
			if isMissing(e) {
				e = nil
			}
		case "ovirt-backup":
			var backup ovBackup
			path := "/vms/" + j.SourceServer + "/backups/" + r.ID
			e = c.api(ctx, "GET", path, "", &backup)
			if isMissing(e) {
				e = nil
				break
			}
			if e != nil {
				break
			}
			if backup.Description != j.Name("backup") {
				return errors.New("foreign oVirt backup; cleanup refused")
			}
			if backup.Phase != "succeeded" {
				e = c.api(ctx, "POST", path+"/finalize", "<action/>", nil)
				if e == nil {
					e = c.wait(ctx, func() (bool, error) {
						err := c.api(ctx, "GET", path, "", &backup)
						return backup.Phase == "succeeded" || isMissing(err), func() error {
							if isMissing(err) {
								return nil
							}
							return err
						}()
					})
				}
			}
		case "ovirt-disk":
			var disk ovDisk
			e = c.api(ctx, "GET", "/disks/"+r.ID, "", &disk)
			if isMissing(e) {
				e = nil
				break
			}
			if e != nil {
				break
			}
			if disk.Description != c.own(j) || disk.Alias != r.Name {
				return errors.New("foreign oVirt disk; cleanup refused")
			}
			e = c.api(ctx, "DELETE", "/disks/"+r.ID, "", nil)
		case "ovirt-vm":
			var vm ovVM
			e = c.api(ctx, "GET", "/vms/"+r.ID, "", &vm)
			if isMissing(e) {
				e = nil
				break
			}
			if e != nil {
				break
			}
			if vm.Description != c.own(j) || vm.ID == j.SourceServer {
				return errors.New("foreign oVirt VM; cleanup refused")
			}
			if vm.Status != "down" {
				e = c.api(ctx, "POST", "/vms/"+r.ID+"/stop", "<action/>", nil)
				if e != nil {
					break
				}
				e = c.wait(ctx, func() (bool, error) { err := c.api(ctx, "GET", "/vms/"+r.ID, "", &vm); return vm.Status == "down", err })
				if e != nil {
					break
				}
			}
			e = c.api(ctx, "DELETE", "/vms/"+r.ID, "", nil)
		default:
			return errors.New("unknown oVirt journal resource")
		}
		if e != nil && !isMissing(e) {
			j.LastError = e.Error()
			_ = j.Save()
			return e
		}
		if r.Kind == "ovirt-disk" || r.Kind == "ovirt-vm" || r.Kind == "ovirt-transfer" {
			path := "/disks/" + r.ID
			if r.Kind == "ovirt-vm" {
				path = "/vms/" + r.ID
			}
			if r.Kind == "ovirt-transfer" {
				path = "/imagetransfers/" + r.ID
			}
			if e = c.wait(ctx, func() (bool, error) {
				err := c.api(ctx, "GET", path, "", new(ovRef))
				if isMissing(err) {
					return true, nil
				}
				return false, err
			}); e != nil {
				return e
			}
		}
		r.Deleted = true
		if e = j.Save(); e != nil {
			return e
		}
		if log != nil {
			log("released " + r.Kind + " " + r.ID)
		}
	}
	j.Done = true
	j.LastError = ""
	return j.Save()
}
