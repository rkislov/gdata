// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"encoding/json"
	"errors"
	"gdata/srk/internal/repository"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var davID = regexp.MustCompile(`^[a-f0-9]{32}$`)

type davSaved struct {
	Session   *davSession `json:"session"`
	Header    []byte      `json:"header,omitempty"`
	OldHeader int64       `json:"old_header,omitempty"`
}

func (s *davStore) stateDir() string { return filepath.Join(s.m.Repo.Root, "state", "dav") }
func (s *davStore) save(session *davSession) error {
	if !davID.MatchString(session.ID) {
		return errors.New("invalid DAV session ID")
	}
	if e := os.MkdirAll(s.stateDir(), 0700); e != nil {
		return e
	}
	b, e := json.Marshal(davSaved{session, session.header, session.oldHeader})
	if e != nil {
		return e
	}
	return repository.Atomic(filepath.Join(s.stateDir(), session.ID+".json"), b, 0600)
}
func (s *davStore) load() error {
	entries, e := os.ReadDir(s.stateDir())
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !davID.MatchString(id) || !entry.Type().IsRegular() {
			return errors.New("invalid DAV state file")
		}
		b, e := os.ReadFile(filepath.Join(s.stateDir(), entry.Name()))
		if e != nil {
			return e
		}
		var row davSaved
		if json.Unmarshal(b, &row) != nil || row.Session == nil || row.Session.ID != id {
			return errors.New("corrupt DAV session state")
		}
		session := row.Session
		session.header = row.Header
		session.oldHeader = row.OldHeader
		if !time.Now().Before(session.ExpiresAt) {
			_ = s.root.RemoveAll("staging/" + id)
			_ = s.root.RemoveAll("restore/" + id)
			_ = os.Remove(filepath.Join(s.stateDir(), entry.Name()))
			continue
		}
		if e = s.recount(session); e != nil {
			return e
		}
		s.sessions[id] = session
	}
	return nil
}

// Expiration removes only sessions with durable ownership and no active writers.
func (s *davStore) expireSessions(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.mu.Lock()
			for id, session := range s.sessions {
				if !time.Now().Before(session.ExpiresAt) && session.writers == 0 {
					if s.root.RemoveAll("staging/"+id) != nil || s.root.RemoveAll("restore/"+id) != nil {
						continue
					}
					if os.Remove(filepath.Join(s.stateDir(), id+".json")) != nil {
						continue
					}
					delete(s.sessions, id)
				}
			}
			s.mu.Unlock()
		}
	}
}
