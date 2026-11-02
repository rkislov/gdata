// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

import (
	"errors"
	"regexp"
	"time"
)

var cloudUUID = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

func CloudUUID(s string) bool { return cloudUUID.MatchString(s) }

type VMOptions struct {
	Consistency string `json:"consistency"`
}
type VMNetwork struct {
	ID       string   `json:"id"`
	MAC      string   `json:"mac,omitempty"`
	FixedIPs []string `json:"fixed_ips,omitempty"`
}
type VMFlavor struct {
	ID    string `json:"id"`
	VCPUs int    `json:"vcpus"`
	RAM   int    `json:"ram"`
	Disk  int    `json:"disk"`
}
type VMDisk struct {
	File       string            `json:"file"`
	VolumeID   string            `json:"volume_id,omitempty"`
	Device     string            `json:"device,omitempty"`
	BootIndex  int               `json:"boot_index"`
	SizeGiB    int               `json:"size_gib"`
	Bytes      int64             `json:"bytes"`
	DiskFormat string            `json:"disk_format"`
	SHA256     string            `json:"sha256"`
	Properties map[string]string `json:"properties,omitempty"`
}
type VMBackup struct {
	Archive        *VMArchive        `json:"archive,omitempty"`
	Provider       string            `json:"provider"`
	Profile        string            `json:"profile"`
	ProjectID      string            `json:"project_id"`
	ServerID       string            `json:"server_id"`
	Name           string            `json:"name"`
	Status         string            `json:"status"`
	Flavor         VMFlavor          `json:"flavor"`
	Networks       []VMNetwork       `json:"networks"`
	SecurityGroups []string          `json:"security_groups"`
	KeyName        string            `json:"key_name,omitempty"`
	ConfigDrive    bool              `json:"config_drive"`
	Metadata       map[string]string `json:"metadata"`
	VolumeBacked   bool              `json:"volume_backed"`
	Consistency    string            `json:"consistency"`
	SnapshotAt     time.Time         `json:"snapshot_at"`
	Disks          []VMDisk          `json:"disks"`
}
type VMRestoreRequest struct {
	Provider         string   `json:"provider,omitempty"`
	ImportStorageID  string   `json:"import_storage_id,omitempty"`
	StorageDomainID  string   `json:"storage_domain_id,omitempty"`
	ProxyID          string   `json:"proxy_id,omitempty"`
	Profile          string   `json:"profile,omitempty"`
	Name             string   `json:"name"`
	FlavorID         string   `json:"flavor_id"`
	Networks         []string `json:"networks"`
	SecurityGroups   []string `json:"security_groups,omitempty"`
	KeyName          string   `json:"key_name,omitempty"`
	AvailabilityZone string   `json:"availability_zone,omitempty"`
}

func (r VMRestoreRequest) Validate() error {
	kind := r.Provider
	if kind == "" {
		kind = "openstack"
	}
	return r.ValidateFor(kind)
}
func (r VMRestoreRequest) ValidateFor(provider string) error {
	if r.Provider != "" && r.Provider != provider {
		return errors.New("restore provider mismatch")
	}
	if r.Name == "" || len(r.Name) > 200 || r.FlavorID == "" || len(r.FlavorID) > 128 || len(r.Networks) == 0 || len(r.Networks) > 16 || len(r.SecurityGroups) > 64 || len(r.KeyName) > 255 || len(r.AvailabilityZone) > 255 {
		return errors.New("new VM name, target compute and 1–16 networks are required")
	}
	seen := map[string]bool{}
	for _, id := range r.Networks {
		valid := CloudUUID(id)
		if provider == "proxmox" {
			valid = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`).MatchString(id)
		}
		if provider == "opennebula" {
			valid = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})$`).MatchString(id)
		}
		if !valid || seen[id] {
			return errors.New("distinct platform-specific target networks are required")
		}
		seen[id] = true
	}
	if provider == "proxmox" || provider == "opennebula" {
		if len(r.SecurityGroups) > 0 || r.KeyName != "" || r.AvailabilityZone != "" {
			return errors.New("OpenStack-only options cannot be used for this provider")
		}
	}
	if provider != "proxmox" && r.ImportStorageID != "" {
		return errors.New("import_storage_id is only supported by Proxmox")
	}
	return nil
}

type VMRestoreResult struct {
	ServerID             string   `json:"server_id"`
	Name                 string   `json:"name"`
	Status               string   `json:"status"`
	Volumes              []string `json:"volumes"`
	ControlPlaneVerified bool     `json:"control_plane_verified"`
}

type VMArchive struct {
	File   string `json:"file"`
	Name   string `json:"name"`
	Format string `json:"format"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type DAVSession struct {
	ID         string     `json:"id"`
	Subject    string     `json:"subject"`
	User       string     `json:"user"`
	Operation  string     `json:"operation"`
	SnapshotID string     `json:"snapshot_id,omitempty"`
	ExpiresAt  time.Time  `json:"expires_at"`
	MaxBytes   int64      `json:"max_bytes"`
	Sealed     bool       `json:"sealed"`
	Archive    *VMArchive `json:"archive,omitempty"`
}
