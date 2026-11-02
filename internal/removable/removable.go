// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package removable

import (
	"context"
	"encoding/json"
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"gdata/srk/internal/tape"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type Config struct {
	ID            string `json:"id"`
	UUID          string `json:"uuid"`
	Mount         string `json:"mount"`
	CapacityBytes int64  `json:"capacity_bytes"`
}

func (c Config) Validate() error {
	if !identifier.MatchString(c.ID) || !identifier.MatchString(c.UUID) || !filepath.IsAbs(c.Mount) || filepath.Clean(c.Mount) == "/" || c.CapacityBytes < 1<<20 {
		return errors.New("flash requires ID, UUID, dedicated absolute mount and capacity")
	}
	return nil
}

type Status struct {
	ID      string        `json:"id"`
	UUID    string        `json:"uuid"`
	State   string        `json:"state"`
	Error   string        `json:"error,omitempty"`
	Catalog *tape.Library `json:"catalog,omitempty"`
}
type Volume struct {
	mu       sync.Mutex
	Config   Config
	cache    string
	released bool
	probe    func(Config) error
	anchor   func(Config) (string, func(), error)
	syncFS   func(Config) error
}

func Open(c Config, cache string) (*Volume, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	return &Volume{Config: c, cache: cache, probe: checkMount, anchor: anchorVolume, syncFS: syncVolume}, nil
}
func (v *Volume) Status(ctx context.Context) Status {
	if !v.mu.TryLock() {
		return Status{ID: v.Config.ID, UUID: v.Config.UUID, State: "busy"}
	}
	defer v.mu.Unlock()
	s := Status{ID: v.Config.ID, UUID: v.Config.UUID, State: "disconnected"}
	b, e := os.ReadFile(v.cache)
	if e == nil {
		var catalog tape.Library
		if json.Unmarshal(b, &catalog) == nil {
			s.Catalog = &catalog
		}
	}
	if e = v.probe(v.Config); e != nil {
		s.Error = e.Error()
		return s
	}
	s.State = "connected"
	if v.released {
		s.State = "released"
	}
	return s
}
func (v *Volume) Execute(ctx context.Context, id string, req model.TapeRequest, r *repository.Repository, report func(model.Result)) (model.Result, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	var res model.Result
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(id) {
		return res, errors.New("invalid operation ID")
	}
	if req.Barcode != v.Config.ID {
		return res, errors.New("volume ID mismatch")
	}
	if req.Confirm != "" && req.Confirm != v.Config.ID {
		return res, errors.New("exact volume ID confirmation required")
	}
	if e := v.probe(v.Config); e != nil {
		return res, e
	}
	if req.Action == "attach" {
		v.released = false
		res.Verified = true
		return res, nil
	}
	if v.released {
		return res, errors.New("volume released; attach it before use")
	}
	anchor, closeAnchor, e := v.anchor(v.Config)
	if e != nil {
		return res, e
	}
	defer closeAnchor()
	root := filepath.Join(anchor, ".srk-removable")
	marker := filepath.Join(root, "identity.json")
	b, e := os.ReadFile(marker)
	if os.IsNotExist(e) {
		if req.Action != "label" || req.Confirm != req.Barcode || req.Reason == "" {
			return res, errors.New("unregistered volume: explicit label confirmation required")
		}
		if _, e = os.Stat(root); e == nil {
			return res, errors.New("existing unrecognised SRK directory; refusing initialization")
		}
		if e = os.Mkdir(root, 0700); e != nil {
			return res, e
		}
		b, _ = json.Marshal(struct {
			UUID string `json:"uuid"`
		}{v.Config.UUID})
		if e = repository.Atomic(marker, b, 0600); e != nil {
			return res, e
		}
	} else if e != nil {
		return res, e
	}
	var identity struct {
		UUID string `json:"uuid"`
	}
	if json.Unmarshal(b, &identity) != nil || identity.UUID != v.Config.UUID {
		return res, errors.New("SRK volume identity mismatch")
	}
	lock, e := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return res, e
	}
	defer lock.Close()
	if e = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		return res, errors.New("volume already in use")
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	store, e := tape.Open(root, tape.Config{Backend: "emulator", LibraryID: "FLASH", Drives: []string{"VOLUME"}, Cartridges: []tape.MediumSpec{{Barcode: "FLASH", Slot: 1, Generation: 9, CapacityBytes: v.Config.CapacityBytes}}})
	if e != nil {
		return res, e
	}
	if req.Action == "release" {
		if e = v.syncFS(v.Config); e != nil {
			return res, e
		}
		v.released = true
		res.Verified = true
		return res, nil
	}
	req.Barcode = "FLASH"
	if req.Confirm != "" {
		req.Confirm = "FLASH"
	}
	req.DriveID = "VOLUME"
	inv, e := store.Inventory(ctx)
	if e != nil {
		return res, e
	}
	if !inv.Cartridges[0].Mounted {
		if e = store.Control(ctx, model.TapeRequest{Action: "load", Barcode: "FLASH", DriveID: "VOLUME"}); e != nil {
			return res, e
		}
	}
	res, e = store.Execute(ctx, id, req, r, report)
	if e == nil {
		e = v.probe(v.Config)
	}
	if e == nil {
		e = v.syncFS(v.Config)
	}
	if e != nil {
		return res, e
	}
	inv, e = store.Inventory(ctx)
	if e != nil {
		return res, e
	}
	b, e = json.Marshal(inv)
	if e != nil {
		return res, e
	}
	if e = repository.Atomic(v.cache, b, 0600); e != nil {
		return res, e
	}
	return res, nil
}
