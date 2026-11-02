// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"gdata/srk/internal/model"
)

const ChunkSize = 4 * 1024 * 1024

var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Repository struct {
	ReserveBytes int64
	Root         string
	mu           sync.RWMutex
	walMu        sync.RWMutex
	lock         *os.File
	Info         model.RepositoryInfo
	failed       error
}

func Open(root string) (*Repository, error) {
	for _, d := range []string{"chunks", "snapshots", "state", "leases", "maintenance", "audit", ".gc"} {
		if e := os.MkdirAll(filepath.Join(root, d), 0700); e != nil {
			return nil, e
		}
	}
	f, e := os.OpenFile(filepath.Join(root, ".writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("repository already has an active writer")
	}
	r := &Repository{Root: root, lock: f}
	if e = r.initialize(); e != nil {
		f.Close()
		return nil, e
	}
	return r, nil
}
func (r *Repository) Close() error {
	r.walMu.Lock()
	defer r.walMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lock == nil {
		return nil
	}
	e := r.lock.Close()
	r.lock = nil
	r.failed = errors.New("repository closed")
	return e
}
func (r *Repository) Check() error { r.mu.RLock(); defer r.mu.RUnlock(); return r.failed }
func Hash(b []byte) string         { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func (r *Repository) chunk(h string) (string, error) {
	if !hashPattern.MatchString(h) {
		return "", errors.New("invalid chunk hash")
	}
	return filepath.Join(r.Root, "chunks", h[:2], h), nil
}
func (r *Repository) Has(h string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.has(h)
}
func (r *Repository) has(h string) bool {
	p, e := r.chunk(h)
	if e != nil {
		return false
	}
	i, e := os.Stat(p)
	return e == nil && i.Mode().IsRegular()
}
func Atomic(filename string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(filename)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".write-*")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if e = f.Chmod(mode); e != nil {
		f.Close()
		return e
	}
	if _, e = f.Write(data); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(tmp, filename); e != nil {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (r *Repository) Put(h string, b []byte) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.failed != nil {
		return r.failed
	}
	return r.put(h, b)
}
func (r *Repository) put(h string, b []byte) error {
	if len(b) > ChunkSize || len(b) == 0 {
		return errors.New("invalid chunk size")
	}
	p, e := r.chunk(h)
	if e != nil {
		return e
	}
	if Hash(b) != h {
		return errors.New("chunk checksum mismatch")
	}
	if r.has(h) {
		_, e = r.get(h)
		return e
	}
	var stat syscall.Statfs_t
	if e = syscall.Statfs(r.Root, &stat); e != nil {
		return e
	}
	if uint64(stat.Bavail)*uint64(stat.Bsize) < uint64(len(b))+uint64(r.ReserveBytes) {
		return errors.New("repository free space reserve reached")
	}
	return Atomic(p, b, 0600)
}
func (r *Repository) Get(h string) ([]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.failed != nil {
		return nil, r.failed
	}
	return r.get(h)
}
func (r *Repository) get(h string) ([]byte, error) {
	p, e := r.chunk(h)
	if e != nil {
		return nil, e
	}
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, ChunkSize+1))
	if e != nil {
		return nil, e
	}
	if len(b) > ChunkSize || Hash(b) != h {
		return nil, errors.New("stored chunk checksum mismatch")
	}
	return b, nil
}
func ValidPath(s string) bool {
	return s != "" && s != "." && !strings.Contains(s, "\\") && !strings.ContainsRune(s, 0) && !path.IsAbs(s) && path.Clean(s) == s && s != ".." && !strings.HasPrefix(s, "../")
}
func Validate(m model.Manifest) error {
	if e := validateDataManifest(m); e != nil {
		return e
	}
	if e := validateVMManifest(m); e != nil {
		return e
	}

	if m.Disk != nil {
		if e := m.Disk.Validate(); e != nil {
			return e
		}
		if m.Consistency != "offline-disk-image" || len(m.Files) != 1 || m.Files[0].Size != m.Disk.Bytes {
			return errors.New("invalid disk geometry metadata")
		}
	}

	if m.Consistency == "offline-disk-image" {
		if m.PostgreSQL != nil || m.Mode != "full" || len(m.Files) != 1 || m.Files[0].Kind != "file" || !strings.HasSuffix(m.Files[0].Path, ".img") || m.Files[0].Size < 512 || m.Files[0].Size%512 != 0 {
			return errors.New("invalid offline RAW image manifest")
		}
	}
	if m.PostgreSQL != nil {
		pg := m.PostgreSQL
		if m.Consistency != "postgresql-physical" || m.Mode != "full" || !pg.NativeVerified || pg.Major < 13 || pg.Major > 18 || len(pg.WALRanges) == 0 || pg.StartedAt.IsZero() || pg.FinishedAt.Before(pg.StartedAt) {
			return errors.New("invalid PostgreSQL manifest metadata")
		}
	}
	if m.Consistency == "postgresql-physical" && m.PostgreSQL == nil {
		return errors.New("PostgreSQL manifest requires native metadata")
	}
	if m.Version != 1 || !idPattern.MatchString(m.ID) || m.JobID == "" || len(m.Files) > 1000000 {
		return errors.New("invalid manifest header")
	}
	seen := map[string]string{}
	for _, f := range m.Files {
		if !ValidPath(f.Path) {
			return fmt.Errorf("unsafe manifest path %q", f.Path)
		}
		if _, ok := seen[f.Path]; ok {
			return errors.New("duplicate manifest path")
		}
		seen[f.Path] = f.Kind
		if f.Kind != "file" && f.Kind != "dir" && f.Kind != "symlink" {
			return errors.New("unsupported file kind")
		}
		if f.Size < 0 {
			return errors.New("negative size")
		}
		if f.Kind == "file" {
			for _, h := range f.Chunks {
				if !hashPattern.MatchString(h) {
					return errors.New("invalid file chunk")
				}
			}
			if (f.Size+ChunkSize-1)/ChunkSize != int64(len(f.Chunks)) {
				return errors.New("file size and chunk count differ")
			}
		}
	}
	for _, f := range m.Files {
		for p := path.Dir(f.Path); p != "."; p = path.Dir(p) {
			if k, ok := seen[p]; ok && k != "dir" {
				return errors.New("non-directory manifest ancestor")
			}
		}
	}
	return nil
}
func (r *Repository) Commit(m model.Manifest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed != nil {
		return r.failed
	}
	return r.commit(m)
}
func (r *Repository) commit(m model.Manifest) error { return r.commitContext(context.Background(), m) }
func (r *Repository) commitContext(ctx context.Context, m model.Manifest) error {
	if e := Validate(m); e != nil {
		return e
	}
	if _, e := os.Stat(filepath.Join(r.Root, "snapshots", m.ID+".json")); !os.IsNotExist(e) {
		return errors.New("snapshot already exists")
	}
	for _, f := range m.Files {
		if f.Kind != "file" {
			continue
		}
		var size int64
		for i, h := range f.Chunks {
			if e := ctx.Err(); e != nil {
				return e
			}
			b, e := r.get(h)
			if e != nil {
				return fmt.Errorf("verify %s: %w", f.Path, e)
			}
			if i < len(f.Chunks)-1 && len(b) != ChunkSize {
				return errors.New("short non-final chunk")
			}
			size += int64(len(b))
		}
		if size != f.Size {
			return errors.New("stored file size mismatch")
		}
	}
	m.Result.Verified = true
	m.Result.RepositoryID = r.Info.ID
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	envelope := struct {
		Checksum string          `json:"checksum"`
		Data     json.RawMessage `json:"data"`
	}{Hash(b), b}
	b, e = json.Marshal(envelope)
	if e != nil {
		return e
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	return Atomic(filepath.Join(r.Root, "snapshots", m.ID+".json"), b, 0600)
}
func (r *Repository) Manifest(id string) (model.Manifest, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.failed != nil {
		return model.Manifest{}, r.failed
	}
	return r.manifest(id)
}
func (r *Repository) manifest(id string) (model.Manifest, error) {
	var m model.Manifest
	if !idPattern.MatchString(id) {
		return m, errors.New("invalid snapshot ID")
	}
	b, e := os.ReadFile(filepath.Join(r.Root, "snapshots", id+".json"))
	if e != nil {
		return m, e
	}
	var x struct {
		Checksum string          `json:"checksum"`
		Data     json.RawMessage `json:"data"`
	}
	if e = json.Unmarshal(b, &x); e != nil {
		return m, e
	}
	if Hash(x.Data) != x.Checksum {
		return m, errors.New("manifest checksum mismatch")
	}
	if e = json.Unmarshal(x.Data, &m); e != nil {
		return m, e
	}
	if m.ID != id {
		return m, errors.New("manifest file identity mismatch")
	}
	return m, Validate(m)
}
func (r *Repository) Verify(id string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.failed != nil {
		return r.failed
	}
	return r.verify(id)
}
func (r *Repository) verify(id string) error {
	m, e := r.manifest(id)
	if e != nil {
		return e
	}
	for _, f := range m.Files {
		for _, h := range f.Chunks {
			if _, e = r.get(h); e != nil {
				return e
			}
		}
	}
	return nil
}
func (r *Repository) Stats() (map[string]any, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var bytes int64
	var logicalBytes, chunkBytes int64
	var walBytes int64
	var walFiles int
	var chunks, snapshots int
	err := filepath.WalkDir(r.Root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() {
			if strings.HasPrefix(d.Name(), ".write-") {
				return nil
			}
			i, e := d.Info()
			if e != nil {
				if os.IsNotExist(e) {
					return nil
				}
				return e
			}
			bytes += i.Size()
			if strings.HasPrefix(p, filepath.Join(r.Root, "wal")+string(filepath.Separator)) && WALName(d.Name()) {
				walBytes += i.Size()
				walFiles++
			}
			if hashPattern.MatchString(d.Name()) {
				chunks++
				chunkBytes += i.Size()
			}
			if filepath.Dir(p) == filepath.Join(r.Root, "snapshots") && strings.HasSuffix(d.Name(), ".json") {
				snapshots++
				m, e := r.manifest(strings.TrimSuffix(d.Name(), ".json"))
				if e != nil {
					return e
				}
				logicalBytes += m.Result.LogicalBytes
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var stat syscall.Statfs_t
	if err = syscall.Statfs(r.Root, &stat); err != nil {
		return nil, err
	}
	s := map[string]any{"repository_id": r.Info.ID, "physical_bytes": bytes, "chunks": chunks, "snapshots": snapshots, "filesystem_total_bytes": uint64(stat.Blocks) * uint64(stat.Bsize), "filesystem_available_bytes": uint64(stat.Bavail) * uint64(stat.Bsize), "reserve_bytes": r.ReserveBytes, "checked_at": time.Now().UTC()}
	saved := max(int64(0), logicalBytes-chunkBytes)
	percent := 0.0
	if logicalBytes > 0 {
		percent = 100 * float64(saved) / float64(logicalBytes)
	}
	s["logical_bytes"] = logicalBytes
	s["wal_bytes"] = walBytes
	s["wal_files"] = walFiles
	s["unique_chunk_bytes"] = chunkBytes
	s["dedup_saved_bytes"] = saved
	s["dedup_saved_percent"] = percent
	if r.failed != nil {
		s["error"] = r.failed.Error()
	}
	return s, nil
}
func CopyChunk(w io.Writer, b []byte) error { _, e := w.Write(b); return e }
