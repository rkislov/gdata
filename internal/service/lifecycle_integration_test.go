// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func seedPoint(t *testing.T, m *Media, expired bool) model.Manifest {
	t.Helper()
	data := []byte("данные из неизвестного репозитория")
	if e := m.Repo.Put(repository.Hash(data), data); e != nil {
		t.Fatal(e)
	}
	p := model.Manifest{Version: 1, ID: model.ID(), JobID: model.ID(), Source: "/lost/original", CreatedAt: time.Now().UTC().Add(-2 * time.Hour), ExpiresAt: time.Now().UTC().Add(time.Hour), Mode: "full", Consistency: "live-files", Files: []model.File{{Path: "recovered.txt", Kind: "file", Mode: 0640, ModTime: time.Now().UTC(), Size: int64(len(data)), Chunks: []string{repository.Hash(data)}}}, Result: model.Result{Files: 1, LogicalBytes: int64(len(data)), EffectiveMode: "full", Consistency: "live-files"}}
	if expired {
		p.ExpiresAt = time.Now().UTC().Add(-time.Hour)
	}
	if e := m.Repo.Commit(p); e != nil {
		t.Fatal(e)
	}
	return p
}
func waitMaintenance(t *testing.T, m *Manager, id string) model.Run {
	t.Helper()
	var out model.Run
	eventually(t, func() bool {
		r, e := m.DB.Run(context.Background(), id)
		if e == nil && (r.Status == "error" || r.Status == "cancelled") {
			t.Fatalf("operation %s: %s", id, r.Error)
		}
		out = r
		return e == nil && r.Status == "success"
	})
	return out
}

