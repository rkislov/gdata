// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package tape

import (
	"bytes"
	"context"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func emulatorConfig() Config {
	return Config{Backend: "emulator", LibraryID: "LAB", Drives: []string{"DRIVE1", "DRIVE2"}, Cartridges: []MediumSpec{{Barcode: "SRK001L9", Slot: 1, Generation: 9, CapacityBytes: 32 << 20}, {Barcode: "SRK002L9", Slot: 2, Generation: 9, CapacityBytes: 32 << 20}}}
}
func tapeFixture(t *testing.T) (*Store, *repository.Repository, model.Manifest) {
	t.Helper()
	s, e := Open(t.TempDir(), emulatorConfig())
	if e != nil {
		t.Fatal(e)
	}
	r, e := repository.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { r.Close() })
	data := bytes.Repeat([]byte("tape-data"), 1000)
	h := repository.Hash(data)
	if e = r.Put(h, data); e != nil {
		t.Fatal(e)
	}
	m := model.Manifest{Version: 1, ID: model.ID(), JobID: model.ID(), CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour), Mode: "full", Consistency: "crash-consistent", Files: []model.File{{Path: "data.bin", Kind: "file", Size: int64(len(data)), Chunks: []string{h}}, {Path: "duplicate.bin", Kind: "file", Size: int64(len(data)), Chunks: []string{h}}}}
	if e = r.Commit(m); e != nil {
		t.Fatal(e)
	}
	return s, r, m
}
func runTape(t *testing.T, s *Store, r *repository.Repository, v model.TapeRequest) model.Result {
	t.Helper()
	res, e := s.Execute(context.Background(), model.ID(), v, r, func(model.Result) {})
	if e != nil {
		t.Fatal(v.Action, e)
	}
	if !res.Verified {
		t.Fatal("unverified tape result")
	}
	return res
}
func loadLabel(t *testing.T, s *Store, r *repository.Repository) {
	runTape(t, s, r, model.TapeRequest{Action: "load", Barcode: "SRK001L9", DriveID: "DRIVE1"})
	runTape(t, s, r, model.TapeRequest{Action: "label", Barcode: "SRK001L9", Label: "Offsite 01", Confirm: "SRK001L9", Reason: "initial label"})
}
func TestTapeStandaloneRestoreImportRetentionAndRecycle(t *testing.T) {
	s, r, m := tapeFixture(t)
	loadLabel(t, s, r)
	written := runTape(t, s, r, model.TapeRequest{Action: "write", Barcode: "SRK001L9", SnapshotID: m.ID, RetentionDays: 30})
	if written.UploadedBytes != 9000 {
		t.Fatalf("not deduplicated within archive: %d", written.UploadedBytes)
	}
	erase := model.TapeRequest{Action: "erase", Barcode: "SRK001L9", Confirm: "SRK001L9", Reason: "retire", EraseMode: "quick"}
	if _, e := s.Execute(context.Background(), model.ID(), erase, r, func(model.Result) {}); e == nil {
		t.Fatal("retained tape erased")
	}
	// Rebuild a lost central catalog from the medium, then restore into an empty repository.
	if e := os.Remove(filepath.Join(s.root, "catalog.json")); e != nil {
		t.Fatal(e)
	}
	other, e := Open(s.root, emulatorConfig())
	if e != nil {
		t.Fatal(e)
	}
	runTape(t, other, r, model.TapeRequest{Action: "load", Barcode: "SRK001L9", DriveID: "DRIVE1"})
	if e = other.Control(context.Background(), model.TapeRequest{Action: "label", Barcode: "SRK001L9", Confirm: "SRK001L9", Label: "hide data", Reason: "test"}); e == nil {
		t.Fatal("unknown tape relabelled")
	}
	runTape(t, other, r, model.TapeRequest{Action: "import", Barcode: "SRK001L9"})
	restoredRepo, e := repository.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer restoredRepo.Close()
	out := runTape(t, other, restoredRepo, model.TapeRequest{Action: "restore", Barcode: "SRK001L9", CopyID: written.Tape.CopyID, RetentionDays: 7})
	got, e := restoredRepo.Manifest(out.Tape.Snapshot.ID)
	if e != nil || len(got.Files) != 2 || !out.Tape.Snapshot.Hold {
		t.Fatalf("standalone restore %+v %v", got, e)
	}
	if e = restoredRepo.Verify(got.ID); e != nil {
		t.Fatal(e)
	}
	other.now = func() time.Time { return time.Now().AddDate(0, 0, 31) }
	erase.Action = "recycle"
	runTape(t, other, r, erase)
	inv, _ := other.Inventory(context.Background())
	if inv.Cartridges[0].Dirty || len(inv.Cartridges[0].Copies) != 0 || inv.Cartridges[0].UsedBytes != 0 {
		t.Fatal("recycle not blank", inv)
	}
	// Write again after explicit recycling; no implicit overwrite of retained data.
	other.now = time.Now
	runTape(t, other, r, model.TapeRequest{Action: "write", Barcode: "SRK001L9", SnapshotID: m.ID, RetentionDays: 1})
}
func TestTapeCorruptionCancellationBusyAndReplay(t *testing.T) {
	s, r, m := tapeFixture(t)
	loadLabel(t, s, r)
	ctx, cancel := context.WithCancel(context.Background())
	id := model.ID()
	req := model.TapeRequest{Action: "write", Barcode: "SRK001L9", SnapshotID: m.ID, RetentionDays: 2}
	_, e := s.Execute(ctx, id, req, r, func(model.Result) { cancel() })
	if e == nil {
		t.Fatal("cancelled write accepted")
	}
	inv, _ := s.Inventory(context.Background())
	if !inv.Cartridges[0].Dirty || inv.Cartridges[0].Busy {
		t.Fatal("incomplete write not protected")
	}
	if _, e = s.Execute(context.Background(), id, req, r, func(model.Result) {}); e == nil {
		t.Fatal("incomplete operation replayed")
	}
	if e = s.Control(context.Background(), model.TapeRequest{Action: "label", Barcode: "SRK001L9", Confirm: "SRK001L9", Label: "new", Reason: "test"}); e == nil {
		t.Fatal("dirty cartridge relabelled")
	}
	// Completed archive bytes can exist despite a cancelled verification; importing verifies them.
	runTape(t, s, r, model.TapeRequest{Action: "import", Barcode: "SRK001L9"})
	inv, _ = s.Inventory(context.Background())
	if len(inv.Cartridges[0].Copies) != 1 {
		t.Fatal("completed bytes not imported")
	}
	path := filepath.Join(s.volumeDir("SRK001L9"), id+".srktape")
	f, e := os.OpenFile(path, os.O_WRONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = f.WriteAt([]byte{0xFF}, 100)
	f.Close()
	if _, e = s.Execute(context.Background(), model.ID(), model.TapeRequest{Action: "verify", Barcode: "SRK001L9"}, r, func(model.Result) {}); e == nil {
		t.Fatal("corrupt archive verified")
	}
}

func TestTapeHoldsDurablePolicyAndLongErase(t *testing.T) {
	s, r, m := tapeFixture(t)
	loadLabel(t, s, r)
	written := runTape(t, s, r, model.TapeRequest{Action: "write", Barcode: "SRK001L9", SnapshotID: m.ID, RetentionDays: 1})
	runTape(t, s, r, model.TapeRequest{Action: "hold", Barcode: "SRK001L9", CopyID: written.Tape.CopyID})
	s.now = func() time.Time { return time.Now().AddDate(0, 0, 2) }
	erase := model.TapeRequest{Action: "erase", Barcode: "SRK001L9", Confirm: "SRK001L9", Reason: "expired", EraseMode: "long"}
	if _, e := s.Execute(context.Background(), model.ID(), erase, r, func(model.Result) {}); e == nil {
		t.Fatal("hold bypassed")
	}
	// A catalogue rebuild must preserve policy stored with the medium.
	if e := os.Remove(filepath.Join(s.root, "catalog.json")); e != nil {
		t.Fatal(e)
	}
	s2, e := Open(s.root, emulatorConfig())
	if e != nil {
		t.Fatal(e)
	}
	runTape(t, s2, r, model.TapeRequest{Action: "load", Barcode: "SRK001L9", DriveID: "DRIVE1"})
	runTape(t, s2, r, model.TapeRequest{Action: "import", Barcode: "SRK001L9"})
	inv, _ := s2.Inventory(context.Background())
	if !inv.Cartridges[0].Copies[0].Hold {
		t.Fatal("medium lost hold")
	}
	runTape(t, s2, r, model.TapeRequest{Action: "release-hold", Barcode: "SRK001L9", CopyID: written.Tape.CopyID, Confirm: "SRK001L9", Reason: "approved release"})
	// Removing a hold does not shorten retention.
	if _, e = s2.Execute(context.Background(), model.ID(), erase, r, func(model.Result) {}); e == nil {
		t.Fatal("release shortened retention")
	}
	s2.now = func() time.Time { return time.Now().AddDate(0, 0, 2) }
	id := model.ID()
	first, e := s2.Execute(context.Background(), id, erase, r, func(model.Result) {})
	if e != nil {
		t.Fatal(e)
	}
	again, e := s2.Execute(context.Background(), id, erase, r, func(model.Result) {})
	if e != nil || !again.Verified || !first.Verified {
		t.Fatal("completed erase replay", e)
	}
	inv, _ = s2.Inventory(context.Background())
	if inv.Cartridges[0].UsedBytes != 0 || inv.Cartridges[0].Dirty {
		t.Fatal("long erase not finished")
	}
}
func TestTapeCapacityAndConcurrentDrives(t *testing.T) {
	s, r, m := tapeFixture(t)
	loadLabel(t, s, r)
	s.mu.Lock()
	s.state.Cartridges[0].CapacityBytes = 1
	s.mu.Unlock()
	if _, e := s.Execute(context.Background(), model.ID(), model.TapeRequest{Action: "write", Barcode: "SRK001L9", SnapshotID: m.ID, RetentionDays: 1}, r, func(model.Result) {}); e == nil {
		t.Fatal("capacity ignored")
	}
	inv, _ := s.Inventory(context.Background())
	if inv.Cartridges[0].Dirty || len(inv.Cartridges[0].Copies) != 0 {
		t.Fatal("preflight failure dirtied cartridge")
	}
	s.mu.Lock()
	s.state.Cartridges[0].CapacityBytes = 32 << 20
	s.mu.Unlock()
	runTape(t, s, r, model.TapeRequest{Action: "load", Barcode: "SRK002L9", DriveID: "DRIVE2"})
	runTape(t, s, r, model.TapeRequest{Action: "label", Barcode: "SRK002L9", Label: "second", Confirm: "SRK002L9", Reason: "initial"})
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		once := false
		_, e := s.Execute(context.Background(), model.ID(), model.TapeRequest{Action: "write", Barcode: "SRK001L9", SnapshotID: m.ID, RetentionDays: 1}, r, func(model.Result) {
			if !once {
				once = true
				close(entered)
				<-release
			}
		})
		done <- e
	}()
	<-entered
	if e := s.Control(context.Background(), model.TapeRequest{Action: "unload", Barcode: "SRK001L9"}); e == nil {
		t.Error("busy cartridge unloaded")
	}
	runTape(t, s, r, model.TapeRequest{Action: "write", Barcode: "SRK002L9", SnapshotID: m.ID, RetentionDays: 1})
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}
func TestTapeInterruptedLongEraseCanResumeAfterRestart(t *testing.T) {
	s, r, m := tapeFixture(t)
	loadLabel(t, s, r)
	runTape(t, s, r, model.TapeRequest{Action: "write", Barcode: "SRK001L9", SnapshotID: m.ID, RetentionDays: 1})
	s.now = func() time.Time { return time.Now().AddDate(0, 0, 2) }
	ctx, cancel := context.WithCancel(context.Background())
	req := model.TapeRequest{Action: "erase", Barcode: "SRK001L9", Confirm: "SRK001L9", Reason: "expired", EraseMode: "long"}
	_, e := s.Execute(ctx, model.ID(), req, r, func(model.Result) {
		s.mu.Lock()
		erasing := s.state.Cartridges[0].Erasing
		s.mu.Unlock()
		if erasing {
			cancel()
		}
	})
	if e == nil {
		t.Fatal("interrupted erase reported success")
	}
	inv, _ := s.Inventory(context.Background())
	if !inv.Cartridges[0].Erasing || !inv.Cartridges[0].Dirty {
		t.Fatal("erase intent lost")
	}
	s2, e := Open(s.root, emulatorConfig())
	if e != nil {
		t.Fatal(e)
	}
	s2.now = func() time.Time { return time.Now().AddDate(0, 0, 2) }
	runTape(t, s2, r, req)
	inv, _ = s2.Inventory(context.Background())
	if inv.Cartridges[0].Erasing || inv.Cartridges[0].Dirty || len(inv.Cartridges[0].Copies) != 0 {
		t.Fatal("erase not recovered")
	}
}
