// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization

import (
	"context"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"io"
	"strings"
)

func uuid() string {
	h := model.ID()
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func (c *OpenStack) uploadRestoredImage(ctx context.Context, j *Journal, d model.VMDisk, index int, reader io.Reader) (string, error) {
	name := j.Name(fmt.Sprintf("restored-image-%02d", index))
	slot, e := j.Intent("image", name, true)
	if e != nil {
		return "", e
	}
	id := uuid()
	if e = j.Record(slot, id); e != nil {
		return "", e
	}
	body := map[string]any{"id": id, "name": name, "disk_format": d.DiskFormat, "container_format": "bare", "visibility": "private", "min_disk": d.SizeGiB, "srk_run": j.ID, "srk_proxy": j.Owner}
	for k, v := range d.Properties {
		if allowedImageProperty(k) {
			body[k] = v
		}
	}
	var out Image
	if _, e = c.json(ctx, "Glance", "POST", "/images", body, &out); e != nil {
		return "", e
	}
	if out.ID != id || out.Owner != c.base.profile.ProjectID {
		return "", errors.New("restored Glance image identity mismatch")
	}
	resp, e := c.do(ctx, "Glance", "PUT", "/images/"+id+"/file", reader, d.Bytes)
	if e != nil {
		return "", e
	}
	resp.Body.Close()
	image, e := c.WaitImage(ctx, id, false)
	if e != nil {
		return "", e
	}
	if image.Size != d.Bytes || image.DiskFormat != d.DiskFormat {
		return "", errors.New("uploaded image size or format mismatch")
	}
	return id, nil
}
func allowedImageProperty(s string) bool {
	for _, k := range []string{"architecture", "hw_firmware_type", "hw_machine_type", "hw_disk_bus", "hw_scsi_model", "hw_video_model", "hw_qemu_guest_agent", "os_type", "os_distro", "os_version"} {
		if k == s {
			return true
		}
	}
	return false
}

// RestoreReader supplies independently verified SRK bytes, never source-cloud URLs.
func (c *OpenStack) Restore(ctx context.Context, j *Journal, backup model.VMBackup, request model.VMRestoreRequest, open func(model.VMDisk) (io.ReadCloser, error), log func(string)) (model.VMRestoreResult, error) {
	var result model.VMRestoreResult
	flavor, e := c.ValidateRestore(ctx, request)
	if e != nil {
		return result, e
	}
	if flavor.RAM < backup.Flavor.RAM || flavor.VCPUs < backup.Flavor.VCPUs {
		return result, errors.New("target flavor has less RAM or CPUs than the source")
	}
	rootImage := ""
	mapping := []map[string]any{}
	volumes := []string{}
	for i, d := range backup.Disks {
		if log != nil {
			log("uploading restored disk " + d.File)
		}
		stream, e := open(d)
		if e != nil {
			return result, e
		}
		image, e := c.uploadRestoredImage(ctx, j, d, i, stream)
		closeError := stream.Close()
		if e != nil {
			return result, e
		}
		if closeError != nil {
			return result, closeError
		}
		if d.BootIndex == 0 && !backup.VolumeBacked {
			if flavor.Disk < d.SizeGiB {
				return result, errors.New("target root disk is smaller than source")
			}
			rootImage = image
			continue
		}
		name := j.Name(fmt.Sprintf("restored-volume-%02d", i))
		slot, e := j.Intent("volume", name, true)
		if e != nil {
			return result, e
		}
		var out struct {
			Volume CloudVolume `json:"volume"`
		}
		_, e = c.json(ctx, "Cinder", "POST", "/volumes", map[string]any{"volume": map[string]any{"name": name, "size": d.SizeGiB, "imageRef": image, "metadata": j.tags()}}, &out)
		if e != nil {
			return result, e
		}
		if e = j.Record(slot, out.Volume.ID); e != nil {
			return result, e
		}
		if _, e = c.WaitVolume(ctx, out.Volume.ID); e != nil {
			return result, e
		}
		volumes = append(volumes, out.Volume.ID)
		mapping = append(mapping, map[string]any{"uuid": out.Volume.ID, "source_type": "volume", "destination_type": "volume", "boot_index": d.BootIndex, "delete_on_termination": false})
	}
	if !backup.VolumeBacked && rootImage == "" {
		return result, errors.New("backup has no bootable root image")
	}
	metadata := map[string]string{}
	for k, v := range backup.Metadata {
		metadata[k] = v
	}
	for k, v := range j.tags() {
		metadata[k] = v
	}
	networks := []map[string]string{}
	for _, id := range request.Networks {
		networks = append(networks, map[string]string{"uuid": id})
	}
	body := map[string]any{"name": request.Name, "flavorRef": request.FlavorID, "networks": networks, "metadata": metadata, "config_drive": backup.ConfigDrive, "min_count": 1, "max_count": 1}
	if rootImage != "" {
		body["imageRef"] = rootImage
	}
	if len(mapping) > 0 {
		body["block_device_mapping_v2"] = mapping
	}
	if request.KeyName != "" {
		body["key_name"] = request.KeyName
	}
	if request.AvailabilityZone != "" {
		body["availability_zone"] = request.AvailabilityZone
	}
	if len(request.SecurityGroups) > 0 {
		groups := []map[string]string{}
		for _, name := range request.SecurityGroups {
			if name == "" || len(name) > 255 || strings.ContainsAny(name, "\x00\r\n") {
				return result, errors.New("invalid security group")
			}
			groups = append(groups, map[string]string{"name": name})
		}
		body["security_groups"] = groups
	}
	slot, e := j.Intent("server", request.Name, true)
	if e != nil {
		return result, e
	}
	if log != nil {
		log("creating new VM")
	}
	var out struct {
		Server Server `json:"server"`
	}
	if _, e = c.json(ctx, "Nova", "POST", "/servers", map[string]any{"server": body}, &out); e != nil {
		return result, e
	}
	if out.Server.ID == backup.ServerID {
		return result, errors.New("Nova returned source VM ID; source will not be modified")
	}
	if e = j.Record(slot, out.Server.ID); e != nil {
		return result, e
	}
	var server Server
	e = c.wait(ctx, func() (bool, error) {
		var e error
		server, e = c.Server(ctx, out.Server.ID)
		if e != nil {
			return false, e
		}
		if !owned(server.Metadata, j) {
			return false, errors.New("restored VM ownership mismatch")
		}
		if server.Status == "ERROR" {
			return false, errors.New("restored VM entered ERROR")
		}
		return server.Status == "ACTIVE", nil
	})
	if e != nil {
		return result, e
	}
	attached := map[string]bool{}
	for _, v := range server.Volumes {
		attached[v.ID] = true
	}
	if len(attached) != len(volumes) {
		return result, errors.New("restored VM attachment count mismatch")
	}
	for _, v := range volumes {
		if !attached[v] {
			return result, errors.New("restored disk is not attached to the new VM")
		}
	}
	if server.Flavor.ID != request.FlavorID {
		return result, errors.New("restored VM flavor mismatch")
	}
	actualNetworks, e := c.Networks(ctx, server.ID)
	if e != nil {
		return result, e
	}
	if len(actualNetworks) != len(request.Networks) {
		return result, errors.New("restored VM network count mismatch")
	}
	for i, n := range actualNetworks {
		if n.ID != request.Networks[i] {
			return result, errors.New("restored VM network mapping mismatch")
		}
	}
	result = model.VMRestoreResult{ServerID: server.ID, Name: server.Name, Status: server.Status, Volumes: volumes, ControlPlaneVerified: true}
	j.Restore = &result
	return result, nil
}
