// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/tape"
	"gdata/srk/internal/testcloud"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestTapeLibraryHTTPSOperationsAndVMRecovery(t *testing.T) {
	e := newVMEnvironmentKind(t, "tape")
	source := e.backup(t, testcloud.ImageVM)
	if source.Status != "success" {
		t.Fatal(source.Error)
	}
	base := e.url + "/v1/media/" + e.media.Config.ID + "/tape"
	var inv tape.Library
	if err := Call(context.Background(), e.admin, "GET", base, nil, &inv); err != nil || len(inv.Drives) != 2 {
		t.Fatal("inventory", err)
	}
	run := func(req model.TapeRequest) model.Run {
		t.Helper()
		var r model.Run
		if err := Call(context.Background(), e.admin, "POST", base+"/operations", req, &r); err != nil {
			t.Fatal(err)
		}
		return e.wait(t, r.ID)
	}
	must := func(req model.TapeRequest) model.Run {
		t.Helper()
		r := run(req)
		if r.Status != "success" || r.Result == nil || !r.Result.Verified {
			t.Fatal(req.Action, r.Error)
		}
		return r
	}
	must(model.TapeRequest{Action: "load", Barcode: "SRK001L9", DriveID: "DRIVE1"})
	must(model.TapeRequest{Action: "label", Barcode: "SRK001L9", Label: "Offsite 01", Confirm: "SRK001L9", Reason: "initial marking"})
	written := must(model.TapeRequest{Action: "write", Barcode: "SRK001L9", SnapshotID: source.ID, RetentionDays: 30})
	var queue []model.Run
	if err := Call(context.Background(), e.admin, "GET", base+"/queue", nil, &queue); err != nil {
		t.Fatal(err)
	}
	foundWrite := false
	for _, item := range queue {
		if item.Tape == nil || item.Job.MediaID != e.media.Config.ID {
			t.Fatal("unrelated run in tape queue")
		}
		if item.ID == written.ID {
			foundWrite = true
		}
	}
	if !foundWrite {
		t.Fatal("completed tape operation missing from history")
	}

	must(model.TapeRequest{Action: "verify", Barcode: "SRK001L9"})
	failed := run(model.TapeRequest{Action: "erase", Barcode: "SRK001L9", Confirm: "SRK001L9", Reason: "must be rejected", EraseMode: "long"})
	if failed.Status != "error" {
		t.Fatal("retained copy erased")
	}
	restored := must(model.TapeRequest{Action: "restore", Barcode: "SRK001L9", CopyID: written.Result.Tape.CopyID, AgentID: e.proxy.Config.ID, RetentionDays: 7})
	points, err := e.manager.DB.Runs(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	var pointID string
	for _, point := range points {
		if point.Kind == "backup" && point.SnapshotID == restored.ID && point.Status == "success" {
			found = true
			pointID = point.ID
			if point.Job.Type != "openstack" || point.Job.AgentID != e.proxy.Config.ID || point.SnapshotState != "held" {
				t.Fatal("restored point registration mismatch")
			}
		}
	}
	if !found {
		t.Fatal("tape restore not registered")
	}
	var vmRestore model.Run
	if err = Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+pointID+"/restore", map[string]any{"vm_restore": model.VMRestoreRequest{Name: "VM from tape", Profile: "lab", FlavorID: "small", Networks: []string{testcloud.Network}}}, &vmRestore); err != nil {
		t.Fatal(err)
	}
	recovered := e.wait(t, vmRestore.ID)
	if recovered.Status != "success" || recovered.Result.VMRestore == nil || !recovered.Result.VMRestore.ControlPlaneVerified {
		t.Fatal("new VM from tape", recovered.Error)
	}
	must(model.TapeRequest{Action: "unload", Barcode: "SRK001L9"})
	pool := tape.Pool{ID: "DAILY", Name: "Daily", Barcodes: []string{"SRK001L9"}, Drives: []string{"DRIVE1", "DRIVE2"}}
	if err = Call(context.Background(), e.admin, "PUT", base+"/pools/DAILY", pool, nil); err != nil {
		t.Fatal("pool configuration", err)
	}
	pooled := must(model.TapeRequest{Action: "write", PoolID: "DAILY", SnapshotID: source.ID, RetentionDays: 7})
	if pooled.Result.Tape.PoolID != "DAILY" || pooled.Result.Tape.Barcode != "SRK001L9" {
		t.Fatal("pool receipt", pooled.Result)
	}
	if err = Call(context.Background(), e.admin, "GET", base, nil, &inv); err != nil || len(inv.Pools) != 1 || inv.Cartridges[0].Mounted {
		t.Fatal("pool unload/inventory", err)
	}

	// Mutating the media directly with an admin certificate cannot bypass manager audit.
	if err = Call(context.Background(), e.admin, "POST", e.media.Config.URL+"/v1/tape/operations/"+model.ID(), model.TapeRequest{Action: "load", Barcode: "SRK001L9", DriveID: "DRIVE1"}, nil); err == nil {
		t.Fatal("admin bypassed manager-only mutation")
	}
	// Auditors can inspect inventory but cannot queue destructive operations.
	cfg := e.manager.Config
	cfg.Role = "auditor"
	cfg.ID = "auditor-test"
	cfg.Cert = filepath.Join(filepath.Dir(cfg.Cert), "auditor-test.crt")
	cfg.Key = filepath.Join(filepath.Dir(cfg.Key), "auditor-test.key")
	auditor, err := identity.Client(cfg, identity.Peer{Role: "manager"})
	if err != nil {
		t.Fatal(err)
	}
	defer auditor.CloseIdleConnections()
	if err = Call(context.Background(), auditor, "GET", base, nil, &inv); err != nil {
		t.Fatal("auditor inventory", err)
	}
	if err = Call(context.Background(), auditor, "POST", base+"/operations", model.TapeRequest{Action: "load", Barcode: "SRK001L9", DriveID: "DRIVE1"}, nil); err == nil {
		t.Fatal("auditor queued mutation")
	}
}

