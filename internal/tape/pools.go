// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package tape

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Pool struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Barcodes []string `json:"barcodes"`
	Drives   []string `json:"drives"`
}
type PoolController interface {
	SetPool(context.Context, Pool) error
}
type Managed struct {
	backend   Backend
	root      string
	mu        sync.Mutex
	pools     map[string]Pool
	queue     driveQueue
	selecting sync.Mutex
	waiting   map[string]QueueEntry
}

func manage(root string, b Backend) (*Managed, error) {
	m := &Managed{backend: b, root: root, pools: map[string]Pool{}, waiting: map[string]QueueEntry{}}
	e := read(filepath.Join(root, "pools.json"), &m.pools)
	if e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	if m.pools == nil {
		return nil, errors.New("invalid tape pools")
	}
	return m, nil
}
func (m *Managed) Inventory(ctx context.Context) (Library, error) {
	v, e := m.backend.Inventory(ctx)
	if e != nil {
		return v, e
	}
	v.Queue = nil
	m.mu.Lock()
	for _, p := range m.pools {
		v.Pools = append(v.Pools, p)
	}
	for _, w := range m.waiting {
		v.Queue = append(v.Queue, w)
	}
	m.mu.Unlock()
	sort.Slice(v.Pools, func(i, j int) bool { return v.Pools[i].ID < v.Pools[j].ID })
	// Outer queue owns whole workflows, while the backend journals individual steps.
	v.Queue = append(v.Queue, m.queue.snapshot()...)
	return v, nil
}
func (m *Managed) SetPool(ctx context.Context, p Pool) error {
	if !barcodePattern.MatchString(p.ID) || p.Name == "" || len(p.Name) > 128 || len(p.Barcodes) == 0 || len(p.Drives) == 0 {
		return errors.New("pool ID, name, cartridges and drives required")
	}
	inv, e := m.backend.Inventory(ctx)
	if e != nil {
		return e
	}
	cs, ds := map[string]bool{}, map[string]bool{}
	for _, c := range inv.Cartridges {
		cs[c.Barcode] = true
	}
	for _, d := range inv.Drives {
		ds[d.ID] = true
	}
	for _, b := range p.Barcodes {
		if !cs[b] {
			return errors.New("pool contains unknown or repeated cartridge")
		}
		delete(cs, b)
	}
	for _, d := range p.Drives {
		if !ds[d] {
			return errors.New("pool contains unknown or repeated drive")
		}
		delete(ds, d)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, other := range m.pools {
		if id == p.ID {
			continue
		}
		for _, a := range other.Barcodes {
			for _, b := range p.Barcodes {
				if a == b {
					return errors.New("cartridge already belongs to another pool")
				}
			}
		}
	}
	next := map[string]Pool{}
	for id, p := range m.pools {
		next[id] = p
	}
	next[p.ID] = p
	if e = save(filepath.Join(m.root, "pools.json"), next); e != nil {
		return e
	}
	m.pools = next
	return nil
}
func (m *Managed) Execute(ctx context.Context, id string, v model.TapeRequest, r *repository.Repository, report func(model.Result)) (model.Result, error) {
	if report == nil {
		report = func(model.Result) {}
	}
	if !idPattern.MatchString(id) {
		return model.Result{}, errors.New("invalid operation ID")
	}
	if e := ValidateRequest(v); e != nil {
		return model.Result{}, e
	}
	if v.PoolID != "" {
		return m.archive(ctx, id, v, r, report)
	}
	inv, e := m.backend.Inventory(ctx)
	if e != nil {
		return model.Result{}, e
	}
	drive := v.DriveID
	for _, c := range inv.Cartridges {
		if c.Barcode == v.Barcode && c.DriveID != "" {
			drive = c.DriveID
		}
	}
	if v.Action == "recover" {
		for _, j := range inv.Positions {
			if j.Barcode == v.Barcode {
				drive = j.Drive
			}
		}
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
		m.queue.mu.Lock()
		entry.Result = res
		m.queue.mu.Unlock()
		report(res)
	}
	free, e := m.queue.acquire(ctx, entry, update)
	if e != nil {
		return model.Result{}, e
	}
	defer free()
	return m.backend.Execute(ctx, id, v, r, update)
}
func stepID(root, step string) string {
	h := sha256.Sum256([]byte(root + "/" + step))
	return hex.EncodeToString(h[:16])
}
func (m *Managed) archive(ctx context.Context, id string, v model.TapeRequest, r *repository.Repository, report func(model.Result)) (res model.Result, err error) {
	path := filepath.Join(m.root, "pool-operations", id+".json")
	// A second request must never start another workflow with the same identity.
	m.mu.Lock()
	if _, exists := m.waiting[id]; exists {
		m.mu.Unlock()
		return res, errors.New("pool operation already active")
	}
	var old operation
	if e := read(path, &old); e == nil {
		m.mu.Unlock()
		a, _ := json.Marshal(v)
		b, _ := json.Marshal(old.Request)
		if string(a) != string(b) {
			return res, errors.New("pool operation identity conflict")
		}
		if old.Phase == "complete" {
			return old.Result, nil
		}
		return old.Result, errors.New("interrupted pool operation; inspect drive journal before submitting a new operation")
	} else if !os.IsNotExist(e) {
		m.mu.Unlock()
		return res, e
	}
	m.waiting[id] = QueueEntry{ID: id, Action: "write", State: "waiting-media", QueuedAt: time.Now().UTC()}
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.waiting, id); m.mu.Unlock() }()
	manifest, e := r.Manifest(v.SnapshotID)
	if e != nil {
		return res, e
	}
	// Conservative upper bound includes un-deduplicated logical files and metadata.
	encoded, e := json.Marshal(manifest)
	if e != nil {
		return res, e
	}
	needed := int64(len(encoded)) + 1<<20
	for _, f := range manifest.Files {
		if f.Size > 1<<50-needed {
			return res, errors.New("archive size overflow")
		}
		needed += f.Size
	}
	var chosen Medium
	var drive string
	var release func()
	for {
		if e := ctx.Err(); e != nil {
			return res, e
		}
		m.mu.Lock()
		pool, ok := m.pools[v.PoolID]
		m.mu.Unlock()
		if !ok {
			return res, errors.New("unknown tape pool")
		}
		m.selecting.Lock()
		inv, e := m.backend.Inventory(ctx)
		if e != nil {
			m.selecting.Unlock()
			return res, e
		}
		chosen, drive, ok = choosePool(pool, inv, m.queue.snapshot(), needed)
		if ok {
			// Register under selection lock before another pool workflow can choose it.
			entry := &QueueEntry{ID: id, Action: "pool-write", Barcode: chosen.Barcode, Drive: drive}
			release, ok = m.queue.tryAcquire(entry)
			m.selecting.Unlock()
			if ok {
				break
			}
			continue
		}
		m.selecting.Unlock()
		res = model.Result{Phase: "tape-waiting-media", Tape: &model.TapeResult{PoolID: v.PoolID}, TotalBytes: needed, ProgressScope: "tape-pool"}
		report(res)
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return res, ctx.Err()
		case <-timer.C:
		}
	}
	defer release()
	m.mu.Lock()
	delete(m.waiting, id)
	m.mu.Unlock()
	res = model.Result{Phase: "tape-pool-selected", Tape: &model.TapeResult{PoolID: v.PoolID, Barcode: chosen.Barcode, DriveID: drive}}
	if e = save(path, operation{Request: v, Result: res, Phase: "started"}); e != nil {
		return res, e
	}
	defer func() {
		op := operation{Request: v, Result: res, Phase: "complete"}
		if err != nil {
			op.Phase = "failed"
			op.Error = err.Error()
		}
		if e := save(path, op); e != nil {
			err = errors.Join(err, e)
		}
	}()
	emit := func(x model.Result) {
		if x.Tape == nil {
			x.Tape = &model.TapeResult{}
		} else {
			cp := *x.Tape
			x.Tape = &cp
		}
		x.Tape.PoolID = v.PoolID
		x.Tape.Barcode = chosen.Barcode
		x.Tape.DriveID = drive
		report(x)
	}
	emit(res)
	if !chosen.Mounted {
		emit(model.Result{Phase: "tape-pool-load"})
		if _, err = m.backend.Execute(ctx, stepID(id, "load"), model.TapeRequest{Action: "load", Barcode: chosen.Barcode, DriveID: drive}, r, emit); err != nil {
			return res, err
		}
	}
	req := v
	req.PoolID = ""
	req.Barcode = chosen.Barcode
	req.DriveID = drive
	res, err = m.backend.Execute(ctx, stepID(id, "write"), req, r, emit)
	if err != nil {
		return res, err
	}
	res.Tape.PoolID = v.PoolID
	res.Tape.DriveID = drive
	emit(model.Result{Phase: "tape-pool-unload"})
	if _, err = m.backend.Execute(ctx, stepID(id, "unload"), model.TapeRequest{Action: "unload", Barcode: chosen.Barcode, DriveID: drive}, r, emit); err != nil {
		return res, err
	}
	res.Phase = "complete"
	return res, nil
}
func choosePool(p Pool, inv Library, queue []QueueEntry, needed int64) (Medium, string, bool) {
	busyC, busyD := map[string]bool{}, map[string]bool{}
	for _, q := range queue {
		busyC[q.Barcode] = true
		if q.Drive != "" {
			busyD[q.Drive] = true
		}
	}
	for _, j := range inv.Positions {
		busyC[j.Barcode] = true
		busyD[j.Drive] = true
		if j.Action == "load" || j.Action == "unload" {
			return Medium{}, "", false
		}
	}
	for _, barcode := range p.Barcodes {
		for _, c := range inv.Cartridges {
			if c.Barcode != barcode || busyC[c.Barcode] || !c.Known || c.Label == "" || c.Dirty || c.Busy || c.WORM || c.WriteProtected || c.Cleaning || needed > c.CapacityBytes-c.UsedBytes {
				continue
			}
			for _, id := range p.Drives {
				for _, d := range inv.Drives {
					if d.ID != id || busyD[id] || !d.Online || d.Generation != 9 || (c.Generation != 8 && c.Generation != 9) {
						continue
					}
					if c.Mounted && c.DriveID == id || !c.Mounted && d.Empty {
						return c, id, true
					}
				}
			}
		}
	}
	return Medium{}, "", false
}
