// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gdata/srk/internal/model"
)

func testRepo(t *testing.T) *Repository {
	t.Helper()
	r, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { r.Close() })
	return r
}
func manifest(data []byte) model.Manifest {
	return model.Manifest{Version: 1, ID: model.ID(), JobID: model.ID(), CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour), Files: []model.File{{Path: "данные.txt", Kind: "file", Size: int64(len(data)), Chunks: []string{Hash(data)}}}}
}
func TestDedupAndCorruption(t *testing.T) {
	r := testRepo(t)
	data := []byte("same content")
	h := Hash(data)
	if e := r.Put(h, data); e != nil {
		t.Fatal(e)
	}
	if e := r.Put(h, data); e != nil {
		t.Fatal(e)
	}
	s, e := r.Stats()
	if e != nil || s["chunks"] != 1 {
		t.Fatalf("dedup: %v %v", s, e)
	}
	m := manifest(data)
	if e = r.Commit(m); e != nil {
		t.Fatal(e)
	}
	if e = r.Verify(m.ID); e != nil {
		t.Fatal(e)
	}
	p, _ := r.chunk(h)
	if e = os.WriteFile(p, []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	if r.Verify(m.ID) == nil {
		t.Fatal("corruption not detected")
	}
	if r.Put(h, data) == nil {
		t.Fatal("existing corrupt chunk silently accepted")
	}
}
func TestRepositoryDedupStatistics(t *testing.T) {
	r := testRepo(t)
	data := []byte("shared contents")
	if e := r.Put(Hash(data), data); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		m := manifest(data)
		m.Result.LogicalBytes = int64(len(data))
		if e := r.Commit(m); e != nil {
			t.Fatal(e)
		}
	}
	s, e := r.Stats()
	if e != nil {
		t.Fatal(e)
	}
	if s["logical_bytes"] != int64(2*len(data)) || s["unique_chunk_bytes"] != int64(len(data)) || s["dedup_saved_bytes"] != int64(len(data)) || s["dedup_saved_percent"] != float64(50) {
		t.Fatalf("incorrect dedup savings: %+v", s)
	}
	if s["physical_bytes"].(int64) <= s["unique_chunk_bytes"].(int64) {
		t.Fatal("metadata unexpectedly included in unique chunk statistics")
	}
}
func TestIncompleteCopyIsNotPublished(t *testing.T) {
	r := testRepo(t)
	m := manifest([]byte("not uploaded"))
	if r.Commit(m) == nil {
		t.Fatal("missing chunk accepted")
	}
	if _, e := r.Manifest(m.ID); !os.IsNotExist(e) {
		t.Fatalf("partial snapshot published: %v", e)
	}
}
func TestManifestIntegrityAndDuplicateCommit(t *testing.T) {
	r := testRepo(t)
	d := []byte("test")
	if e := r.Put(Hash(d), d); e != nil {
		t.Fatal(e)
	}
	m := manifest(d)
	if e := r.Commit(m); e != nil {
		t.Fatal(e)
	}
	if r.Commit(m) == nil {
		t.Fatal("existing snapshot overwritten")
	}
	p := filepath.Join(r.Root, "snapshots", m.ID+".json")
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	b = bytes.Replace(b, []byte("данные.txt"), []byte("другие.txt"), 1)
	if e = os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Manifest(m.ID); e == nil {
		t.Fatal("manifest corruption accepted")
	}
}
func TestManifestRejectsTraversalAndFileAncestors(t *testing.T) {
	for _, p := range []string{"../escape", "/absolute", "a/../../escape", "a\\b", "a//b", ".", "a/../b", "x\x00y"} {
		t.Run(p, func(t *testing.T) {
			m := manifest([]byte("a"))
			m.Files[0].Path = p
			if Validate(m) == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
	m := manifest([]byte("a"))
	m.Files[0].Path = "parent"
	m.Files = append(m.Files, model.File{Path: "parent/child", Kind: "dir"})
	if Validate(m) == nil {
		t.Fatal("file ancestor accepted")
	}
}
func TestRepositorySingleWriter(t *testing.T) {
	r := testRepo(t)
	if second, e := Open(r.Root); e == nil {
		second.Close()
		t.Fatal("two writers acquired repository")
	}
}
func TestChunkChecksumAndSize(t *testing.T) {
	r := testRepo(t)
	if r.Put(Hash([]byte("a")), []byte("b")) == nil {
		t.Fatal("wrong hash accepted")
	}
	if r.Put(Hash(nil), nil) == nil {
		t.Fatal("empty chunk accepted")
	}
	b := make([]byte, ChunkSize+1)
	if r.Put(Hash(b), b) == nil {
		t.Fatal("oversize chunk accepted")
	}
}
func TestFreeSpaceReservePreventsNewWrites(t *testing.T) {
	r := testRepo(t)
	r.ReserveBytes = 1 << 62
	b := []byte("new data")
	if e := r.Put(Hash(b), b); e == nil {
		t.Fatal("free space reserve ignored")
	}
	if r.Has(Hash(b)) {
		t.Fatal("chunk published despite reserve error")
	}
}
