// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Proxmox struct {
	*nativeHTTP
	DAV DAVStore
}

func NewProxmox(p Profile) (*Proxmox, error) {
	if p.Kind != "proxmox" {
		return nil, errors.New("Proxmox profile required")
	}
	c, e := newNativeHTTP(p)
	if e != nil {
		return nil, e
	}
	if p.DAVMountPath == "" && (c.base.credentials["pbs_token_id"] == "" || c.base.credentials["pbs_secret"] == "" || strings.ContainsAny(c.base.credentials["pbs_token_id"]+c.base.credentials["pbs_secret"], "\r\n")) {
		c.Close()
		return nil, errors.New("PBS API token credentials required")
	}
	return &Proxmox{nativeHTTP: c}, nil
}
func pvString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	}
	return ""
}
func pvNumber(v any, def int) int {
	n, e := strconv.Atoi(pvString(v))
	if e != nil {
		return def
	}
	return n
}
func (c *Proxmox) node(ctx context.Context, id string) (string, error) {
	if !numericID.MatchString(id) {
		return "", errors.New("invalid PVE VM ID")
	}
	vms, e := c.base.Inventory(ctx)
	if e != nil {
		return "", e
	}
	for _, vm := range vms {
		if vm.ID == id && nativeName.MatchString(vm.Node) {
			return vm.Node, nil
		}
	}
	return "", errors.New("PVE QEMU VM not found")
}
func (c *Proxmox) config(ctx context.Context, node, id string) (map[string]any, error) {
	var cfg map[string]any
	e := c.form(ctx, "PVE", "GET", "/nodes/"+node+"/qemu/"+id+"/config", nil, &cfg)
	return cfg, e
}

var pveHardware = []string{"memory", "cores", "sockets", "cpu", "bios", "machine", "scsihw", "ostype", "numa", "tablet"}

