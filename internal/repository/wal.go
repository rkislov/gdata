// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gdata/srk/internal/model"
)

var walSegment = regexp.MustCompile(`^[0-9A-F]{24}$`)
var walHistory = regexp.MustCompile(`^([0-9A-F]{8}\.history|[0-9A-F]{24}\.[0-9A-F]{8}\.backup)$`)
var walProfile = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func ValidWALArchiveID(id string) bool { return hashPattern.MatchString(id) }
func WALName(name string) bool         { return walSegment.MatchString(name) || walHistory.MatchString(name) }
func WALArchiveID(agent, profile, systemID string) string {
	return Hash([]byte(agent + "\x00" + profile + "\x00" + systemID))
}
func WALSegmentSize(size int64) bool { return size >= 1<<20 && size <= 1<<30 && size&(size-1) == 0 }
func WALPosition(name string, size int64) (uint64, error) {
	if !walSegment.MatchString(name) || !WALSegmentSize(size) {
		return 0, errors.New("invalid WAL segment name or size")
	}
	log, _ := strconv.ParseUint(name[8:16], 16, 32)
	seg, _ := strconv.ParseUint(name[16:], 16, 32)
	if seg >= 1<<32/uint64(size) {
		return 0, errors.New("WAL segment number exceeds segment size geometry")
	}
	return log<<32 | seg*uint64(size), nil
}
func ValidateWALRecord(v model.WALRecord) error {
	if (v.FirstCommitAt == nil) != (v.LastCommitAt == nil) || (!v.TimeBoundsVerified && (v.FirstCommitAt != nil || v.LastCommitAt != nil)) || (v.FirstCommitAt != nil && v.LastCommitAt.Before(*v.FirstCommitAt)) {
		return errors.New("invalid WAL transaction time evidence")
	}
	id, e := strconv.ParseUint(v.SystemID, 10, 64)
	if e != nil || id == 0 || !walProfile.MatchString(v.Profile) || v.AgentID == "" || v.Major < 13 || v.Major > 18 || !WALSegmentSize(v.SegmentSize) || !WALName(v.Name) || v.ArchiveID != WALArchiveID(v.AgentID, v.Profile, v.SystemID) {
		return errors.New("invalid WAL identity")
	}
	return nil
}
func validateWALFile(f *os.File, v model.WALRecord) error {
	info, e := f.Stat()
	if e != nil {
		return e
	}
	if !info.Mode().IsRegular() {
		return errors.New("WAL must be a regular file")
	}
	if !walSegment.MatchString(v.Name) {
		if info.Size() < 1 || info.Size() > 1<<20 {
			return errors.New("invalid WAL history length")
		}
		return nil
	}
	if info.Size() != v.SegmentSize {
		return errors.New("incomplete WAL segment")
	}
	head := make([]byte, 40)
	if _, e = f.ReadAt(head, 0); e != nil {
		return e
	}
	var order binary.ByteOrder = binary.LittleEndian
	if order.Uint16(head[2:4])&2 == 0 {
		order = binary.BigEndian
	}
	position, e := WALPosition(v.Name, v.SegmentSize)
	if e != nil {
		return e
	}
	system, _ := strconv.ParseUint(v.SystemID, 10, 64)
	timeline, _ := strconv.ParseUint(v.Name[:8], 16, 32)
	if order.Uint16(head[2:4])&2 == 0 || order.Uint64(head[24:32]) != system || int64(order.Uint32(head[32:36])) != v.SegmentSize || order.Uint64(head[8:16]) != position || order.Uint32(head[4:8]) == 0 || uint64(order.Uint32(head[4:8])) > timeline {
		return errors.New("WAL header does not match its cluster, size or position")
	}
	return nil
}
func (r *Repository) PutWAL(v model.WALRecord, src io.Reader) (model.WALRecord, error) {
	return r.putWAL(v, src, false)
}

