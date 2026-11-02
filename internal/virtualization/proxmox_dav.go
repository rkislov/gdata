// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"github.com/klauspost/compress/zstd"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (c *Proxmox) inspectDAVStorage(ctx context.Context, node string) error {
	p := c.base.profile
	var cfg map[string]any
	if e := c.form(ctx, "PVE", "GET", "/storage/"+p.BackupStorage, nil, &cfg); e != nil {
		return e
	}
	if pvString(cfg["type"]) != "dir" || filepath.Clean(pvString(cfg["path"])) != filepath.Clean(p.DAVMountPath) || !slicesContains(strings.Split(pvString(cfg["content"]), ","), "backup") || pvString(cfg["is_mountpoint"]) != "1" && pvString(cfg["is_mountpoint"]) != p.DAVMountPath {
		return errors.New("PVE WebDAV backup storage must be directory storage at dav_mount_path with backup content and is_mountpoint protection")
	}
	var status map[string]any
	if e := c.form(ctx, "PVE", "GET", "/nodes/"+node+"/storage/"+p.BackupStorage+"/status", nil, &status); e != nil {
		return e
	}
	if pvNumber(status["active"], 0) != 1 {
		return errors.New("PVE WebDAV mount is not active")
	}
	return nil
}
func (c *Proxmox) backupDAV(ctx context.Context, j *Journal, profile, id, policy, dir string, report func(int64, int64)) (model.VMBackup, error) {
	meta, e := c.Inspect(ctx, id, profile, policy)
	if e != nil {
		return meta, e
	}
	if c.DAV == nil {
		return meta, errors.New("assigned media WebDAV bridge required")
	}
	j.SourceServer = id
	if e = j.Save(); e != nil {
		return meta, e
	}
	slot, e := j.Intent("dav-session", id, false)
	if e != nil {
		return meta, e
	}
	if _, e = c.DAV.Create(ctx, j, false, c.base.profile.DAVUser, c.base.profile.StagingMaxBytes); e != nil {
		return meta, e
	}
	if e = j.RecordOpaque(slot, j.ID); e != nil {
		return meta, e
	}
	node := meta.ProjectID
	params := url.Values{"vmid": {id}, "dumpdir": {filepath.Join(c.base.profile.DAVMountPath, "staging", j.ID)}, "mode": {"snapshot"}, "compress": {"zstd"}, "remove": {"0"}, "notes-template": {pveOwner(j)}}
	if e = c.runTask(ctx, j, node, "POST", "/nodes/"+node+"/vzdump", params); e != nil {
		return meta, e
	}
	var archive model.VMArchive
	if e = c.wait(ctx, func() (bool, error) {
		var err error
		archive, err = c.DAV.Seal(ctx, j)
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, nil
		}
		return true, nil
	}); e != nil {
		return meta, e
	}
	if !strings.HasPrefix(archive.Name, "vzdump-qemu-"+id+"-") || !strings.HasSuffix(archive.Name, ".vma.zst") {
		return meta, errors.New("DAV archive does not belong to the requested PVE VM")
	}
	input, e := c.DAV.Open(ctx, j, archive.Name)
	if e != nil {
		return meta, e
	}
	defer input.Close()
	decoder, e := zstd.NewReader(input, zstd.WithDecoderMaxMemory(256<<20), zstd.WithDecoderConcurrency(1))
	if e != nil {
		return meta, e
	}
	defer decoder.Close()
	out, e := os.OpenFile(filepath.Join(dir, "vm.vma"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return meta, e
	}
	count, err := copyWithProgress(ctx, out, io.LimitReader(decoder, c.base.profile.StagingMaxBytes+1), 0, archive.Bytes, report)
	syncErr := out.Sync()
	closeErr := out.Close()
	if err != nil {
		return meta, errors.New("incomplete/corrupt compressed VMA archive")
	}
	if syncErr != nil {
		return meta, syncErr
	}
	if closeErr != nil {
		return meta, closeErr
	}
	if count > c.base.profile.StagingMaxBytes {
		return meta, errors.New("VMA archive exceeds staging quota")
	}
	devices, e := ValidateVMA(filepath.Join(dir, "vm.vma"))
	if e != nil {
		return meta, e
	}
	if len(devices) != len(meta.Disks) {
		return meta, errors.New("VMA does not cover all source VM disks")
	}
	for i, disk := range meta.Disks {
		entry, ok := devices["drive-"+disk.Device]
		if !ok {
			return meta, errors.New("VMA omitted source disk")
		}
		meta.Disks[i].File = ""
		meta.Disks[i].Bytes = entry.Bytes
		meta.Disks[i].SizeGiB = int((entry.Bytes + (1 << 30) - 1) >> 30)
	}
	current, e := c.Inspect(ctx, id, profile, policy)
	if e != nil {
		return meta, e
	}
	if current.Status != meta.Status || current.Flavor != meta.Flavor || !equalStringsMap(current.Metadata, meta.Metadata) || len(current.Disks) != len(meta.Disks) {
		return meta, errors.New("PVE source configuration changed during archive backup")
	}
	for i, d := range meta.Disks {
		if current.Disks[i].Device != d.Device || current.Disks[i].VolumeID != d.VolumeID || current.Disks[i].BootIndex != d.BootIndex || !equalStringsMap(current.Disks[i].Properties, d.Properties) {
			return meta, errors.New("PVE source disk changed during archive backup")
		}
	}
	meta.Archive = &model.VMArchive{File: "vm.vma", Name: strings.TrimSuffix(archive.Name, ".zst"), Format: "vma", Bytes: count}
	meta.SnapshotAt = time.Now().UTC()
	return meta, nil
}
func (c *Proxmox) restoreDAV(ctx context.Context, j *Journal, meta model.VMBackup, r model.VMRestoreRequest) (model.VMRestoreResult, error) {
	result := model.VMRestoreResult{Name: r.Name}
	if e := r.ValidateFor("proxmox"); e != nil {
		return result, e
	}
	if meta.Archive == nil || c.DAV == nil {
		return result, errors.New("native VMA archive and assigned WebDAV media required")
	}
	node := r.FlavorID
	if !nativeName.MatchString(node) || !nativeName.MatchString(r.StorageDomainID) {
		return result, errors.New("target PVE node and image storage are required")
	}
	if e := c.inspectDAVStorage(ctx, node); e != nil {
		return result, e
	}
	var storage map[string]any
	if e := c.form(ctx, "PVE", "GET", "/nodes/"+node+"/storage/"+r.StorageDomainID+"/status", nil, &storage); e != nil {
		return result, e
	}
	if pvNumber(storage["active"], 0) != 1 || !slicesContains(strings.Split(pvString(storage["content"]), ","), "images") {
		return result, errors.New("target PVE image storage is inactive or unsupported")
	}
	var networks []map[string]any
	if e := c.form(ctx, "PVE", "GET", "/nodes/"+node+"/network", nil, &networks); e != nil {
		return result, e
	}
	for _, bridge := range r.Networks {
		found := false
		for _, network := range networks {
			if pvString(network["iface"]) == bridge && pvNumber(network["active"], 0) == 1 {
				found = true
			}
		}
		if !found {
			return result, errors.New("target PVE bridge is unavailable")
		}
	}
	id, e := c.nextVMID(ctx, meta.ServerID)
	if e != nil {
		return result, e
	}
	j.SourceServer = meta.ServerID
	if e = j.Save(); e != nil {
		return result, e
	}
	slot, e := j.Intent("dav-session", meta.ServerID, false)
	if e != nil {
		return result, e
	}
	if _, e = c.DAV.Create(ctx, j, true, c.base.profile.DAVUser, c.base.profile.StagingMaxBytes); e != nil {
		return result, e
	}
	if e = j.RecordOpaque(slot, j.ID); e != nil {
		return result, e
	}
	slot, e = j.Intent("pve-vm", node, true)
	if e != nil {
		return result, e
	}
	if e = j.RecordOpaque(slot, id); e != nil {
		return result, e
	}
	archive := filepath.Join(c.base.profile.DAVMountPath, "restore", j.ID, meta.Archive.Name)
	params := url.Values{"vmid": {id}, "archive": {archive}, "storage": {r.StorageDomainID}, "unique": {"1"}, "start": {"0"}}
	if e = c.runTask(ctx, j, node, "POST", "/nodes/"+node+"/qemu", params); e != nil {
		return result, e
	}
	// Native restore is stopped. Replace all network interfaces and identity values
	// before the first boot, and tag ownership immediately for cleanup.
	cfg, e := c.config(ctx, node, id)
	if e != nil {
		return result, e
	}
	deleteKeys := []string{}
	for key := range cfg {
		if strings.HasPrefix(key, "net") {
			deleteKeys = append(deleteKeys, key)
		}
	}
	update := url.Values{"name": {r.Name}, "description": {pveOwner(j)}, "vmgenid": {"1"}, "smbios1": {"uuid=" + cloudUUIDFromRun(j.ID)}}
	if len(deleteKeys) > 0 {
		update.Set("delete", strings.Join(deleteKeys, ","))
	}
	if e = c.runTask(ctx, j, node, "POST", "/nodes/"+node+"/qemu/"+id+"/config", update); e != nil {
		return result, e
	}
	update = url.Values{}
	for i, bridge := range r.Networks {
		kind := meta.Metadata[fmt.Sprintf("net%d_model", i)]
		if kind == "" {
			kind = "virtio"
		}
		if !nativeName.MatchString(bridge) || !nativeName.MatchString(kind) {
			return result, errors.New("invalid PVE target bridge or NIC model")
		}
		update.Set(fmt.Sprintf("net%d", i), kind+",bridge="+bridge)
	}
	if e = c.runTask(ctx, j, node, "POST", "/nodes/"+node+"/qemu/"+id+"/config", update); e != nil {
		return result, e
	}
	if e = c.runTask(ctx, j, node, "POST", "/nodes/"+node+"/qemu/"+id+"/status/start", nil); e != nil {
		return result, e
	}
	cfg, e = c.config(ctx, node, id)
	if e != nil {
		return result, e
	}
	for _, disk := range meta.Disks {
		value := pveParts(pvString(cfg[disk.Device]))["file"]
		if !strings.HasPrefix(value, r.StorageDomainID+":") {
			return result, errors.New("restored VMA disk attachment differs")
		}
		result.Volumes = append(result.Volumes, value)
	}
	for i, bridge := range r.Networks {
		if pveParts(pvString(cfg[fmt.Sprintf("net%d", i)]))["bridge"] != bridge {
			return result, errors.New("restored PVE bridge differs")
		}
	}
	var state struct {
		Status string `json:"status"`
	}
	if e = c.form(ctx, "PVE", "GET", "/nodes/"+node+"/qemu/"+id+"/status/current", nil, &state); e != nil {
		return result, e
	}
	if state.Status != "running" || pvString(cfg["description"]) != pveOwner(j) || pvNumber(cfg["memory"], 0) != meta.Flavor.RAM || pvNumber(cfg["cores"], 1)*pvNumber(cfg["sockets"], 1) != meta.Flavor.VCPUs {
		return result, errors.New("restored PVE VM was not verified")
	}
	result.ServerID = id
	result.Status = state.Status
	result.ControlPlaneVerified = true
	j.Restore = &result
	return result, j.Save()
}
func (c *Proxmox) nextVMID(ctx context.Context, source string) (string, error) {
	var raw json.RawMessage
	if e := c.form(ctx, "PVE", "GET", "/cluster/nextid", nil, &raw); e != nil {
		return "", e
	}
	id, e := parseAPIID(raw)
	if e != nil || id == source {
		return "", errors.New("fresh PVE VM ID required")
	}
	return id, nil
}
