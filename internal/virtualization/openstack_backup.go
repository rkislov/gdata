// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"strings"
	"time"
)

type Export struct {
	Disk  model.VMDisk
	Image Image
}

func (c *OpenStack) createSnapshot(ctx context.Context, j *Journal, volume string, index int) (string, error) {
	name := j.Name(fmt.Sprintf("snapshot-%02d", index))
	slot, e := j.Intent("snapshot", name, false)
	if e != nil {
		return "", e
	}
	var out struct {
		Snapshot CloudSnapshot `json:"snapshot"`
	}
	_, e = c.json(ctx, "Cinder", "POST", "/snapshots", map[string]any{"snapshot": map[string]any{"name": name, "volume_id": volume, "force": true, "metadata": j.tags()}}, &out)
	if e != nil {
		return "", e
	}
	if e = j.Record(slot, out.Snapshot.ID); e != nil {
		return "", e
	}
	_, e = c.WaitSnapshot(ctx, out.Snapshot.ID)
	return out.Snapshot.ID, e
}
func (c *OpenStack) tagNativeSnapshot(ctx context.Context, j *Journal, snap CloudSnapshot) error {
	if owned(snap.Metadata, j) {
		return nil
	}
	allowed := false
	for _, id := range j.SourceVolumes {
		if snap.VolumeID == id {
			allowed = true
		}
	}
	created, e := parseCloudTime(snap.Created)
	if !allowed || e != nil || created.Before(j.StartedAt.Add(-time.Minute)) || !strings.HasSuffix(snap.Name, j.Name("anchor")) {
		return errors.New("Nova snapshot ownership cannot be proven; no deletion or export performed")
	}
	if _, e = c.json(ctx, "Cinder", "POST", "/snapshots/"+snap.ID+"/metadata", map[string]any{"metadata": j.tags()}, new(any)); e != nil {
		return e
	}
	return nil
}
func (c *OpenStack) adoptSnapshotMapping(ctx context.Context, j *Journal) error {
	if j.Kind != "backup" || !j.VolumeBacked || len(j.SourceVolumes) == 0 {
		return nil
	}
	known := map[string]bool{}
	for _, r := range j.Resources {
		if r.Kind == "snapshot" && r.ID != "" {
			known[r.ID] = true
		}
	}
	var snapshots []CloudSnapshot
	// Recover native snapshots even if Nova failed before returning an image ID.
	var out struct {
		Snapshots []CloudSnapshot `json:"snapshots"`
	}
	_, e := c.json(ctx, "Cinder", "GET", "/snapshots/detail?name="+urlQuery("snapshot for "+j.Name("anchor")), nil, &out)
	if e != nil {
		return e
	}
	for _, s := range out.Snapshots {
		if strings.HasSuffix(s.Name, j.Name("anchor")) {
			snapshots = append(snapshots, s)
		}
	}
	for _, r := range j.Resources {
		if r.Kind != "image" || r.Name != j.Name("anchor") || r.ID == "" || r.Deleted {
			continue
		}
		im, e := c.Image(ctx, r.ID)
		if isMissing(e) {
			continue
		}
		if e != nil {
			return e
		}
		if len(im.Extra["block_device_mapping"]) == 0 {
			continue
		}
		mapping, e := parseBDM(im)
		if e != nil {
			return e
		}
		for _, b := range mapping {
			if b.SourceType != "snapshot" {
				continue
			}
			s, e := c.Snapshot(ctx, b.SnapshotID)
			if isMissing(e) {
				continue
			}
			if e != nil {
				return e
			}
			snapshots = append(snapshots, s)
		}
	}
	for _, snap := range snapshots {
		if !known[snap.ID] {
			idx, e := j.Intent("snapshot", snap.Name, false)
			if e != nil {
				return e
			}
			if e = j.Record(idx, snap.ID); e != nil {
				return e
			}
			known[snap.ID] = true
		}
		if e = c.tagNativeSnapshot(ctx, j, snap); e != nil {
			return e
		}
	}
	return nil
}
func (c *OpenStack) exportSnapshot(ctx context.Context, j *Journal, snapshot string, index int) (Image, error) {
	name := j.Name(fmt.Sprintf("clone-%02d", index))
	slot, e := j.Intent("volume", name, false)
	if e != nil {
		return Image{}, e
	}
	var out struct {
		Volume CloudVolume `json:"volume"`
	}
	_, e = c.json(ctx, "Cinder", "POST", "/volumes", map[string]any{"volume": map[string]any{"name": name, "snapshot_id": snapshot, "metadata": j.tags()}}, &out)
	if e != nil {
		return Image{}, e
	}
	if e = j.Record(slot, out.Volume.ID); e != nil {
		return Image{}, e
	}
	if _, e = c.WaitVolume(ctx, out.Volume.ID); e != nil {
		return Image{}, e
	}
	imageName := j.Name(fmt.Sprintf("export-%02d", index))
	imageSlot, e := j.Intent("image", imageName, false)
	if e != nil {
		return Image{}, e
	}
	var response struct {
		Upload struct {
			ImageID string `json:"image_id"`
		} `json:"os-volume_upload_image"`
	}
	_, e = c.json(ctx, "Cinder", "POST", "/volumes/"+out.Volume.ID+"/action", map[string]any{"os-volume_upload_image": map[string]any{"image_name": imageName, "force": false, "disk_format": "raw", "container_format": "bare"}}, &response)
	if e != nil {
		return Image{}, e
	}
	if e = j.Record(imageSlot, response.Upload.ImageID); e != nil {
		return Image{}, e
	}
	// Cinder upload-to-image cannot set arbitrary image properties. Establish
	// ownership before cleanup, using the clone's volume_image_metadata receipt.
	im, e := c.WaitImage(ctx, response.Upload.ImageID, false)
	if e != nil {
		return Image{}, e
	}
	if im.Name != imageName {
		return im, errors.New("Cinder export image name mismatch")
	}
	patch := []map[string]any{{"op": "add", "path": "/srk_run", "value": j.ID}, {"op": "add", "path": "/srk_proxy", "value": j.Owner}}
	b, _ := json.Marshal(patch)
	resp, e := c.patchImage(ctx, im.ID, b)
	if e != nil {
		return im, e
	}
	resp.Close()
	im, e = c.Image(ctx, im.ID)
	if e != nil {
		return im, e
	}
	if _, e = c.WaitVolume(ctx, out.Volume.ID); e != nil {
		return im, e
	}
	return im, nil
}
func (c *OpenStack) Prepare(ctx context.Context, j *Journal, profile, vmID, consistency string, log func(string)) (model.VMBackup, []Export, error) {
	backup, e := c.Preflight(ctx, vmID, profile, consistency)
	if e != nil {
		return backup, nil, e
	}
	source, e := c.Server(ctx, vmID)
	if e != nil {
		return backup, nil, e
	}
	j.SourceServer = vmID
	j.VolumeBacked = backup.VolumeBacked
	for _, v := range source.Volumes {
		j.SourceVolumes = append(j.SourceVolumes, v.ID)
	}
	if e = j.Save(); e != nil {
		return backup, nil, e
	}
	var imageDataSnapshots []string
	if !backup.VolumeBacked {
		for i, v := range source.Volumes {
			if log != nil {
				log("snapshotting volume " + v.ID)
			}
			snap, e := c.createSnapshot(ctx, j, v.ID, i)
			if e != nil {
				return backup, nil, e
			}
			imageDataSnapshots = append(imageDataSnapshots, snap)
		}
	}
	name := j.Name("anchor")
	slot, e := j.Intent("image", name, false)
	if e != nil {
		return backup, nil, e
	}
	var created struct {
		ImageID string `json:"image_id"`
	}
	header, e := c.json(ctx, "Nova", "POST", "/servers/"+vmID+"/action", map[string]any{"createImage": map[string]any{"name": name, "metadata": j.tags()}}, &optionalImageResponse{Out: &created.ImageID})
	if e != nil {
		return backup, nil, e
	}
	anchor, e := imageID(header, created.ImageID)
	if e != nil {
		return backup, nil, e
	}
	if e = j.Record(slot, anchor); e != nil {
		return backup, nil, e
	}
	if log != nil {
		log("waiting for Nova snapshot")
	}
	im, e := c.WaitImage(ctx, anchor, backup.VolumeBacked)
	if e != nil {
		return backup, nil, e
	}
	if im.Name != name || im.Property("srk_run") != j.ID || im.Property("srk_proxy") != j.Owner {
		return backup, nil, errors.New("Nova snapshot ownership mismatch")
	}
	if e = c.adoptSnapshotMapping(ctx, j); e != nil {
		return backup, nil, e
	}
	type snapshotDisk struct {
		id, volume, device string
		boot, size         int
	}
	snapshots := []snapshotDisk{}
	exports := []Export{}
	if backup.VolumeBacked {
		mapping, e := parseBDM(im)
		if e != nil {
			return backup, nil, e
		}
		seen := map[string]bool{}
		roots := 0
		for _, m := range mapping {
			if m.SourceType != "snapshot" || m.DestinationType != "volume" || !model.CloudUUID(m.SnapshotID) || m.BootIndex != 0 && m.BootIndex != -1 {
				return backup, nil, errors.New("unsupported Nova disk mapping; no partial backup")
			}
			snap, e := c.WaitSnapshot(ctx, m.SnapshotID)
			if e != nil {
				return backup, nil, e
			}
			if seen[snap.VolumeID] {
				return backup, nil, errors.New("duplicate volume snapshot")
			}
			seen[snap.VolumeID] = true
			if m.BootIndex == 0 {
				roots++
			}
			snapshots = append(snapshots, snapshotDisk{snap.ID, snap.VolumeID, m.Device, m.BootIndex, snap.Size})
		}
		if roots != 1 || len(seen) != len(source.Volumes) {
			return backup, nil, errors.New("snapshot does not include exactly one boot disk and all attached volumes")
		}
		for _, v := range source.Volumes {
			if !seen[v.ID] {
				return backup, nil, errors.New("attached source volume omitted from snapshot")
			}
		}
	} else {
		if im.DiskFormat != "raw" && im.DiskFormat != "qcow2" {
			return backup, nil, errors.New("unsupported root image format")
		}
		exports = append(exports, Export{Image: im, Disk: model.VMDisk{File: "disks/000-root.img", BootIndex: 0, SizeGiB: max(backup.Flavor.Disk, im.MinDisk), Bytes: im.Size, DiskFormat: im.DiskFormat, Properties: safeImageProperties(im)}})
		for i, snapID := range imageDataSnapshots {
			s, e := c.WaitSnapshot(ctx, snapID)
			if e != nil {
				return backup, nil, e
			}
			snapshots = append(snapshots, snapshotDisk{s.ID, source.Volumes[i].ID, "", -1, s.Size})
		}
	}
	for i, s := range snapshots {
		if log != nil {
			log("exporting volume " + s.volume)
		}
		image, e := c.exportSnapshot(ctx, j, s.id, i)
		if e != nil {
			return backup, nil, e
		}
		if image.DiskFormat != "raw" || image.Size != int64(s.size)*(1<<30) {
			return backup, nil, errors.New("Cinder RAW export size or format does not match volume geometry")
		}
		exports = append(exports, Export{Image: image, Disk: model.VMDisk{File: fmt.Sprintf("disks/%03d-volume.img", len(exports)), VolumeID: s.volume, Device: s.device, BootIndex: s.boot, SizeGiB: s.size, Bytes: image.Size, DiskFormat: image.DiskFormat, Properties: safeImageProperties(image)}})
	}
	// Reject changes to attachment set or cold-only state during snapshot creation.
	after, e := c.Server(ctx, vmID)
	if e != nil {
		return backup, nil, e
	}
	if after.Status != source.Status || !sameVolumes(source, after) {
		return backup, nil, errors.New("source VM state or attachment set changed during snapshot")
	}
	backup.SnapshotAt = time.Now().UTC()
	for _, x := range exports {
		backup.Disks = append(backup.Disks, x.Disk)
	}
	return backup, exports, nil
}
func sameVolumes(a, b Server) bool {
	if len(a.Volumes) != len(b.Volumes) {
		return false
	}
	set := map[string]bool{}
	for _, v := range a.Volumes {
		set[v.ID] = true
	}
	for _, v := range b.Volumes {
		if !set[v.ID] {
			return false
		}
	}
	return true
}

// Nova 2.1 returns an empty body with Location; newer versions return image_id.
type optionalImageResponse struct{ Out *string }

func (o *optionalImageResponse) UnmarshalJSON(b []byte) error {
	var v struct {
		ID string `json:"image_id"`
	}
	if e := json.Unmarshal(b, &v); e != nil {
		return e
	}
	*o.Out = v.ID
	return nil
}
