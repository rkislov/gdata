// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization

import (
	"context"
	"encoding/json"
	"errors"
	"gdata/srk/internal/model"
	"io"
	"net/url"
	"strings"
	"time"
)

type Server struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Status  string          `json:"status"`
	Project string          `json:"tenant_id"`
	Image   json.RawMessage `json:"image"`
	Flavor  struct {
		ID string `json:"id"`
	} `json:"flavor"`
	Metadata    map[string]string `json:"metadata"`
	KeyName     string            `json:"key_name"`
	ConfigDrive string            `json:"config_drive"`
	Volumes     []struct {
		ID string `json:"id"`
	} `json:"os-extended-volumes:volumes_attached"`
	Groups []struct {
		Name string `json:"name"`
	} `json:"security_groups"`
	Task *string `json:"OS-EXT-STS:task_state"`
}
type CloudFlavor struct {
	ID        string          `json:"id"`
	VCPUs     int             `json:"vcpus"`
	RAM       int             `json:"ram"`
	Disk      int             `json:"disk"`
	Ephemeral int             `json:"OS-FLV-EXT-DATA:ephemeral"`
	Swap      json.RawMessage `json:"swap"`
}
type Image struct {
	ID         string                     `json:"id"`
	Name       string                     `json:"name"`
	Status     string                     `json:"status"`
	Owner      string                     `json:"owner"`
	Size       int64                      `json:"size"`
	DiskFormat string                     `json:"disk_format"`
	MinDisk    int                        `json:"min_disk"`
	MinRAM     int                        `json:"min_ram"`
	Checksum   string                     `json:"checksum"`
	HashAlgo   string                     `json:"os_hash_algo"`
	HashValue  string                     `json:"os_hash_value"`
	Extra      map[string]json.RawMessage `json:"-"`
}

func (i *Image) UnmarshalJSON(b []byte) error {
	type plain Image
	var p plain
	if e := json.Unmarshal(b, &p); e != nil {
		return e
	}
	*i = Image(p)
	return json.Unmarshal(b, &i.Extra)
}
func (i Image) Property(key string) string {
	var s string
	_ = json.Unmarshal(i.Extra[key], &s)
	return s
}

type CloudVolume struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Status      string            `json:"status"`
	Size        int               `json:"size"`
	Encrypted   bool              `json:"encrypted"`
	Multiattach bool              `json:"multiattach"`
	Project     string            `json:"os-vol-tenant-attr:tenant_id"`
	Metadata    map[string]string `json:"metadata"`
}
type CloudSnapshot struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Status   string            `json:"status"`
	VolumeID string            `json:"volume_id"`
	Size     int               `json:"size"`
	Created  string            `json:"created_at"`
	Metadata map[string]string `json:"metadata"`
}
type BDM struct {
	SnapshotID      string `json:"snapshot_id"`
	SourceType      string `json:"source_type"`
	DestinationType string `json:"destination_type"`
	Device          string `json:"device_name"`
	BootIndex       int    `json:"boot_index"`
	Size            int    `json:"volume_size"`
}