// Replica metadata is published only after matching the producer's exact bytes.
func (r *Repository) PutReplicatedWAL(v model.WALRecord, src io.Reader) (model.WALRecord, error) {
	if !hashPattern.MatchString(v.SHA256) || v.Bytes < 1 || v.ArchivedAt.IsZero() {
		return v, errors.New("verified producer WAL metadata required")
	}
	return r.putWAL(v, src, true)
}
func (r *Repository) putWAL(v model.WALRecord, src io.Reader, replica bool) (model.WALRecord, error) {
	expectedSHA, expectedBytes, archivedAt := v.SHA256, v.Bytes, v.ArchivedAt
	r.walMu.Lock()
	defer r.walMu.Unlock()
	if e := r.Check(); e != nil {
		return v, e
	}
	if e := ValidateWALRecord(v); e != nil {
		return v, e
	}
	dir := filepath.Join(r.Root, "wal", v.ArchiveID)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return v, e
	}
	limit := v.SegmentSize
	if !walSegment.MatchString(v.Name) {
		limit = 1 << 20
	}
	var stat syscall.Statfs_t
	if e := syscall.Statfs(dir, &stat); e != nil {
		return v, e
	}
	if uint64(stat.Bavail)*uint64(stat.Bsize) < uint64(limit)+uint64(r.ReserveBytes) {
		return v, errors.New("WAL archive free space reserve reached")
	}
	f, e := os.CreateTemp(dir, ".write-*")
	if e != nil {
		return v, e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	hash := sha256.New()
	v.Bytes, e = io.Copy(io.MultiWriter(f, hash), io.LimitReader(src, limit+1))
	if e != nil {
		return v, e
	}
	if v.Bytes > limit {
		return v, errors.New("WAL exceeds expected size")
	}
	if e = validateWALFile(f, v); e != nil {
		return v, e
	}
	v.SHA256 = hex.EncodeToString(hash.Sum(nil))
	if replica && (v.SHA256 != expectedSHA || v.Bytes != expectedBytes) {
		return v, errors.New("replicated WAL SHA-256 or length mismatch")
	}
	if replica {
		v.ArchivedAt = archivedAt
	} else {
		v.ArchivedAt = time.Now().UTC()
	}
	name := filepath.Join(dir, v.Name)
	if old, err := os.Open(name); err == nil {
		defer old.Close()
		h := sha256.New()
		if _, e = io.Copy(h, old); e != nil {
			return v, e
		}
		if hex.EncodeToString(h.Sum(nil)) != v.SHA256 {
			return v, errors.New("WAL filename collision with different contents")
		}
	} else if !os.IsNotExist(err) {
		return v, err
	}
	var existing model.WALRecord
	if e = readEnvelope(name+".json", &existing); e == nil {
		if existing.SHA256 != v.SHA256 || existing.ArchiveID != v.ArchiveID || existing.Name != v.Name {
			return v, errors.New("WAL metadata collision")
		}
		return existing, nil
	} else if !os.IsNotExist(e) {
		return v, e
	}
	if e = f.Sync(); e != nil {
		return v, e
	}
	if e = f.Close(); e != nil {
		return v, e
	}
	if e = os.Rename(f.Name(), name); e != nil {
		return v, e
	}
	d, e := os.Open(dir)
	if e != nil {
		return v, e
	}
	e = d.Sync()
	d.Close()
	if e != nil {
		return v, e
	}
	if e = writeEnvelope(name+".json", v); e != nil {
		return v, e
	}
	return v, nil
}
func (r *Repository) WALInventory(archive string) ([]model.WALRecord, error) {
	r.walMu.RLock()
	defer r.walMu.RUnlock()
	return r.walInventory(archive)
}
func (r *Repository) walInventory(archive string) ([]model.WALRecord, error) {
	if !hashPattern.MatchString(archive) {
		return nil, errors.New("invalid archive ID")
	}
	dir := filepath.Join(r.Root, "wal", archive)
	entries, e := os.ReadDir(dir)
	if os.IsNotExist(e) {
		return []model.WALRecord{}, nil
	}
	if e != nil {
		return nil, e
	}
	out := []model.WALRecord{}
	for _, f := range entries {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		var v model.WALRecord
		if e = readEnvelope(filepath.Join(dir, f.Name()), &v); e != nil {
			return nil, e
		}
		if e = ValidateWALRecord(v); e != nil {
			return nil, e
		}
		if v.ArchiveID != archive || f.Name() != v.Name+".json" {
			return nil, errors.New("WAL record identity mismatch")
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func (r *Repository) OpenWAL(archive, name string) (*os.File, model.WALRecord, error) {
	r.walMu.RLock()
	defer r.walMu.RUnlock()
	var v model.WALRecord
	if !hashPattern.MatchString(archive) || !WALName(name) {
		return nil, v, errors.New("invalid WAL path")
	}
	filename := filepath.Join(r.Root, "wal", archive, name)
	if e := readEnvelope(filename+".json", &v); e != nil {
		return nil, v, e
	}
	if e := ValidateWALRecord(v); e != nil {
		return nil, v, e
	}
	if v.ArchiveID != archive || v.Name != name {
		return nil, v, errors.New("WAL identity mismatch")
	}
	f, e := os.Open(filename)
	if e != nil {
		return nil, v, e
	}
	h := sha256.New()
	n, e := io.Copy(h, f)
	if e != nil || n != v.Bytes || hex.EncodeToString(h.Sum(nil)) != v.SHA256 {
		f.Close()
		return nil, v, errors.New("archived WAL checksum mismatch")
	}
	if _, e = f.Seek(0, 0); e != nil {
		f.Close()
		return nil, v, e
	}
	return f, v, nil
}

// WALContinuity checks the base timeline; it never guesses a recovery window from upload timestamps.
func WALContinuity(base *model.PostgreSQLBackup, names []string) error {
	if base == nil || !WALSegmentSize(base.SegmentSize) || len(base.WALRanges) != 1 {
		return errors.New("PITR requires a single-timeline base with WAL geometry")
	}
	timeline := fmt.Sprintf("%08X", base.WALRanges[0].Timeline)
	parts := strings.Split(base.WALRanges[0].StartLSN, "/")
	if len(parts) != 2 {
		return errors.New("invalid base LSN")
	}
	hi, e := strconv.ParseUint(parts[0], 16, 32)
	if e != nil {
		return e
	}
	lo, e := strconv.ParseUint(parts[1], 16, 32)
	if e != nil {
		return e
	}
	start := (hi<<32 | lo) / uint64(base.SegmentSize) * uint64(base.SegmentSize)
	positions := map[uint64]bool{}
	last := start
	for _, name := range names {
		if strings.HasSuffix(name, ".history") {
			t, _ := strconv.ParseUint(name[:8], 16, 32)
			if t > uint64(base.WALRanges[0].Timeline) {
				return errors.New("timeline switch requires a new base backup")
			}
		}
		if !walSegment.MatchString(name) || name[:8] != timeline {
			continue
		}
		p, e := WALPosition(name, base.SegmentSize)
		if e != nil {
			return e
		}
		if p >= start {
			positions[p] = true
			if p > last {
				last = p
			}
		}
	}
	if (last-start)/uint64(base.SegmentSize) > 1000000 {
		return errors.New("WAL range is too large")
	}
	for p := start; p <= last; {
		if !positions[p] {
			return fmt.Errorf("missing WAL segment at %X/%08X", p>>32, p&0xffffffff)
		}
		if p == last {
			break
		}
		p += uint64(base.SegmentSize)
	}
	return nil
}
func WALBasePosition(base *model.PostgreSQLBackup) (uint64, error) {
	if base == nil || len(base.WALRanges) != 1 || !WALSegmentSize(base.SegmentSize) {
		return 0, errors.New("invalid base WAL geometry")
	}
	parts := strings.Split(base.WALRanges[0].StartLSN, "/")
	if len(parts) != 2 {
		return 0, errors.New("invalid base LSN")
	}
	hi, e := strconv.ParseUint(parts[0], 16, 32)
	if e != nil {
		return 0, e
	}
	lo, e := strconv.ParseUint(parts[1], 16, 32)
	if e != nil {
		return 0, e
	}
	return (hi<<32 | lo) / uint64(base.SegmentSize) * uint64(base.SegmentSize), nil
}
