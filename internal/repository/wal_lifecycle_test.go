// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"bytes"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func oldWAL(t *testing.T, r *Repository, n int) model.WALRecord {
	t.Helper()
	v, b := walFixture(fmt.Sprintf("0000000100000000%08X", n))
	at := time.Now().UTC().Add(-72*time.Hour + time.Duration(n)*time.Minute)
	v.FirstCommitAt = &at
	v.LastCommitAt = &at
	v.TimeBoundsVerified = true
	got, e := r.PutWAL(v, bytes.NewReader(b))
	if e != nil {
		t.Fatal(e)
	}
	got.ArchivedAt = time.Now().UTC().Add(-48 * time.Hour)
	if e = writeEnvelope(filepath.Join(r.Root, "wal", v.ArchiveID, v.Name+".json"), got); e != nil {
		t.Fatal(e)
	}
	return got
}
func baseWAL(t *testing.T, r *Repository, v model.WALRecord) model.Manifest {
	t.Helper()
	finished := time.Now().UTC().Add(-72 * time.Hour)
	pg := &model.PostgreSQLBackup{Profile: v.Profile, SystemID: v.SystemID, SegmentSize: v.SegmentSize, ArchiveID: v.ArchiveID, Major: 16, StartedAt: finished.Add(-time.Minute), FinishedAt: finished, NativeVerified: true, WALRanges: []model.PostgreSQLWALRange{{Timeline: 1, StartLSN: "0/200028", EndLSN: "0/300000"}}}
	m := model.Manifest{Version: 1, ID: model.ID(), JobID: model.ID(), CreatedAt: finished, ExpiresAt: time.Now().Add(time.Hour), Mode: "full", Consistency: "postgresql-physical", PostgreSQL: pg, Result: model.Result{PostgreSQL: pg}, Files: []model.File{}}
	if e := r.Commit(m); e != nil {
		t.Fatal(e)
	}
	return m
}
func TestWALGCKeepsDependentExpiredBasesAndPins(t *testing.T) {
	r := testRepo(t)
	v := oldWAL(t, r, 1)
	oldWAL(t, r, 2)
	oldWAL(t, r, 3)
	oldWAL(t, r, 4)
	base := baseWAL(t, r, v)
	before := time.Now().Add(-24 * time.Hour)
	plan, e := r.PreviewWALGC(v.ArchiveID, before, false)
	if e != nil {
		t.Fatal(e)
	}
	if len(plan.Names) != 1 || plan.Names[0] != v.Name {
		t.Fatalf("dependency not retained: %+v", plan)
	}
	expired := time.Now().Add(-time.Hour)
	if _, e = r.UpdatePolicy(base.ID, model.SnapshotPolicy{ExpiresAt: &expired}); e != nil {
		t.Fatal(e)
	}
	plan, e = r.PreviewWALGC(v.ArchiveID, before, false)
	if e != nil {
		t.Fatal(e)
	}
	if len(plan.Names) != 1 || len(plan.Dependencies) != 1 {
		t.Fatal("expired physical base lost protection")
	}
	pin := model.WALPin{ID: model.ID(), ArchiveID: v.ArchiveID, AgentID: v.AgentID, ExpiresAt: time.Now().Add(time.Hour)}
	if e = r.PinWAL(pin); e != nil {
		t.Fatal(e)
	}
	if _, e = r.ApplyWALGC(plan, model.ID()); !errors.Is(e, ErrPlanStale) {
		t.Fatalf("new pin did not invalidate plan: %v", e)
	}
	blocked, e := r.PreviewWALGC(v.ArchiveID, before, false)
	if e != nil || len(blocked.Names) != 0 || blocked.ActivePins != 1 {
		t.Fatal("pin not respected", e)
	}
	if e = r.ReleaseWALPin(pin.ArchiveID, pin.ID, pin.AgentID); e != nil {
		t.Fatal(e)
	}
	plan, e = r.PreviewWALGC(v.ArchiveID, before, false)
	if e != nil {
		t.Fatal(e)
	}
	id := model.ID()
	result, e := r.ApplyWALGC(plan, id)
	if e != nil || result.WALRemoved != 1 {
		t.Fatal("cleanup failed", result, e)
	}
	if _, _, e = r.OpenWAL(v.ArchiveID, v.Name); !os.IsNotExist(e) {
		t.Fatal("candidate not removed", e)
	}
	if _, e = r.ApplyWALGC(plan, id); e != nil {
		t.Fatal("retry failed", e)
	}
	plan.Token = "changed"
	if _, e = r.ApplyWALGC(plan, id); e == nil {
		t.Fatal("different plan reused operation receipt")
	}
}
func TestWALGCOrphanAndUnknownData(t *testing.T) {
	r := testRepo(t)
	v := oldWAL(t, r, 1)
	oldWAL(t, r, 2)
	before := time.Now().Add(-24 * time.Hour)
	p, e := r.PreviewWALGC(v.ArchiveID, before, false)
	if e != nil || len(p.Names) != 0 || len(p.Blocked) == 0 {
		t.Fatal("orphan archive cleaned without explicit review", e)
	}
	p, e = r.PreviewWALGC(v.ArchiveID, before, true)
	if e != nil || len(p.Names) != 1 {
		t.Fatal("orphan review failed", e)
	}
	if e = os.WriteFile(filepath.Join(r.Root, "wal", v.ArchiveID, "000000010000000000000003"), []byte("unpublished"), 0600); e != nil {
		t.Fatal(e)
	}
	p, e = r.PreviewWALGC(v.ArchiveID, before, true)
	if e != nil || len(p.Names) != 0 || len(p.Blocked) == 0 {
		t.Fatal("unknown archive data not protected", e)
	}
}
func TestCalendarExposesGapAndUnknownTimeEvidence(t *testing.T) {
	r := testRepo(t)
	v := oldWAL(t, r, 1)
	oldWAL(t, r, 2)
	oldWAL(t, r, 3)
	oldWAL(t, r, 5)
	base := baseWAL(t, r, v)
	c, e := r.WALCalendar(v.ArchiveID)
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Windows) != 1 || c.Windows[0].Status != "gap" || len(c.Windows[0].Gaps) != 1 || c.Windows[0].ToExclusive == nil {
		t.Fatalf("unexpected gap calendar: %+v", c)
	}
	name := "000000010000000000000002"
	var meta model.WALRecord
	file := filepath.Join(r.Root, "wal", v.ArchiveID, name+".json")
	if e = readEnvelope(file, &meta); e != nil {
		t.Fatal(e)
	}
	meta.TimeBoundsVerified = false
	meta.FirstCommitAt = nil
	meta.LastCommitAt = nil
	_ = writeEnvelope(file, meta)
	c, e = r.WALCalendar(v.ArchiveID)
	if e != nil || c.Windows[0].ToExclusive != nil || c.Windows[0].BaseID != base.ID {
		t.Fatal("unverified timestamps gave availability", e)
	}
}

