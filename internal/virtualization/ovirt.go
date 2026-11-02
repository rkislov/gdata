// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type OVirt struct {
	base         *Client
	PollInterval time.Duration
}
type ovRef struct {
	ID string `xml:"id,attr"`
}
type ovVM struct {
	ID          string `xml:"id,attr"`
	Name        string `xml:"name"`
	Description string `xml:"description"`
	Status      string `xml:"status"`
	Memory      int64  `xml:"memory"`
	Cluster     ovRef  `xml:"cluster"`
	OS          string `xml:"os>type"`
	Bios        string `xml:"bios>type"`
	CPU         struct {
		Architecture string `xml:"architecture"`
		Topology     struct {
			Cores   int `xml:"cores,attr"`
			Sockets int `xml:"sockets,attr"`
			Threads int `xml:"threads,attr"`
		} `xml:"topology"`
	} `xml:"cpu"`
}
type ovAttachment struct {
	ID        string `xml:"id,attr"`
	Bootable  bool   `xml:"bootable"`
	Active    bool   `xml:"active"`
	Interface string `xml:"interface"`
	Disk      ovRef  `xml:"disk"`
}
type ovDisk struct {
	ID          string `xml:"id,attr"`
	Alias       string `xml:"alias"`
	Description string `xml:"description"`
	Status      string `xml:"status"`
	StorageType string `xml:"storage_type"`
	Shareable   bool   `xml:"shareable"`
	Size        int64  `xml:"provisioned_size"`
}
type ovTransfer struct {
	ID       string `xml:"id,attr"`
	Phase    string `xml:"phase"`
	URL      string `xml:"transfer_url"`
	ProxyURL string `xml:"proxy_url"`
	Disk     ovRef  `xml:"disk"`
	Backup   ovRef  `xml:"backup"`
}
type ovBackup struct {
	ID          string  `xml:"id,attr"`
	Phase       string  `xml:"phase"`
	Description string  `xml:"description"`
	Disks       []ovRef `xml:"disks>disk"`
}

