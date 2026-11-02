// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (r *Repository) walReceiverPath(id string) string {
	return filepath.Join(r.Root, "wal-receivers", id+".json")
}
func (r *Repository) walAckPath(v model.WALReceiver, name string) string {
	return filepath.Join(r.Root, "wal-acknowledgements", v.ID, v.Generation, name+".json")
}
func (r *Repository) walReceiver(id string) (model.WALReceiver, error) {
	var v model.WALReceiver
	if !idPattern.MatchString(id) {
		return v, errors.New("invalid WAL receiver ID")
	}
	if e := readEnvelope(r.walReceiverPath(id), &v); e != nil {
		return v, e
	}
	if v.ID != id || !hashPattern.MatchString(v.ArchiveID) || v.SourceRepositoryID != r.Info.ID || !idPattern.MatchString(v.Generation) || v.TargetMediaID == "" || v.TargetRepositoryID == "" {
		return v, errors.New("invalid WAL receiver metadata")
	}
	return v, nil
}
func (r *Repository) RegisterWALReceiver(v model.WALReceiver, reactivate bool) (model.WALReceiver, error) {
	r.walMu.Lock()
	defer r.walMu.Unlock()
	if e := r.Check(); e != nil {
		return v, e
	}
	if !idPattern.MatchString(v.ID) || !hashPattern.MatchString(v.ArchiveID) || v.SourceRepositoryID != r.Info.ID || v.TargetMediaID == "" || !idPattern.MatchString(v.TargetRepositoryID) || v.TargetRepositoryID == r.Info.ID {
		return v, errors.New("invalid independent WAL receiver")
	}
	old, e := r.walReceiver(v.ID)
	if e == nil {
		if !old.Released {
			if old.ArchiveID != v.ArchiveID || old.TargetMediaID != v.TargetMediaID || old.TargetRepositoryID != v.TargetRepositoryID {
				return old, errors.New("WAL receiver repository changed; retire previous protection explicitly")
			}
			return old, nil
		}
		if !reactivate {
			return old, errors.New("WAL receiver protection was released; explicitly enable it again")
		}
	} else if !os.IsNotExist(e) {
		return v, e
	}
	v.Generation = model.ID()
	v.CreatedAt = time.Now().UTC()
	v.Released = false
	v.ReleaseReason = ""
	v.LastContactAt = nil
	v.LastAcknowledgedAt = nil
	if e = os.MkdirAll(filepath.Dir(r.walReceiverPath(v.ID)), 0700); e != nil {
		return v, e
	}
	return v, writeEnvelope(r.walReceiverPath(v.ID), v)
}
func (r *Repository) WALReceiver(id string) (model.WALReceiver, error) {
	r.walMu.RLock()
	defer r.walMu.RUnlock()
	return r.walReceiver(id)
}
func (r *Repository) ReleaseWALReceiver(id, reason string) error {
	r.walMu.Lock()
	defer r.walMu.Unlock()
	if e := r.Check(); e != nil {
		return e
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 500 {
		return errors.New("reason required to abandon unconfirmed WAL")
	}
	v, e := r.walReceiver(id)
	if e != nil {
		return e
	}
	v.Released = true
	v.ReleaseReason = reason
	return writeEnvelope(r.walReceiverPath(id), v)
}
func (r *Repository) walAcknowledged(v model.WALReceiver, record model.WALRecord) (bool, error) {
	var a model.WALAcknowledgement
	e := readEnvelope(r.walAckPath(v, record.Name), &a)
	if os.IsNotExist(e) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if a.Name != record.Name || !hashPattern.MatchString(a.SHA256) || a.Bytes < 1 {
		return false, errors.New("corrupt WAL acknowledgement")
	}
	return a.SHA256 == record.SHA256 && a.Bytes == record.Bytes, nil
}
func (r *Repository) AcknowledgeWAL(id, media string, request model.WALReceiverAck) error {
	r.walMu.Lock()
	defer r.walMu.Unlock()
	if e := r.Check(); e != nil {
		return e
	}
	v, e := r.walReceiver(id)
	if e != nil {
		return e
	}
	if v.Released || v.Generation != request.Generation || v.TargetMediaID != media || v.TargetRepositoryID != request.TargetRepositoryID || len(request.Files) > 256 {
		return errors.New("WAL acknowledgement receiver/generation mismatch")
	}
	seen := map[string]bool{}
	for _, a := range request.Files {
		if !WALName(a.Name) || !hashPattern.MatchString(a.SHA256) || a.Bytes < 1 || seen[a.Name] {
			return errors.New("invalid WAL acknowledgement set")
		}
		seen[a.Name] = true
		var record model.WALRecord
		e = readEnvelope(filepath.Join(r.Root, "wal", v.ArchiveID, a.Name+".json"), &record)
		if os.IsNotExist(e) {
			var old model.WALAcknowledgement
			if readEnvelope(r.walAckPath(v, a.Name), &old) == nil && old == a {
				continue
			}
			return errors.New("cannot acknowledge an unknown producer WAL file")
		}
		if e != nil {
			return e
		}
		if record.ArchiveID != v.ArchiveID || record.Name != a.Name || record.SHA256 != a.SHA256 || record.Bytes != a.Bytes {
			return errors.New("WAL acknowledgement does not match producer digest/size")
		}
	}
	for _, a := range request.Files {
		name := r.walAckPath(v, a.Name)
		var old model.WALAcknowledgement
		if readEnvelope(name, &old) == nil && old == a {
			continue
		}
		if e = os.MkdirAll(filepath.Dir(name), 0700); e != nil {
			return e
		}
		if e = writeEnvelope(name, a); e != nil {
			return e
		}
	}
	now := time.Now().UTC()
	v.LastContactAt = &now
	if len(request.Files) > 0 {
		v.LastAcknowledgedAt = &now
	}
	return writeEnvelope(r.walReceiverPath(id), v)
}
func (r *Repository) WALReceiverPending(id string) ([]model.WALRecord, model.WALReceiver, error) {
	r.walMu.RLock()
	defer r.walMu.RUnlock()
	v, e := r.walReceiver(id)
	if e != nil {
		return nil, v, e
	}
	if v.Released {
		return nil, v, errors.New("WAL receiver protection released")
	}
	records, e := r.walInventory(v.ArchiveID)
	if e != nil {
		return nil, v, e
	}
	pending := []model.WALRecord{}
	for _, record := range records {
		ok, e := r.walAcknowledged(v, record)
		if e != nil {
			return nil, v, e
		}
		if !ok {
			pending = append(pending, record)
		}
	}
	return pending, v, nil
}
func (r *Repository) WALReceiverLag(id string) (model.WALReplicationLag, error) {
	now := time.Now().UTC()
	out := model.WALReplicationLag{ObservedAt: now}
	r.walMu.RLock()
	defer r.walMu.RUnlock()
	v, e := r.walReceiver(id)
	if e != nil {
		return out, e
	}
	out.LastContactAt = v.LastContactAt
	out.LastAcknowledgedAt = v.LastAcknowledgedAt
	out.SourceProtected = !v.Released
	if v.Released {
		out.State = "released"
		return out, nil
	}
	records, e := r.walInventory(v.ArchiveID)
	if e != nil {
		return out, e
	}
	var count, bytes int64
	for _, record := range records {
		ok, e := r.walAcknowledged(v, record)
		if e != nil {
			return out, e
		}
		if ok {
			continue
		}
		count++
		bytes += record.Bytes
		if out.OldestPendingAt == nil || record.ArchivedAt.Before(*out.OldestPendingAt) {
			at := record.ArchivedAt
			out.OldestPendingAt = &at
		}
	}
	out.PendingFiles = &count
	out.PendingBytes = &bytes
	out.State = "caught_up"
	age := int64(0)
	if count > 0 {
		out.State = "pending"
		if out.OldestPendingAt != nil {
			age = max(int64(0), int64(now.Sub(*out.OldestPendingAt).Seconds()))
		}
	}
	out.PendingAgeSeconds = &age
	return out, nil
}

// Caller holds walMu and mu. New enrollment/receipts invalidate a GC preview.
func (r *Repository) walReplicationHolds(archive string, records []model.WALRecord, fingerprint interface{ Write([]byte) (int, error) }) (map[string]bool, error) {
	held := map[string]bool{}
	entries, e := os.ReadDir(filepath.Join(r.Root, "wal-receivers"))
	if errors.Is(e, fs.ErrNotExist) {
		return held, nil
	}
	if e != nil {
		return nil, e
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".write-") {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") || !entry.Type().IsRegular() {
			return nil, errors.New("unexpected WAL receiver metadata entry")
		}
		v, e := r.walReceiver(strings.TrimSuffix(entry.Name(), ".json"))
		if e != nil {
			return nil, e
		}
		if v.ArchiveID != archive {
			continue
		}
		_, _ = fingerprint.Write([]byte(fmt.Sprintf("receiver:%s:%s:%s:%t;", v.ID, v.Generation, v.TargetRepositoryID, v.Released)))
		if v.Released {
			continue
		}
		for _, record := range records {
			ok, e := r.walAcknowledged(v, record)
			if e != nil {
				return nil, e
			}
			_, _ = fingerprint.Write([]byte(fmt.Sprintf("%s:%s:%t;", v.ID, record.Name, ok)))
			if !ok {
				held[record.Name] = true
			}
		}
	}
	return held, nil
}
