// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package tape

import (
	"context"
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"io"
	"sort"
	"sync"
	"time"
)

// Queue entries are snapshots. The manager's persistent run catalog owns the
// durable queue; an interrupted physical command is never automatically replayed.
type QueueEntry struct {
	ID       string       `json:"id"`
	Barcode  string       `json:"barcode"`
	Drive    string       `json:"drive"`
	Action   string       `json:"action"`
	State    string       `json:"state"`
	QueuedAt time.Time    `json:"queued_at"`
	Result   model.Result `json:"result"`
}
type driveQueue struct {
	mu      sync.Mutex
	entries []*QueueEntry
	changed chan struct{}
}

func (q *driveQueue) signal() {
	if q.changed != nil {
		close(q.changed)
	}
	q.changed = make(chan struct{})
}
func (q *driveQueue) snapshot() []QueueEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]QueueEntry, 0, len(q.entries))
	for _, e := range q.entries {
		out = append(out, *e)
	}
	return out
}
func (q *driveQueue) acquire(ctx context.Context, e *QueueEntry, report func(model.Result)) (func(), error) {
	q.mu.Lock()
	for _, old := range q.entries {
		if old.ID == e.ID {
			q.mu.Unlock()
			return nil, errors.New("operation already queued or running")
		}
	}
	if len(q.entries) >= 1024 {
		q.mu.Unlock()
		return nil, errors.New("tape queue is full")
	}
	e.State = "queued"
	e.QueuedAt = time.Now().UTC()
	q.entries = append(q.entries, e)
	q.signal()
	q.mu.Unlock()
	remove := func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		for i, v := range q.entries {
			if v == e {
				q.entries = append(q.entries[:i], q.entries[i+1:]...)
				break
			}
		}
		q.signal()
	}
	report(model.Result{Phase: "tape-queued", Tape: &model.TapeResult{Barcode: e.Barcode, DriveID: e.Drive}})
	for {
		q.mu.Lock()
		ready := true
		for _, prior := range q.entries {
			if prior == e {
				break
			}
			if prior.Drive == e.Drive || prior.Barcode == e.Barcode {
				ready = false
				break
			}
		}
		if ctx.Err() != nil {
			q.mu.Unlock()
			remove()
			return nil, ctx.Err()
		}
		if ready {
			e.State = "running"
			q.mu.Unlock()
			return remove, nil
		}
		ch := q.changed
		q.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			remove()
			return nil, ctx.Err()
		}
	}
}
func (s *SCSIStore) files(barcode string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.index.Files[barcode]
	if !ok {
		return nil
	}
	return append([]string{}, v...)
}
func (s *SCSIStore) setFiles(barcode string, files []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.index.Files[barcode] = files
	return s.persist()
}
func (s *SCSIStore) pendingPositions() []PositionJournal {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := make([]PositionJournal, 0, len(s.index.PendingByDrive))
	for _, p := range s.index.PendingByDrive {
		v = append(v, p)
	}
	sort.Slice(v, func(i, j int) bool { return v[i].Drive < v[j].Drive })
	return v
}
func (s *SCSIStore) pendingFor(barcode, drive string) *PositionJournal {
	for _, p := range s.pendingPositions() {
		if p.Barcode == barcode || (drive != "" && p.Drive == drive) {
			return &p
		}
	}
	return nil
}
func (s *SCSIStore) robotPending() bool {
	for _, p := range s.pendingPositions() {
		if p.Action == "load" || p.Action == "unload" {
			return true
		}
	}
	return false
}
func (s *SCSIStore) clearPending(j PositionJournal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.index.PendingByDrive, j.Drive)
	if e := s.persist(); e != nil {
		s.index.PendingByDrive[j.Drive] = j
		return e
	}
	return nil
}
func (s *SCSIStore) Execute(ctx context.Context, id string, v model.TapeRequest, r *repository.Repository, report func(model.Result)) (model.Result, error) {
	if !idPattern.MatchString(id) {
		return model.Result{}, errors.New("invalid operation ID")
	}
	if e := ValidateRequest(v); e != nil {
		return model.Result{}, e
	}
	drive := v.DriveID
	// Resolve the reservation from physical inventory, never a client device path.
	s.robot.Lock()
	elements, err := s.elements(ctx)
	s.robot.Unlock()
	if err != nil {
		return model.Result{}, err
	}
	for _, el := range elements {
		if el.Full && el.Barcode == v.Barcode {
			for name, p := range s.cfg.SCSI.Drives {
				if p.Element == el.Address {
					if drive != "" && drive != name {
						return model.Result{}, errors.New("requested drive differs from loaded cartridge")
					}
					drive = name
				}
			}
		}
	}
	if v.Action == "recover" {
		if p := s.pendingFor(v.Barcode, ""); p != nil {
			drive = p.Drive
		}
	}
	if _, ok := s.cfg.SCSI.Drives[drive]; !ok {
		return model.Result{}, errors.New("configured drive required; load the cartridge first")
	}
	entry := &QueueEntry{ID: id, Barcode: v.Barcode, Drive: drive, Action: v.Action}
	update := func(res model.Result) {
		if res.Tape == nil {
			res.Tape = &model.TapeResult{Barcode: v.Barcode}
		} else {
			cp := *res.Tape
			res.Tape = &cp
		}
		res.Tape.DriveID = drive
		s.queue.mu.Lock()
		entry.Result = res
		s.queue.mu.Unlock()
		if report != nil {
			report(res)
		}
	}
	release, err := s.queue.acquire(ctx, entry, update)
	if err != nil {
		return model.Result{}, err
	}
	defer release()
	update(model.Result{Phase: "tape-dispatched"})
	res, err := s.execute(ctx, id, v, r, update, drive)
	if res.Tape != nil {
		cp := *res.Tape
		res.Tape = &cp
		res.Tape.DriveID = drive
	}
	return res, err
}

// Per-phase progress stops below 100 until filemark and integrity are confirmed.
func tapeByteProgress(barcode, phase string, total int64, report func(model.Result)) func(int64) {
	var last time.Time
	return func(n int64) {
		if report == nil || (time.Since(last) < 250*time.Millisecond && n < total) {
			return
		}
		last = time.Now()
		res := model.Result{Phase: phase, ProgressScope: phase, ProgressBytes: n, TotalBytes: total, Tape: &model.TapeResult{Barcode: barcode}}
		if total > 0 {
			p := float64(n) * 100 / float64(total)
			if p > 99 {
				p = 99
			}
			res.ProgressPercent = &p
		}
		report(res)
	}
}

type tapeProgressWriter struct {
	io.Writer
	n      int64
	report func(int64)
}

func (w *tapeProgressWriter) Write(b []byte) (int, error) {
	n, e := w.Writer.Write(b)
	w.n += int64(n)
	if w.report != nil {
		w.report(w.n)
	}
	return n, e
}

// tryAcquire reserves a free workflow resource without waiting behind a manual
// operation that arrived during inventory. The caller can rescan other drives.
func (q *driveQueue) tryAcquire(e *QueueEntry) (func(), bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, old := range q.entries {
		if old.ID == e.ID || old.Drive == e.Drive || old.Barcode == e.Barcode {
			return nil, false
		}
	}
	e.State = "running"
	e.QueuedAt = time.Now().UTC()
	q.entries = append(q.entries, e)
	q.signal()
	return func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		for i, old := range q.entries {
			if old == e {
				q.entries = append(q.entries[:i], q.entries[i+1:]...)
				break
			}
		}
		q.signal()
	}, true
}
