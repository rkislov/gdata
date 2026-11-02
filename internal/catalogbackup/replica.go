// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalogbackup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ReplicaStore stores only ciphertext. Retention is owned by the receiving media server.
type ReplicaStore struct {
	mu  sync.Mutex
	dir string
}
type Receipt struct {
	ID         string    `json:"id"`
	SHA256     string    `json:"sha256"`
	Bytes      int64     `json:"bytes"`
	ReceivedAt time.Time `json:"received_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func OpenReplicas(dir string) (*ReplicaStore, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	files, e := os.ReadDir(dir)
	if e != nil {
		return nil, e
	}
	for _, f := range files {
		if !f.IsDir() && strings.HasPrefix(f.Name(), ".upload-") {
			if e = os.Remove(filepath.Join(dir, f.Name())); e != nil {
				return nil, e
			}
		}
	}
	return &ReplicaStore{dir: dir}, nil
}
func (s *ReplicaStore) read(id string) (Receipt, error) {
	var v Receipt
	if !validID(id) {
		return v, errors.New("invalid archive ID")
	}
	b, e := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if e != nil {
		return v, e
	}
	e = json.Unmarshal(b, &v)
	if e == nil && v.ID != id {
		e = errors.New("invalid receipt")
	}
	return v, e
}
func (s *ReplicaStore) Put(id, hash string, days int, r io.Reader) (Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero Receipt
	sumBytes, e := hex.DecodeString(hash)
	if !validID(id) || e != nil || len(sumBytes) != 32 || days < 1 || days > 36500 {
		return zero, errors.New("invalid replica parameters")
	}
	// Retries cannot shorten or silently extend retention, nor replace an archive.
	old, err := s.read(id)
	if err != nil && !os.IsNotExist(err) {
		return zero, err
	}
	if err == nil && old.SHA256 != hash {
		return zero, errors.New("archive identity conflict")
	}
	f, e := os.CreateTemp(s.dir, ".upload-")
	if e != nil {
		return zero, e
	}
	defer os.Remove(f.Name())
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, (16<<30)+1))
	if e == nil && (n > 16<<30 || hex.EncodeToString(h.Sum(nil)) != hash) {
		e = errors.New("archive size or checksum mismatch")
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return zero, e
	}
	if err == nil {
		// Replacing with the fully verified retry repairs damaged ciphertext while preserving retention.
		if old.Bytes != n {
			return zero, errors.New("archive size conflict")
		}
		if e = os.Rename(f.Name(), filepath.Join(s.dir, id+".srkcat")); e != nil {
			return zero, e
		}
		if e = syncDir(s.dir); e != nil {
			return zero, e
		}
		return old, nil
	}
	now := time.Now().UTC()
	v := Receipt{ID: id, SHA256: hash, Bytes: n, ReceivedAt: now, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour)}
	if e = os.Rename(f.Name(), filepath.Join(s.dir, id+".srkcat")); e != nil {
		return zero, e
	}
	b, _ := json.Marshal(v)
	meta, e := os.CreateTemp(s.dir, ".upload-")
	if e != nil {
		return zero, e
	}
	defer os.Remove(meta.Name())
	_, e = meta.Write(b)
	if e == nil {
		e = meta.Sync()
	}
	ce = meta.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return zero, e
	}
	if e = os.Rename(meta.Name(), filepath.Join(s.dir, id+".json")); e != nil {
		return zero, e
	}
	if e = syncDir(s.dir); e != nil {
		return zero, e
	}
	return v, nil
}
func (s *ReplicaStore) List() ([]Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, e := os.ReadDir(s.dir)
	if e != nil {
		return nil, e
	}
	out := []Receipt{}
	for _, f := range files {
		id := f.Name()
		if len(id) != 37 || filepath.Ext(id) != ".json" {
			continue
		}
		v, e := s.read(id[:32])
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReceivedAt.After(out[j].ReceivedAt) })
	return out, nil
}

// Open and GC serialize; an already opened inode remains readable during expiry cleanup.
func (s *ReplicaStore) Open(id string) (*os.File, Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, e := s.read(id)
	if e != nil {
		return nil, v, e
	}
	root, e := os.OpenRoot(s.dir)
	if e != nil {
		return nil, v, e
	}
	defer root.Close()
	f, e := root.Open(id + ".srkcat")
	return f, v, e
}
func (s *ReplicaStore) GC(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, e := os.ReadDir(s.dir)
	if e != nil {
		return e
	}
	for _, f := range files {
		name := f.Name()
		if len(name) != 37 || filepath.Ext(name) != ".json" {
			continue
		}
		v, e := s.read(name[:32])
		if e != nil {
			return e
		}
		if v.ExpiresAt.IsZero() || now.Before(v.ExpiresAt) {
			continue
		}
		if e = os.Remove(filepath.Join(s.dir, v.ID+".srkcat")); e != nil && !os.IsNotExist(e) {
			return e
		}
		if e = os.Remove(filepath.Join(s.dir, name)); e != nil {
			return e
		}
	}
	return syncDir(s.dir)
}
