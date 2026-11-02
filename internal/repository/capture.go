// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"gdata/srk/internal/model"
)

// Each sequential 80-byte acknowledgement is fsynced after its chunk. A torn tail
// is never silently accepted; complete records bind index, length and SHA-256.
const captureRecordSize = 80

type captureState struct {
	mu                      sync.Mutex
	ticket                  model.CaptureTicket
	phase                   string
	next, logical, uploaded int64
	updated                 time.Time
	failure                 string
	worker                  bool
}
type CaptureStore struct {
	repo   *Repository
	mu     sync.Mutex
	states map[string]*captureState
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed bool
}

func NewCaptureStore(r *Repository) *CaptureStore {
	ctx, cancel := context.WithCancel(context.Background())
	return &CaptureStore{repo: r, ctx: ctx, cancel: cancel, states: map[string]*captureState{}}
}
func (s *CaptureStore) Close()               { s.mu.Lock(); s.closed = true; s.cancel(); s.mu.Unlock(); s.wg.Wait() }
func (s *CaptureStore) dir(id string) string { return filepath.Join(s.repo.Root, "captures", id) }
func validateTicket(t model.CaptureTicket) error {
	if !idPattern.MatchString(t.ID) || !idPattern.MatchString(t.Job.ID) || !idPattern.MatchString(t.Capture.ScopeID) || t.Job.Type != "disk-image" || t.Job.Mode != "full" || t.Job.RetentionDays < 1 || t.Job.RetentionDays > 36500 || t.CreatedAt.IsZero() || t.Capture.ExpiresAt.IsZero() {
		return errors.New("invalid capture ticket")
	}
	return t.Capture.Disk.Validate()
}
func (s *CaptureStore) Create(t model.CaptureTicket) error {
	if e := validateTicket(t); e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("capture service closed")
	}
	file := filepath.Join(s.dir(t.ID), "ticket.json")
	var old model.CaptureTicket
	if e := readEnvelope(file, &old); e == nil {
		if !reflect.DeepEqual(old, t) {
			return errors.New("capture ticket conflict")
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	if e := s.repo.BeginLease(model.Lease{ID: t.ID, Subject: t.Capture.ScopeID, Kind: "write", ExpiresAt: t.Capture.ExpiresAt}); e != nil {
		return e
	}
	if e := writeEnvelope(file, t); e != nil {
		return e
	}
	state := &captureState{ticket: t, phase: "uploading", updated: time.Now().UTC()}
	s.states[t.ID] = state
	return nil
}
func (s *CaptureStore) find(id string) (*captureState, error) {
	if !idPattern.MatchString(id) {
		return nil, errors.New("invalid capture ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("capture service closed")
	}
	if c := s.states[id]; c != nil {
		return c, nil
	}
	c := &captureState{phase: "uploading"}
	if e := readEnvelope(filepath.Join(s.dir(id), "ticket.json"), &c.ticket); e != nil {
		return nil, e
	}
	if e := validateTicket(c.ticket); e != nil {
		return nil, e
	}
	if c.ticket.ID != id {
		return nil, errors.New("capture ticket identity mismatch")
	}
	var phase struct {
		Phase string `json:"phase"`
		Error string `json:"error"`
	}
	if e := readEnvelope(filepath.Join(s.dir(id), "phase.json"), &phase); e == nil {
		c.phase = phase.Phase
		c.failure = phase.Error
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(s.dir(id), "blocks.journal"), os.O_RDWR, 0600)
	if e == nil {
		defer f.Close()
		st, e := f.Stat()
		if e != nil {
			return nil, e
		}
		c.updated = st.ModTime().UTC()
		// A crashed append may leave a partial record; no acknowledgement was issued
		// until fsync, so discard only that incomplete tail and require retransmission.
		complete := st.Size() / captureRecordSize * captureRecordSize
		if st.Size() != complete {
			if e = f.Truncate(complete); e != nil {
				return nil, e
			}
			if e = f.Sync(); e != nil {
				return nil, e
			}
		}
		for {
			rec := make([]byte, captureRecordSize)
			_, e := io.ReadFull(f, rec)
			if e == io.EOF {
				break
			}
			if e != nil {
				return nil, e
			}
			index, size, uploaded, _, e := decodeCaptureRecord(rec)
			if e != nil || index != c.next || size != s.blockBytes(c.ticket, index) {
				return nil, errors.New("capture journal integrity error")
			}
			c.next++
			c.logical += size
			c.uploaded += uploaded
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	} else {
		c.updated = time.Now().UTC()
	}
	// Commit is atomic. Recover an acknowledged snapshot even if the process died
	// before saving the phase receipt; absence of a manifest requires commit retry.
	if m, e := s.repo.Manifest(id); e == nil {
		expected, e := s.manifest(c)
		if e != nil {
			return nil, e
		}
		if m.JobID != expected.JobID || m.Source != expected.Source || !reflect.DeepEqual(m.Disk, expected.Disk) || !reflect.DeepEqual(m.Files, expected.Files) || !m.Result.Verified {
			return nil, errors.New("capture snapshot conflicts with ticket or journal")
		}
		c.phase = "complete"
		c.failure = ""
	} else if !os.IsNotExist(e) {
		return nil, e
	} else if c.phase == "verifying" {
		c.phase = "uploading"
	} else if c.phase == "complete" {
		return nil, errors.New("capture receipt has no snapshot")
	}
	s.states[id] = c
	return c, nil
}
func (s *CaptureStore) blockBytes(t model.CaptureTicket, index int64) int64 {
	if index < 0 || index > (t.Capture.Disk.Bytes-1)/ChunkSize {
		return 0
	}
	return min(int64(ChunkSize), t.Capture.Disk.Bytes-index*ChunkSize)
}
func decodeCaptureRecord(b []byte) (index, size, uploaded int64, hash string, err error) {
	if len(b) != captureRecordSize || Hash(b[:48]) != hex.EncodeToString(b[48:]) {
		err = errors.New("bad capture record checksum")
		return
	}
	index = int64(binary.BigEndian.Uint64(b[:8]))
	size = int64(binary.BigEndian.Uint32(b[8:12]))
	uploaded = int64(binary.BigEndian.Uint32(b[12:16]))
	hash = hex.EncodeToString(b[16:48])
	if uploaded != 0 && uploaded != size {
		err = errors.New("bad upload count")
	}
	return
}
func (s *CaptureStore) status(c *captureState) model.CaptureStatus {
	percent := float64(c.logical) * 100 / float64(c.ticket.Capture.Disk.Bytes)
	percent = min(percent, 99)
	verified := c.phase == "complete"
	if verified {
		percent = 100
	}
	disk := c.ticket.Capture.Disk
	result := model.Result{Disk: &disk, TotalBytes: disk.Bytes, ProgressBytes: c.logical, TotalFiles: 1, Files: 1, ChangedFiles: 1, ProgressPercent: &percent, ProgressScope: "data-transfer", Phase: "disk-upload", LogicalBytes: c.logical, UploadedBytes: c.uploaded, Chunks: int(c.next), Consistency: "offline-disk-image", EffectiveMode: "full", Verified: verified, RepositoryID: s.repo.Info.ID}
	if c.phase == "verifying" {
		result.Phase = "disk-verification"
		result.ProgressScope = "validation"
		result.ProgressPercent = nil
	}
	if verified {
		result.Phase = "complete"
	}
	return model.CaptureStatus{ID: c.ticket.ID, Phase: c.phase, NextIndex: c.next, Result: result, UpdatedAt: c.updated, Error: c.failure}
}
func (s *CaptureStore) Status(id string) (model.CaptureStatus, error) {
	c, e := s.find(id)
	if e != nil {
		return model.CaptureStatus{}, e
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return s.status(c), nil
}
func (s *CaptureStore) Ticket(id string) (model.CaptureTicket, error) {
	c, e := s.find(id)
	if e != nil {
		return model.CaptureTicket{}, e
	}
	return c.ticket, nil
}
func (s *CaptureStore) Block(id string, index int64, hash string, data []byte) (bool, model.CaptureStatus, error) {
	c, e := s.find(id)
	if e != nil {
		return false, model.CaptureStatus{}, e
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fail := func(err error) (bool, model.CaptureStatus, error) { return false, s.status(c), err }
	expected := s.blockBytes(c.ticket, index)
	if expected == 0 || !hashPattern.MatchString(hash) {
		return fail(errors.New("invalid block index or hash"))
	}
	if index < c.next {
		f, e := os.Open(filepath.Join(s.dir(id), "blocks.journal"))
		if e != nil {
			return fail(e)
		}
		record := make([]byte, captureRecordSize)
		_, e = f.ReadAt(record, index*captureRecordSize)
		f.Close()
		if e != nil {
			return fail(e)
		}
		_, size, _, prior, e := decodeCaptureRecord(record)
		if e != nil || size != expected || prior != hash {
			return fail(errors.New("block retry conflicts with acknowledged disk contents"))
		}
		return true, s.status(c), nil
	}
	if index != c.next || c.phase != "uploading" {
		return fail(errors.New("capture is not accepting this block"))
	}
	if e = s.repo.BeginLease(model.Lease{ID: id, Subject: c.ticket.Capture.ScopeID, Kind: "write", ExpiresAt: c.ticket.Capture.ExpiresAt}); e != nil {
		return fail(e)
	}
	uploaded := int64(0)
	if data == nil {
		b, e := s.repo.Get(hash)
		if os.IsNotExist(e) {
			return false, s.status(c), nil
		}
		if e != nil {
			return fail(e)
		}
		if int64(len(b)) != expected {
			return fail(errors.New("stored block length mismatch"))
		}
	} else {
		if int64(len(data)) != expected || Hash(data) != hash {
			return fail(errors.New("disk block checksum or size mismatch"))
		}
		if e = s.repo.Put(hash, data); e != nil {
			return fail(e)
		}
		uploaded = expected
	}
	record := make([]byte, captureRecordSize)
	binary.BigEndian.PutUint64(record[:8], uint64(index))
	binary.BigEndian.PutUint32(record[8:12], uint32(expected))
	binary.BigEndian.PutUint32(record[12:16], uint32(uploaded))
	h, _ := hex.DecodeString(hash)
	copy(record[16:48], h)
	checksum, _ := hex.DecodeString(Hash(record[:48]))
	copy(record[48:], checksum)
	filename := filepath.Join(s.dir(id), "blocks.journal")
	f, e := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return fail(e)
	}
	n, e := f.Write(record)
	if e == nil && n != len(record) {
		e = io.ErrShortWrite
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e == nil {
		e = syncDirectory(s.dir(id))
	}
	if e != nil {
		c.phase = "error"
		c.failure = "journal write failed; restart media before retry"
		return fail(e)
	}
	c.next++
	c.logical += expected
	c.uploaded += uploaded
	c.updated = time.Now().UTC()
	return true, s.status(c), nil
}
func (s *CaptureStore) manifest(c *captureState) (model.Manifest, error) {
	if c.logical != c.ticket.Capture.Disk.Bytes {
		return model.Manifest{}, errors.New("disk upload is incomplete")
	}
	f, e := os.Open(filepath.Join(s.dir(c.ticket.ID), "blocks.journal"))
	if e != nil {
		return model.Manifest{}, e
	}
	defer f.Close()
	hashes := make([]string, 0, c.next)
	for index := int64(0); index < c.next; index++ {
		b := make([]byte, captureRecordSize)
		if _, e = io.ReadFull(f, b); e != nil {
			return model.Manifest{}, e
		}
		got, size, _, hash, e := decodeCaptureRecord(b)
		if e != nil || got != index || size != s.blockBytes(c.ticket, index) {
			return model.Manifest{}, errors.New("capture journal mismatch")
		}
		hashes = append(hashes, hash)
	}
	t := c.ticket
	disk := t.Capture.Disk
	result := s.status(c).Result
	result.Verified = true
	result.Phase = "complete"
	percent := 100.0
	result.ProgressPercent = &percent
	return model.Manifest{Version: 1, ID: t.ID, JobID: t.Job.ID, Source: t.Job.Source, CreatedAt: t.CreatedAt, ExpiresAt: t.CreatedAt.AddDate(0, 0, t.Job.RetentionDays), Mode: "full", Consistency: "offline-disk-image", Disk: &disk, Files: []model.File{{Path: "disk.img", Kind: "file", Mode: 0600, Size: disk.Bytes, ModTime: t.CreatedAt, Chunks: hashes}}, Result: result}, nil
}
func (s *CaptureStore) setPhase(c *captureState, phase, failure string) error {
	if e := writeEnvelope(filepath.Join(s.dir(c.ticket.ID), "phase.json"), struct {
		Phase string `json:"phase"`
		Error string `json:"error"`
	}{phase, failure}); e != nil {
		return e
	}
	c.phase = phase
	c.failure = failure
	c.updated = time.Now().UTC()
	return nil
}
func (s *CaptureStore) Commit(id string) (model.CaptureStatus, error) {
	c, e := s.find(id)
	if e != nil {
		return model.CaptureStatus{}, e
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.phase == "complete" || c.worker {
		return s.status(c), nil
	}
	if c.phase != "uploading" {
		return s.status(c), errors.New("capture cannot be committed")
	}
	manifest, e := s.manifest(c)
	if e != nil {
		return s.status(c), e
	}
	if e = s.setPhase(c, "verifying", ""); e != nil {
		return s.status(c), e
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return s.status(c), errors.New("capture store closed")
	}
	s.wg.Add(1)
	c.worker = true
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithDeadline(s.ctx, c.ticket.Capture.ExpiresAt)
		defer cancel()
		err := s.repo.CommitLeaseContext(ctx, manifest, id)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.worker = false
		phase, failure := "complete", ""
		if err != nil {
			phase = "error"
			failure = fmt.Sprintf("disk verification failed: %v", err)
		}
		if e := s.setPhase(c, phase, failure); e != nil {
			c.phase = "error"
			c.failure = "cannot persist capture receipt; check media before retry"
		}
	}()
	return s.status(c), nil
}
