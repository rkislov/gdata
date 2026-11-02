// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"errors"
	"gdata/srk/internal/model"
	"os"
	"testing"
	"time"
)

func TestWALAcknowledgementsProtectEveryReceiverAndHole(t *testing.T) {
	r := testRepo(t)
	a := oldWAL(t, r, 1)
	b := oldWAL(t, r, 2)
	oldWAL(t, r, 3)
	before := time.Now().Add(-24 * time.Hour)
	preview := func() model.WALGCPlan {
		t.Helper()
		p, e := r.PreviewWALGC(a.ArchiveID, before, true)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	initial := preview()
	register := func() model.WALReceiver {
		t.Helper()
		v, e := r.RegisterWALReceiver(model.WALReceiver{ID: model.ID(), ArchiveID: a.ArchiveID, SourceRepositoryID: r.Info.ID, TargetMediaID: "remote", TargetRepositoryID: model.ID()}, false)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	v := register()
	other := register()
	if _, e := r.ApplyWALGC(initial, model.ID()); !errors.Is(e, ErrPlanStale) {
		t.Fatalf("enrollment did not invalidate GC: %v", e)
	}
	ack := func(v model.WALReceiver, record model.WALRecord) {
		t.Helper()
		if e := r.AcknowledgeWAL(v.ID, v.TargetMediaID, model.WALReceiverAck{Generation: v.Generation, TargetRepositoryID: v.TargetRepositoryID, Files: []model.WALAcknowledgement{{Name: record.Name, SHA256: record.SHA256, Bytes: record.Bytes}}}); e != nil {
			t.Fatal(e)
		}
	}
	ack(v, b)
	ack(other, b)
	p := preview()
	if len(p.Names) != 1 || p.Names[0] != b.Name || p.ReplicationProtectedFiles != 2 {
		t.Fatalf("later ACK hid earlier hole: %+v", p)
	}
	ack(v, a)
	if len(preview().Names) != 1 {
		t.Fatal("second receiver not protected")
	}
	root := r.Root
	if e := r.Close(); e != nil {
		t.Fatal(e)
	}
	var e error
	r, e = Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	lag, e := r.WALReceiverLag(other.ID)
	if e != nil || *lag.PendingFiles != 2 || *lag.PendingBytes != 2*a.Bytes || *lag.PendingAgeSeconds < 86400 {
		t.Fatalf("persistent pending lag: %+v %v", lag, e)
	}
	ack(other, a)
	if _, e = r.ApplyWALGC(p, model.ID()); !errors.Is(e, ErrPlanStale) {
		t.Fatalf("ACK did not invalidate plan: %v", e)
	}
	p = preview()
	ack(other, a)
	result, e := r.ApplyWALGC(p, model.ID())
	if e != nil || result.WALRemoved != 2 {
		t.Fatalf("duplicate ACK invalidated plan: %+v %v", result, e)
	}
	ack(other, a) // Retry after source GC is idempotent.
}

func TestWALAcknowledgementIdentityReleaseAndCorruption(t *testing.T) {
	r := testRepo(t)
	a := oldWAL(t, r, 1)
	oldWAL(t, r, 2)
	v, e := r.RegisterWALReceiver(model.WALReceiver{ID: model.ID(), ArchiveID: a.ArchiveID, SourceRepositoryID: r.Info.ID, TargetMediaID: "remote", TargetRepositoryID: model.ID()}, false)
	if e != nil {
		t.Fatal(e)
	}
	req := model.WALReceiverAck{Generation: v.Generation, TargetRepositoryID: v.TargetRepositoryID, Files: []model.WALAcknowledgement{{Name: a.Name, SHA256: a.SHA256, Bytes: a.Bytes}}}
	for _, kind := range []string{"generation", "repository", "digest", "size", "media"} {
		bad := req
		bad.Files = append([]model.WALAcknowledgement(nil), req.Files...)
		media := v.TargetMediaID
		switch kind {
		case "generation":
			bad.Generation = model.ID()
		case "repository":
			bad.TargetRepositoryID = model.ID()
		case "digest":
			bad.Files[0].SHA256 = string(make([]byte, 64))
		case "size":
			bad.Files[0].Bytes++
		case "media":
			media = "other"
		}
		if e = r.AcknowledgeWAL(v.ID, media, bad); e == nil {
			t.Fatalf("accepted wrong %s", kind)
		}
	}
	if e = r.ReleaseWALReceiver(v.ID, ""); e == nil {
		t.Fatal("release without reason")
	}
	if e = r.ReleaseWALReceiver(v.ID, "retire remote site"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.RegisterWALReceiver(v, false); e == nil {
		t.Fatal("automatic reactivation")
	}
	renewed, e := r.RegisterWALReceiver(v, true)
	if e != nil || renewed.Generation == v.Generation {
		t.Fatal("generation not rotated", e)
	}
	if e = r.AcknowledgeWAL(v.ID, v.TargetMediaID, req); e == nil {
		t.Fatal("old generation replay")
	}
	req.Generation = renewed.Generation
	if e = r.AcknowledgeWAL(v.ID, v.TargetMediaID, req); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(r.walAckPath(renewed, a.Name), []byte("broken"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = r.PreviewWALGC(a.ArchiveID, time.Now().Add(-24*time.Hour), true); e == nil {
		t.Fatal("corrupt receipt did not fail closed")
	}
}
