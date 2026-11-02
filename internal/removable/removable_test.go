// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package removable

import (
	"context"
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVolumeLifecycleOfflineAndStandaloneRestore(t *testing.T) {
	mount := t.TempDir()
	v, e := Open(Config{ID: "USB01", UUID: "fixture-uuid", Mount: mount, CapacityBytes: 32 << 20}, filepath.Join(t.TempDir(), "catalog.json"))
	if e != nil {
		t.Fatal(e)
	}
	connected := true
	v.probe = func(Config) error {
		if !connected {
			return errors.New("UUID absent")
		}
		return nil
	}
	v.anchor = func(Config) (string, func(), error) { return mount, func() {}, nil }
	v.syncFS = func(Config) error { return nil }
	r, e := repository.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	data := []byte("portable backup")
	hash := repository.Hash(data)
	if e = r.Put(hash, data); e != nil {
		t.Fatal(e)
	}
	m := model.Manifest{Version: 1, ID: model.ID(), JobID: model.ID(), Mode: "full", ExpiresAt: time.Now().Add(time.Hour), Files: []model.File{{Path: "data", Kind: "file", Size: int64(len(data)), Chunks: []string{hash}}}}
	if e = r.Commit(m); e != nil {
		t.Fatal(e)
	}
	run := func(req model.TapeRequest, repo *repository.Repository) (model.Result, error) {
		req.Barcode = "USB01"
		return v.Execute(context.Background(), model.ID(), req, repo, func(model.Result) {})
	}
	if _, e = run(model.TapeRequest{Action: "write", SnapshotID: m.ID, RetentionDays: 30}, r); e == nil {
		t.Fatal("unregistered volume written")
	}
	if _, e = run(model.TapeRequest{Action: "label", Label: "Offsite", Confirm: "WRONG", Reason: "test"}, r); e == nil {
		t.Fatal("wrong confirmation accepted")
	}
	if _, e = run(model.TapeRequest{Action: "label", Label: "Offsite", Confirm: "USB01", Reason: "test"}, r); e != nil {
		t.Fatal(e)
	}
	written, e := run(model.TapeRequest{Action: "write", SnapshotID: m.ID, RetentionDays: 30}, r)
	if e != nil {
		t.Fatal(e)
	}
	connected = false
	s := v.Status(context.Background())
	if s.State != "disconnected" || s.Catalog == nil || len(s.Catalog.Cartridges[0].Copies) != 1 {
		t.Fatal("offline catalog missing")
	}
	if _, e = run(model.TapeRequest{Action: "write", SnapshotID: m.ID, RetentionDays: 30}, r); e == nil {
		t.Fatal("offline write accepted")
	}
	connected = true
	if _, e = run(model.TapeRequest{Action: "release"}, r); e != nil {
		t.Fatal(e)
	}
	if _, e = run(model.TapeRequest{Action: "verify"}, r); e == nil {
		t.Fatal("released volume still used")
	}
	if _, e = run(model.TapeRequest{Action: "attach"}, r); e != nil {
		t.Fatal(e)
	}
	dest, e := repository.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer dest.Close()
	out, e := run(model.TapeRequest{Action: "restore", CopyID: written.Tape.CopyID, RetentionDays: 7}, dest)
	if e != nil {
		t.Fatal(e)
	}
	if e = dest.Verify(out.Tape.Snapshot.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = run(model.TapeRequest{Action: "recycle", Confirm: "USB01", Reason: "must reject"}, r); e == nil {
		t.Fatal("retained data deleted")
	}
	if e = os.WriteFile(filepath.Join(mount, ".srk-removable", "identity.json"), []byte(`{"uuid":"different"}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = run(model.TapeRequest{Action: "verify"}, r); e == nil {
		t.Fatal("replaced volume accepted")
	}
}