func TestTapeDispatchReservesDriveWithoutBlockingOtherDrive(t *testing.T) {
	cfg := configs(t)["manager"]
	cfg.MaxParallel = 2
	cfg.PerMedia = 2
	m := managerTest(t, cfg)
	ctx := context.Background()
	mk := func(barcode, drive, status string, at time.Time) model.Run {
		job := model.Job{ID: model.ID(), MediaID: "test-media", Source: "tape://test-media/" + barcode}
		r := model.Run{ID: model.ID(), JobID: job.ID, Job: job, Kind: "tape-verify", Status: status, CreatedAt: at, Tape: &model.TapeRequest{Action: "verify", Barcode: barcode, DriveID: drive}}
		if _, e := m.DB.Enqueue(ctx, r, ""); e != nil {
			t.Fatal(e)
		}
		return r
	}
	now := time.Now().UTC()
	mk("A", "D1", "running", now.Add(-time.Minute))
	waiting := mk("B", "D1", "queued", now.Add(-time.Second))
	independent := mk("C", "D2", "queued", now)
	var wg sync.WaitGroup
	if e := m.dispatch(ctx, &wg); e != nil {
		t.Fatal(e)
	}
	wg.Wait()
	a, e := m.DB.Run(ctx, waiting.ID)
	if e != nil {
		t.Fatal(e)
	}
	b, e := m.DB.Run(ctx, independent.ID)
	if e != nil {
		t.Fatal(e)
	}
	if a.Status != "queued" || a.StartedAt != nil {
		t.Fatal("busy drive was dispatched")
	}
	// No media is registered: dispatch must still start D2; execution then fails normally.
	if b.StartedAt == nil || b.Status == "queued" {
		t.Fatal("free drive starved behind busy drive")
	}
}
