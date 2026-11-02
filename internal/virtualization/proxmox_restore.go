// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"io"
	"mime/multipart"
	"net/url"
	"strconv"
	"strings"
)

func (c *Proxmox) runTask(ctx context.Context, j *Journal, node, method, path string, params url.Values) error {
	slot, e := j.Intent("pve-task", node, false)
	if e != nil {
		return e
	}
	var upid string
	if e = c.form(ctx, "PVE", method, path, params, &upid); e != nil {
		return e
	}
	if e = j.RecordOpaque(slot, upid); e != nil {
		return e
	}
	if e = c.task(ctx, node, upid); e != nil {
		return e
	}
	j.Resources[slot].Deleted = true
	return j.Save()
}
func (c *Proxmox) upload(ctx context.Context, j *Journal, node, storage, name string, d model.VMDisk, input io.ReadCloser) error {
	defer input.Close()
	var prefix bytes.Buffer
	writer := multipart.NewWriter(&prefix)
	if e := writer.SetBoundary("srk-" + j.ID); e != nil {
		return e
	}
	for key, value := range map[string]string{"content": "import", "checksum": d.SHA256, "checksum-algorithm": "sha256"} {
		if e := writer.WriteField(key, value); e != nil {
			return e
		}
	}
	if _, e := writer.CreateFormFile("filename", name); e != nil {
		return e
	}
	head := append([]byte(nil), prefix.Bytes()...)
	if e := writer.Close(); e != nil {
		return e
	}
	tail := append([]byte(nil), prefix.Bytes()[len(head):]...)
	body := io.MultiReader(bytes.NewReader(head), input, bytes.NewReader(tail))
	slot, e := j.Intent("pve-task", node, false)
	if e != nil {
		return e
	}
	response, e := c.request(ctx, "PVE", "POST", "/nodes/"+node+"/storage/"+storage+"/upload", body, writer.FormDataContentType(), int64(len(head)+len(tail))+d.Bytes)
	if e != nil {
		return e
	}
	defer response.Body.Close()
	var out struct {
		Data string `json:"data"`
	}
	if e = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&out); e != nil {
		return errors.New("invalid PVE upload receipt")
	}
	if e = input.Close(); e != nil {
		return e
	}
	if e = j.RecordOpaque(slot, out.Data); e != nil {
		return e
	}
	if e = c.task(ctx, node, out.Data); e != nil {
		return e
	}
	j.Resources[slot].Deleted = true
	return j.Save()
}
func (c *Proxmox) Restore(ctx context.Context, j *Journal, meta model.VMBackup, r model.VMRestoreRequest, open func(model.VMDisk) (io.ReadCloser, error)) (model.VMRestoreResult, error) {
	if meta.Archive != nil {
		return c.restoreDAV(ctx, j, meta, r)
	}
	result := model.VMRestoreResult{Name: r.Name}
	if e := r.ValidateFor("proxmox"); e != nil {
		return result, e
	}
	node := r.FlavorID
	target := r.StorageDomainID
	imports := r.ImportStorageID
	if !nativeName.MatchString(node) || !nativeName.MatchString(target) || !nativeName.MatchString(imports) {
		return result, errors.New("PVE target node, image storage and import storage are required")
	}
	for _, storage := range []string{target, imports} {
		var status map[string]any
		if e := c.form(ctx, "PVE", "GET", "/nodes/"+node+"/storage/"+storage+"/status", nil, &status); e != nil {
			return result, e
		}
		if pvNumber(status["active"], 0) != 1 {
			return result, errors.New("PVE restore storage is inactive")
		}
		needed := "images"
		if storage == imports {
			needed = "import"
		}
		if !slicesContains(strings.Split(pvString(status["content"]), ","), needed) {
			return result, errors.New("PVE storage lacks required images/import content")
		}
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
			return result, errors.New("PVE target bridge is unavailable")
		}
	}
	var raw json.RawMessage
	if e := c.form(ctx, "PVE", "GET", "/cluster/nextid", nil, &raw); e != nil {
		return result, e
	}
	id, e := parseAPIID(raw)
	if e != nil || id == meta.ServerID {
		return result, errors.New("PVE fresh VM ID could not be allocated")
	}
	j.SourceServer = meta.ServerID
	if e = j.Save(); e != nil {
		return result, e
	}
	slot, e := j.Intent("pve-vm", node, true)
	if e != nil {
		return result, e
	}
	if e = j.RecordOpaque(slot, id); e != nil {
		return result, e
	}
	params := url.Values{"vmid": {id}, "name": {j.Name("vm")}, "description": {pveOwner(j)}, "start": {"0"}, "memory": {strconv.Itoa(meta.Flavor.RAM)}}
	for _, key := range pveHardware {
		if value := meta.Metadata[key]; value != "" {
			params.Set(key, value)
		}
	}
	for i, bridge := range r.Networks {
		kind := meta.Metadata[fmt.Sprintf("net%d_model", i)]
		if kind == "" {
			kind = "virtio"
		}
		if !nativeName.MatchString(kind) {
			return result, errors.New("invalid saved PVE network model")
		}
		params.Set(fmt.Sprintf("net%d", i), kind+",bridge="+bridge)
	}
	if e = c.runTask(ctx, j, node, "POST", "/nodes/"+node+"/qemu", params); e != nil {
		return result, e
	}
	var boot string
	for i, disk := range meta.Disks {
		if !nativeDrive.MatchString(disk.Device) {
			return result, errors.New("invalid saved PVE disk bus")
		}
		name := j.Name(fmt.Sprintf("disk-%02d.raw", i))
		volid := imports + ":import/" + name
		slot, e := j.Intent("pve-import", node+"/"+imports+"/"+name, false)
		if e != nil {
			return result, e
		}
		if e = j.RecordOpaque(slot, volid); e != nil {
			return result, e
		}
		input, e := open(disk)
		if e != nil {
			return result, e
		}
		if e = c.upload(ctx, j, node, imports, name, disk, input); e != nil {
			return result, e
		}
		spec := target + ":0,import-from=" + volid
		allowed := map[string]bool{"efitype": true, "pre-enrolled-keys": true, "version": true, "discard": true, "ssd": true, "iothread": true, "cache": true, "aio": true, "replicate": true, "ro": true}
		for key, value := range disk.Properties {
			if allowed[key] {
				if strings.ContainsAny(value, ",\x00\r\n") {
					return result, errors.New("invalid saved disk options")
				}
				spec += "," + key + "=" + value
			}
		}
		if e = c.runTask(ctx, j, node, "POST", "/nodes/"+node+"/qemu/"+id+"/config", url.Values{disk.Device: {spec}}); e != nil {
			return result, e
		}
		if disk.BootIndex == 0 {
			boot = disk.Device
		}
	}
	if boot == "" {
		return result, errors.New("PVE boot disk is missing")
	}
	if e = c.runTask(ctx, j, node, "POST", "/nodes/"+node+"/qemu/"+id+"/config", url.Values{"name": {r.Name}, "boot": {"order=" + boot}}); e != nil {
		return result, e
	}
	if e = c.runTask(ctx, j, node, "POST", "/nodes/"+node+"/qemu/"+id+"/status/start", nil); e != nil {
		return result, e
	}
	cfg, e := c.config(ctx, node, id)
	if e != nil {
		return result, e
	}
	if pvString(cfg["description"]) != pveOwner(j) || pvString(cfg["name"]) != r.Name || pvNumber(cfg["memory"], 0) != meta.Flavor.RAM {
		return result, errors.New("restored PVE VM identity/hardware differs")
	}
	for _, disk := range meta.Disks {
		parts := pveParts(pvString(cfg[disk.Device]))
		if !strings.HasPrefix(parts["file"], target+":") {
			return result, errors.New("restored PVE disk is not attached to target storage")
		}
		result.Volumes = append(result.Volumes, parts["file"])
	}
	for i, bridge := range r.Networks {
		if pveParts(pvString(cfg[fmt.Sprintf("net%d", i)]))["bridge"] != bridge {
			return result, errors.New("restored PVE network differs")
		}
	}
	var state struct {
		Status string `json:"status"`
	}
	if e = c.form(ctx, "PVE", "GET", "/nodes/"+node+"/qemu/"+id+"/status/current", nil, &state); e != nil {
		return result, e
	}
	if state.Status != "running" {
		return result, errors.New("new PVE VM is not running")
	}
	result.ServerID = id
	result.Status = state.Status
	result.ControlPlaneVerified = true
	j.Restore = &result
	return result, j.Save()
}
func (c *Proxmox) Cleanup(ctx context.Context, j *Journal, log func(string)) error {
	// Discover the owned PBS point even when the vzdump response was lost.
	if j.Kind == "backup" && c.base.profile.DAVMountPath == "" {
		has := false
		for _, r := range j.Resources {
			has = has || r.Kind == "pbs-snapshot"
		}
		if !has {
			snapshot, e := c.ownedSnapshot(ctx, j)
			if e == nil {
				slot, e := j.Intent("pbs-snapshot", j.SourceServer, false)
				if e != nil {
					return e
				}
				if e = j.RecordOpaque(slot, strconv.FormatInt(snapshot.Time, 10)); e != nil {
					return e
				}
			}
		}
	}
	for i := len(j.Resources) - 1; i >= 0; i-- {
		r := &j.Resources[i]
		if r.Deleted || j.Success && r.Keep {
			continue
		}
		var e error
		if r.ID == "" {
			if r.Kind == "pve-task" || r.Kind == "pve-backup-task" {
				return errors.New("PVE task create outcome is unresolved; inspect native tasks before cleanup")
			}
			return errors.New("unresolved PVE resource intent")
		}
		switch r.Kind {
		case "dav-session":
			if c.DAV == nil {
				return errors.New("WebDAV cleanup requires assigned media bridge")
			}
			e = c.DAV.Close(ctx, j)
		case "pve-task", "pve-backup-task":
			if !nativeName.MatchString(r.Name) || !strings.HasPrefix(r.ID, "UPID:"+r.Name+":") {
				return errors.New("invalid PVE task ownership")
			}
			var state struct {
				Status string `json:"status"`
			}
			path := "/nodes/" + r.Name + "/tasks/" + url.PathEscape(r.ID)
			e = c.form(ctx, "PVE", "GET", path+"/status", nil, &state)
			if e != nil {
				break
			}
			if state.Status != "stopped" {
				e = c.form(ctx, "PVE", "DELETE", path, nil, nil)
				if e == nil {
					e = c.wait(ctx, func() (bool, error) {
						err := c.form(ctx, "PVE", "GET", path+"/status", nil, &state)
						return state.Status == "stopped", err
					})
				}
			}
		case "pbs-snapshot":
			if !numericID.MatchString(r.Name) {
				return errors.New("invalid PBS snapshot VM scope")
			}
			at, err := strconv.ParseInt(r.ID, 10, 64)
			if err != nil || at < 1 {
				return errors.New("invalid PBS snapshot time")
			}
			list, err := c.snapshots(ctx, r.Name)
			if err != nil {
				return err
			}
			found := false
			for _, snapshot := range list {
				if snapshot.Time == at {
					if strings.TrimSpace(snapshot.Comment) != pveOwner(j) {
						return errors.New("foreign PBS snapshot; deletion refused")
					}
					found = true
				}
			}
			if found {
				e = c.form(ctx, "PBS", "DELETE", c.pbsPath("snapshots")+"?"+c.pbsParams(r.Name, at).Encode(), nil, nil)
			}
		case "pve-import":
			fields := strings.Split(r.Name, "/")
			if len(fields) != 3 || !nativeName.MatchString(fields[0]) || !nativeName.MatchString(fields[1]) || !strings.HasPrefix(fields[2], j.Name("disk-")) || r.ID != fields[1]+":import/"+fields[2] {
				return errors.New("foreign PVE import file; deletion refused")
			}
			e = c.form(ctx, "PVE", "DELETE", "/nodes/"+fields[0]+"/storage/"+fields[1]+"/content/"+url.PathEscape(r.ID), nil, nil)
		case "pve-vm":
			if !nativeName.MatchString(r.Name) || !numericID.MatchString(r.ID) || r.ID == j.SourceServer {
				return errors.New("foreign/source PVE VM; deletion refused")
			}
			cfg, err := c.config(ctx, r.Name, r.ID)
			if isMissing(err) {
				break
			}
			if err != nil {
				return err
			}
			if pvString(cfg["description"]) != pveOwner(j) {
				return errors.New("PVE VM ownership mismatch; deletion refused")
			}
			var state struct {
				Status string `json:"status"`
			}
			if err = c.form(ctx, "PVE", "GET", "/nodes/"+r.Name+"/qemu/"+r.ID+"/status/current", nil, &state); err != nil {
				return err
			}
			if state.Status != "stopped" {
				var upid string
				if err = c.form(ctx, "PVE", "POST", "/nodes/"+r.Name+"/qemu/"+r.ID+"/status/stop", nil, &upid); err != nil {
					return err
				}
				if err = c.task(ctx, r.Name, upid); err != nil {
					return err
				}
			}
			var upid string
			e = c.form(ctx, "PVE", "DELETE", "/nodes/"+r.Name+"/qemu/"+r.ID, url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}}, &upid)
			if e == nil {
				e = c.task(ctx, r.Name, upid)
			}
		default:
			return errors.New("unknown PVE journal resource")
		}
		if e != nil && !isMissing(e) {
			j.LastError = e.Error()
			_ = j.Save()
			return e
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
