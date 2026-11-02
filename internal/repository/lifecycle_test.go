// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"errors"
	"gdata/srk/internal/model"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func point(t *testing.T, r *Repository, b []byte, expired bool) model.Manifest {
	t.Helper()
	m := manifest(b)
	m.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	if expired {
		m.ExpiresAt = time.Now().UTC().Add(-time.Hour)
	}
	if e := r.Put(Hash(b), b); e != nil {
		t.Fatal(e)
	}
	if e := r.Commit(m); e != nil {
		t.Fatal(e)
	}
	return m
}
func TestGCSharedChunksAndIdempotency(t *testing.T) {
	r := testRepo(t)
	expired := point(t, r, []byte("shared"), true)
	retained := point(t, r, []byte("shared"), false)
	plan, e := r.PreviewGC()
	if e != nil {
		t.Fatal(e)
	}
	if len(plan.Snapshots) != 1 || len(plan.Chunks) != 0 {
		t.Fatalf("shared blocks selected: %+v", plan)
	}
	op := model.ID()
	report, e := r.ApplyGC(plan, op)
	if e != nil {
		t.Fatal(e)
	}
	if report.SnapshotsRemoved != 1 || report.ChunksRemoved != 0 {
		t.Fatal(report)
	}
	if e = r.Verify(retained.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Manifest(expired.ID); !os.IsNotExist(e) {
		t.Fatal("expired snapshot not removed")
	}
	again, e := r.ApplyGC(plan, op)
	if e != nil || again.OperationID != op {
		t.Fatalf("idempotent cleanup: %+v %v", again, e)
	}
}
func TestGCProtectsPersistentWriterLease(t *testing.T) {
	r := testRepo(t)
	root := r.Root
	lease := model.Lease{ID: model.ID(), Subject: "agent", Kind: "write", ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if e := r.BeginLease(lease); e != nil {
		t.Fatal(e)
	}
	b := []byte("uncommitted")
	if e := r.Put(Hash(b), b); e != nil {
		t.Fatal(e)
	}
	r.Close()
	r, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	p, e := r.PreviewGC()
	if e != nil {
		t.Fatal(e)
	}
	if p.ActiveWriters != 1 || len(p.Chunks) != 0 {
		t.Fatal("writer protection lost after restart")
	}
	if e = r.EndLease(lease.ID); e != nil {
		t.Fatal(e)
	}
	if r.BeginLease(lease) != ErrLeaseClosed {
		t.Fatal("closed capability reopened")
	}
	p, e = r.PreviewGC()
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Chunks) != 1 {
		t.Fatal("unreferenced chunk not collected")
	}
	if _, e = r.ApplyGC(p, model.ID()); e != nil {
		t.Fatal(e)
	}
	if r.Has(Hash(b)) {
		t.Fatal("orphan block remains")
	}
}
func TestGCProtectsReadAndHoldAcrossExpiry(t *testing.T) {
	r := testRepo(t)
	m := point(t, r, []byte("read protected"), false)
	lease := model.Lease{ID: model.ID(), Subject: "agent", Kind: "read", SnapshotID: m.ID, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if e := r.BeginLease(lease); e != nil {
		t.Fatal(e)
	}
	past := time.Now().UTC().Add(-time.Minute)
	if _, e := r.UpdatePolicy(m.ID, model.SnapshotPolicy{ExpiresAt: &past}); e != nil {
		t.Fatal(e)
	}
	p, e := r.PreviewGC()
	if e != nil || len(p.Snapshots) != 0 || p.ActiveReaders != 1 {
		t.Fatalf("reader not pinned: %+v %v", p, e)
	}
	if e = r.EndLease(lease.ID); e != nil {
		t.Fatal(e)
	}
	hold := true
	if _, e = r.UpdatePolicy(m.ID, model.SnapshotPolicy{Hold: &hold, Reason: "investigation"}); e != nil {
		t.Fatal(e)
	}
	if e = r.Available(m.ID); e != nil {
		t.Fatal("held point unavailable", e)
	}
	p, e = r.PreviewGC()
	if e != nil || len(p.Snapshots) != 0 {
		t.Fatal("held point selected")
	}
	hold = false
	if _, e = r.UpdatePolicy(m.ID, model.SnapshotPolicy{Hold: &hold}); e != nil {
		t.Fatal(e)
	}
	p, e = r.PreviewGC()
	if e != nil || len(p.Snapshots) != 1 || len(p.Chunks) != 1 {
		t.Fatalf("released point not selected: %+v %v", p, e)
	}
}
func TestGCRejectsStalePreviewWithoutDeleting(t *testing.T) {
	r := testRepo(t)
	m := point(t, r, []byte("old"), true)
	p, e := r.PreviewGC()
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Put(Hash([]byte("new")), []byte("new")); e != nil {
		t.Fatal(e)
	}
	if _, e = r.ApplyGC(p, model.ID()); !errors.Is(e, ErrPlanStale) {
		t.Fatal("stale preview accepted", e)
	}
	if e = r.Verify(m.ID); e != nil {
		t.Fatal("stale cleanup mutated data", e)
	}
}
func TestGCCorruptManifestFailsClosed(t *testing.T) {
	r := testRepo(t)
	m := point(t, r, []byte("safe"), true)
	if e := os.WriteFile(filepath.Join(r.Root, "snapshots", m.ID+".json"), []byte("broken"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := r.PreviewGC(); e == nil {
		t.Fatal("corrupt manifest accepted")
	}
	if !r.Has(Hash([]byte("safe"))) {
		t.Fatal("data removed on catalog corruption")
	}
}
func TestCleanupJournalResumesAfterInterruptedQuarantine(t *testing.T) {
	r := testRepo(t)
	expired := point(t, r, []byte("expire"), true)
	retained := point(t, r, []byte("keep"), false)
	p, e := r.PreviewGC()
	if e != nil {
		t.Fatal(e)
	}
	op := model.ID()
	j := gcJournal{Version: 1, OperationID: op, Phase: "moving", Plan: p, Report: model.MaintenanceResult{OperationID: op, RepositoryID: r.Info.ID, SnapshotsRemoved: 1, ChunksRemoved: 1}}
	if e = writeEnvelope(filepath.Join(r.Root, ".gc", "journal.json"), j); e != nil {
		t.Fatal(e)
	}
	stage := filepath.Join(r.Root, ".gc", op, "snapshots")
	if e = os.MkdirAll(stage, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(filepath.Join(r.Root, "snapshots", expired.ID+".json"), filepath.Join(stage, expired.ID+".json")); e != nil {
		t.Fatal(e)
	}
	root := r.Root
	id := r.Info.ID
	r.Close()
	reopened, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	if reopened.Info.ID != id {
		t.Fatal("repository identity changed")
	}
	if e = reopened.Verify(retained.ID); e != nil {
		t.Fatal(e)
	}
	if reopened.Has(Hash([]byte("expire"))) {
		t.Fatal("journal did not complete cleanup")
	}
	if _, e = os.Stat(filepath.Join(root, ".gc", "journal.json")); !os.IsNotExist(e) {
		t.Fatal("journal remains")
	}
}
func TestImportExpiredPointRenewsAndHolds(t *testing.T) {
	r := testRepo(t)
	m := point(t, r, []byte("import"), true)
	inventory, e := r.Inventory(true)
	if e != nil || len(inventory) != 1 || !inventory[0].Verified {
		t.Fatal("inventory verification failed", e)
	}
	imported, e := r.PrepareImport([]string{m.ID}, 30)
	if e != nil {
		t.Fatal(e)
	}
	if !imported[0].Hold || !imported[0].ExpiresAt.After(time.Now()) {
		t.Fatal("import was not quarantined and renewed")
	}
	p, e := r.PreviewGC()
	if e != nil || len(p.Snapshots) != 0 {
		t.Fatal("fresh import selected for deletion")
	}
	if e = r.Available(m.ID); e != nil {
		t.Fatal(e)
	}
}
func TestGCDoesNotCollectBlocksWhileCopyCommits(t *testing.T) {
	r := testRepo(t)
	m := manifest([]byte("active copy"))
	lease := model.Lease{ID: m.ID, Kind: "write", Subject: "agent", ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if e := r.BeginLease(lease); e != nil {
		t.Fatal(e)
	}
	if e := r.Put(Hash([]byte("active copy")), []byte("active copy")); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := r.AutomaticGC(model.ID()); done <- e }()
	if e := r.CommitLease(m, m.ID); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if e := r.Verify(m.ID); e != nil {
		t.Fatal("concurrent cleanup damaged committed copy", e)
	}
}
