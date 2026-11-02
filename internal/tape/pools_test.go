// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package tape

import (
	"context"
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"testing"
	"time"
)

func TestPoolArchiveLoadWriteUnloadReplayAndRestore(t *testing.T) {
	raw, repo, manifest := tapeFixture(t)
	loadLabel(t, raw, repo)
	runTape(t, raw, repo, model.TapeRequest{Action: "unload", Barcode: "SRK001L9"})
	managed, e := manage(raw.root, raw)
	if e != nil {
		t.Fatal(e)
	}
	p := Pool{ID: "DAILY", Name: "Daily", Barcodes: []string{"SRK001L9"}, Drives: []string{"DRIVE1", "DRIVE2"}}
	if e = managed.SetPool(context.Background(), p); e != nil {
		t.Fatal(e)
	}
	id := model.ID()
	req := model.TapeRequest{Action: "write", PoolID: p.ID, SnapshotID: manifest.ID, RetentionDays: 7}
	res, e := managed.Execute(context.Background(), id, req, repo, nil)
	if e != nil || !res.Verified || res.Tape.PoolID != p.ID {
		t.Fatal(res, e)
	}
	inv, e := managed.Inventory(context.Background())
	if e != nil || inv.Cartridges[0].Mounted || len(inv.Cartridges[0].Copies) != 1 {
		t.Fatal(inv, e)
	}
	reopened, e := manage(raw.root, raw)
	if e != nil {
		t.Fatal(e)
	}
	again, e := reopened.Execute(context.Background(), id, req, repo, nil)
	if e != nil || again.Tape.CopyID != res.Tape.CopyID {
		t.Fatal("receipt replay", e)
	}
	runTape(t, raw, repo, model.TapeRequest{Action: "load", Barcode: "SRK001L9", DriveID: "DRIVE1"})
	restored := runTape(t, raw, repo, model.TapeRequest{Action: "restore", Barcode: "SRK001L9", CopyID: res.Tape.CopyID, RetentionDays: 7})
	if e = repo.Verify(restored.Tape.Snapshot.ID); e != nil {
		t.Fatal(e)
	}
	p.ID = "OTHER"
	if e = managed.SetPool(context.Background(), p); e == nil {
		t.Fatal("overlapping pools allowed")
	}
}
func TestPoolWaitAndCancelDoesNotTouchUnknownMedia(t *testing.T) {
	raw, repo, manifest := tapeFixture(t)
	managed, e := manage(raw.root, raw)
	if e != nil {
		t.Fatal(e)
	}
	p := Pool{ID: "WAIT", Name: "Wait", Barcodes: []string{"SRK001L9"}, Drives: []string{"DRIVE1"}}
	if e = managed.SetPool(context.Background(), p); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, e := managed.Execute(ctx, model.ID(), model.TapeRequest{Action: "write", PoolID: p.ID, SnapshotID: manifest.ID, RetentionDays: 7}, repo, func(r model.Result) {
		if r.Phase == "tape-waiting-media" {
			cancel()
		}
	})
	if !errors.Is(e, context.Canceled) || res.Phase != "tape-waiting-media" {
		t.Fatal(res, e)
	}
	inv, _ := managed.Inventory(context.Background())
	if inv.Cartridges[0].Known || inv.Cartridges[0].Mounted || len(inv.Queue) > 0 {
		t.Fatal("wait mutated media or leaked queue")
	}
}
func TestPoolSelectionSkipsBusyProtectedAndInsufficient(t *testing.T) {
	inv := Library{Drives: []Drive{{ID: "D1", Online: true, Empty: true, Generation: 9}, {ID: "D2", Online: true, Empty: true, Generation: 9}}, Cartridges: []Medium{{Cartridge: Cartridge{Barcode: "A", Known: true, Generation: 9}, Label: "a", CapacityBytes: 100}, {Cartridge: Cartridge{Barcode: "B", Known: true, Generation: 9}, Label: "b", CapacityBytes: 100}}}
	p := Pool{Barcodes: []string{"A", "B"}, Drives: []string{"D1", "D2"}}
	c, d, ok := choosePool(p, inv, []QueueEntry{{Barcode: "A", Drive: "D1", QueuedAt: time.Now()}}, 50)
	if !ok || c.Barcode != "B" || d != "D2" {
		t.Fatal(c, d, ok)
	}
	inv.Cartridges[1].WriteProtected = true
	if _, _, ok = choosePool(p, inv, []QueueEntry{{Barcode: "A", Drive: "D1"}}, 50); ok {
		t.Fatal("protected media chosen")
	}
	if _, _, ok = choosePool(p, inv, nil, 101); ok {
		t.Fatal("oversized archive chosen")
	}
}

type poolBarrier struct {
	Backend
	entered chan string
	release chan struct{}
}

func (b poolBarrier) Execute(ctx context.Context, id string, v model.TapeRequest, r *repository.Repository, report func(model.Result)) (model.Result, error) {
	if v.Action == "write" {
		b.entered <- v.Barcode
		select {
		case <-b.release:
		case <-ctx.Done():
			return model.Result{}, ctx.Err()
		}
	}
	return b.Backend.Execute(ctx, id, v, r, report)
}
func TestPoolParallelAllocationReservesWholeWorkflow(t *testing.T) {
	raw, repo, manifest := tapeFixture(t)
	for i, c := range raw.cfg.Cartridges {
		drive := raw.cfg.Drives[i]
		runTape(t, raw, repo, model.TapeRequest{Action: "load", Barcode: c.Barcode, DriveID: drive})
		runTape(t, raw, repo, model.TapeRequest{Action: "label", Barcode: c.Barcode, Label: c.Barcode, Reason: "init", Confirm: c.Barcode})
		runTape(t, raw, repo, model.TapeRequest{Action: "unload", Barcode: c.Barcode})
	}
	barrier := poolBarrier{Backend: raw, entered: make(chan string, 2), release: make(chan struct{})}
	managed, e := manage(raw.root, barrier)
	if e != nil {
		t.Fatal(e)
	}
	if e = managed.SetPool(context.Background(), Pool{ID: "DAILY", Name: "Daily", Barcodes: []string{"SRK001L9", "SRK002L9"}, Drives: []string{"DRIVE1", "DRIVE2"}}); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, e := managed.Execute(ctx, model.ID(), model.TapeRequest{Action: "write", PoolID: "DAILY", SnapshotID: manifest.ID, RetentionDays: 7}, repo, nil)
			done <- e
		}()
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case b := <-barrier.entered:
			seen[b] = true
		case <-ctx.Done():
			t.Fatal("pool did not allocate two independent drives")
		}
	}
	close(barrier.release)
	for i := 0; i < 2; i++ {
		if e = <-done; e != nil {
			t.Fatal(e)
		}
	}
	inv, _ := managed.Inventory(context.Background())
	for _, c := range inv.Cartridges {
		if c.Mounted || len(c.Copies) != 1 {
			t.Fatal("workflow failed to unload", c)
		}
	}
}
