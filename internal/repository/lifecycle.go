// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gdata/srk/internal/model"
)

var ErrPlanStale = errors.New("repository changed; obtain a new cleanup preview")
var ErrExpired = errors.New("snapshot retention expired")
var ErrLeaseClosed = errors.New("operation lease is closed")

type snapshotState struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
	Hold      bool      `json:"hold"`
	Reason    string    `json:"reason,omitempty"`
}
type gcJournal struct {
	Version     int                     `json:"version"`
	OperationID string                  `json:"operation_id"`
	Phase       string                  `json:"phase"`
	Plan        model.GCPlan            `json:"plan"`
	Report      model.MaintenanceResult `json:"report"`
}
type envelope struct {
	Checksum string          `json:"checksum"`
	Data     json.RawMessage `json:"data"`
}

func writeEnvelope(name string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	b, e = json.Marshal(envelope{Hash(b), b})
	if e != nil {
		return e
	}
	return Atomic(name, b, 0600)
}
func readEnvelope(name string, v any) error {
	f, e := os.Open(name)
	if e != nil {
		return e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return e
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return errors.New("invalid metadata file")
	}
	b, e := io.ReadAll(io.LimitReader(f, 64<<20))
	if e != nil {
		return e
	}
	var x envelope
	if e = json.Unmarshal(b, &x); e != nil {
		return e
	}
	if Hash(x.Data) != x.Checksum {
		return fmt.Errorf("metadata checksum mismatch: %s", filepath.Base(name))
	}
	d := json.NewDecoder(bytes.NewReader(x.Data))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func syncDirectory(dir string) error {
	f, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (r *Repository) RecordAudit(v model.RepositoryAudit) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed != nil {
		return r.failed
	}
	if !idPattern.MatchString(v.ID) {
		return errors.New("invalid audit ID")
	}
	return writeEnvelope(filepath.Join(r.Root, "audit", v.ID+".json"), v)
}
func (r *Repository) Audits() ([]model.RepositoryAudit, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entries, e := os.ReadDir(filepath.Join(r.Root, "audit"))
	if e != nil {
		return nil, e
	}
	out := []model.RepositoryAudit{}
	for _, f := range entries {
		if strings.HasPrefix(f.Name(), ".write-") {
			continue
		}
		var a model.RepositoryAudit
		if e = readEnvelope(filepath.Join(r.Root, "audit", f.Name()), &a); e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if len(out) > 200 {
		out = out[:200]
	}
	return out, nil
}

func (r *Repository) initialize() error {
	name := filepath.Join(r.Root, "repository.json")
	if e := readEnvelope(name, &r.Info); os.IsNotExist(e) {
		r.Info = model.RepositoryInfo{ID: model.ID(), Version: 1, CreatedAt: time.Now().UTC()}
		if e = writeEnvelope(name, r.Info); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	if r.Info.Version != 1 || !idPattern.MatchString(r.Info.ID) {
		return errors.New("unsupported repository identity")
	}
	var j gcJournal
	if e := readEnvelope(filepath.Join(r.Root, ".gc", "journal.json"), &j); e == nil {
		if e = r.recoverGC(j); e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	return r.initializeWALGC()
}
func (r *Repository) state(id string, m model.Manifest) (snapshotState, error) {
	s := snapshotState{ID: id, ExpiresAt: m.ExpiresAt}
	e := readEnvelope(filepath.Join(r.Root, "state", id+".json"), &s)
	if e != nil && !os.IsNotExist(e) {
		return s, e
	}
	if s.ID != id || s.ExpiresAt.IsZero() {
		return s, errors.New("invalid snapshot policy")
	}
	return s, nil
}
func (r *Repository) snapshotInfo(id string, verify bool) (model.SnapshotInfo, error) {
	m, e := r.manifest(id)
	if e != nil {
		return model.SnapshotInfo{}, e
	}
	s, e := r.state(id, m)
	if e != nil {
		return model.SnapshotInfo{}, e
	}
	b, e := os.ReadFile(filepath.Join(r.Root, "snapshots", id+".json"))
	if e != nil {
		return model.SnapshotInfo{}, e
	}
	v := model.SnapshotInfo{ID: id, JobID: m.JobID, Source: m.Source, CreatedAt: m.CreatedAt, ExpiresAt: s.ExpiresAt, Hold: s.Hold, HoldReason: s.Reason, Mode: m.Mode, Consistency: m.Consistency, ManifestHash: Hash(b), Result: m.Result}
	if verify {
		if e = r.verify(id); e != nil {
			return v, e
		}
		v.Verified = true
	}
	return v, nil
}
func (r *Repository) Snapshot(id string, verify bool) (model.SnapshotInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.failed != nil {
		return model.SnapshotInfo{}, r.failed
	}
	return r.snapshotInfo(id, verify)
}
func (r *Repository) Available(id string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, e := r.snapshotInfo(id, false)
	if e != nil {
		return e
	}
	if !s.Hold && !time.Now().Before(s.ExpiresAt) {
		return ErrExpired
	}
	return nil
}
func (r *Repository) Inventory(verify bool) ([]model.SnapshotInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.failed != nil {
		return nil, r.failed
	}
	return r.inventory(verify)
}
func (r *Repository) inventory(verify bool) ([]model.SnapshotInfo, error) {
	files, e := os.ReadDir(filepath.Join(r.Root, "snapshots"))
	if e != nil {
		return nil, e
	}
	out := []model.SnapshotInfo{}
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".write-") {
			continue
		}
		id := strings.TrimSuffix(f.Name(), ".json")
		if f.IsDir() || !idPattern.MatchString(id) || f.Name() != id+".json" || f.Type()&os.ModeSymlink != 0 {
			return nil, errors.New("unknown entry in snapshot directory")
		}
		s, e := r.snapshotInfo(id, verify)
		if e != nil {
			return nil, fmt.Errorf("snapshot %s: %w", id, e)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (r *Repository) UpdatePolicy(id string, p model.SnapshotPolicy) (model.SnapshotInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed != nil {
		return model.SnapshotInfo{}, r.failed
	}
	m, e := r.manifest(id)
	if e != nil {
		return model.SnapshotInfo{}, e
	}
	s, e := r.state(id, m)
	if e != nil {
		return model.SnapshotInfo{}, e
	}
	if p.ExpiresAt == nil && p.Hold == nil {
		return model.SnapshotInfo{}, errors.New("policy update is empty")
	}
	if p.ExpiresAt != nil {
		if p.ExpiresAt.IsZero() || p.ExpiresAt.Before(m.CreatedAt) || p.ExpiresAt.After(time.Now().AddDate(100, 0, 0)) {
			return model.SnapshotInfo{}, errors.New("invalid retention deadline")
		}
		s.ExpiresAt = p.ExpiresAt.UTC()
	}
	if p.Hold != nil {
		s.Hold = *p.Hold
		s.Reason = strings.TrimSpace(p.Reason)
		if s.Hold && (s.Reason == "" || len(s.Reason) > 1000) {
			return model.SnapshotInfo{}, errors.New("hold requires a reason of at most 1000 bytes")
		}
	}
	if e = writeEnvelope(filepath.Join(r.Root, "state", id+".json"), s); e != nil {
		return model.SnapshotInfo{}, e
	}
	return r.snapshotInfo(id, false)
}

func (r *Repository) BeginLease(l model.Lease) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed != nil {
		return r.failed
	}
	now := time.Now()
	if !idPattern.MatchString(l.ID) || l.Subject == "" || (l.Kind != "read" && l.Kind != "write") || !l.ExpiresAt.After(now) || l.ExpiresAt.After(now.Add(12*time.Hour+time.Minute)) || l.Closed {
		return errors.New("invalid operation lease")
	}
	for _, id := range []string{l.SnapshotID, l.Base} {
		if id != "" && !idPattern.MatchString(id) {
			return errors.New("invalid lease snapshot")
		}
	}
	var old model.Lease
	name := filepath.Join(r.Root, "leases", l.ID+".json")
	if e := readEnvelope(name, &old); e == nil {
		if old.Closed {
			return ErrLeaseClosed
		}
		if old.ID != l.ID || old.Subject != l.Subject || old.Kind != l.Kind || old.SnapshotID != l.SnapshotID || old.Base != l.Base || !old.ExpiresAt.Equal(l.ExpiresAt) {
			return errors.New("lease identity conflict")
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	id := l.Base
	if l.Kind == "read" {
		id = l.SnapshotID
		if id == "" {
			return errors.New("read lease needs a snapshot")
		}
	}
	if id != "" {
		s, e := r.snapshotInfo(id, false)
		if e != nil {
			return e
		}
		if !s.Hold && !now.Before(s.ExpiresAt) {
			return ErrExpired
		}
	}
	return writeEnvelope(name, l)
}
func (r *Repository) EndLease(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed != nil {
		return r.failed
	}
	return r.endLease(id)
}
func (r *Repository) endLease(id string) error {
	if !idPattern.MatchString(id) {
		return errors.New("invalid lease ID")
	}
	name := filepath.Join(r.Root, "leases", id+".json")
	var l model.Lease
	if e := readEnvelope(name, &l); os.IsNotExist(e) {
		return nil
	} else if e != nil {
		return e
	}
	l.Closed = true
	return writeEnvelope(name, l)
}
func (r *Repository) CommitLease(m model.Manifest, id string) error {
	return r.CommitLeaseContext(context.Background(), m, id)
}
func (r *Repository) CommitLeaseContext(ctx context.Context, m model.Manifest, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed != nil {
		return r.failed
	}
	var l model.Lease
	if e := readEnvelope(filepath.Join(r.Root, "leases", id+".json"), &l); e != nil {
		return e
	}
	if l.Closed || l.Kind != "write" || l.ID != m.ID || !time.Now().Before(l.ExpiresAt) {
		return errors.New("write lease not active")
	}
	if e := r.commitContext(ctx, m); e != nil {
		return e
	}
	return r.endLease(id)
}

func (r *Repository) PrepareImport(ids []string, days int) ([]model.SnapshotInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed != nil {
		return nil, r.failed
	}
	if len(ids) == 0 || len(ids) > 1000 || days < 1 || days > 36500 {
		return nil, errors.New("select 1–1000 snapshots and a valid retention period")
	}
	seen := map[string]bool{}
	out := []model.SnapshotInfo{}
	for _, id := range ids {
		if seen[id] {
			return nil, errors.New("duplicate import snapshot")
		}
		seen[id] = true
		s, e := r.snapshotInfo(id, true)
		if e != nil {
			return nil, e
		}
		out = append(out, s)
	}
	// Every selected point is held before returning it for catalog registration.
	// A partial metadata write can only retain extra data, never permit deletion.
	expires := time.Now().UTC().AddDate(0, 0, days)
	for i, s := range out {
		expiry := expires
		if s.ExpiresAt.After(expiry) {
			expiry = s.ExpiresAt
		}
		if e := writeEnvelope(filepath.Join(r.Root, "state", s.ID+".json"), snapshotState{ID: s.ID, ExpiresAt: expiry, Hold: true, Reason: "Импорт: защита до явного снятия удержания"}); e != nil {
			return nil, e
		}
		s.ExpiresAt = expiry
		s.Hold = true
		s.HoldReason = "Импорт: защита до явного снятия удержания"
		out[i] = s
	}
	return out, nil
}

func (r *Repository) PreviewGC() (model.GCPlan, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.failed != nil {
		return model.GCPlan{}, r.failed
	}
	return r.plan(time.Now().UTC())
}
func (r *Repository) plan(at time.Time) (model.GCPlan, error) {
	p := model.GCPlan{RepositoryID: r.Info.ID, At: at, Snapshots: []string{}, Chunks: []string{}, ExpiredLeases: []string{}}
	points, e := r.inventory(true)
	if e != nil {
		return p, e
	}
	var fingerprint bytes.Buffer
	pinned := map[string]bool{}
	referenced := map[string]bool{}
	leases, e := os.ReadDir(filepath.Join(r.Root, "leases"))
	if e != nil {
		return p, e
	}
	for _, f := range leases {
		if strings.HasPrefix(f.Name(), ".write-") {
			continue
		}
		id := strings.TrimSuffix(f.Name(), ".json")
		if !idPattern.MatchString(id) || f.Name() != id+".json" || f.Type()&os.ModeSymlink != 0 {
			return p, errors.New("unknown lease metadata")
		}
		var l model.Lease
		if e = readEnvelope(filepath.Join(r.Root, "leases", f.Name()), &l); e != nil {
			return p, e
		}
		if l.ID != id || (l.Kind != "read" && l.Kind != "write") {
			return p, errors.New("invalid persisted lease")
		}
		b, _ := json.Marshal(l)
		fingerprint.Write(b)
		if !at.Before(l.ExpiresAt) {
			p.ExpiredLeases = append(p.ExpiredLeases, id)
			continue
		}
		if l.Closed {
			continue
		}
		if l.Kind == "write" {
			p.ActiveWriters++
		} else {
			p.ActiveReaders++
		}
		if l.SnapshotID != "" {
			pinned[l.SnapshotID] = true
		}
		if l.Base != "" {
			pinned[l.Base] = true
		}
	}
	for _, s := range points {
		b, _ := json.Marshal(s)
		fingerprint.Write(b)
		if !s.Hold && !at.Before(s.ExpiresAt) && !pinned[s.ID] {
			p.Snapshots = append(p.Snapshots, s.ID)
			continue
		}
		m, e := r.manifest(s.ID)
		if e != nil {
			return p, e
		}
		for _, f := range m.Files {
			for _, h := range f.Chunks {
				referenced[h] = true
			}
		}
	}
	e = filepath.WalkDir(filepath.Join(r.Root, "chunks"), func(name string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".write-") {
			return nil
		}
		h := d.Name()
		if !hashPattern.MatchString(h) || filepath.Dir(name) != filepath.Join(r.Root, "chunks", h[:2]) || d.Type()&os.ModeSymlink != 0 {
			return errors.New("unknown entry in chunk storage")
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		fmt.Fprintf(&fingerprint, "%s:%d:%d;", h, info.Size(), info.ModTime().UnixNano())
		if p.ActiveWriters == 0 && !referenced[h] {
			p.Chunks = append(p.Chunks, h)
			p.Bytes += info.Size()
		}
		return nil
	})
	if e != nil {
		return p, e
	}
	sort.Strings(p.Snapshots)
	sort.Strings(p.Chunks)
	sort.Strings(p.ExpiredLeases)
	p.Fingerprint = Hash(fingerprint.Bytes())
	copy := p
	copy.Token = ""
	b, _ := json.Marshal(copy)
	p.Token = Hash(b)
	return p, nil
}
func (r *Repository) ApplyGC(p model.GCPlan, operation string) (model.MaintenanceResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed != nil {
		return model.MaintenanceResult{}, r.failed
	}
	if !idPattern.MatchString(operation) {
		return model.MaintenanceResult{}, errors.New("invalid maintenance ID")
	}
	var old model.MaintenanceResult
	if e := readEnvelope(filepath.Join(r.Root, "maintenance", operation+".json"), &old); e == nil {
		return old, nil
	} else if !os.IsNotExist(e) {
		return old, e
	}
	if p.At.IsZero() || time.Since(p.At) > 15*time.Minute || p.At.After(time.Now()) {
		return model.MaintenanceResult{}, ErrPlanStale
	}
	actual, e := r.plan(p.At)
	if e != nil {
		return model.MaintenanceResult{}, e
	}
	if actual.Token != p.Token || actual.RepositoryID != p.RepositoryID {
		return model.MaintenanceResult{}, ErrPlanStale
	}
	return r.applyGC(actual, operation)
}
func (r *Repository) AutomaticGC(operation string) (model.MaintenanceResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed != nil {
		return model.MaintenanceResult{}, r.failed
	}
	p, e := r.plan(time.Now().UTC())
	if e != nil {
		return model.MaintenanceResult{}, e
	}
	return r.applyGC(p, operation)
}
func (r *Repository) applyGC(p model.GCPlan, operation string) (model.MaintenanceResult, error) {
	if !idPattern.MatchString(operation) {
		return model.MaintenanceResult{}, errors.New("invalid maintenance ID")
	}
	var previous model.MaintenanceResult
	if e := readEnvelope(filepath.Join(r.Root, "maintenance", operation+".json"), &previous); e == nil {
		return previous, nil
	} else if !os.IsNotExist(e) {
		return previous, e
	}
	report := model.MaintenanceResult{OperationID: operation, RepositoryID: r.Info.ID, At: time.Now().UTC(), SnapshotsRemoved: len(p.Snapshots), ChunksRemoved: len(p.Chunks), BytesRemoved: p.Bytes, DeferredWriters: p.ActiveWriters, RemovedSnapshots: p.Snapshots}
	j := gcJournal{Version: 1, OperationID: operation, Phase: "moving", Plan: p, Report: report}
	if e := writeEnvelope(filepath.Join(r.Root, ".gc", "journal.json"), j); e != nil {
		r.failed = fmt.Errorf("cleanup recovery required after journal write: %w", e)
		return report, r.failed
	}
	if e := r.recoverGC(j); e != nil {
		r.failed = fmt.Errorf("cleanup recovery required: %w", e)
		return report, r.failed
	}
	return report, nil
}
func (r *Repository) recoverGC(j gcJournal) error {
	if j.Version != 1 || !idPattern.MatchString(j.OperationID) || j.Plan.RepositoryID != r.Info.ID || j.Report.OperationID != j.OperationID || j.Report.RepositoryID != r.Info.ID || (j.Phase != "moving" && j.Phase != "purging") {
		return errors.New("invalid cleanup journal")
	}
	type move struct {
		from, to string
		optional bool
	}
	moves := []move{}
	stage := filepath.Join(r.Root, ".gc", j.OperationID)
	for _, id := range j.Plan.Snapshots {
		if !idPattern.MatchString(id) {
			return errors.New("unsafe snapshot in cleanup journal")
		}
		for _, kind := range []string{"snapshots", "state"} {
			moves = append(moves, move{filepath.Join(r.Root, kind, id+".json"), filepath.Join(stage, kind, id+".json"), kind == "state"})
		}
	}
	for _, h := range j.Plan.Chunks {
		if !hashPattern.MatchString(h) {
			return errors.New("unsafe chunk in cleanup journal")
		}
		moves = append(moves, move{filepath.Join(r.Root, "chunks", h[:2], h), filepath.Join(stage, "chunks", h), false})
	}
	for _, id := range j.Plan.ExpiredLeases {
		if !idPattern.MatchString(id) {
			return errors.New("unsafe lease in cleanup journal")
		}
		moves = append(moves, move{filepath.Join(r.Root, "leases", id+".json"), filepath.Join(stage, "leases", id+".json"), false})
	}
	if j.Phase == "moving" {
		for _, m := range moves {
			_, src := os.Lstat(m.from)
			_, dst := os.Lstat(m.to)
			if src != nil && !os.IsNotExist(src) {
				return src
			}
			if dst != nil && !os.IsNotExist(dst) {
				return dst
			}
			if os.IsNotExist(src) {
				if os.IsNotExist(dst) && !m.optional {
					return errors.New("cleanup source disappeared before quarantine")
				}
				continue
			}
			if dst == nil {
				return errors.New("conflicting cleanup quarantine file")
			}
			if e := os.MkdirAll(filepath.Dir(m.to), 0700); e != nil {
				return e
			}
			if e := os.Rename(m.from, m.to); e != nil {
				return e
			}
			if e := syncDirectory(filepath.Dir(m.from)); e != nil {
				return e
			}
			if e := syncDirectory(filepath.Dir(m.to)); e != nil {
				return e
			}
		}
		j.Phase = "purging"
		if e := writeEnvelope(filepath.Join(r.Root, ".gc", "journal.json"), j); e != nil {
			return e
		}
	}
	for _, m := range moves {
		if e := os.Remove(m.to); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	for _, dir := range []string{"snapshots", "state", "chunks", "leases"} {
		d := filepath.Join(stage, dir)
		if e := syncDirectory(d); e != nil && !os.IsNotExist(e) {
			return e
		}
		if e := os.Remove(d); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	if e := os.Remove(stage); e != nil && !os.IsNotExist(e) {
		return e
	}
	if e := writeEnvelope(filepath.Join(r.Root, "maintenance", j.OperationID+".json"), j.Report); e != nil {
		return e
	}
	if e := os.Remove(filepath.Join(r.Root, ".gc", "journal.json")); e != nil {
		return e
	}
	return syncDirectory(filepath.Join(r.Root, ".gc"))
}
