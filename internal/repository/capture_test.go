// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"bytes"
	"context"
	"errors"
	"gdata/srk/internal/model"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func captureFixture() *model.CaptureTicket {
	now := time.Now().UTC()
	return &model.CaptureTicket{ID: model.ID(), Job: model.Job{ID: model.ID(), Type: "disk-image", Mode: "full", Name: "physical disk", MediaID: "media", Source: "disk://rescue/dev/vda", RetentionDays: 30}, Capture: model.DiskCapture{ScopeID: model.ID(), ExpiresAt: now.Add(time.Hour), Disk: model.DiskGeometry{Host: "rescue", Device: "/dev/vda", Bytes: ChunkSize + 4096, SectorSize: 4096}}, CreatedAt: now}
}
func waitCapture(t *testing.T, s *CaptureStore, id string) model.CaptureStatus {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		status, e := s.Status(id)
		if e != nil {
			t.Fatal(e)
		}
		if status.Phase == "complete" || status.Phase == "error" {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("capture verification timed out")
	return model.CaptureStatus{}
}
func TestCaptureJournalDedupRestartAndCommit(t *testing.T) {
	r := testRepo(t)
	store := NewCaptureStore(r)
	defer store.Close()
	ticket := captureFixture()
	if e := store.Create(*ticket); e != nil {
		t.Fatal(e)
	}
	if e := store.Create(*ticket); e != nil {
		t.Fatal("idempotent create", e)
	}
	first := bytes.Repeat([]byte{42}, ChunkSize)
	last := bytes.Repeat([]byte{91}, 4096)
	if _, e := store.Commit(ticket.ID); e == nil {
		t.Fatal("incomplete disk committed")
	}
	if _, _, e := store.Block(ticket.ID, 1, Hash(last), last); e == nil {
		t.Fatal("out of order block accepted")
	}
	if present, _, e := store.Block(ticket.ID, 0, Hash(first), nil); e != nil || present {
		t.Fatal("missing block probe", e)
	}
	if _, status, e := store.Block(ticket.ID, 0, Hash(first), first); e != nil || status.Result.UploadedBytes != ChunkSize {
		t.Fatal("upload", status, e)
	}
	if _, status, e := store.Block(ticket.ID, 0, Hash(first), first); e != nil || status.Result.LogicalBytes != ChunkSize {
		t.Fatal("retry double counted", status, e)
	}
	if _, _, e := store.Block(ticket.ID, 0, Hash(bytes.Repeat([]byte{1}, ChunkSize)), nil); e == nil {
		t.Fatal("changed retry accepted")
	}
	if p, e := r.PreviewGC(); e != nil || p.ActiveWriters != 1 || len(p.Chunks) != 0 {
		t.Fatal("upload chunks are not pinned", p, e)
	}
	store.Close()
	root := r.Root
	_ = r.Close()
	reopened, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	again := NewCaptureStore(reopened)
	defer again.Close()
	status, e := again.Status(ticket.ID)
	if e != nil || status.NextIndex != 1 || status.Result.UploadedBytes != ChunkSize {
		t.Fatal("restart lost acknowledged blocks", status, e)
	}
	if _, _, e = again.Block(ticket.ID, 1, Hash(last), last); e != nil {
		t.Fatal(e)
	}
	if _, e = again.Commit(ticket.ID); e != nil {
		t.Fatal(e)
	}
	status = waitCapture(t, again, ticket.ID)
	if !status.Result.Verified || status.Result.ProgressPercent == nil || *status.Result.ProgressPercent != 100 {
		t.Fatal(status)
	}
	manifest, e := reopened.Manifest(ticket.ID)
	if e != nil || manifest.Disk == nil || manifest.Disk.SectorSize != 4096 || manifest.Files[0].Chunks[0] != Hash(first) {
		t.Fatal("disk metadata", manifest, e)
	}
	next := *ticket
	next.ID = model.ID()
	next.Job.ID = model.ID()
	if e = again.Create(next); e != nil {
		t.Fatal(e)
	}
	if present, _, e := again.Block(next.ID, 0, Hash(first), nil); e != nil || !present {
		t.Fatal("dedup failed", e)
	}
	if present, st, e := again.Block(next.ID, 1, Hash(last), nil); e != nil || !present || st.Result.UploadedBytes != 0 {
		t.Fatal("dedup accounting", st, e)
	}
	if _, e = again.Commit(next.ID); e != nil {
		t.Fatal(e)
	}
	if !waitCapture(t, again, next.ID).Result.Verified {
		t.Fatal("dedup capture not verified")
	}
}
func TestCaptureCorruptionAndCanceledVerification(t *testing.T) {
	r := testRepo(t)
	store := NewCaptureStore(r)
	defer store.Close()
	ticket := captureFixture()
	ticket.Capture.Disk.Bytes = 4096
	if e := store.Create(*ticket); e != nil {
		t.Fatal(e)
	}
	b := bytes.Repeat([]byte{9}, 4096)
	hash := Hash(b)
	if _, _, e := store.Block(ticket.ID, 0, hash, []byte("corrupt")); e == nil {
		t.Fatal("unverified bytes accepted")
	}
	if _, _, e := store.Block(ticket.ID, 0, hash, b); e != nil {
		t.Fatal(e)
	}
	file, _ := r.chunk(hash)
	if e := os.WriteFile(file, bytes.Repeat([]byte{1}, 4096), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := store.Commit(ticket.ID); e != nil {
		t.Fatal(e)
	}
	status := waitCapture(t, store, ticket.ID)
	if status.Phase != "error" || status.Result.Verified {
		t.Fatal("corrupt snapshot marked successful")
	}
	if _, e := r.Manifest(ticket.ID); !os.IsNotExist(e) {
		t.Fatal("corrupt snapshot exists", e)
	}
	fresh := captureFixture()
	fresh.Capture.Disk.Bytes = 4096
	if e := store.Create(*fresh); e != nil {
		t.Fatal(e)
	}
	good := bytes.Repeat([]byte{44}, 4096)
	if _, _, e := store.Block(fresh.ID, 0, Hash(good), good); e != nil {
		t.Fatal(e)
	}
	state, e := store.find(fresh.ID)
	if e != nil {
		t.Fatal(e)
	}
	state.mu.Lock()
	manifest, e := store.manifest(state)
	state.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e = r.CommitLeaseContext(ctx, manifest, fresh.ID); !errors.Is(e, context.Canceled) {
		t.Fatalf("canceled verification was not honored: %v", e)
	}
	if _, e = r.Manifest(fresh.ID); !os.IsNotExist(e) {
		t.Fatal("canceled verification produced a snapshot", e)
	}
}
func TestCaptureRepairsOnlyUnacknowledgedTail(t *testing.T) {
	r := testRepo(t)
	store := NewCaptureStore(r)
	ticket := captureFixture()
	ticket.Capture.Disk.Bytes = 4096
	if e := store.Create(*ticket); e != nil {
		t.Fatal(e)
	}
	dir := store.dir(ticket.ID)
	store.Close()
	if e := os.WriteFile(filepath.Join(dir, "blocks.journal"), []byte("torn append"), 0600); e != nil {
		t.Fatal(e)
	}
	recovered := NewCaptureStore(r)
	defer recovered.Close()
	status, e := recovered.Status(ticket.ID)
	if e != nil || status.NextIndex != 0 {
		t.Fatal("unacknowledged tail recovery", status, e)
	}
	b := make([]byte, captureRecordSize)
	if e = os.WriteFile(filepath.Join(dir, "blocks.journal"), b, 0600); e != nil {
		t.Fatal(e)
	}
	fresh := NewCaptureStore(r)
	defer fresh.Close()
	if _, e = fresh.Status(ticket.ID); e == nil {
		t.Fatal("bad complete record silently discarded")
	}
}
