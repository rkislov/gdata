// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package tape

import (
	"context"
	"encoding/binary"
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/tape/scsi"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type twoDriveBench struct {
	drives  [2]*commandLibrary
	entered chan int
	release chan struct{}
	block   bool // set before goroutines start; read-only during execution
}
type twoDriveDevice struct {
	bench *twoDriveBench
	drive int
}

func (d twoDriveDevice) Execute(ctx context.Context, c, b []byte, dir scsi.Direction) (int, error) {
	x := d.bench
	if d.drive < 0 {
		if c[0] == 0xb8 {
			// A single data-transfer page with two occupied drives.
			clear(b)
			b[7] = 104
			b[8] = 4
			b[9] = 128
			b[11] = 48
			b[15] = 96
			for i := 0; i < 2; i++ {
				p := 16 + i*48
				binary.BigEndian.PutUint16(b[p:p+2], uint16(200+i))
				b[p+2] = 1
				for k := p + 12; k < p+44; k++ {
					b[k] = ' '
				}
				copy(b[p+12:p+44], []string{"SRK001L9", "SRK002L9"}[i])
			}
			return 112, nil
		}
		return (commandDevice{lib: x.drives[0], kind: 8, serial: "ROBOT"}).Execute(ctx, c, b, dir)
	}
	if c[0] == 0x0a && x.block {
		select {
		case x.entered <- d.drive:
		default:
		}
		select {
		case <-x.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return (commandDevice{lib: x.drives[d.drive], kind: 1, serial: []string{"SERIAL1", "SERIAL2"}[d.drive]}).Execute(ctx, c, b, dir)
}
func TestSCSIParallelDrivesQueueAndFailureIsolation(t *testing.T) {
	_, repo, manifest := tapeFixture(t)
	cfg := emulatorConfig()
	cfg.Backend = "scsi"
	cfg.Drives = []string{"D1", "D2"}
	cfg.Cartridges = cfg.Cartridges[:2]
	cfg.Cartridges[0].Barcode = "SRK001L9"
	cfg.Cartridges[1].Barcode = "SRK002L9"
	cfg.Cartridges[0].Slot = 100
	cfg.Cartridges[1].Slot = 101
	cfg.SCSI = &SCSIConfig{Changer: SCSIPath{Path: "/dev/robot", Serial: "ROBOT"}, Transport: 1, Drives: map[string]SCSIPath{"D1": {Path: "/dev/d1", Serial: "SERIAL1", Element: 200}, "D2": {Path: "/dev/d2", Serial: "SERIAL2", Element: 201}}}
	bench := &twoDriveBench{drives: [2]*commandLibrary{{loaded: true}, {loaded: true}}, entered: make(chan int, 8), release: make(chan struct{})}
	open := func(p SCSIPath) (*scsi.Device, func(), error) {
		i := -1
		if p.Path == "/dev/d1" {
			i = 0
		}
		if p.Path == "/dev/d2" {
			i = 1
		}
		return &scsi.Device{IO: twoDriveDevice{bench, i}}, func() {}, nil
	}
	root := filepath.Join(t.TempDir(), "library")
	s, e := OpenSCSI(root, cfg, open)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i, c := range cfg.Cartridges {
		_, e = s.Execute(ctx, model.ID(), model.TapeRequest{Action: "label", Barcode: c.Barcode, DriveID: cfg.Drives[i], Label: "Archive-" + c.Barcode, Reason: "init", Confirm: c.Barcode}, repo, func(model.Result) {})
		if e != nil {
			t.Fatal(e)
		}
	}
	bench.block = true
	bench.drives[0].failRead = true
	done := make(chan error, 2)
	for i, c := range cfg.Cartridges {
		go func(i int, barcode string) {
			_, e := s.Execute(ctx, model.ID(), model.TapeRequest{Action: "write", Barcode: barcode, DriveID: cfg.Drives[i], SnapshotID: manifest.ID, RetentionDays: 1}, repo, func(model.Result) {})
			done <- e
		}(i, c.Barcode)
	}
	seen := map[int]bool{}
	for len(seen) < 2 {
		select {
		case i := <-bench.entered:
			seen[i] = true
		case <-ctx.Done():
			t.Fatal("drives did not write concurrently")
		}
	}
	queuedCtx, stop := context.WithCancel(ctx)
	queued := make(chan struct{}, 1)
	cancelled := make(chan error, 1)
	go func() {
		_, e := s.Execute(queuedCtx, model.ID(), model.TapeRequest{Action: "verify", Barcode: "SRK001L9", DriveID: "D1"}, repo, func(r model.Result) {
			if r.Phase == "tape-queued" {
				queued <- struct{}{}
			}
		})
		cancelled <- e
	}()
	select {
	case <-queued:
	case <-ctx.Done():
		t.Fatal("operation not queued")
	}
	inv, e := s.Inventory(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if len(inv.Queue) != 3 || len(inv.Positions) != 2 {
		t.Fatalf("inventory must remain responsive with two active journals: %+v", inv)
	}
	stop()
	if e = <-cancelled; !errors.Is(e, context.Canceled) {
		t.Fatalf("queued cancellation: %v", e)
	}
	close(bench.release)
	failures := 0
	for i := 0; i < 2; i++ {
		if e = <-done; e != nil {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("want one isolated failure, got %d", failures)
	}
	if s.pendingFor("SRK001L9", "D1") == nil || s.pendingFor("SRK002L9", "D2") != nil {
		t.Fatal("pending journals crossed drives")
	}
	_, e = s.Execute(ctx, model.ID(), model.TapeRequest{Action: "verify", Barcode: "SRK002L9", DriveID: "D2"}, repo, nil)
	if e != nil {
		t.Fatalf("healthy drive blocked: %v", e)
	}
	s, e = OpenSCSI(root, cfg, open)
	if e != nil {
		t.Fatal(e)
	}
	if s.pendingFor("SRK001L9", "D1") == nil || len(s.files("SRK002L9")) != 1 {
		t.Fatal("parallel index not persisted")
	}
	bench.drives[0].failRead = false
	_, e = s.Execute(ctx, model.ID(), model.TapeRequest{Action: "recover", Barcode: "SRK001L9", DriveID: "D1"}, repo, nil)
	if e != nil {
		t.Fatal(e)
	}
}

func TestDriveQueueFIFOAndIndependentDrive(t *testing.T) {
	var q driveQueue
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	report := func(model.Result) {}
	first := &QueueEntry{ID: "first", Drive: "D1", Barcode: "A"}
	release, e := q.acquire(ctx, first, report)
	if e != nil {
		t.Fatal(e)
	}
	order := make(chan string, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"second", "third"} {
		queued := make(chan struct{})
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			free, e := q.acquire(ctx, &QueueEntry{ID: id, Drive: "D1", Barcode: "A"}, func(model.Result) { close(queued) })
			if e == nil {
				order <- id
				free()
			}
		}(id)
		<-queued
	}
	other, e := q.acquire(ctx, &QueueEntry{ID: "other", Drive: "D2", Barcode: "B"}, report)
	if e != nil {
		t.Fatal("unrelated drive must bypass waiting D1", e)
	}
	other()
	release()
	wg.Wait()
	if <-order != "second" || <-order != "third" {
		t.Fatal("FIFO violated")
	}
	if len(q.snapshot()) != 0 {
		t.Fatal("queue leaked resources")
	}
}

func TestLegacySCSIJournalMigration(t *testing.T) {
	cfg := emulatorConfig()
	cfg.Backend = "scsi"
	cfg.Drives = []string{"D1", "D2"}
	cfg.SCSI = &SCSIConfig{Changer: SCSIPath{Path: "/dev/robot", Serial: "R"}, Transport: 10, Drives: map[string]SCSIPath{"D1": {Path: "/dev/d1", Serial: "1", Element: 200}, "D2": {Path: "/dev/d2", Serial: "2", Element: 201}}}
	root := t.TempDir()
	j := PositionJournal{Operation: model.ID(), Drive: "D1", Barcode: "SRK001L9", Action: "write", Phase: "verify"}
	if e := save(filepath.Join(root, "scsi-index.json"), scsiIndex{Files: map[string][]string{}, Pending: &j}); e != nil {
		t.Fatal(e)
	}
	s, e := OpenSCSI(root, cfg, nil)
	if e != nil {
		t.Fatal(e)
	}
	if s.index.Pending != nil || s.pendingFor(j.Barcode, j.Drive) == nil {
		t.Fatal("legacy pending journal lost")
	}
	reopened, e := OpenSCSI(root, cfg, nil)
	if e != nil {
		t.Fatal(e)
	}
	if reopened.pendingFor(j.Barcode, j.Drive) == nil {
		t.Fatal("migration not durable")
	}
}