func NewOVirt(p Profile) (*OVirt, error) {
	if p.Kind != "ovirt" || !p.BackupEnabled {
		return nil, errors.New("oVirt backup profile is not enabled")
	}
	b, e := New(p)
	if e != nil {
		return nil, e
	}
	b.http.Timeout = 0
	return &OVirt{base: b, PollInterval: time.Second}, nil
}
func (c *OVirt) Close() { c.base.Close() }
func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func (c *OVirt) api(ctx context.Context, method, path, body string, out any) error {
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.base.profile.Endpoint, "/")+path, strings.NewReader(body))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+c.base.credentials["token"])
	req.Header.Set("Accept", "application/xml")
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("All-Content", "true")
	response, e := c.base.http.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("oVirt connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &APIError{"oVirt", method, response.StatusCode}
	}
	if out != nil {
		if e = xml.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(out); e != nil {
			return errors.New("invalid oVirt XML response")
		}
	}
	return nil
}
func (c *OVirt) wait(ctx context.Context, check func() (bool, error)) error {
	for {
		done, e := check()
		if done || e != nil {
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.PollInterval):
		}
	}
}
func (c *OVirt) Preflight(ctx context.Context, id, profile, policy string) (model.VMBackup, []ovAttachment, error) {
	var meta model.VMBackup
	var vm ovVM
	if !model.CloudUUID(id) {
		return meta, nil, errors.New("oVirt VM UUID required")
	}
	if e := c.api(ctx, "GET", "/vms/"+id, "", &vm); e != nil {
		return meta, nil, e
	}
	if vm.ID != id || vm.Status != "up" && vm.Status != "down" || policy == "cold-only" && vm.Status != "down" {
		return meta, nil, errors.New("oVirt source must be stable up/down; cold-only requires down")
	}
	var devices struct {
		Items []ovRef `xml:"host_device"`
	}
	if e := c.api(ctx, "GET", "/vms/"+id+"/hostdevices", "", &devices); e != nil {
		return meta, nil, e
	}
	if len(devices.Items) > 0 {
		return meta, nil, errors.New("oVirt host devices are unsupported")
	}
	var attachments struct {
		Items []ovAttachment `xml:"disk_attachment"`
	}
	if e := c.api(ctx, "GET", "/vms/"+id+"/diskattachments", "", &attachments); e != nil {
		return meta, nil, e
	}
	if len(attachments.Items) == 0 || len(attachments.Items) > 32 {
		return meta, nil, errors.New("oVirt requires 1–32 image disks")
	}
	meta = model.VMBackup{Provider: "ovirt", Profile: profile, ProjectID: vm.Cluster.ID, ServerID: id, Name: vm.Name, Status: vm.Status, Flavor: model.VMFlavor{ID: vm.Cluster.ID, VCPUs: vm.CPU.Topology.Cores * vm.CPU.Topology.Sockets * vm.CPU.Topology.Threads, RAM: int(vm.Memory >> 20)}, Metadata: map[string]string{"os": vm.OS, "bios": vm.Bios, "architecture": vm.CPU.Architecture}, Consistency: "ovirt-crash-consistent", SnapshotAt: time.Now().UTC(), Disks: []model.VMDisk{}}
	if vm.Status == "down" {
		meta.Consistency = "ovirt-cold-consistent"
	}
	roots := 0
	var total int64
	for i, attachment := range attachments.Items {
		if !model.CloudUUID(attachment.Disk.ID) || !attachment.Active || attachment.Interface != "virtio" && attachment.Interface != "virtio_scsi" && attachment.Interface != "sata" && attachment.Interface != "ide" {
			return meta, nil, errors.New("unsupported oVirt disk attachment")
		}
		var disk ovDisk
		if e := c.api(ctx, "GET", "/disks/"+attachment.Disk.ID, "", &disk); e != nil {
			return meta, nil, e
		}
		if disk.StorageType != "image" || disk.Shareable || disk.Status != "ok" || disk.Size <= 0 || disk.Size > model.MaxCaptureBytes-total {
			return meta, nil, errors.New("oVirt shared/LUN/locked disks or unsupported geometry")
		}
		total += disk.Size
		boot := -1
		if attachment.Bootable {
			boot = 0
			roots++
		}
		meta.Disks = append(meta.Disks, model.VMDisk{File: fmt.Sprintf("%03d-disk.img", i), VolumeID: disk.ID, Device: attachment.Interface, BootIndex: boot, Bytes: disk.Size, SizeGiB: int((disk.Size + (1 << 30) - 1) >> 30), DiskFormat: "raw"})
	}
	if roots != 1 || meta.Flavor.VCPUs < 1 || meta.Flavor.RAM < 1 || !model.CloudUUID(meta.ProjectID) {
		return meta, nil, errors.New("oVirt requires one boot disk and valid CPU/memory/cluster")
	}
	var nics struct {
		Items []struct {
			Profile ovRef `xml:"vnic_profile"`
		} `xml:"nic"`
	}
	if e := c.api(ctx, "GET", "/vms/"+id+"/nics", "", &nics); e != nil {
		return meta, nil, e
	}
	for _, n := range nics.Items {
		if !model.CloudUUID(n.Profile.ID) {
			return meta, nil, errors.New("unmapped oVirt NIC")
		}
		meta.Networks = append(meta.Networks, model.VMNetwork{ID: n.Profile.ID})
	}
	return meta, attachments.Items, nil
}
func (c *OVirt) transfer(ctx context.Context, j *Journal, disk, backup, direction string) (ovTransfer, error) {
	var out ovTransfer
	slot, e := j.Intent("ovirt-transfer", disk, false)
	if e != nil {
		return out, e
	}
	body := `<image_transfer><disk id="` + disk + `"/><direction>` + direction + `</direction><format>raw</format>`
	if backup != "" {
		body += `<backup id="` + backup + `"/>`
	}
	body += `<inactivity_timeout>3600</inactivity_timeout></image_transfer>`
	if e = c.api(ctx, "POST", "/imagetransfers", body, &out); e != nil {
		return out, e
	}
	if e = j.Record(slot, out.ID); e != nil {
		return out, e
	}
	e = c.wait(ctx, func() (bool, error) {
		if err := c.api(ctx, "GET", "/imagetransfers/"+out.ID, "", &out); err != nil {
			return false, err
		}
		if out.Phase == "transferring" {
			return true, nil
		}
		if strings.Contains(out.Phase, "finished") || strings.Contains(out.Phase, "paused") || strings.Contains(out.Phase, "cancel") {
			return false, errors.New("oVirt transfer did not become ready")
		}
		return false, nil
	})
	if e == nil && (out.Disk.ID != disk || backup != "" && out.Backup.ID != backup) {
		return out, errors.New("oVirt transfer scope does not match requested disk/backup")
	}
	return out, e
}
func (c *OVirt) imageIO(ctx context.Context, transfer ovTransfer, method string, input io.Reader, size int64) (io.ReadCloser, error) {
	endpoint := transfer.ProxyURL
	if endpoint == "" {
		endpoint = transfer.URL
	}
	u, e := url.Parse(endpoint)
	origin := ""
	if e == nil {
		origin = u.Scheme + "://" + u.Host
	}
	engine, _ := url.Parse(c.base.profile.Endpoint)
	if e != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/images/") || !model.CloudUUID(strings.TrimPrefix(u.Path, "/images/")) || origin != "https://"+engine.Host && !slices.Contains(c.base.profile.TransferOrigins, origin) {
		return nil, errors.New("oVirt imageio endpoint is not explicitly trusted")
	}
	req, e := http.NewRequestWithContext(ctx, method, endpoint, input)
	if e != nil {
		return nil, e
	}
	if method == "PUT" {
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	response, e := c.base.http.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("oVirt imageio connection failed")
	}
	if response.StatusCode != 200 {
		response.Body.Close()
		return nil, &APIError{"imageio", method, response.StatusCode}
	}
	return response.Body, nil
}
func (c *OVirt) finishTransfer(ctx context.Context, j *Journal, id string) error {
	if e := c.api(ctx, "POST", "/imagetransfers/"+id+"/finalize", "<action/>", nil); e != nil && !isMissing(e) {
		return e
	}
	if e := c.wait(ctx, func() (bool, error) {
		var transfer ovTransfer
		e := c.api(ctx, "GET", "/imagetransfers/"+id, "", &transfer)
		if isMissing(e) {
			return true, nil
		}
		if e != nil {
			return false, e
		}
		if transfer.Phase == "finished_failure" {
			return false, errors.New("oVirt image transfer finalization failed")
		}
		return transfer.Phase == "finished_success", nil
	}); e != nil {
		return e
	}
	for i := range j.Resources {
		if j.Resources[i].ID == id {
			j.Resources[i].Deleted = true
		}
	}
	return j.Save()
}
func (c *OVirt) Backup(ctx context.Context, j *Journal, profile, id, policy, dir string, report func(int64, int64)) (model.VMBackup, error) {
	meta, _, e := c.Preflight(ctx, id, profile, policy)
	if e != nil {
		return meta, e
	}
	j.SourceServer = id
	if e = j.Save(); e != nil {
		return meta, e
	}
	var total int64
	for _, disk := range meta.Disks {
		total += disk.Bytes
	}
	if total > c.base.profile.StagingMaxBytes {
		return meta, errors.New("oVirt staging quota exceeded")
	}
	backupID := cloudUUIDFromRun(j.ID)
	slot, e := j.Intent("ovirt-backup", j.Name("backup"), false)
	if e != nil {
		return meta, e
	}
	if e = j.Record(slot, backupID); e != nil {
		return meta, e
	}
	body := `<backup id="` + backupID + `"><description>` + xmlText(j.Name("backup")) + `</description><disks>`
	for _, d := range meta.Disks {
		body += `<disk id="` + d.VolumeID + `"/>`
	}
	body += `</disks></backup>`
	var backup ovBackup
	if e = c.api(ctx, "POST", "/vms/"+id+"/backups", body, &backup); e != nil {
		return meta, e
	}
	if backup.ID != backupID {
		return meta, errors.New("oVirt did not honor client backup identity")
	}
	if e = c.wait(ctx, func() (bool, error) {
		e := c.api(ctx, "GET", "/vms/"+id+"/backups/"+backupID, "", &backup)
		if backup.Phase == "failed" {
			return false, errors.New("oVirt backup failed")
		}
		return backup.Phase == "ready", e
	}); e != nil {
		return meta, e
	}
	var disks struct {
		Items []ovRef `xml:"disk"`
	}
	if e = c.api(ctx, "GET", "/vms/"+id+"/backups/"+backupID+"/disks", "", &disks); e != nil {
		return meta, e
	}
	if len(disks.Items) != len(meta.Disks) {
		return meta, errors.New("oVirt backup omitted disks")
	}
	for _, d := range meta.Disks {
		found := false
		for _, actual := range disks.Items {
			found = found || actual.ID == d.VolumeID
		}
		if !found {
			return meta, errors.New("oVirt backup disk coverage mismatch")
		}
	}
	var done int64
	for i, disk := range meta.Disks {
		transfer, e := c.transfer(ctx, j, disk.VolumeID, backupID, "download")
		if e != nil {
			return meta, e
		}
		in, e := c.imageIO(ctx, transfer, "GET", nil, 0)
		if e != nil {
			return meta, e
		}
		out, e := os.OpenFile(filepath.Join(dir, disk.File), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			in.Close()
			return meta, e
		}
		n, err := io.Copy(out, io.LimitReader(in, disk.Bytes+1))
		in.Close()
		syncErr := out.Sync()
		closeErr := out.Close()
		if err != nil {
			return meta, err
		}
		if syncErr != nil {
			return meta, syncErr
		}
		if closeErr != nil {
			return meta, closeErr
		}
		if n != disk.Bytes {
			return meta, errors.New("oVirt disk length mismatch")
		}
		if e = c.finishTransfer(ctx, j, transfer.ID); e != nil {
			return meta, e
		}
		done += n
		if report != nil {
			report(done, total)
		}
		meta.Disks[i].SHA256 = ""
	}
	current, _, e := c.Preflight(ctx, id, profile, policy)
	if e != nil {
		return meta, e
	}
	if current.Status != meta.Status || len(current.Disks) != len(meta.Disks) {
		return meta, errors.New("oVirt source configuration changed during backup")
	}
	for i := range meta.Disks {
		if current.Disks[i].VolumeID != meta.Disks[i].VolumeID || current.Disks[i].Bytes != meta.Disks[i].Bytes {
			return meta, errors.New("oVirt source disks changed during backup")
		}
	}
	return meta, nil
}
func cloudUUIDFromRun(id string) string {
	return id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
}
