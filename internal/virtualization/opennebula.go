// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type OpenNebula struct{ *nativeHTTP }

func NewOpenNebula(p Profile) (*OpenNebula, error) {
	if p.Kind != "opennebula" {
		return nil, errors.New("OpenNebula profile required")
	}
	c, e := newNativeHTTP(p)
	if e != nil {
		return nil, e
	}
	return &OpenNebula{c}, nil
}
func (c *OpenNebula) rpc(ctx context.Context, method string, args ...any) (string, error) {
	var b strings.Builder
	b.WriteString(`<methodCall><methodName>` + method + `</methodName><params>`)
	values := append([]any{c.base.credentials["session"]}, args...)
	for _, v := range values {
		b.WriteString("<param><value>")
		switch x := v.(type) {
		case string:
			b.WriteString("<string>" + xmlText(x) + "</string>")
		case int:
			b.WriteString("<int>" + strconv.Itoa(x) + "</int>")
		case bool:
			if x {
				b.WriteString("<boolean>1</boolean>")
			} else {
				b.WriteString("<boolean>0</boolean>")
			}
		default:
			return "", errors.New("invalid XML-RPC argument")
		}
		b.WriteString("</value></param>")
	}
	b.WriteString("</params></methodCall>")
	req, e := http.NewRequestWithContext(ctx, "POST", c.base.profile.Endpoint, strings.NewReader(b.String()))
	if e != nil {
		return "", e
	}
	req.Header.Set("Content-Type", "text/xml")
	response, e := c.base.http.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("OpenNebula XML-RPC connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return "", &APIError{"OpenNebula", method, response.StatusCode}
	}
	var out struct {
		Values []struct {
			Bool   string `xml:"boolean"`
			String string `xml:"string"`
			Int    string `xml:"int"`
			I4     string `xml:"i4"`
		} `xml:"params>param>value>array>data>value"`
	}
	if xml.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&out) != nil || len(out.Values) < 2 || out.Values[0].Bool != "1" {
		return "", fmt.Errorf("OpenNebula %s failed; inspect native control-plane log", method)
	}
	v := out.Values[1]
	if v.Int != "" {
		return v.Int, nil
	}
	if v.I4 != "" {
		return v.I4, nil
	}
	return v.String, nil
}
func oneInt(s string) int { n, _ := strconv.Atoi(s); return n }

type oneBackupConfig struct {
	Mode        string `xml:"MODE"`
	Interactive string `xml:"INTERACTIVE"`
	Keep        string `xml:"KEEP_LAST"`
	Disks       string `xml:"DISK_IDS"`
	Freeze      string `xml:"FS_FREEZE"`
	Bridge      string `xml:"LAST_BRIDGE"`
}
type oneDisk struct {
	ID        string `xml:"DISK_ID"`
	Size      string `xml:"SIZE"`
	Image     string `xml:"IMAGE_ID"`
	Type      string `xml:"TYPE"`
	Target    string `xml:"TARGET"`
	Prefix    string `xml:"DEV_PREFIX"`
	Shareable string `xml:"SHAREABLE"`
}
type oneVM struct {
	ID       string `xml:"ID"`
	Name     string `xml:"NAME"`
	State    int    `xml:"STATE"`
	LCM      int    `xml:"LCM_STATE"`
	Template struct {
		CPU    string `xml:"CPU"`
		VCPU   string `xml:"VCPU"`
		Memory string `xml:"MEMORY"`
		OS     struct {
			Arch     string `xml:"ARCH"`
			Machine  string `xml:"MACHINE"`
			Firmware string `xml:"FIRMWARE"`
			Boot     string `xml:"BOOT"`
		} `xml:"OS"`
		Disks []oneDisk `xml:"DISK"`
		NICs  []struct {
			Network string `xml:"NETWORK_ID"`
			Model   string `xml:"MODEL"`
		} `xml:"NIC"`
		PCI []struct {
			ID string `xml:"PCI_ID"`
		} `xml:"PCI"`
		Backup oneBackupConfig `xml:"BACKUP_CONFIG"`
	} `xml:"TEMPLATE"`
	Backups struct {
		Config oneBackupConfig `xml:"BACKUP_CONFIG"`
		IDs    []string        `xml:"BACKUP_IDS>ID"`
	} `xml:"BACKUPS"`
	History struct {
		Items []struct {
			Host    string `xml:"HOSTNAME"`
			Cluster string `xml:"CID"`
		} `xml:"HISTORY"`
	} `xml:"HISTORY_RECORDS"`
	UserTemplate struct {
		Run   string `xml:"SRK_RUN"`
		Proxy string `xml:"SRK_PROXY"`
	} `xml:"USER_TEMPLATE"`
}

