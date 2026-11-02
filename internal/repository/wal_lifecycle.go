// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gdata/srk/internal/model"
)

type walGCJournal struct {
	Version     int                     `json:"version"`
	OperationID string                  `json:"operation_id"`
	Phase       string                  `json:"phase"`
	Plan        model.WALGCPlan         `json:"plan"`
	Report      model.MaintenanceResult `json:"report"`
}

func (r *Repository) initializeWALGC() error {
	for _, name := range []string{"wal-pins", ".wal-gc"} {
		if e := os.MkdirAll(filepath.Join(r.Root, name), 0700); e != nil {
			return e
		}
	}
	var j walGCJournal
	e := readEnvelope(filepath.Join(r.Root, ".wal-gc", "journal.json"), &j)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	return r.recoverWALGC(j)
}
func (r *Repository) PinWAL(v model.WALPin) error {
	r.walMu.Lock()
	defer r.walMu.Unlock()
	if e := r.Check(); e != nil {
		return e
	}
	if !idPattern.MatchString(v.ID) || !hashPattern.MatchString(v.ArchiveID) || v.AgentID == "" || !v.ExpiresAt.After(time.Now()) || v.ExpiresAt.After(time.Now().Add(12*time.Hour)) {
		return errors.New("invalid WAL pin")
	}
	file := filepath.Join(r.Root, "wal-pins", v.ID+".json")
	var old model.WALPin
	if e := readEnvelope(file, &old); e == nil {
		if old.ID != v.ID || old.AgentID != v.AgentID || old.ArchiveID != v.ArchiveID || !old.ExpiresAt.Equal(v.ExpiresAt) {
			return errors.New("WAL pin identity conflict")
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	return writeEnvelope(file, v)
}
func (r *Repository) ReleaseWALPin(archive, id, agent string) error {
	r.walMu.Lock()
	defer r.walMu.Unlock()
	if !idPattern.MatchString(id) {
		return errors.New("invalid WAL pin ID")
	}
	file := filepath.Join(r.Root, "wal-pins", id+".json")
	var v model.WALPin
	if e := readEnvelope(file, &v); os.IsNotExist(e) {
		return nil
	} else if e != nil {
		return e
	}
	if v.ArchiveID != archive || v.AgentID != agent {
		return errors.New("WAL pin belongs to a different producer")
	}
	if e := os.Remove(file); e != nil {
		return e
	}
	return syncDirectory(filepath.Dir(file))
}
func (r *Repository) PreviewWALGC(archive string, before time.Time, orphan bool) (model.WALGCPlan, error) {
	r.walMu.RLock()
	defer r.walMu.RUnlock()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.walGCPlan(archive, before, orphan, time.Now().UTC())
}
func (r *Repository) walGCPlan(archive string, before time.Time, orphan bool, at time.Time) (model.WALGCPlan, error) {
	p := model.WALGCPlan{RepositoryID: r.Info.ID, ArchiveID: archive, At: at, Before: before, AllowOrphan: orphan, Names: []string{}, Dependencies: []string{}, Blocked: []string{}}
	if r.failed != nil {
		return p, r.failed
	}
	if before.IsZero() || before.After(at.Add(-24*time.Hour)) {
		return p, errors.New("WAL cleanup must keep at least the last 24 hours")
	}
	records, e := r.walInventory(archive)
	if e != nil {
		return p, e
	}
	points, e := r.inventory(false)
	if e != nil {
		return p, e
	}
	var fingerprint bytes.Buffer
	held, e := r.walReplicationHolds(archive, records, &fingerprint)
	if e != nil {
		return p, e
	}
	for _, record := range records {
		if held[record.Name] {
			p.ReplicationProtectedFiles++
			p.ReplicationProtectedBytes += record.Bytes
		}
	}
	floor := map[int]uint64{}
	latest := map[int]uint64{}
	for _, s := range points {
		pg := s.Result.PostgreSQL
		if pg == nil {
			continue
		}
		if pg.ArchiveID == "" && len(records) > 0 && pg.Profile == records[0].Profile {
			p.Blocked = append(p.Blocked, "legacy PostgreSQL base has no archive identity: "+s.ID)
		}
		if pg.ArchiveID != archive {
			continue
		}
		b, _ := json.Marshal(s)
		fingerprint.Write(b)
		p.Dependencies = append(p.Dependencies, s.ID)
		start, e := WALBasePosition(pg)
		if e != nil {
			return p, e
		}
		t := pg.WALRanges[0].Timeline
		if old, ok := floor[t]; !ok || start < old {
			floor[t] = start
		}
	}
	pins, e := os.ReadDir(filepath.Join(r.Root, "wal-pins"))
	if e != nil {
		return p, e
	}
	for _, f := range pins {
		if strings.HasPrefix(f.Name(), ".write-") {
			continue
		}
		var pin model.WALPin
		if e = readEnvelope(filepath.Join(r.Root, "wal-pins", f.Name()), &pin); e != nil {
			return p, e
		}
		if !idPattern.MatchString(pin.ID) || f.Name() != pin.ID+".json" || !hashPattern.MatchString(pin.ArchiveID) {
			return p, errors.New("invalid WAL pin metadata")
		}
		if pin.ArchiveID != archive {
			continue
		}
		b, _ := json.Marshal(pin)
		fingerprint.Write(b)
		if at.Before(pin.ExpiresAt) {
			p.ActivePins++
		}
	}
	if p.ActivePins > 0 {
		p.Blocked = append(p.Blocked, "base backup creation is active")
	}
	if len(p.Dependencies) == 0 && !orphan {
		p.Blocked = append(p.Blocked, "archive has no physical base; explicit orphan review required")
	}
	dir := filepath.Join(r.Root, "wal", archive)
	entries, e := os.ReadDir(dir)
	if e != nil && !os.IsNotExist(e) {
		return p, e
	}
	known := map[string]bool{}
	for _, v := range records {
		known[v.Name] = true
		if len(v.Name) == 24 {
			pos, e := WALPosition(v.Name, v.SegmentSize)
			if e != nil {
				return p, e
			}
			t, _ := strconv.ParseInt(v.Name[:8], 16, 32)
			if old, ok := latest[int(t)]; !ok || pos > old {
				latest[int(t)] = pos
			}
		}
	}
	for _, f := range entries {
		if strings.HasPrefix(f.Name(), ".write-") {
			continue
		}
		if f.Type()&os.ModeSymlink != 0 || f.IsDir() {
			return p, errors.New("unexpected WAL storage entry")
		}
		if WALName(f.Name()) && !known[f.Name()] {
			p.Blocked = append(p.Blocked, "unpublished WAL file: "+f.Name())
		} else if !WALName(f.Name()) && !strings.HasSuffix(f.Name(), ".json") {
			return p, errors.New("unknown WAL storage file")
		}
	}
	for _, v := range records {
		info, e := os.Lstat(filepath.Join(dir, v.Name))
		if e != nil {
			return p, e
		}
		if !info.Mode().IsRegular() || info.Size() != v.Bytes {
			return p, errors.New("WAL file is missing or incomplete")
		}
		b, _ := json.Marshal(v)
		fingerprint.Write(b)
		fmt.Fprintf(&fingerprint, "%d:%d;", info.Size(), info.ModTime().UnixNano())
		if held[v.Name] {
			continue
		}
		if len(v.Name) != 24 || !v.ArchivedAt.Before(before) {
			continue
		} // History is preserved indefinitely.
		pos, e := WALPosition(v.Name, v.SegmentSize)
		if e != nil {
			return p, e
		}
		t64, _ := strconv.ParseInt(v.Name[:8], 16, 32)
		t := int(t64)
		if pos == latest[t] {
			continue
		}
		if start, ok := floor[t]; ok {
			if pos >= start {
				continue
			}
		} else if len(p.Dependencies) > 0 || !orphan {
			continue
		}
		p.Names = append(p.Names, v.Name)
		p.Bytes += v.Bytes
	}
	if len(p.Blocked) > 0 {
		p.Names = []string{}
		p.Bytes = 0
	}
	sort.Strings(p.Names)
	sort.Strings(p.Dependencies)
	sort.Strings(p.Blocked)
	p.Fingerprint = Hash(fingerprint.Bytes())
	b, _ := json.Marshal(p)
	p.Token = Hash(b)
	return p, nil
}
func (r *Repository) ApplyWALGC(p model.WALGCPlan, operation string) (model.MaintenanceResult, error) {
	r.walMu.Lock()
	defer r.walMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	var result model.MaintenanceResult
	if r.failed != nil {
		return result, r.failed
	}
	if !idPattern.MatchString(operation) {
		return result, errors.New("invalid WAL cleanup operation")
	}
	receipt := filepath.Join(r.Root, "maintenance", "wal-"+operation+".json")
	var saved struct {
		Token  string                  `json:"token"`
		Report model.MaintenanceResult `json:"report"`
	}
	if e := readEnvelope(receipt, &saved); e == nil {
		if saved.Token != p.Token || saved.Report.ArchiveID != p.ArchiveID {
			return result, ErrPlanStale
		}
		return saved.Report, nil
	} else if !os.IsNotExist(e) {
		return result, e
	}
	if p.At.IsZero() || p.At.After(time.Now()) || time.Since(p.At) > 15*time.Minute {
		return result, ErrPlanStale
	}
	actual, e := r.walGCPlan(p.ArchiveID, p.Before, p.AllowOrphan, p.At)
	if e != nil {
		return result, e
	}
	if actual.Token != p.Token {
		return result, ErrPlanStale
	}
	for _, name := range actual.Names {
		var v model.WALRecord
		file := filepath.Join(r.Root, "wal", p.ArchiveID, name)
		if e = readEnvelope(file+".json", &v); e != nil {
			return result, e
		}
		f, e := os.Open(file)
		if e != nil {
			return result, e
		}
		h := sha256.New()
		_, e = io.Copy(h, f)
		f.Close()
		if e != nil {
			return result, e
		}
		if hex.EncodeToString(h.Sum(nil)) != v.SHA256 {
			return result, errors.New("cleanup refused: WAL checksum mismatch")
		}
	}
	result = model.MaintenanceResult{RepositoryID: r.Info.ID, ArchiveID: p.ArchiveID, OperationID: operation, At: time.Now().UTC(), WALRemoved: len(actual.Names), BytesRemoved: actual.Bytes}
	j := walGCJournal{Version: 1, OperationID: operation, Phase: "moving", Plan: actual, Report: result}
	if e = writeEnvelope(filepath.Join(r.Root, ".wal-gc", "journal.json"), j); e == nil {
		e = r.recoverWALGC(j)
	}
	if e != nil {
		r.failed = fmt.Errorf("WAL cleanup recovery required: %w", e)
		return result, r.failed
	}
	return result, nil
}
func (r *Repository) recoverWALGC(j walGCJournal) error {
	if j.Version != 1 || !idPattern.MatchString(j.OperationID) || !hashPattern.MatchString(j.Plan.ArchiveID) || j.Plan.RepositoryID != r.Info.ID || j.Report.OperationID != j.OperationID || j.Report.RepositoryID != r.Info.ID || j.Report.ArchiveID != j.Plan.ArchiveID || (j.Phase != "moving" && j.Phase != "purging") {
		return errors.New("invalid WAL cleanup journal")
	}
	stage := filepath.Join(r.Root, ".wal-gc", j.OperationID)
	source := filepath.Join(r.Root, "wal", j.Plan.ArchiveID)
	if e := os.MkdirAll(stage, 0700); e != nil {
		return e
	}
	if j.Phase == "moving" {
		for _, name := range j.Plan.Names {
			if !walSegment.MatchString(name) {
				return errors.New("unsafe WAL cleanup entry")
			}
			for _, suffix := range []string{"", ".json"} {
				from, to := filepath.Join(source, name+suffix), filepath.Join(stage, name+suffix)
				_, a := os.Lstat(from)
				_, b := os.Lstat(to)
				if os.IsNotExist(a) && b == nil {
					continue
				}
				if a != nil {
					return errors.New("WAL cleanup source disappeared")
				}
				if b == nil {
					return errors.New("WAL quarantine conflict")
				}
				if !os.IsNotExist(b) {
					return b
				}
				if e := os.Rename(from, to); e != nil {
					return e
				}
				if e := syncDirectory(source); e != nil {
					return e
				}
				if e := syncDirectory(stage); e != nil {
					return e
				}
			}
		}
		j.Phase = "purging"
		if e := writeEnvelope(filepath.Join(r.Root, ".wal-gc", "journal.json"), j); e != nil {
			return e
		}
	}
	for _, name := range j.Plan.Names {
		if !walSegment.MatchString(name) {
			return errors.New("unsafe WAL purge entry")
		}
		for _, suffix := range []string{"", ".json"} {
			if e := os.Remove(filepath.Join(stage, name+suffix)); e != nil && !os.IsNotExist(e) {
				return e
			}
		}
	}
	if e := syncDirectory(stage); e != nil {
		return e
	}
	if e := os.Remove(stage); e != nil {
		return e
	}
	if e := writeEnvelope(filepath.Join(r.Root, "maintenance", "wal-"+j.OperationID+".json"), struct {
		Token  string                  `json:"token"`
		Report model.MaintenanceResult `json:"report"`
	}{j.Plan.Token, j.Report}); e != nil {
		return e
	}
	if e := os.Remove(filepath.Join(r.Root, ".wal-gc", "journal.json")); e != nil {
		return e
	}
	return syncDirectory(filepath.Join(r.Root, ".wal-gc"))
}
