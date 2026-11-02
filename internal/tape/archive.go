// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package tape

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const archiveMagic = "SRKTAPE1"

type archiveHeader struct {
	Version            int            `json:"version"`
	VolumeID           string         `json:"volume_id"`
	Barcode            string         `json:"barcode"`
	SourceRepositoryID string         `json:"source_repository_id"`
	Copy               Copy           `json:"copy"`
	Manifest           model.Manifest `json:"manifest"`
}

func chunks(m model.Manifest) ([]string, map[string]int64, int64, error) {
	sizes := map[string]int64{}
	var total int64
	for _, f := range m.Files {
		if f.Kind != "file" {
			continue
		}
		for i, h := range f.Chunks {
			n := min(int64(repository.ChunkSize), f.Size-int64(i)*repository.ChunkSize)
			if old, ok := sizes[h]; ok {
				if old != n {
					return nil, nil, 0, errors.New("conflicting chunk lengths")
				}
			} else {
				sizes[h] = n
				total += n
				if total < 0 {
					return nil, nil, 0, errors.New("archive size overflow")
				}
			}
		}
	}
	order := make([]string, 0, len(sizes))
	for h := range sizes {
		order = append(order, h)
	}
	sort.Strings(order)
	return order, sizes, total, nil
}
func parseArchive(ctx context.Context, path string, put func(string, []byte) error, progress func(int64, int64)) (archiveHeader, error) {
	var h archiveHeader
	f, e := os.Open(path)
	if e != nil {
		return h, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		return h, errors.New("archive is not a regular file")
	}
	digest := sha256.New()
	r := io.TeeReader(f, digest)
	var pre [16]byte
	if _, e = io.ReadFull(r, pre[:]); e != nil {
		return h, e
	}
	n := binary.BigEndian.Uint64(pre[8:])
	if string(pre[:8]) != archiveMagic || n < 1 || n > uint64(model.MaxManifestBytes) {
		return h, errors.New("invalid tape header")
	}
	b := make([]byte, int(n))
	if _, e = io.ReadFull(r, b); e != nil {
		return h, e
	}
	if json.Unmarshal(b, &h) != nil || h.Version != 1 || !idPattern.MatchString(h.VolumeID) || !barcodePattern.MatchString(h.Barcode) || !idPattern.MatchString(h.Copy.ID) || !idPattern.MatchString(h.SourceRepositoryID) || h.Copy.SnapshotID != h.Manifest.ID || h.Copy.ExpiresAt.IsZero() || repository.Validate(h.Manifest) != nil {
		return h, errors.New("invalid tape archive identity or manifest")
	}
	order, sizes, total, e := chunks(h.Manifest)
	if e != nil {
		return h, e
	}
	if info.Size() != 16+int64(n)+total+32 {
		return h, errors.New("truncated or oversized tape archive")
	}
	var done int64
	progress(0, total)
	for _, hash := range order {
		if e = ctx.Err(); e != nil {
			return h, e
		}
		b = make([]byte, int(sizes[hash]))
		if _, e = io.ReadFull(r, b); e != nil {
			return h, e
		}
		if repository.Hash(b) != hash {
			return h, errors.New("tape chunk checksum mismatch")
		}
		if put != nil {
			if e = put(hash, b); e != nil {
				return h, e
			}
		}
		done += int64(len(b))
		progress(done, total)
	}
	want := digest.Sum(nil)
	var got [32]byte
	if _, e = io.ReadFull(f, got[:]); e != nil {
		return h, e
	}
	if string(want) != string(got[:]) {
		return h, errors.New("tape archive digest mismatch")
	}
	if e = ctx.Err(); e != nil {
		return h, e
	}
	h.Copy.Bytes = info.Size()
	h.Copy.Verified = true
	return h, nil
}
func (s *Store) volumeLabel(c Medium) (VolumeLabel, error) {
	var label VolumeLabel
	e := read(filepath.Join(s.volumeDir(c.Barcode), "label.json"), &label)
	if e != nil {
		return label, e
	}
	if label.Barcode != c.Barcode || !idPattern.MatchString(label.VolumeID) || label.Label == "" {
		return label, errors.New("invalid tape label")
	}
	if c.VolumeID != "" && label.VolumeID != c.VolumeID {
		return label, errors.New("volume identity changed")
	}
	return label, nil
}
func (s *Store) scan(ctx context.Context, c Medium, partials bool, progress func(int64, int64)) ([]Copy, int64, *VolumeLabel, error) {
	label, e := s.volumeLabel(c)
	if e != nil {
		return nil, 0, nil, e
	}
	entries, e := os.ReadDir(s.volumeDir(c.Barcode))
	if e != nil {
		return nil, 0, nil, e
	}
	copies := []Copy{}
	var used int64
	known := map[string]Copy{}
	for _, p := range c.Copies {
		known[p.ID] = p
	}
	for _, f := range entries {
		if e = ctx.Err(); e != nil {
			return nil, 0, nil, e
		}
		if f.Name() == "label.json" {
			continue
		}
		if f.Type()&os.ModeSymlink != 0 || !f.Type().IsRegular() {
			return nil, 0, nil, errors.New("unexpected cartridge entry")
		}
		name := strings.TrimSuffix(f.Name(), ".srktape")
		if !idPattern.MatchString(name) || f.Name() != name+".srktape" {
			id := strings.TrimSuffix(f.Name(), ".partial")
			_, ok := known[id]
			if partials && c.Dirty && idPattern.MatchString(id) && f.Name() == id+".partial" && ok {
				continue
			}
			return nil, 0, nil, errors.New("unknown/incomplete tape file; cartridge remains protected")
		}
		h, e := parseArchive(ctx, filepath.Join(s.volumeDir(c.Barcode), f.Name()), nil, progress)
		if e != nil {
			return nil, 0, nil, e
		}
		if h.Copy.ID != name || h.VolumeID != label.VolumeID || h.Barcode != c.Barcode {
			return nil, 0, nil, errors.New("tape file volume mismatch")
		}
		if policy, ok := label.Policies[name]; ok {
			if policy.ID != h.Copy.ID || policy.SnapshotID != h.Copy.SnapshotID || policy.ExpiresAt.Before(h.Copy.ExpiresAt) {
				return nil, 0, nil, errors.New("invalid tape copy policy")
			}
			h.Copy.Hold = policy.Hold
			h.Copy.ExpiresAt = policy.ExpiresAt
		} else if old, ok := known[name]; ok {
			h.Copy.Hold = h.Copy.Hold || old.Hold
			if old.ExpiresAt.After(h.Copy.ExpiresAt) {
				h.Copy.ExpiresAt = old.ExpiresAt
			}
		}
		copies = append(copies, h.Copy)
		used += h.Copy.Bytes
	}
	// Reservations for interrupted files must also retain their original deadline.
	if partials {
		for _, p := range c.Copies {
			found := false
			for _, copy := range copies {
				if copy.ID == p.ID {
					found = true
				}
			}
			if !found {
				copies = append(copies, p)
			}
		}
	}
	return copies, used, &label, nil
}
func (s *Store) execute(ctx context.Context, id string, v model.TapeRequest, r *repository.Repository, report func(model.Result)) (res model.Result, err error) {
	if !idPattern.MatchString(id) {
		return res, errors.New("valid operation ID required")
	}
	if err = ValidateRequest(v); err != nil {
		return res, err
	}
	res = model.Result{Phase: "tape-" + v.Action, ProgressScope: "tape", Tape: &model.TapeResult{Barcode: v.Barcode}}
	progress := func(done, total int64) {
		res.ProgressBytes = done
		res.TotalBytes = total
		p := 0.
		if total > 0 {
			p = min(99., float64(done)*100/float64(total))
		}
		res.ProgressPercent = &p
		report(res)
	}
	if v.Action == "load" || v.Action == "unload" || v.Action == "label" || v.Action == "hold" || v.Action == "release-hold" {
		err = s.Control(ctx, v)
		if err == nil {
			p := 100.
			res.ProgressPercent = &p
			res.Phase = "complete"
			res.Verified = true
			report(res)
		}
		return res, err
	}
	if v.Action == "write" {
		return s.writeArchive(ctx, id, v, r, res, report)
	}
	c, err := s.reserve(v, false, nil)
	if err != nil {
		return res, err
	}
	success := false
	used := c.UsedBytes
	var copies []Copy
	var label *VolumeLabel
	defer func() {
		if e := s.finish(v.Barcode, success, used, copies, label); err == nil {
			err = e
		}
	}()
	switch v.Action {
	case "import", "verify":
		copies, used, label, err = s.scan(ctx, c, false, progress)
		if err != nil {
			return res, err
		}
		success = true
	case "erase", "recycle":
		if !c.Known || c.WriteProtected || c.WORM {
			return res, errors.New("unknown or protected cartridge cannot be erased")
		}
		if c.Erasing {
			copies = append([]Copy(nil), c.Copies...)
			clone := c
			clone.VolumeID = ""
			l, e := s.volumeLabel(clone)
			if e != nil {
				return res, e
			}
			label = &l
			entries, e := os.ReadDir(s.volumeDir(c.Barcode))
			if e != nil {
				return res, e
			}
			known := map[string]bool{}
			for _, p := range copies {
				known[p.ID] = true
			}
			for _, f := range entries {
				if f.Name() == "label.json" {
					continue
				}
				id := strings.TrimSuffix(strings.TrimSuffix(f.Name(), ".srktape"), ".partial")
				if !known[id] || f.Type()&os.ModeSymlink != 0 || !f.Type().IsRegular() || (f.Name() != id+".srktape" && f.Name() != id+".partial") {
					return res, errors.New("unknown data prevents erase recovery")
				}
			}
		} else {
			copies, used, label, err = s.scan(ctx, c, true, progress)
			if err != nil {
				return res, err
			}
		}

		guard := c.Cartridge
		guard.Mounted = false
		guard.Copies = copies
		if err = CanRecycle(guard, s.now()); err != nil {
			return res, err
		}
		// Mark dirty before the first irreversible byte; a failed erase never looks blank.
		s.mu.Lock()
		live, _ := s.medium(c.Barcode)
		live.Dirty = true
		live.Erasing = true
		live.Copies = append([]Copy(nil), copies...)
		err = s.persist()
		s.mu.Unlock()
		if err != nil {
			return res, err
		}
		entries, e := os.ReadDir(s.volumeDir(c.Barcode))
		if e != nil {
			return res, e
		}
		for _, f := range entries {
			if f.Name() == "label.json" {
				continue
			}
			if err = ctx.Err(); err != nil {
				return res, err
			}
			path := filepath.Join(s.volumeDir(c.Barcode), f.Name())
			if v.EraseMode == "long" {
				if err = zeroFile(ctx, path, progress); err != nil {
					return res, err
				}
			}
			if err = os.Remove(path); err != nil {
				return res, err
			}
		}
		label.VolumeID = model.ID()
		label.Policies = nil
		if err = save(filepath.Join(s.volumeDir(c.Barcode), "label.json"), label); err != nil {
			return res, err
		}
		copies = []Copy{}
		used = 0
		success = true
	case "restore":
		label, errValue := s.volumeLabel(c)
		if errValue != nil {
			return res, errValue
		}
		if err = r.BeginLease(model.Lease{ID: id, Subject: "tape", Kind: "write", ExpiresAt: time.Now().Add(12 * time.Hour)}); err != nil {
			return res, err
		}
		defer r.EndLease(id)
		h, e := parseArchive(ctx, filepath.Join(s.volumeDir(c.Barcode), v.CopyID+".srktape"), r.Put, progress)
		if e != nil {
			return res, e
		}
		if h.VolumeID != label.VolumeID || h.Barcode != c.Barcode || h.Copy.ID != v.CopyID {
			return res, errors.New("restored tape identity mismatch")
		}
		m := h.Manifest
		m.ID = id
		m.Base = ""
		m.Mode = "full"
		m.ExpiresAt = s.now().UTC().AddDate(0, 0, v.RetentionDays)
		m.Result.Replica = nil
		m.Result.WALReplication = nil
		m.Result.Tape = nil
		m.Result.Verified = true
		if err = r.CommitLeaseContext(ctx, m, id); err != nil {
			return res, err
		}
		hold := true
		info, e := r.UpdatePolicy(id, model.SnapshotPolicy{Hold: &hold, Reason: "Restored from tape; review before releasing"})
		if e != nil {
			return res, e
		}
		info, e = r.Snapshot(id, true)
		if e != nil {
			return res, e
		}
		res.Tape.CopyID = v.CopyID
		res.Tape.Snapshot = &info
		res.RepositoryID = r.Info.ID
	default:
		return res, errors.New("unsupported streaming tape operation")
	}
	p := 100.
	res.ProgressPercent = &p
	res.Verified = true
	res.Phase = "complete"
	report(res)
	return res, nil
}
func (s *Store) writeArchive(ctx context.Context, id string, v model.TapeRequest, r *repository.Repository, res model.Result, report func(model.Result)) (result model.Result, err error) {
	if err = r.BeginLease(model.Lease{ID: id, Subject: "tape", Kind: "read", SnapshotID: v.SnapshotID, ExpiresAt: time.Now().Add(12 * time.Hour)}); err != nil {
		return res, err
	}
	defer r.EndLease(id)
	m, err := r.Manifest(v.SnapshotID)
	if err != nil {
		return res, err
	}
	copy := Copy{ID: id, SnapshotID: m.ID, ExpiresAt: s.now().UTC().AddDate(0, 0, v.RetentionDays), Hold: v.Hold}
	c, err := s.reserve(v, false, nil)
	if err != nil {
		return res, err
	}
	success := false
	used := c.UsedBytes
	copies := append([]Copy(nil), c.Copies...)
	defer func() {
		if e := s.finish(c.Barcode, success, used, copies, nil); err == nil {
			err = e
		}
	}()
	if !c.Known || c.Dirty || c.WriteProtected || c.WORM || c.Copies == nil {
		return res, errors.New("unlabelled, dirty or protected cartridge")
	}
	label, e := s.volumeLabel(c)
	if e != nil {
		return res, e
	}
	order, sizes, total, e := chunks(m)
	if e != nil {
		return res, e
	}
	h := archiveHeader{Version: 1, VolumeID: label.VolumeID, Barcode: c.Barcode, SourceRepositoryID: r.Info.ID, Copy: copy, Manifest: m}
	b, e := json.Marshal(h)
	if e != nil {
		return res, e
	}
	if int64(len(b)) > model.MaxManifestBytes {
		return res, errors.New("manifest exceeds tape header bound")
	}
	needed := 16 + int64(len(b)) + total + 32
	if c.UsedBytes > c.CapacityBytes-needed {
		return res, errors.New("end of medium: copy does not fit; spanning is not supported")
	}
	s.mu.Lock()
	live, _ := s.medium(c.Barcode)
	live.Dirty = true
	live.Copies = append(live.Copies, copy)
	err = s.persist()
	s.mu.Unlock()
	if err != nil {
		return res, err
	}
	copies = append(copies, copy)
	filename := filepath.Join(s.volumeDir(c.Barcode), id+".srktape")
	part := filepath.Join(s.volumeDir(c.Barcode), id+".partial")
	f, e := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return res, e
	}
	defer f.Close()
	hash := sha256.New()
	w := io.MultiWriter(f, hash)
	var prefix [16]byte
	copyPrefix := []byte(archiveMagic)
	copyBytes(prefix[:8], copyPrefix)
	binary.BigEndian.PutUint64(prefix[8:], uint64(len(b)))
	if _, e = w.Write(prefix[:]); e != nil {
		return res, e
	}
	if _, e = w.Write(b); e != nil {
		return res, e
	}
	res.TotalBytes = total
	res.Tape.CopyID = id
	for _, key := range order {
		if err = ctx.Err(); err != nil {
			return res, err
		}
		data, e := r.Get(key)
		if e != nil {
			return res, e
		}
		if int64(len(data)) != sizes[key] {
			return res, errors.New("source chunk length mismatch")
		}
		if _, e = w.Write(data); e != nil {
			return res, e
		}
		res.ProgressBytes += int64(len(data))
		res.UploadedBytes += int64(len(data))
		res.LogicalBytes += int64(len(data))
		p := min(99., float64(res.ProgressBytes)*100/float64(max(1, total)))
		res.ProgressPercent = &p
		report(res)
	}
	if _, e = f.Write(hash.Sum(nil)); e != nil {
		return res, e
	}
	if e = f.Sync(); e != nil {
		return res, e
	}
	if e = f.Close(); e != nil {
		return res, e
	}
	if e = os.Rename(part, filename); e != nil {
		return res, e
	}
	dir, e := os.Open(s.volumeDir(c.Barcode))
	if e != nil {
		return res, e
	}
	e = dir.Sync()
	dir.Close()
	if e != nil {
		return res, e
	}
	res.Phase = "tape-readback"
	report(res)
	verified, e := parseArchive(ctx, filename, nil, func(int64, int64) {})
	if e != nil {
		return res, e
	}
	copies[len(copies)-1] = verified.Copy
	used += needed
	success = true
	p := 100.
	res.ProgressPercent = &p
	res.Verified = true
	res.Phase = "complete"
	report(res)
	return res, nil
}
func copyBytes(dst, src []byte) { copy(dst, src) }
func zeroFile(ctx context.Context, path string, progress func(int64, int64)) error {
	f, e := os.OpenFile(path, os.O_WRONLY, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return e
	}
	buf := make([]byte, 1<<20)
	var done int64
	for done < info.Size() {
		if e = ctx.Err(); e != nil {
			return e
		}
		n, e := f.Write(buf[:min(int64(len(buf)), info.Size()-done)])
		if e != nil {
			return e
		}
		done += int64(n)
		progress(done, info.Size())
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	return f.Sync()
}