func (c *OpenStack) Server(ctx context.Context, id string) (Server, error) {
	var out struct {
		Server Server `json:"server"`
	}
	if _, e := cloudID(id); e != nil {
		return out.Server, e
	}
	_, e := c.json(ctx, "Nova", "GET", "/servers/"+id, nil, &out)
	if e == nil && (out.Server.ID != id || out.Server.Project != c.base.profile.ProjectID) {
		e = errors.New("Nova server identity or project mismatch")
	}
	return out.Server, e
}
func (c *OpenStack) Flavor(ctx context.Context, id string) (CloudFlavor, error) {
	var out struct {
		Flavor CloudFlavor `json:"flavor"`
	}
	if id == "" || len(id) > 128 || strings.ContainsAny(id, "/\\?#") {
		return out.Flavor, errors.New("invalid flavor ID")
	}
	_, e := c.json(ctx, "Nova", "GET", "/flavors/"+url.PathEscape(id), nil, &out)
	if e == nil && out.Flavor.ID != id {
		e = errors.New("flavor identity mismatch")
	}
	return out.Flavor, e
}
func (c *OpenStack) Image(ctx context.Context, id string) (Image, error) {
	var out Image
	if _, e := cloudID(id); e != nil {
		return out, e
	}
	_, e := c.json(ctx, "Glance", "GET", "/images/"+id, nil, &out)
	if e == nil && (out.ID != id || out.Owner != c.base.profile.ProjectID) {
		e = errors.New("Glance image identity or project mismatch")
	}
	return out, e
}
func (c *OpenStack) Volume(ctx context.Context, id string) (CloudVolume, error) {
	var out struct {
		Volume CloudVolume `json:"volume"`
	}
	if _, e := cloudID(id); e != nil {
		return out.Volume, e
	}
	_, e := c.json(ctx, "Cinder", "GET", "/volumes/"+id, nil, &out)
	if e == nil && (out.Volume.ID != id || out.Volume.Project != "" && out.Volume.Project != c.base.profile.ProjectID) {
		e = errors.New("Cinder volume identity or project mismatch")
	}
	return out.Volume, e
}
func (c *OpenStack) Snapshot(ctx context.Context, id string) (CloudSnapshot, error) {
	var out struct {
		Snapshot CloudSnapshot `json:"snapshot"`
	}
	if _, e := cloudID(id); e != nil {
		return out.Snapshot, e
	}
	_, e := c.json(ctx, "Cinder", "GET", "/snapshots/"+id, nil, &out)
	if e == nil && out.Snapshot.ID != id {
		e = errors.New("snapshot identity mismatch")
	}
	return out.Snapshot, e
}
func (c *OpenStack) Networks(ctx context.Context, id string) ([]model.VMNetwork, error) {
	var out struct {
		Interfaces []struct {
			NetID string `json:"net_id"`
			MAC   string `json:"mac_addr"`
			IPs   []struct {
				IP string `json:"ip_address"`
			} `json:"fixed_ips"`
		} `json:"interfaceAttachments"`
	}
	_, e := c.json(ctx, "Nova", "GET", "/servers/"+id+"/os-interface", nil, &out)
	if e != nil {
		return nil, e
	}
	nets := []model.VMNetwork{}
	for _, n := range out.Interfaces {
		if !model.CloudUUID(n.NetID) {
			return nil, errors.New("invalid source network ID")
		}
		v := model.VMNetwork{ID: n.NetID, MAC: n.MAC}
		for _, ip := range n.IPs {
			v.FixedIPs = append(v.FixedIPs, ip.IP)
		}
		nets = append(nets, v)
	}
	return nets, nil
}
func hasSwap(v json.RawMessage) bool {
	var n int
	if json.Unmarshal(v, &n) == nil {
		return n != 0
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s != "" && s != "0"
	}
	return len(v) > 0 && string(v) != "null"
}
func imageBacked(s Server) bool {
	var im struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(s.Image, &im)
	return im.ID != ""
}
func (c *OpenStack) Preflight(ctx context.Context, id, profile, consistency string) (model.VMBackup, error) {
	var b model.VMBackup
	if consistency != "crash-consistent" && consistency != "cold-only" {
		return b, errors.New("choose crash-consistent or cold-only VM policy")
	}
	s, e := c.Server(ctx, id)
	if e != nil {
		return b, e
	}
	if s.Status != "ACTIVE" && s.Status != "SHUTOFF" {
		return b, errors.New("source VM must be ACTIVE or SHUTOFF")
	}
	if s.Task != nil && *s.Task != "" {
		return b, errors.New("source VM already has an active task")
	}
	if consistency == "cold-only" && s.Status != "SHUTOFF" {
		return b, errors.New("cold-only policy requires an already stopped VM; SRK does not stop the source")
	}
	f, e := c.Flavor(ctx, s.Flavor.ID)
	if e != nil {
		return b, e
	}
	if f.Ephemeral != 0 || hasSwap(f.Swap) {
		return b, errors.New("local ephemeral and swap disks cannot be exported by this API; no partial VM backup will be created")
	}
	if len(s.Volumes) > 32 {
		return b, errors.New("more than 32 attached volumes are not supported")
	}
	seen := map[string]bool{}
	for _, v := range s.Volumes {
		if !model.CloudUUID(v.ID) || seen[v.ID] {
			return b, errors.New("invalid or duplicate attached volume")
		}
		seen[v.ID] = true
		volume, e := c.Volume(ctx, v.ID)
		if e != nil {
			return b, e
		}
		if volume.Encrypted || volume.Multiattach || volume.Status != "in-use" && volume.Status != "available" {
			return b, errors.New("encrypted, multiattach or unstable Cinder volume is not supported")
		}
	}
	nets, e := c.Networks(ctx, id)
	if e != nil {
		return b, e
	}
	if len(nets) > 16 {
		return b, errors.New("too many source network interfaces")
	}
	b = model.VMBackup{Provider: "openstack", Profile: profile, ProjectID: s.Project, ServerID: s.ID, Name: s.Name, Status: s.Status, Flavor: model.VMFlavor{ID: f.ID, VCPUs: f.VCPUs, RAM: f.RAM, Disk: f.Disk}, Metadata: s.Metadata, KeyName: s.KeyName, ConfigDrive: s.ConfigDrive != "" && s.ConfigDrive != "False" && s.ConfigDrive != "false", Networks: nets, VolumeBacked: !imageBacked(s), Consistency: "openstack-crash-consistent", Disks: []model.VMDisk{}}
	if s.Status == "SHUTOFF" {
		b.Consistency = "openstack-cold-consistent"
	}
	for _, g := range s.Groups {
		b.SecurityGroups = append(b.SecurityGroups, g.Name)
	}
	if len(s.Metadata) > 126 {
		return b, errors.New("source metadata exceeds supported size")
	}
	return b, nil
}
func (c *OpenStack) WaitImage(ctx context.Context, id string, zero bool) (Image, error) {
	var out Image
	e := c.wait(ctx, func() (bool, error) {
		var e error
		out, e = c.Image(ctx, id)
		if e != nil {
			return false, e
		}
		switch out.Status {
		case "active":
			if !zero && out.Size <= 0 {
				return false, errors.New("snapshot has no image data")
			}
			return true, nil
		case "killed", "deleted", "deactivated", "pending_delete":
			return false, errors.New("Glance image failed")
		}
		return false, nil
	})
	return out, e
}
func (c *OpenStack) WaitSnapshot(ctx context.Context, id string) (CloudSnapshot, error) {
	var out CloudSnapshot
	e := c.wait(ctx, func() (bool, error) {
		var e error
		out, e = c.Snapshot(ctx, id)
		if e != nil {
			return false, e
		}
		if out.Status == "error" || out.Status == "error_deleting" {
			return false, errors.New("Cinder snapshot failed")
		}
		return out.Status == "available", nil
	})
	return out, e
}
func (c *OpenStack) WaitVolume(ctx context.Context, id string) (CloudVolume, error) {
	var out CloudVolume
	e := c.wait(ctx, func() (bool, error) {
		var e error
		out, e = c.Volume(ctx, id)
		if e != nil {
			return false, e
		}
		if strings.HasPrefix(out.Status, "error") {
			return false, errors.New("Cinder volume failed")
		}
		return out.Status == "available", nil
	})
	return out, e
}
func (c *OpenStack) Download(ctx context.Context, image Image) (io.ReadCloser, error) {
	resp, e := c.do(ctx, "Glance", "GET", "/images/"+image.ID+"/file", nil, -1)
	if e != nil {
		return nil, e
	}
	if resp.StatusCode != 200 || resp.ContentLength >= 0 && resp.ContentLength != image.Size {
		resp.Body.Close()
		return nil, errors.New("Glance download length or status mismatch")
	}
	return resp.Body, nil
}
func (c *OpenStack) ValidateRestore(ctx context.Context, r model.VMRestoreRequest) (CloudFlavor, error) {
	if e := r.Validate(); e != nil {
		return CloudFlavor{}, e
	}
	f, e := c.Flavor(ctx, r.FlavorID)
	if e != nil {
		return f, e
	}
	if f.Ephemeral != 0 || hasSwap(f.Swap) {
		return f, errors.New("target flavor has unsupported ephemeral or swap disks")
	}
	for _, id := range r.Networks {
		var n struct {
			Network struct {
				ID, Status string
				Shared     bool   `json:"shared"`
				Project    string `json:"project_id"`
				Tenant     string `json:"tenant_id"`
			} `json:"network"`
		}
		if _, e = c.json(ctx, "Neutron", "GET", "/networks/"+id, nil, &n); e != nil {
			return f, e
		}
		owner := n.Network.Project
		if owner == "" {
			owner = n.Network.Tenant
		}
		if n.Network.ID != id || n.Network.Status != "ACTIVE" || owner != c.base.profile.ProjectID && !n.Network.Shared {
			return f, errors.New("target network is unavailable or belongs to another project")
		}
	}
	if r.KeyName != "" {
		if _, e = c.json(ctx, "Nova", "GET", "/os-keypairs/"+url.PathEscape(r.KeyName), nil, new(any)); e != nil {
			return f, e
		}
	}
	return f, nil
}
func safeImageProperties(im Image) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"architecture", "hw_firmware_type", "hw_machine_type", "hw_disk_bus", "hw_scsi_model", "hw_video_model", "hw_qemu_guest_agent", "os_type", "os_distro", "os_version"} {
		if v := im.Property(k); v != "" && len(v) <= 255 {
			out[k] = v
		}
	}
	return out
}
func parseBDM(im Image) ([]BDM, error) {
	b := im.Extra["block_device_mapping"]
	var text string
	if json.Unmarshal(b, &text) == nil {
		b = []byte(text)
	}
	var out []BDM
	if e := json.Unmarshal(b, &out); e != nil {
		return nil, errors.New("invalid Nova snapshot block-device mapping")
	}
	if len(out) == 0 || len(out) > 32 {
		return nil, errors.New("invalid snapshot disk count")
	}
	return out, nil
}
func parseCloudTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999"} {
		if t, e := time.Parse(layout, s); e == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("invalid cloud timestamp")
}
func sourceID(raw string) (string, error) {
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "openstack" && u.Scheme != "ovirt") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" {
		return "", errors.New("source must be openstack://PROFILE/VM_UUID or ovirt://PROFILE/VM_UUID")
	}
	id := strings.TrimPrefix(u.Path, "/")
	if _, e = cloudID(id); e != nil {
		return "", e
	}
	return id, nil
}
func Source(raw string) (string, string, error) {
 if u,e:=url.Parse(raw);e==nil&&NativeProvider(u.Scheme){return NativeSource(raw)}
	id, e := sourceID(raw)
	if e != nil {
		return "", "", e
	}
	u, _ := url.Parse(raw)
	return u.Host, id, nil
}
