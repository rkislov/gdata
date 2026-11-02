// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"gdata/srk/internal/model"
	"strings"
	"testing"
)

func TestNativeArchiveRejectsEscapingRestoreNames(t *testing.T) {
	a := &model.VMArchive{File: "vm.vma", Name: "vzdump-qemu-100-2026_10_29-00_00_00.vma", Format: "vma", Bytes: 12288, SHA256: strings.Repeat("a", 64)}
	m := model.Manifest{VM: &model.VMBackup{Provider: "proxmox", Archive: a, Disks: []model.VMDisk{{Device: "scsi0", Bytes: 1 << 20, BootIndex: 0}}}, Files: []model.File{{Path: "vm.vma", Kind: "file", Size: 12288}}}
	if err := validateVMArchive(m); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../other.vma", "/etc/passwd", "vzdump-qemu-100-../../foreign.vma", "vzdump-qemu-100-backup.vma\n"} {
		a.Name = name
		if err := validateVMArchive(m); err == nil {
			t.Errorf("accepted unsafe archive name %q", name)
		}
	}
}
