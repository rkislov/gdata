// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalogbackup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReplicaIntegrityRetention(t *testing.T) {
	dir := t.TempDir()
	s, e := OpenReplicas(dir)
	if e != nil {
		t.Fatal(e)
	}
	id := "0123456789abcdef0123456789abcdef"
	body := []byte("encrypted catalog fixture")
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	if _, e = s.Put(id, hash, 7, bytes.NewReader([]byte("damaged"))); e == nil {
		t.Fatal("bad checksum accepted")
	}
	if v, _ := s.List(); len(v) != 0 {
		t.Fatal("partial copy published")
	}
	v, e := s.Put(id, hash, 7, bytes.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	retry, e := s.Put(id, hash, 1, bytes.NewReader(body))
	if e != nil || !retry.ExpiresAt.Equal(v.ExpiresAt) {
		t.Fatal("retry changed retention", e)
	}
	f, _, e := s.Open(id)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if e = s.GC(v.ExpiresAt.Add(-time.Second)); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(dir, id+".srkcat")); e != nil {
		t.Fatal("early expiry", e)
	}
	if e = s.GC(v.ExpiresAt); e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(f)
	if e != nil || !bytes.Equal(b, body) {
		t.Fatal("active reader interrupted")
	}
	if list, _ := s.List(); len(list) != 0 {
		t.Fatal("expired replica retained")
	}
	if _, _, e = s.Open("../escape"); e == nil {
		t.Fatal("path traversal")
	}
}

func TestLocalExpiryWaitsForRemoteReceipt(t *testing.T) {
	dir := t.TempDir()
	id := "abcdef0123456789abcdef0123456789"
	now := time.Now().UTC()
	created := now.Add(-48 * time.Hour)
	v := Entry{ID: id, State: "success", CreatedAt: created, SHA256: "hash", Manifest: &Manifest{ID: id}}
	s := &Store{dir: dir, entries: map[string]Entry{id: v}, policy: Policy{TargetMediaID: "remote", LocalRetentionDays: 1, RemoteRetentionDays: 7}}
	path := filepath.Join(dir, id+".srkcat")
	if e := os.WriteFile(path, []byte("ciphertext"), 0600); e != nil {
		t.Fatal(e)
	}
	s.maintenanceTick(now)
	if _, e := os.Stat(path); e != nil {
		t.Fatal("unconfirmed local copy removed")
	}
	s.operations = []Operation{{ArchiveID: id, Kind: "replicate", MediaID: "remote", State: "success", Receipt: &Receipt{ExpiresAt: now.Add(-time.Hour)}}}
	s.maintenanceTick(now)
	if _, e := os.Stat(path); e != nil {
		t.Fatal("expired remote receipt permitted cleanup")
	}
	s.operations[0].Receipt.ExpiresAt = now.Add(24 * time.Hour)
	s.maintenanceTick(now)
	if _, e := os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("confirmed expired local copy retained", e)
	}
	if s.List()[0].State != "expired" {
		t.Fatal("expiry not recorded")
	}
}