func (c *OpenNebula) vm(ctx context.Context, id string) (oneVM, error) {
	var vm oneVM
	raw, e := c.rpc(ctx, "one.vm.info", oneInt(id))
	if e != nil {
		return vm, e
	}
	if xml.Unmarshal([]byte(raw), &vm) != nil || vm.ID != id {
		return vm, errors.New("invalid OpenNebula VM information")
	}
	return vm, nil
}

type oneDatastore struct {
	ID       string `xml:"ID"`
	Template struct {
		Driver string `xml:"DS_MAD"`
		Verify string `xml:"SRK_HTTPS_VERIFY"`
	} `xml:"TEMPLATE"`
}

func (c *OpenNebula) datastore(ctx context.Context, id string) (oneDatastore, error) {
	var d oneDatastore
	raw, e := c.rpc(ctx, "one.datastore.info", oneInt(id))
	if e != nil {
		return d, e
	}
	if xml.Unmarshal([]byte(raw), &d) != nil || d.ID != id {
		return d, errors.New("invalid OpenNebula datastore")
	}
	return d, nil
}
func (c *OpenNebula) Inspect(ctx context.Context, id, profile, policy string) (model.VMBackup, error) {
	meta := model.VMBackup{}
	if !numericID.MatchString(id) {
		return meta, errors.New("numeric OpenNebula VM ID required")
	}
	vm, e := c.vm(ctx, id)
	if e != nil {
		return meta, e
	}
	status := ""
	if vm.State == 3 && vm.LCM == 3 {
		status = "running"
	}
	if vm.State == 8 {
		status = "stopped"
	}
	if status == "" || policy == "cold-only" && status != "stopped" {
		return meta, errors.New("OpenNebula source must be RUNNING/POWEROFF; cold-only requires POWEROFF")
	}
	if len(vm.Template.PCI) > 0 {
		return meta, errors.New("OpenNebula PCI devices are unsupported")
	}
	conf := vm.Backups.Config
	if conf.Interactive == "" {
		conf = vm.Template.Backup
	}
	if conf.Interactive != "YES" || conf.Mode != "FULL" || conf.Keep != "0" || (conf.Freeze != "NONE" && conf.Freeze != "AGENT") {
		return meta, errors.New("configure OpenNebula BACKUP_CONFIG with MODE=FULL, INTERACTIVE=YES, KEEP_LAST=0 and FS_FREEZE=NONE/AGENT")
	}
	if len(vm.History.Items) == 0 {
		return meta, errors.New("OpenNebula source has no current hypervisor")
	}
	host := vm.History.Items[len(vm.History.Items)-1]
	if conf.Bridge != "" {
		host.Host = conf.Bridge
	}
	if c.base.profile.OneBEXEndpoints[host.Host] == "" {
		return meta, errors.New("current OpenNebula hypervisor lacks trusted HTTPS OneBEX mapping")
	}
	ds, e := c.datastore(ctx, c.base.profile.BackupDatastore)
	if e != nil {
		return meta, e
	}
	if ds.Template.Driver != "interactive" {
		return meta, errors.New("OpenNebula requires a dedicated interactive backup datastore")
	}
	cpu := oneInt(vm.Template.VCPU)
	if cpu < 1 {
		cpu = oneInt(vm.Template.CPU)
	}
	memory := oneInt(vm.Template.Memory)
	if cpu < 1 || memory < 1 || !numericID.MatchString(host.Cluster) {
		return meta, errors.New("invalid OpenNebula compute geometry")
	}
	meta = model.VMBackup{Provider: "opennebula", Profile: profile, ProjectID: host.Cluster, ServerID: id, Name: vm.Name, Status: status, Flavor: model.VMFlavor{ID: host.Cluster, VCPUs: cpu, RAM: memory}, Metadata: map[string]string{"host": host.Host, "architecture": vm.Template.OS.Arch, "machine": vm.Template.OS.Machine, "firmware": vm.Template.OS.Firmware, "cpu": vm.Template.CPU}, Consistency: "opennebula-crash-consistent", SnapshotAt: time.Now().UTC()}
	if status == "stopped" {
		meta.Consistency = "opennebula-cold-consistent"
	}
	if len(vm.Template.Disks) == 0 || len(vm.Template.Disks) > 32 {
		return meta, errors.New("OpenNebula requires 1–32 disks")
	}
	boot := ""
	for _, entry := range strings.Split(vm.Template.OS.Boot, ",") {
		entry = strings.TrimSpace(entry)
		if strings.HasPrefix(entry, "disk") {
			boot = strings.TrimPrefix(entry, "disk")
			break
		}
	}
	if boot == "" {
		boot = vm.Template.Disks[0].ID
	}
	var total int64
	all := []string{}
	for i, disk := range vm.Template.Disks {
		if !numericID.MatchString(disk.ID) || disk.Shareable == "YES" || disk.Type == "RBD" {
			return meta, errors.New("OpenNebula shared/RBD/invalid disk is unsupported")
		}
		size := int64(oneInt(disk.Size)) << 20
		if size <= 0 || size > model.MaxCaptureBytes-total || size > c.base.profile.StagingMaxBytes-total {
			return meta, errors.New("OpenNebula disk size or staging quota is unsupported")
		}
		total += size
		index := -1
		if disk.ID == boot {
			index = 0
		}
		meta.Disks = append(meta.Disks, model.VMDisk{File: fmt.Sprintf("%03d-disk-%s.img", i, disk.ID), VolumeID: disk.Image, Device: disk.ID, BootIndex: index, Bytes: size, SizeGiB: int((size + (1 << 30) - 1) >> 30), DiskFormat: "raw", Properties: map[string]string{"target": disk.Target, "dev_prefix": disk.Prefix}})
		all = append(all, disk.ID)
	}
	if conf.Disks != "" {
		selected := strings.Split(conf.Disks, ",")
		if len(selected) != len(all) {
			return meta, errors.New("OpenNebula selected-disk backup would be incomplete")
		}
		seen := map[string]bool{}
		for _, id := range selected {
			id = strings.TrimSpace(id)
			if seen[id] || !slicesContains(all, id) {
				return meta, errors.New("OpenNebula backup disk set differs")
			}
			seen[id] = true
		}
	}
	for _, nic := range vm.Template.NICs {
		if !numericID.MatchString(nic.Network) {
			return meta, errors.New("invalid OpenNebula source network")
		}
		meta.Networks = append(meta.Networks, model.VMNetwork{ID: nic.Network})
	}
	return meta, nil
}