func pveParts(value string) map[string]string {
	out := map[string]string{}
	for i, part := range strings.Split(value, ",") {
		key, val, ok := strings.Cut(part, "=")
		if !ok && i == 0 {
			out["file"] = part
		} else if ok {
			out[key] = val
		}
	}
	return out
}
func pveVersion(version string) bool {
	major, _, _ := strings.Cut(version, ".")
	n, e := strconv.Atoi(major)
	return e == nil && n >= 9
}
func (c *Proxmox) Inspect(ctx context.Context, id, profile, policy string) (model.VMBackup, error) {
	meta := model.VMBackup{}
	node, e := c.node(ctx, id)
	if e != nil {
		return meta, e
	}
	var version struct {
		Version string `json:"version"`
	}
	if e = c.form(ctx, "PVE", "GET", "/version", nil, &version); e != nil {
		return meta, e
	}
	if c.base.profile.DAVMountPath == "" && !pveVersion(version.Version) {
		return meta, errors.New("HTTPS disk import adapter requires Proxmox VE 9 or newer")
	}
	cfg, e := c.config(ctx, node, id)
	if e != nil {
		return meta, e
	}
	if pvString(cfg["lock"]) != "" || pvNumber(cfg["template"], 0) != 0 {
		return meta, errors.New("PVE VM is locked or is a template")
	}
	var state struct {
		Status string `json:"status"`
	}
	if e = c.form(ctx, "PVE", "GET", "/nodes/"+node+"/qemu/"+id+"/status/current", nil, &state); e != nil {
		return meta, e
	}
	if state.Status != "running" && state.Status != "stopped" || policy == "cold-only" && state.Status != "stopped" {
		return meta, errors.New("PVE source must be running/stopped; cold-only requires stopped")
	}
	for key, v := range cfg {
		if pvString(v) != "" && (strings.HasPrefix(key, "hostpci") || strings.HasPrefix(key, "usb") || key == "args" || key == "hookscript" || key == "virtiofs0") {
			return meta, errors.New("PVE passthrough, custom args, hookscript and virtiofs are unsupported")
		}
	}
	meta = model.VMBackup{Provider: "proxmox", Profile: profile, ProjectID: node, ServerID: id, Name: pvString(cfg["name"]), Status: state.Status, Flavor: model.VMFlavor{ID: node, VCPUs: pvNumber(cfg["cores"], 1) * pvNumber(cfg["sockets"], 1), RAM: pvNumber(cfg["memory"], 512)}, Metadata: map[string]string{}, Consistency: "proxmox-crash-consistent", SnapshotAt: time.Now().UTC()}
	if state.Status == "stopped" {
		meta.Consistency = "proxmox-cold-consistent"
	}
	for _, key := range pveHardware {
		if value := pvString(cfg[key]); value != "" {
			meta.Metadata[key] = value
		}
	}
	var devices []string
	for key, v := range cfg {
		if !nativeDrive.MatchString(key) {
			continue
		}
		parts := pveParts(pvString(v))
		if parts["media"] == "cdrom" || parts["file"] == "none" {
			continue
		}
		if strings.Contains(parts["file"], "cloudinit") {
			return meta, errors.New("PVE cloud-init media must be detached before a complete SRK disk backup")
		}
		if strings.HasPrefix(parts["file"], "/") || !strings.Contains(parts["file"], ":") || parts["backup"] == "0" {
			return meta, errors.New("PVE host device or backup-excluded disk cannot be backed up completely")
		}
		devices = append(devices, key)
	}
	sort.Strings(devices)
	if len(devices) == 0 || len(devices) > 32 {
		return meta, errors.New("PVE requires 1–32 backed-up disks")
	}
	boot := pvString(cfg["bootdisk"])
	if order := pveParts(pvString(cfg["boot"]))["order"]; order != "" {
		for _, item := range strings.Split(order, ";") {
			if slicesContains(devices, item) && item != "efidisk0" && item != "tpmstate0" {
				boot = item
				break
			}
		}
	}
	if !slicesContains(devices, boot) {
		for _, d := range devices {
			if d != "efidisk0" && d != "tpmstate0" {
				boot = d
				break
			}
		}
	}
	for i, device := range devices {
		index := -1
		if device == boot {
			index = 0
		}
		parts := pveParts(pvString(cfg[device]))
		properties := map[string]string{}
		for _, key := range []string{"efitype", "pre-enrolled-keys", "version", "discard", "ssd", "iothread", "cache", "aio", "replicate", "ro"} {
			if value, ok := parts[key]; ok {
				properties[key] = value
			}
		}
		meta.Disks = append(meta.Disks, model.VMDisk{File: fmt.Sprintf("%03d-%s.img", i, device), Device: device, VolumeID: parts["file"], BootIndex: index, DiskFormat: "raw", Properties: properties})
	}
	for i := 0; i < 16; i++ {
		if value := pvString(cfg[fmt.Sprintf("net%d", i)]); value != "" {
			parts := pveParts(value)
			bridge := parts["bridge"]
			if !nativeName.MatchString(bridge) {
				return meta, errors.New("PVE NIC requires a supported bridge")
			}
			meta.Networks = append(meta.Networks, model.VMNetwork{ID: bridge})
			modelName, _, ok := strings.Cut(strings.Split(value, ",")[0], "=")
			if !ok {
				modelName = "virtio"
			}
			if !nativeName.MatchString(modelName) {
				return meta, errors.New("invalid PVE network model")
			}
			meta.Metadata[fmt.Sprintf("net%d_model", i)] = modelName
		}
	}
	if c.base.profile.DAVMountPath != "" {
		return meta, c.inspectDAVStorage(ctx, node)
	}
	var storage map[string]any
	if e = c.form(ctx, "PVE", "GET", "/storage/"+c.base.profile.BackupStorage, nil, &storage); e != nil {
		return meta, e
	}
	p := c.base.profile
	u, _ := url.Parse(p.PBSEndpoint)
	if pvString(storage["type"]) != "pbs" || pvString(storage["datastore"]) != p.PBSStore || pvString(storage["namespace"]) != p.PBSNamespace || pvString(storage["server"]) != u.Hostname() {
		return meta, errors.New("PVE backup storage does not match the configured PBS datastore/namespace/server")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	configured := pvString(storage["port"])
	if configured == "" {
		configured = "8007"
	}
	if port != configured {
		return meta, errors.New("PVE PBS port mismatch")
	}
	return meta, nil
}
func slicesContains(items []string, s string) bool {
	for _, v := range items {
		if v == s {
			return true
		}
	}
	return false
}
func (c *Proxmox) task(ctx context.Context, node, upid string) error {
	if !strings.HasPrefix(upid, "UPID:"+node+":") || len(upid) > 512 || strings.ContainsAny(upid, "\x00\r\n") {
		return errors.New("invalid PVE task receipt")
	}
	return c.wait(ctx, func() (bool, error) {
		var status struct {
			Status string `json:"status"`
			Exit   string `json:"exitstatus"`
		}
		if e := c.form(ctx, "PVE", "GET", "/nodes/"+node+"/tasks/"+url.PathEscape(upid)+"/status", nil, &status); e != nil {
			return false, e
		}
		if status.Status == "stopped" {
			if status.Exit != "OK" {
				return false, errors.New("PVE task failed; inspect native task log")
			}
			return true, nil
		}
		return false, nil
	})
}
func (c *Proxmox) pbsPath(action string) string {
	return "/admin/datastore/" + url.PathEscape(c.base.profile.PBSStore) + "/" + action
}
func (c *Proxmox) pbsParams(id string, at int64) url.Values {
	p := url.Values{"backup-type": {"vm"}, "backup-id": {id}}
	if at > 0 {
		p.Set("backup-time", strconv.FormatInt(at, 10))
	}
	if c.base.profile.PBSNamespace != "" {
		p.Set("ns", c.base.profile.PBSNamespace)
	}
	return p
}

type pbsSnapshot struct {
	Time    int64  `json:"backup-time"`
	Comment string `json:"comment"`
	Owner   string `json:"owner"`
}

func (c *Proxmox) snapshots(ctx context.Context, id string) ([]pbsSnapshot, error) {
	var out []pbsSnapshot
	e := c.form(ctx, "PBS", "GET", c.pbsPath("snapshots")+"?"+c.pbsParams(id, 0).Encode(), nil, &out)
	return out, e
}
func pveOwner(j *Journal) string { return "srk_run=" + j.ID + ";srk_proxy=" + j.Owner }
func (c *Proxmox) ownedSnapshot(ctx context.Context, j *Journal) (pbsSnapshot, error) {
	list, e := c.snapshots(ctx, j.SourceServer)
	if e != nil {
		return pbsSnapshot{}, e
	}
	var found []pbsSnapshot
	for _, v := range list {
		if strings.TrimSpace(v.Comment) == pveOwner(j) {
			found = append(found, v)
		}
	}
	if len(found) != 1 {
		return pbsSnapshot{}, errors.New("PBS snapshot ownership is unresolved or ambiguous; cleanup remains pending")
	}
	return found[0], nil
}

type pbsFile struct {
	Name     string `json:"filename"`
	Size     int64  `json:"size"`
	Checksum string `json:"csum"`
	Crypt    string `json:"crypt-mode"`
}

func (c *Proxmox) pbsDownload(ctx context.Context, id string, at int64, file string, decoded bool) (io.ReadCloser, error) {
	params := c.pbsParams(id, at)
	params.Set("file-name", file)
	action := "download"
	if decoded {
		action = "download-decoded"
	}
	resp, e := c.request(ctx, "PBS", "GET", c.pbsPath(action)+"?"+params.Encode(), nil, "", -1)
	if e != nil {
		return nil, e
	}
	return resp.Body, nil
}
func readFixedIndex(reader io.Reader, expected string) (int64, error) {
	header := make([]byte, 4096)
	if _, e := io.ReadFull(reader, header); e != nil {
		return 0, errors.New("truncated PBS fixed index")
	}
	magic := []byte{47, 127, 65, 237, 145, 253, 15, 205}
	if string(header[:8]) != string(magic) {
		return 0, errors.New("unsupported PBS disk index format")
	}
	size := binary.LittleEndian.Uint64(header[64:72])
	chunk := binary.LittleEndian.Uint64(header[72:80])
	if size == 0 || size > uint64(model.MaxCaptureBytes) || chunk < 64<<10 || chunk > 16<<20 || chunk&(chunk-1) != 0 {
		return 0, errors.New("invalid PBS fixed index geometry")
	}
	h := sha256.New()
	count, e := io.Copy(h, io.LimitReader(reader, 64<<20+1))
	if e != nil {
		return 0, e
	}
	if count != int64((size+chunk-1)/chunk)*32 || hex.EncodeToString(h.Sum(nil)) != hex.EncodeToString(header[32:64]) || expected != "" && expected != hex.EncodeToString(h.Sum(nil)) {
		return 0, errors.New("PBS fixed index checksum mismatch")
	}
	return int64(size), nil
}
func (c *Proxmox) Backup(ctx context.Context, j *Journal, profile, id, policy, dir string, report func(int64, int64)) (model.VMBackup, error) {
	if c.base.profile.DAVMountPath != "" {
		return c.backupDAV(ctx, j, profile, id, policy, dir, report)
	}
	meta, e := c.Inspect(ctx, id, profile, policy)
	if e != nil {
		return meta, e
	}
	j.SourceServer = id
	if e = j.Save(); e != nil {
		return meta, e
	}
	slot, e := j.Intent("pve-backup-task", meta.ProjectID, false)
	if e != nil {
		return meta, e
	}
	var upid string
	values := url.Values{"vmid": {id}, "storage": {c.base.profile.BackupStorage}, "mode": {"snapshot"}, "remove": {"0"}, "notes-template": {pveOwner(j)}}
	if e = c.form(ctx, "PVE", "POST", "/nodes/"+meta.ProjectID+"/vzdump", values, &upid); e != nil {
		return meta, e
	}
	if e = j.RecordOpaque(slot, upid); e != nil {
		return meta, e
	}
	if e = c.task(ctx, meta.ProjectID, upid); e != nil {
		return meta, e
	}
	j.Resources[slot].Deleted = true
	if e = j.Save(); e != nil {
		return meta, e
	}
	snapshot, e := c.ownedSnapshot(ctx, j)
	if e != nil {
		return meta, e
	}
	slot, e = j.Intent("pbs-snapshot", id, false)
	if e != nil {
		return meta, e
	}
	if e = j.RecordOpaque(slot, strconv.FormatInt(snapshot.Time, 10)); e != nil {
		return meta, e
	}
	var files []pbsFile
	if e = c.form(ctx, "PBS", "GET", c.pbsPath("files")+"?"+c.pbsParams(id, snapshot.Time).Encode(), nil, &files); e != nil {
		return meta, e
	}
	byName := map[string]pbsFile{}
	for _, f := range files {
		if f.Crypt != "" && f.Crypt != "none" {
			return meta, errors.New("PBS encrypted/sign-only archives are unsupported by HTTPS decoded export")
		}
		byName[f.Name] = f
	}
	var total int64
	for i, d := range meta.Disks {
		name := "drive-" + d.Device + ".img.fidx"
		entry, ok := byName[name]
		if !ok {
			return meta, errors.New("PBS backup omitted a source disk")
		}
		index, e := c.pbsDownload(ctx, id, snapshot.Time, name, false)
		if e != nil {
			return meta, e
		}
		size, e := readFixedIndex(index, entry.Checksum)
		index.Close()
		if e != nil {
			return meta, e
		}
		if size > c.base.profile.StagingMaxBytes-total {
			return meta, errors.New("PVE disk payload exceeds staging quota")
		}
		meta.Disks[i].Bytes = size
		meta.Disks[i].SizeGiB = int(math.Ceil(float64(size) / (1 << 30)))
		total += size
	}
	covered := 0
	for name := range byName {
		if strings.HasSuffix(name, ".fidx") {
			covered++
		}
	}
	if covered != len(meta.Disks) {
		return meta, errors.New("PBS disk set does not match the source configuration")
	}
	var done int64
	for _, disk := range meta.Disks {
		input, e := c.pbsDownload(ctx, id, snapshot.Time, "drive-"+disk.Device+".img.fidx", true)
		if e != nil {
			return meta, e
		}
		out, e := os.OpenFile(filepath.Join(dir, disk.File), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			input.Close()
			return meta, e
		}
		n, err := copyWithProgress(ctx, out, io.LimitReader(input, disk.Bytes+1), done, total, report)
		input.Close()
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
			return meta, errors.New("PBS decoded disk length mismatch")
		}
		done += n
	}
	current, e := c.Inspect(ctx, id, profile, policy)
	if e != nil {
		return meta, e
	}
	if current.Status != meta.Status || current.Flavor != meta.Flavor || !equalStringsMap(current.Metadata, meta.Metadata) || len(current.Disks) != len(meta.Disks) {
		return meta, errors.New("PVE source configuration changed during backup")
	}
	for i, d := range meta.Disks {
		if current.Disks[i].Device != d.Device || current.Disks[i].VolumeID != d.VolumeID {
			return meta, errors.New("PVE source disk changed during backup")
		}
	}
	return meta, nil
}
func equalStringsMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
func copyWithProgress(ctx context.Context, out io.Writer, input io.Reader, base, total int64, report func(int64, int64)) (int64, error) {
	buf := make([]byte, 1<<20)
	var written int64
	last := time.Time{}
	for {
		if e := ctx.Err(); e != nil {
			return written, e
		}
		n, e := input.Read(buf)
		if n > 0 {
			count, err := out.Write(buf[:n])
			written += int64(count)
			if err != nil {
				return written, err
			}
			if count != n {
				return written, io.ErrShortWrite
			}
			if report != nil && time.Since(last) >= time.Second {
				report(base+written, total)
				last = time.Now()
			}
		}
		if e == io.EOF {
			if report != nil {
				report(base+written, total)
			}
			return written, nil
		}
		if e != nil {
			return written, e
		}
	}
}