func TestWALGCRecoversPartialQuarantineAndRejectsCorruption(t *testing.T) {
	r := testRepo(t)
	v := oldWAL(t, r, 1)
	oldWAL(t, r, 2)
	p, e := r.PreviewWALGC(v.ArchiveID, time.Now().Add(-24*time.Hour), true)
	if e != nil {
		t.Fatal(e)
	}
	operation := model.ID()
	stage := filepath.Join(r.Root, ".wal-gc", operation)
	if e = os.MkdirAll(stage, 0700); e != nil {
		t.Fatal(e)
	}
	j := walGCJournal{Version: 1, OperationID: operation, Phase: "moving", Plan: p, Report: model.MaintenanceResult{RepositoryID: r.Info.ID, ArchiveID: v.ArchiveID, OperationID: operation, WALRemoved: 1, BytesRemoved: v.Bytes, At: time.Now()}}
	if e = writeEnvelope(filepath.Join(r.Root, ".wal-gc", "journal.json"), j); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(filepath.Join(r.Root, "wal", v.ArchiveID, v.Name), filepath.Join(stage, v.Name)); e != nil {
		t.Fatal(e)
	}
	root := r.Root
	_ = r.Close()
	reopened, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	if report, e := reopened.ApplyWALGC(p, operation); e != nil || report.WALRemoved != 1 {
		t.Fatal("recovery receipt", report, e)
	}
	v = oldWAL(t, reopened, 3)
	oldWAL(t, reopened, 4)
	p, e = reopened.PreviewWALGC(v.ArchiveID, time.Now().Add(-24*time.Hour), true)
	if e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(reopened.Root, "wal", v.ArchiveID, v.Name)
	info, e := os.Stat(file)
	if e != nil {
		t.Fatal(e)
	}
	fd, e := os.OpenFile(file, os.O_WRONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = fd.WriteAt([]byte{255}, 100)
	_ = fd.Close()
	_ = os.Chtimes(file, info.ModTime(), info.ModTime())
	if _, e = reopened.ApplyWALGC(p, model.ID()); e == nil {
		t.Fatal("corrupt WAL deleted")
	}
	if _, e = os.Stat(file); e != nil {
		t.Fatal("corrupt WAL should remain for investigation")
	}
}