type oneTransfer struct {
	ID     string `json:"TRANSFER_ID"`
	Disk   int    `json:"DISK_ID"`
	Status string `json:"STATUS"`
	OK     bool   `json:"RC"`
}
type oneStatus struct {
	VM        int           `json:"VM_ID"`
	Status    string        `json:"STATUS"`
	Success   *bool         `json:"SUCCESS"`
	Transfers []oneTransfer `json:"TRANSFERS"`
}

func (c *OpenNebula) status(ctx context.Context, host, id string) (oneStatus, error) {
	var out oneStatus
	endpoint := c.base.profile.OneBEXEndpoints[host]
	if endpoint == "" {
		return out, errors.New("unknown OneBEX host")
	}
	e := c.jsonResponse(ctx, "GET", strings.TrimRight(endpoint, "/")+"/status", map[string]int{"VM_ID": oneInt(id)}, &out)
	return out, e
}
func (c *OpenNebula) Backup(ctx context.Context, j *Journal, profile, id, policy, dir string, report func(int64, int64)) (model.VMBackup, error) {
	meta, e := c.Inspect(ctx, id, profile, policy)
	if e != nil {
		return meta, e
	}
	vm, e := c.vm(ctx, id)
	if e != nil {
		return meta, e
	}
	j.SourceServer = id
	j.SourceVolumes = append([]string(nil), vm.Backups.IDs...)
	if e = j.Save(); e != nil {
		return meta, e
	}
	slot, e := j.Intent("one-session", meta.Metadata["host"], false)
	if e != nil {
		return meta, e
	}
	if _, e = c.rpc(ctx, "one.vm.backup", oneInt(id), oneInt(c.base.profile.BackupDatastore), true); e != nil {
		return meta, e
	}
	if e = j.RecordOpaque(slot, id); e != nil {
		return meta, e
	}
	var status oneStatus
	if e = c.wait(ctx, func() (bool, error) {
		var err error
		status, err = c.status(ctx, meta.Metadata["host"], id)
		if err != nil {
			return false, nil
		}
		if status.VM != oneInt(id) {
			return false, errors.New("OneBEX VM scope mismatch")
		}
		if status.Success != nil && !*status.Success {
			return false, errors.New("OneBEX backup failed")
		}
		return status.Status == "executing" && len(status.Transfers) > 0, nil
	}); e != nil {
		return meta, e
	}
	if len(status.Transfers) != len(meta.Disks) {
		return meta, errors.New("OneBEX disk coverage mismatch")
	}
	transfers := map[string]oneTransfer{}
	for _, tr := range status.Transfers {
		if !nativeName.MatchString(tr.ID) || tr.Status != "ready" || !tr.OK {
			return meta, errors.New("OneBEX disk transfer is not ready")
		}
		key := strconv.Itoa(tr.Disk)
		if _, ok := transfers[key]; ok {
			return meta, errors.New("duplicate OneBEX transfer")
		}
		transfers[key] = tr
		slot, e := j.Intent("one-transfer", meta.Metadata["host"], false)
		if e != nil {
			return meta, e
		}
		if e = j.RecordOpaque(slot, tr.ID); e != nil {
			return meta, e
		}
	}
	endpoint := strings.TrimRight(c.base.profile.OneBEXEndpoints[meta.Metadata["host"]], "/")
	var total int64
	for _, d := range meta.Disks {
		total += d.Bytes
	}
	var done int64
	for _, disk := range meta.Disks {
		tr, ok := transfers[disk.Device]
		if !ok {
			return meta, errors.New("OneBEX omitted source disk")
		}
		var info struct {
			ID   string `json:"TRANSFER_ID"`
			Size int64  `json:"SIZE"`
		}
		if e = c.jsonResponse(ctx, "GET", endpoint+"/transfers/"+tr.ID+"/info", nil, &info); e != nil {
			return meta, e
		}
		if info.ID != tr.ID || info.Size <= 0 || info.Size > model.MaxCaptureBytes>>20 || info.Size<<20 != disk.Bytes {
			return meta, errors.New("OneBEX disk geometry differs from source")
		}
		out, e := os.OpenFile(filepath.Join(dir, disk.File), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return meta, e
		}
		for offset := int64(0); offset < disk.Bytes; {
			length := min(int64(4<<20), disk.Bytes-offset)
			req, _ := http.NewRequestWithContext(ctx, "GET", endpoint+"/images/"+tr.ID, nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
			response, err := c.base.http.Do(req)
			if err != nil {
				out.Close()
				return meta, errors.New("OneBEX HTTPS disk read failed")
			}
			expected := fmt.Sprintf("bytes %d-%d/*", offset, offset+length-1)
			if response.StatusCode != 206 || response.Header.Get("Content-Range") != expected {
				response.Body.Close()
				out.Close()
				return meta, errors.New("OneBEX range scope mismatch")
			}
			n, err := copyWithProgress(ctx, out, io.LimitReader(response.Body, length+1), done+offset, total, report)
			response.Body.Close()
			if err != nil || n != length {
				out.Close()
				return meta, errors.New("OneBEX disk range was truncated")
			}
			offset += n
		}
		if e = out.Sync(); e != nil {
			out.Close()
			return meta, e
		}
		if e = out.Close(); e != nil {
			return meta, e
		}
		done += disk.Bytes
		if e = c.jsonResponse(ctx, "POST", endpoint+"/transfer/"+tr.ID+"/finalize", map[string]any{}, nil); e != nil {
			return meta, e
		}
		for i := range j.Resources {
			if j.Resources[i].Kind == "one-transfer" && j.Resources[i].ID == tr.ID {
				j.Resources[i].Deleted = true
			}
		}
		if e = j.Save(); e != nil {
			return meta, e
		}
	}
	if e = c.jsonResponse(ctx, "POST", endpoint+"/vms/"+id+"/finish", map[string]any{}, nil); e != nil {
		return meta, e
	}
	for i := range j.Resources {
		if j.Resources[i].Kind == "one-session" {
			j.Resources[i].Deleted = true
		}
	}
	if e = j.Save(); e != nil {
		return meta, e
	}
	if e = c.wait(ctx, func() (bool, error) {
		current, e := c.vm(ctx, id)
		if e != nil {
			return false, e
		}
		return current.State == vm.State && current.LCM == vm.LCM, nil
	}); e != nil {
		return meta, e
	}
	if e = c.adoptBackupImage(ctx, j); e != nil {
		return meta, e
	}
	current, e := c.Inspect(ctx, id, profile, policy)
	if e != nil {
		return meta, e
	}
	if current.Flavor != meta.Flavor || !equalStringsMap(current.Metadata, meta.Metadata) || len(current.Disks) != len(meta.Disks) {
		return meta, errors.New("OpenNebula source changed during backup")
	}
	for i, d := range meta.Disks {
		other := current.Disks[i]
		if d.Device != other.Device || d.VolumeID != other.VolumeID || d.Bytes != other.Bytes || d.BootIndex != other.BootIndex || !equalStringsMap(d.Properties, other.Properties) {
			return meta, errors.New("OpenNebula source disk changed during backup")
		}
	}
	if len(meta.Networks) != len(current.Networks) {
		return meta, errors.New("OpenNebula source networks changed during backup")
	}
	for i, n := range meta.Networks {
		if n.ID != current.Networks[i].ID {
			return meta, errors.New("OpenNebula source networks changed during backup")
		}
	}
	return meta, nil
}

type oneImage struct {
	ID        string `xml:"ID"`
	Name      string `xml:"NAME"`
	State     int    `xml:"STATE"`
	Datastore string `xml:"DATASTORE_ID"`
	Template  struct {
		Run   string `xml:"SRK_RUN"`
		Proxy string `xml:"SRK_PROXY"`
	} `xml:"TEMPLATE"`
}

func (c *OpenNebula) image(ctx context.Context, id string) (oneImage, error) {
	var image oneImage
	raw, e := c.rpc(ctx, "one.image.info", oneInt(id))
	if e != nil {
		return image, e
	}
	if xml.Unmarshal([]byte(raw), &image) != nil || image.ID != id {
		return image, errors.New("invalid OpenNebula image")
	}
	return image, nil
}
func (c *OpenNebula) adoptBackupImage(ctx context.Context, j *Journal) error {
	vm, e := c.vm(ctx, j.SourceServer)
	if e != nil {
		return e
	}
	var ids []string
	for _, id := range vm.Backups.IDs {
		if !slicesContains(j.SourceVolumes, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) != 1 {
		return errors.New("OpenNebula backup metadata ownership is unresolved")
	}
	image, e := c.image(ctx, ids[0])
	if e != nil {
		return e
	}
	if image.Datastore != c.base.profile.BackupDatastore {
		return errors.New("OpenNebula backup metadata belongs to another datastore")
	}
	if _, e = c.rpc(ctx, "one.image.update", oneInt(image.ID), "<TEMPLATE><SRK_RUN>"+j.ID+"</SRK_RUN><SRK_PROXY>"+xmlText(j.Owner)+"</SRK_PROXY></TEMPLATE>", 1); e != nil {
		return e
	}
	for _, r := range j.Resources {
		if r.Kind == "one-image" && r.ID == image.ID {
			return nil
		}
	}
	slot, e := j.Intent("one-image", image.Name, false)
	if e != nil {
		return e
	}
	return j.RecordOpaque(slot, image.ID)
}
