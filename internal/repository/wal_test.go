// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gdata/srk/internal/model"
)

func walFixture(name string) (model.WALRecord, []byte) {
	v := model.WALRecord{AgentID: "agent", Profile: "production", SystemID: "123456", Major: 16, SegmentSize: 1 << 20, Name: name}
	v.ArchiveID = WALArchiveID(v.AgentID, v.Profile, v.SystemID)
	b := make([]byte, v.SegmentSize)
	binary.LittleEndian.PutUint16(b[2:4], 2)
	binary.LittleEndian.PutUint32(b[4:8], 1)
	position, _ := WALPosition(name, v.SegmentSize)
	binary.LittleEndian.PutUint64(b[8:16], position)
	binary.LittleEndian.PutUint64(b[24:32], 123456)
	binary.LittleEndian.PutUint32(b[32:36], uint32(v.SegmentSize))
	return v, b
}
func TestWALArchiveIdempotencyCollisionAndCorruption(t *testing.T) {
	r := testRepo(t)
	v, data := walFixture("000000010000000000000001")
	first, e := r.PutWAL(v, bytes.NewReader(data))
	if e != nil {
		t.Fatal(e)
	}
	second, e := r.PutWAL(v, bytes.NewReader(data))
	if e != nil || first.SHA256 != second.SHA256 || !first.ArchivedAt.Equal(second.ArchivedAt) {
		t.Fatal("identical retry changed archive", e)
	}
	data[100] = 1
	if _, e = r.PutWAL(v, bytes.NewReader(data)); e == nil {
		t.Fatal("different WAL contents overwrote existing name")
	}
	f, _, e := r.OpenWAL(v.ArchiveID, v.Name)
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	if e = os.WriteFile(filepath.Join(r.Root, "wal", v.ArchiveID, v.Name), data, 0600); e != nil {
		t.Fatal(e)
	}
	if _, _, e = r.OpenWAL(v.ArchiveID, v.Name); e == nil {
		t.Fatal("corrupt WAL was served")
	}
}
func TestWALRejectsPartialAndForeignCluster(t *testing.T) {
	r := testRepo(t)
	v, b := walFixture("000000010000000000000001")
	if _, e := r.PutWAL(v, bytes.NewReader(b[:100])); e == nil {
		t.Fatal("partial WAL accepted")
	}
	v.SystemID = "456789"
	v.ArchiveID = WALArchiveID(v.AgentID, v.Profile, v.SystemID)
	if _, e := r.PutWAL(v, bytes.NewReader(b)); e == nil {
		t.Fatal("WAL from a foreign cluster accepted")
	}
	for _, name := range []string{"../file", "000000010000000000000001.partial", strings.Repeat("a", 24), "x.history"} {
		if WALName(name) {
			t.Fatalf("unsafe WAL name accepted: %s", name)
		}
	}
}
func TestWALContinuityGeometryAndTimeline(t *testing.T) {
	base := &model.PostgreSQLBackup{SegmentSize: 1 << 20, WALRanges: []model.PostgreSQLWALRange{{Timeline: 1, StartLSN: "0/100028"}}}
	if e := WALContinuity(base, []string{"000000010000000000000001", "000000010000000000000002"}); e != nil {
		t.Fatal(e)
	}
	if WALContinuity(base, []string{"000000010000000000000001", "000000010000000000000003"}) == nil {
		t.Fatal("gap was accepted")
	}
	if WALContinuity(base, []string{"000000010000000000000001", "00000002.history"}) == nil {
		t.Fatal("timeline switch was silently accepted")
	}
	base.WALRanges[0].StartLSN = "0/FFF00000"
	if e := WALContinuity(base, []string{"000000010000000000000FFF", "000000010000000100000000"}); e != nil {
		t.Fatal("WAL log rollover rejected", e)
	}
}
func TestWALIsNotRemovedBySnapshotCleanup(t *testing.T) {
	r := testRepo(t)
	v, b := walFixture("000000010000000000000001")
	if _, e := r.PutWAL(v, bytes.NewReader(b)); e != nil {
		t.Fatal(e)
	}
	plan, e := r.PreviewGC()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.ApplyGC(plan, model.ID()); e != nil {
		t.Fatal(e)
	}
	f, record, e := r.OpenWAL(v.ArchiveID, v.Name)
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	if record.ArchivedAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatal("invalid WAL timestamp")
	}
}

func TestReplicatedWALChecksBeforePublicationAndPreservesEvidence(t *testing.T) {
	r := testRepo(t)
	record, data := walFixture("000000010000000000000002")
	record.Bytes = int64(len(data))
	record.SHA256 = Hash(data)
	record.ArchivedAt = time.Now().UTC().Add(-time.Hour)
	at := record.ArchivedAt.Add(-time.Minute)
	record.TimeBoundsVerified = true
	record.FirstCommitAt = &at
	record.LastCommitAt = &at
	bad := append([]byte(nil), data...)
	bad[100] ^= 1
	if _, err := r.PutReplicatedWAL(record, bytes.NewReader(bad)); err == nil {
		t.Fatal("WAL with wrong producer checksum accepted")
	}
	records, err := r.WALInventory(record.ArchiveID)
	if err != nil || len(records) != 0 {
		t.Fatal("failed WAL published", err)
	}
	if _, err = os.Stat(filepath.Join(r.Root, "wal", record.ArchiveID, record.Name)); !os.IsNotExist(err) {
		t.Fatal("failed WAL data left at final name")
	}
	copied, err := r.PutReplicatedWAL(record, bytes.NewReader(data))
	if err != nil || copied.SHA256 != record.SHA256 || !copied.ArchivedAt.Equal(record.ArchivedAt) || !copied.LastCommitAt.Equal(at) {
		t.Fatal("WAL evidence not preserved", err)
	}
}
