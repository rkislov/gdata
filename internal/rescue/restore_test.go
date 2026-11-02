// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package rescue

import (
	"bytes"
	"context"
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"os"
	"path/filepath"
	"testing"
)

func imageFixture() (model.Manifest, map[string][]byte) {
	blocks := [][]byte{bytes.Repeat([]byte{71}, repository.ChunkSize), bytes.Repeat([]byte{92}, 512)}
	chunks := []string{}
	store := map[string][]byte{}
	for _, b := range blocks {
		h := repository.Hash(b)
		chunks = append(chunks, h)
		store[h] = b
	}
	return model.Manifest{Version: 1, ID: model.ID(), JobID: model.ID(), Mode: "full", Consistency: "offline-disk-image", Files: []model.File{{Path: "system.img", Kind: "file", Size: int64(repository.ChunkSize + 512), Chunks: chunks}}}, store
}
func TestRawRestoreReadbackAndPreflight(t *testing.T) {
	m, store := imageFixture()
	dst, e := os.Create(filepath.Join(t.TempDir(), "disk"))
	if e != nil {
		t.Fatal(e)
	}
	defer dst.Close()
	_ = dst.Truncate(m.Files[0].Size + 4096)
	fetch := func(_ context.Context, h string) ([]byte, error) { return store[h], nil }
	if e = RestoreImage(context.Background(), m, dst, m.Files[0].Size-1, fetch, nil); e == nil {
		t.Fatal("undersized disk accepted")
	}
	if e = RestoreImage(context.Background(), m, dst, m.Files[0].Size, fetch, nil); e != nil {
		t.Fatal(e)
	}
	b := make([]byte, 512)
	_, _ = dst.ReadAt(b, repository.ChunkSize)
	if !bytes.Equal(b, bytes.Repeat([]byte{92}, 512)) {
		t.Fatal("final block differs")
	}
	m.Consistency = "live-files"
	if _, e = DiskImage(m); e == nil {
		t.Fatal("ordinary file backup accepted as disk image")
	}
}
func TestCorruptChunkNeverWritten(t *testing.T) {
	m, _ := imageFixture()
	dst, e := os.Create(filepath.Join(t.TempDir(), "disk"))
	if e != nil {
		t.Fatal(e)
	}
	defer dst.Close()
	e = RestoreImage(context.Background(), m, dst, m.Files[0].Size, func(context.Context, string) ([]byte, error) { return []byte("wrong"), nil }, nil)
	if e == nil {
		t.Fatal("corruption accepted")
	}
	st, _ := dst.Stat()
	if st.Size() != 0 {
		t.Fatal("unverified bytes written")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(RestoreImage(ctx, m, dst, m.Files[0].Size, func(context.Context, string) ([]byte, error) { t.Fatal("download after cancellation"); return nil, nil }, nil), context.Canceled) {
		t.Fatal("cancellation ignored")
	}
}
