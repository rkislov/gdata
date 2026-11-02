// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"gdata/srk/internal/model"
	"gdata/srk/internal/virtualization"
	"testing"
)

func TestOpenNebulaHTTPSBackupDedupRestore(t *testing.T) {
	e := newVMEnvironmentKind(t, "opennebula")
	first := e.backup(t, "100")
	if first.Status != "success" || first.Result == nil || first.Result.VM == nil {
		t.Fatalf("backup: %s %s", first.Status, first.Error)
	}
	second := e.backup(t, "100")
	if second.Status != "success" || second.Result.UploadedBytes != 0 {
		t.Fatalf("dedup: %s %s", second.Status, second.Error)
	}
	count, images, source := e.one.Summary()
	if count+images != 0 || !source {
		t.Fatalf("backup leaked native resources: %d %d %v", count, images, source)
	}
	var restore model.Run
	req := map[string]any{"vm_restore": model.VMRestoreRequest{Provider: "opennebula", Name: "restored-one", Profile: "lab", FlavorID: "0", StorageDomainID: "1", Networks: []string{"1"}}}
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+first.ID+"/restore", req, &restore); err != nil {
		t.Fatal(err)
	}
	done := e.wait(t, restore.ID)
	if done.Status != "success" || done.Result == nil || done.Result.VMRestore == nil || !done.Result.VMRestore.ControlPlaneVerified || done.Result.VMRestore.ServerID == "100" {
		t.Fatalf("restore: %s %s", done.Status, done.Error)
	}
	data := e.one.RestoredData()
	if len(data) != 1 || !bytes.Equal(data[0], e.one.Data) {
		t.Fatal("restored bytes differ from source")
	}
	pending, err := virtualization.Pending(e.profile)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending: %v %v", pending, err)
	}
}

func TestOpenNebulaRestoreFailureRollsBackOwnedResources(t *testing.T) {
	e := newVMEnvironmentKind(t, "opennebula")
	backup := e.backup(t, "100")
	if backup.Status != "success" {
		t.Fatal(backup.Error)
	}
	e.one.FailStart = true
	var restore model.Run
	req := map[string]any{"vm_restore": model.VMRestoreRequest{Provider: "opennebula", Name: "rollback-one", Profile: "lab", FlavorID: "0", StorageDomainID: "1", Networks: []string{"1"}}}
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+backup.ID+"/restore", req, &restore); err != nil {
		t.Fatal(err)
	}
	done := e.wait(t, restore.ID)
	if done.Status != "error" {
		t.Fatal("failure ignored", done.Status)
	}
	count, images, source := e.one.Summary()
	if count+images != 0 || !source {
		t.Fatalf("rollback leaked/damaged: %d %d %v (%s)", count, images, source, done.Error)
	}
	pending, err := virtualization.Pending(e.profile)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending: %v %v (%s)", pending, err, done.Error)
	}
}
