// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"gdata/srk/internal/model"
	"gdata/srk/internal/testcloud"
	"gdata/srk/internal/virtualization"
	"testing"
)

func TestOVirtFullBackupDedupIndependentNewVMRestore(t *testing.T) {
	e := newVMEnvironmentKind(t, "ovirt")
	first := e.backup(t, testcloud.ImageVM)
	if first.Status != "success" || first.Result.VM == nil || first.Result.VM.Provider != "ovirt" || !first.Result.Verified {
		t.Fatal("oVirt backup", first.Status, first.Error)
	}
	if vm, disks, active := e.ovirt.Summary(); vm+disks+active != 0 {
		t.Fatal("oVirt backup resources leaked", vm, disks, active)
	}
	second := e.backup(t, testcloud.ImageVM)
	if second.Status != "success" || second.Result.UploadedBytes != 0 {
		t.Fatal("oVirt dedup", second.Status, second.Error)
	}
	var restored model.Run
	options := model.VMRestoreRequest{Name: "new oVirt VM", FlavorID: testcloud.Project, StorageDomainID: testcloud.DataVolume, Networks: []string{testcloud.Network}}
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+first.ID+"/restore", map[string]any{"vm_restore": options}, &restored); err != nil {
		t.Fatal(err)
	}
	done := e.wait(t, restored.ID)
	if done.Status != "success" || done.Result.VMRestore == nil || !done.Result.VMRestore.ControlPlaneVerified || done.Result.VMRestore.ServerID == testcloud.ImageVM {
		t.Fatal("oVirt restore", done.Status, done.Error)
	}
	if vm, disks, active := e.ovirt.Summary(); vm != 1 || disks != 1 || active != 0 {
		t.Fatal("oVirt retained dependencies or cleanup incorrect", vm, disks, active)
	}
	events, err := e.manager.DB.RunEvents(context.Background(), done.ID)
	if err != nil || len(events) < 3 {
		t.Fatal("missing oVirt progress events", events, err)
	}
	pending, err := virtualization.Pending(e.profile)
	if err != nil || len(pending) > 0 {
		t.Fatal("oVirt journal pending", pending, err)
	}
}
func TestOVirtFailedNewVMRollsBackOwnedResources(t *testing.T) {
	e := newVMEnvironmentKind(t, "ovirt")
	first := e.backup(t, testcloud.ImageVM)
	if first.Status != "success" {
		t.Fatal(first.Error)
	}
	e.ovirt.FailStart = true
	var restored model.Run
	options := model.VMRestoreRequest{Name: "failed new oVirt VM", FlavorID: testcloud.Project, StorageDomainID: testcloud.DataVolume, Networks: []string{testcloud.Network}}
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+first.ID+"/restore", map[string]any{"vm_restore": options}, &restored); err != nil {
		t.Fatal(err)
	}
	done := e.wait(t, restored.ID)
	if done.Status != "error" {
		t.Fatal("failed oVirt VM reported success")
	}
	if vm, disks, active := e.ovirt.Summary(); vm+disks+active != 0 {
		t.Fatal("failed oVirt restore leaked resources", vm, disks, active)
	}
}
