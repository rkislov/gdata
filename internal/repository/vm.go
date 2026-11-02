// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"errors"
	"gdata/srk/internal/model"
	"regexp"
)

func validateVMManifest(m model.Manifest) error {
	if m.VM == nil {
		if m.Consistency == "openstack-cold-consistent" || m.Consistency == "openstack-crash-consistent" {
			return errors.New("VM consistency requires VM metadata")
		}
		return nil
	}
	v := m.VM
	if m.PostgreSQL != nil || m.Disk != nil || m.Mode != "full" || !validVMIdentity(v.Provider, v.ServerID, v.ProjectID) || v.Profile == "" || v.Flavor.ID == "" || v.SnapshotAt.IsZero() || len(v.Disks) == 0 || len(v.Disks) > 33 || (v.Archive == nil && len(m.Files) != len(v.Disks)) || (m.Consistency != v.Provider+"-cold-consistent" && m.Consistency != v.Provider+"-crash-consistent") || v.Consistency != m.Consistency {
		return errors.New("invalid VM manifest metadata")
	}
	if v.Archive != nil {
		return validateVMArchive(m)
	}
	files := map[string]model.File{}
	for _, f := range m.Files {
		files[f.Path] = f
	}
	roots := 0
	seen := map[string]bool{}
	var total int64
	for _, d := range v.Disks {
		f, ok := files[d.File]
		if !ok || f.Kind != "file" || seen[d.File] || !ValidPath(d.File) || d.Bytes <= 0 || d.Bytes != f.Size || !hashPattern.MatchString(d.SHA256) || (d.DiskFormat != "raw" && d.DiskFormat != "qcow2") || d.BootIndex != 0 && d.BootIndex != -1 || d.VolumeID != "" && (v.Provider == "openstack" || v.Provider == "ovirt") && !model.CloudUUID(d.VolumeID) || d.SizeGiB < 0 || d.SizeGiB > 4096 {
			return errors.New("invalid VM disk descriptor")
		}
		if d.BootIndex == 0 {
			roots++
		}
		if d.VolumeID != "" && d.SizeGiB == 0 {
			return errors.New("volume geometry is missing")
		}
		seen[d.File] = true
		if d.Bytes > model.MaxCaptureBytes-total {
			return errors.New("VM payload exceeds manifest bound")
		}
		total += d.Bytes
	}
	if roots != 1 {
		return errors.New("VM requires exactly one root disk")
	}
	return nil
}

func validVMIdentity(provider, id, project string) bool {
	switch provider {
	case "openstack", "ovirt":
		return model.CloudUUID(id) && model.CloudUUID(project)
	case "proxmox":
		return regexp.MustCompile(`^[1-9][0-9]{0,8}$`).MatchString(id) && regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`).MatchString(project)
	case "opennebula":
		return regexp.MustCompile(`^(0|[1-9][0-9]{0,8})$`).MatchString(id) && regexp.MustCompile(`^(0|[1-9][0-9]{0,8})$`).MatchString(project)
	}
	return false
}

func validateVMArchive(m model.Manifest) error {
	v := m.VM
	a := v.Archive
	if v.Provider != "proxmox" || len(a.Name) > 255 || !regexp.MustCompile(`^vzdump-qemu-[1-9][0-9]{0,8}-[A-Za-z0-9_.-]+\.vma$`).MatchString(a.Name) || a.Format != "vma" || !ValidPath(a.File) || a.Bytes < 512 || a.Bytes > model.MaxCaptureBytes || !hashPattern.MatchString(a.SHA256) || len(m.Files) != 1 || m.Files[0].Kind != "file" || m.Files[0].Path != a.File || m.Files[0].Size != a.Bytes {
		return errors.New("invalid native VM archive")
	}
	roots := 0
	seen := map[string]bool{}
	for _, d := range v.Disks {
		if !regexp.MustCompile(`^(scsi[0-9]{1,2}|sata[0-9]|ide[0-9]|virtio[0-9]{1,2}|efidisk0|tpmstate0)$`).MatchString(d.Device) || seen[d.Device] || d.Bytes <= 0 || d.Bytes > model.MaxCaptureBytes || d.BootIndex != 0 && d.BootIndex != -1 {
			return errors.New("invalid archived VM disk set")
		}
		seen[d.Device] = true
		if d.BootIndex == 0 {
			roots++
		}
	}
	if roots != 1 {
		return errors.New("native VM archive requires exactly one boot disk")
	}
	return nil
}