func TestUnknownExpiredRepositoryImportRestoreAndCleanup(t *testing.T) {
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	mgr := tlsServer(t, cfg["manager"], m.Handler())
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { media.Repo.Close() })
	point := seedPoint(t, media, true)
	ms := tlsServer(t, cfg["media"], media.Handler())
	restore := t.TempDir()
	ac := cfg["agent"]
	ac.RestoreRoots = []string{restore}
	agent, e := NewAgent(ac)
	if e != nil {
		t.Fatal(e)
	}
	as := tlsServer(t, ac, agent.Handler())
	for _, n := range []model.Node{{ID: ac.ID, Role: "agent", URL: as.URL, LastSeen: time.Now().UTC()}, {ID: cfg["media"].ID, Role: "media", URL: ms.URL, LastSeen: time.Now().UTC()}} {
		if e = m.DB.PutNode(context.Background(), n); e != nil {
			t.Fatal(e)
		}
	}
	cl, e := identity.Client(cfg["admin"], identity.Peer{Role: "manager"})
	if e != nil {
		t.Fatal(e)
	}
	defer cl.CloseIdleConnections()
	startScheduler(t, m)
	options := model.MaintenanceOptions{SnapshotIDs: []string{point.ID}, AgentID: ac.ID, RetentionDays: 7}
	var run model.Run
	if e = Call(context.Background(), cl, "POST", mgr.URL+"/v1/media/"+cfg["media"].ID+"/import", options, &run); e != nil {
		t.Fatal(e)
	}
	done := waitMaintenance(t, m, run.ID)
	if done.Maintenance == nil || done.Maintenance.Imported != 1 {
		t.Fatalf("import result: %+v", done.Maintenance)
	}
	jobs, e := m.DB.Jobs(context.Background())
	if e != nil || len(jobs) != 1 || jobs[0].Enabled {
		t.Fatal("imported job scheduled automatically")
	}
	runs, e := m.DB.Runs(context.Background(), 20)
	if e != nil {
		t.Fatal(e)
	}
	var backup model.Run
	for _, r := range runs {
		if r.Kind == "backup" {
			backup = r
		}
	}
	if backup.ID == point.ID || backup.SnapshotID != point.ID || backup.Origin == nil || backup.Origin.OriginalJobID != point.JobID || backup.SnapshotState != "held" {
		t.Fatalf("import identity/provenance lost: %+v", backup)
	}
	dest := filepath.Join(restore, "imported")
	if e = Call(context.Background(), cl, "POST", mgr.URL+"/v1/runs/"+backup.ID+"/restore", map[string]string{"destination": dest}, &run); e != nil {
		t.Fatal(e)
	}
	waitMaintenance(t, m, run.ID)
	data, e := os.ReadFile(filepath.Join(dest, "recovered.txt"))
	if e != nil || string(data) != "данные из неизвестного репозитория" {
		t.Fatal("imported data did not restore", e)
	}
	if e = Call(context.Background(), cl, "POST", mgr.URL+"/v1/media/"+cfg["media"].ID+"/import", options, &run); e != nil {
		t.Fatal(e)
	}
	done = waitMaintenance(t, m, run.ID)
	if done.Maintenance.Imported != 0 || done.Maintenance.Skipped != 1 {
		t.Fatal("repeat import duplicated point")
	}
	hold := false
	past := time.Now().UTC().Add(-time.Minute)
	policy := model.MaintenanceOptions{SnapshotID: point.ID, Policy: &model.SnapshotPolicy{Hold: &hold, ExpiresAt: &past}}
	if e = Call(context.Background(), cl, "POST", mgr.URL+"/v1/media/"+cfg["media"].ID+"/policy", policy, &run); e != nil {
		t.Fatal(e)
	}
	waitMaintenance(t, m, run.ID)
	var plan model.GCPlan
	if e = Call(context.Background(), cl, "GET", mgr.URL+"/v1/media/"+cfg["media"].ID+"/gc", nil, &plan); e != nil {
		t.Fatal(e)
	}
	if len(plan.Snapshots) != 1 || len(plan.Chunks) != 1 {
		t.Fatalf("cleanup preview incorrect: %+v", plan)
	}
	auditor, e := identity.Client(cfg["auditor"], identity.Peer{Role: "manager"})
	if e != nil {
		t.Fatal(e)
	}
	if e = Call(context.Background(), auditor, "POST", mgr.URL+"/v1/media/"+cfg["media"].ID+"/gc", model.MaintenanceOptions{Plan: &plan}, new(any)); e == nil {
		t.Fatal("auditor could remove data")
	}
	if e = Call(context.Background(), cl, "POST", mgr.URL+"/v1/media/"+cfg["media"].ID+"/gc", model.MaintenanceOptions{Plan: &plan}, &run); e != nil {
		t.Fatal(e)
	}
	done = waitMaintenance(t, m, run.ID)
	if done.Maintenance.SnapshotsRemoved != 1 || done.Maintenance.ChunksRemoved != 1 {
		t.Fatal("cleanup did not remove expired data")
	}
	audits, e := media.Repo.Audits()
	if e != nil || len(audits) < 4 {
		t.Fatal("durable media audit absent", e)
	}
	found := false
	for _, a := range audits {
		if a.ID == run.ID && a.Action == "gc" && a.Phase == "success" {
			found = true
		}
	}
	if !found {
		t.Fatal("media audit cannot be correlated with manager operation")
	}
	backup, e = m.DB.Run(context.Background(), backup.ID)
	if e != nil || backup.Status != "success" || backup.SnapshotState != "deleted" {
		t.Fatal("cleanup lost historical result or did not mark deletion")
	}
	if e = Call(context.Background(), cl, "POST", mgr.URL+"/v1/runs/"+backup.ID+"/restore", map[string]string{"destination": filepath.Join(restore, "again")}, new(any)); e == nil {
		t.Fatal("deleted point offered for restore")
	}
}
func TestAutomaticRetentionIsOptInAndDeduplicated(t *testing.T) {
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { media.Repo.Close() })
	p := seedPoint(t, media, true)
	server := tlsServer(t, cfg["media"], media.Handler())
	if e = m.DB.PutNode(context.Background(), model.Node{ID: cfg["media"].ID, Role: "media", URL: server.URL, LastSeen: time.Now().UTC(), Status: map[string]any{"repository_id": media.Repo.Info.ID}}); e != nil {
		t.Fatal(e)
	}
	if e = m.scheduleRetention(context.Background(), time.Now().UTC()); e != nil {
		t.Fatal(e)
	}
	runs, e := m.DB.Runs(context.Background(), 100)
	if e != nil || len(runs) != 0 {
		t.Fatal("automatic retention enabled implicitly")
	}
	m.Config.RetentionIntervalSeconds = 60
	now := time.Now().UTC()
	if e = m.scheduleRetention(context.Background(), now); e != nil {
		t.Fatal(e)
	}
	unknown, e := m.DB.Runs(context.Background(), 100)
	if e != nil || len(unknown) != 0 {
		t.Fatal("unknown repository cleaned before adoption")
	}
	job := model.Job{ID: p.JobID, Name: "known", MediaID: cfg["media"].ID, Source: p.Source, RetentionDays: 1}
	knownRun := model.Run{ID: p.ID, JobID: job.ID, Job: job, Kind: "backup", Status: "success", CreatedAt: p.CreatedAt, Result: &model.Result{RepositoryID: media.Repo.Info.ID}}
	if _, e = m.DB.Enqueue(context.Background(), knownRun, ""); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if e = m.scheduleRetention(context.Background(), now); e != nil {
			t.Fatal(e)
		}
	}
	runs, e = m.DB.Runs(context.Background(), 100)
	if e != nil || len(runs) != 2 {
		t.Fatal("automatic cleanup duplicated")
	}
	startScheduler(t, m)
	gcID := ""
	for _, r := range runs {
		if r.Kind == "gc" {
			gcID = r.ID
		}
	}
	if gcID == "" {
		t.Fatal("scheduled cleanup absent")
	}
	waitMaintenance(t, m, gcID)
	if _, e = media.Repo.Manifest(p.ID); !os.IsNotExist(e) {
		t.Fatal("automatic retention did not expire point")
	}
}
func TestReadLeaseSurvivesSnapshotDeadline(t *testing.T) {
	cfg := configs(t)
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { media.Repo.Close() })
	p := seedPoint(t, media, false)
	s := tlsServer(t, cfg["media"], media.Handler())
	ac := cfg["agent"]
	a, e := NewAgent(ac)
	if e != nil {
		t.Fatal(e)
	}
	key, e := os.ReadFile(ac.CapabilityKeyFile)
	if e != nil {
		t.Fatal(e)
	}
	v := model.Execute{RunID: model.ID(), Job: model.Job{MediaID: cfg["media"].ID}, MediaURL: s.URL, SnapshotID: p.ID}
	v.Grant = identity.Sign(identity.Grant{Subject: ac.ID, RunID: v.RunID, Permission: "read", SnapshotID: p.ID, Expires: time.Now().Add(time.Hour).Unix()}, key)
	if _, e = a.manifest(context.Background(), v, p.ID); e != nil {
		t.Fatal(e)
	}
	past := time.Now().UTC().Add(-time.Minute)
	if _, e = media.Repo.UpdatePolicy(p.ID, model.SnapshotPolicy{ExpiresAt: &past}); e != nil {
		t.Fatal(e)
	}
	resp, e := a.media(context.Background(), v, "GET", "/v1/chunks/"+p.Files[0].Chunks[0], nil)
	if e != nil {
		t.Fatal("active restore denied at expiry", e)
	}
	resp.Body.Close()
	if _, e = media.Repo.AutomaticGC(model.ID()); e != nil {
		t.Fatal(e)
	}
	if e = media.Repo.Verify(p.ID); e != nil {
		t.Fatal("read lease did not pin snapshot")
	}
	a.release(v)
	if _, e = media.Repo.AutomaticGC(model.ID()); e != nil {
		t.Fatal(e)
	}
	if _, e = media.Repo.Manifest(p.ID); !os.IsNotExist(e) {
		t.Fatal("expired snapshot not collected after reader released")
	}
}
