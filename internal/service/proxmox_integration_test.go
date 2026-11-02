// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"gdata/srk/internal/model"
	"gdata/srk/internal/virtualization"
	"testing"
)

func TestProxmoxDAVBackupDedupRestore(t *testing.T) {
	e := newVMEnvironmentKind(t, "proxmox")
	first := e.backup(t, "100")
	if first.Status != "success" || first.Result == nil || first.Result.VM == nil || first.Result.VM.Archive == nil {
		t.Fatalf("backup: %s %s", first.Status, first.Error)
	}
	second := e.backup(t, "100")
	if second.Status != "success" || second.Result.UploadedBytes != 0 {
		t.Fatalf("dedup: %s %s", second.Status, second.Error)
	}
	var restore model.Run
	req := map[string]any{"vm_restore": model.VMRestoreRequest{Provider: "proxmox", Name: "restored-pve", Profile: "lab", FlavorID: "pve", StorageDomainID: "local-lvm", Networks: []string{"vmbr0"}}}
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+first.ID+"/restore", req, &restore); err != nil {
		t.Fatal(err)
	}
	done := e.wait(t, restore.ID)
	if done.Status != "success" || done.Result == nil || done.Result.VMRestore == nil || !done.Result.VMRestore.ControlPlaneVerified || done.Result.VMRestore.ServerID == "100" {
		t.Fatalf("restore: %s %s", done.Status, done.Error)
	}
	oldHeader := int(binary.BigEndian.Uint32(e.pve.Archive[56:60]))
	newHeader := int(binary.BigEndian.Uint32(e.pve.Restored[56:60]))
	if !bytes.Equal(e.pve.Archive[oldHeader:], e.pve.Restored[newHeader:]) {
		t.Fatal("restored disk payload differs from independent archive")
	}
	count, source := e.pve.Summary()
	if count != 1 || !source {
		t.Fatalf("source altered or restored VM missing: %d %v", count, source)
	}
	pending, err := virtualization.Pending(e.profile)
	if err != nil || len(pending) != 0 {
		t.Fatalf("unfinished journal: %v %v", pending, err)
	}
	e.media.DAV.mu.Lock()
	n := len(e.media.DAV.sessions)
	e.media.DAV.mu.Unlock()
	if n != 0 {
		t.Fatal("DAV session leak", n)
	}
}

func TestProxmoxDAVCorruptionAndRestoreRollback(t *testing.T) {
	e := newVMEnvironmentKind(t, "proxmox")
	e.pve.CorruptBackup = true
	bad := e.backup(t, "100")
	if bad.Status != "error" {
		t.Fatalf("corrupt archive accepted: %s %s", bad.Status, bad.Error)
	}
	e.pve.CorruptBackup = false
	good := e.backup(t, "100")
	if good.Status != "success" {
		t.Fatal(good.Error)
	}
	e.pve.FailRestore = true
	var restore model.Run
	req := map[string]any{"vm_restore": model.VMRestoreRequest{Provider: "proxmox", Name: "rollback", Profile: "lab", FlavorID: "pve", StorageDomainID: "local-lvm", Networks: []string{"vmbr0"}}}
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+good.ID+"/restore", req, &restore); err != nil {
		t.Fatal(err)
	}
	done := e.wait(t, restore.ID)
	if done.Status != "error" {
		t.Fatalf("injected failure ignored: %s", done.Status)
	}
	count, source := e.pve.Summary()
	if count != 0 || !source {
		t.Fatalf("rollback damaged source or leaked target: %d %v (%s)", count, source, done.Error)
	}
	pending, err := virtualization.Pending(e.profile)
	if err != nil || len(pending) != 0 {
		t.Fatalf("rollback pending: %v %v; %s", pending, err, done.Error)
	}
}
