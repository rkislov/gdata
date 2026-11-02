// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"os"
	"path/filepath"
	"time"
)

type manifestMembership struct {
	hashes   map[string]bool
	size     int64
	modified time.Time
	used     time.Time
}

func (m *Media) snapshotContains(id, hash string) (bool, error) {
	// Immutable manifests and active read leases permit bounded membership indexes.
	// Stat changes invalidate the index; payloads are still SHA-256 checked on GET.
	st, e := os.Stat(filepath.Join(m.Repo.Root, "snapshots", id+".json"))
	if e != nil {
		return false, e
	}
	m.membersMu.Lock()
	defer m.membersMu.Unlock()
	if cached := m.members[id]; cached != nil && cached.size == st.Size() && cached.modified.Equal(st.ModTime()) {
		cached.used = time.Now()
		return cached.hashes[hash], nil
	}
	manifest, e := m.Repo.Manifest(id)
	if e != nil {
		return false, e
	}
	index := map[string]bool{}
	for _, f := range manifest.Files {
		for _, h := range f.Chunks {
			index[h] = true
		}
	}
	if len(m.members) >= 8 {
		oldID := ""
		var oldest time.Time
		for key, v := range m.members {
			if oldID == "" || v.used.Before(oldest) {
				oldID = key
				oldest = v.used
			}
		}
		delete(m.members, oldID)
	}
	m.members[id] = &manifestMembership{hashes: index, size: st.Size(), modified: st.ModTime(), used: time.Now()}
	return index[hash], nil
}
