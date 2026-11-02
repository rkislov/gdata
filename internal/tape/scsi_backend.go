// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package tape

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"gdata/srk/internal/tape/scsi"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Backend interface {
	Inventory(context.Context) (Library, error)
	Execute(context.Context, string, model.TapeRequest, *repository.Repository, func(model.Result)) (model.Result, error)
}
type SCSIPath struct {
	Path    string `json:"path"`
	Serial  string `json:"serial"`
	Element uint16 `json:"element"`
}
type SCSIConfig struct {
	Changer   SCSIPath            `json:"changer"`
	Transport uint16              `json:"transport_element"`
	Drives    map[string]SCSIPath `json:"drives"`
}

func (c SCSIConfig) Validate(cfg Config) error {
	if !filepath.IsAbs(c.Changer.Path) || c.Changer.Serial == "" || len(c.Drives) != len(cfg.Drives) {
		return errors.New("SCSI paths, serials and drive element mapping required")
	}
	seen := map[string]bool{c.Changer.Path: true}
	addresses := map[uint16]bool{c.Transport: true}
	for _, id := range cfg.Drives {
		d, ok := c.Drives[id]
		if !ok || !filepath.IsAbs(d.Path) || d.Serial == "" || seen[d.Path] {
			return errors.New("invalid SCSI drive binding")
		}
		seen[d.Path] = true
		if addresses[d.Element] {
			return errors.New("duplicate SCSI element address")
		}
		addresses[d.Element] = true
	}
	for _, v := range cfg.Cartridges {
		if v.Slot < 0 || v.Slot > 65535 || addresses[uint16(v.Slot)] {
			return errors.New("invalid or duplicate SCSI slot address")
		}
		addresses[uint16(v.Slot)] = true
	}
	return nil
}

type PositionJournal struct {
	NewLabel    *VolumeLabel `json:"new_label,omitempty"`
	EraseMode   string       `json:"erase_mode,omitempty"`
	Source      uint16       `json:"source_element"`
	Destination uint16       `json:"destination_element"`
	Operation   string       `json:"operation"`
	Action      string       `json:"action"`
	Barcode     string       `json:"barcode"`
	Drive       string       `json:"drive"`
	File        int32        `json:"file"`
	Phase       string       `json:"phase"`
	CopyID      string       `json:"copy_id,omitempty"`
	At          time.Time    `json:"at"`
	Error       string       `json:"error,omitempty"`
}
type scsiIndex struct {
	Files          map[string][]string        `json:"files"`
	Pending        *PositionJournal           `json:"pending,omitempty"` // legacy single-drive journal
	PendingByDrive map[string]PositionJournal `json:"pending_by_drive,omitempty"`
}
type SCSIStore struct {
	mu    sync.Mutex // index only; never held during device I/O
	robot sync.Mutex
	queue driveQueue
	stage *Store
	cfg   Config
	root  string
	index scsiIndex
	open  func(SCSIPath) (*scsi.Device, func(), error)
}

func OpenBackend(root string, c Config) (Backend, error) {
	b, e := openRawBackend(root, c)
	if e != nil {
		return nil, e
	}
	return manage(root, b)
}
func openRawBackend(root string, c Config) (Backend, error) {
	if c.Backend != "scsi" {
		return Open(root, c)
	}
	return OpenSCSI(root, c, func(p SCSIPath) (*scsi.Device, func(), error) {
		io, e := scsi.OpenLinux(p.Path, 30*time.Minute)
		if e != nil {
			return nil, nil, e
		}
		return &scsi.Device{IO: io}, func() { io.Close() }, nil
	})
}
func OpenSCSI(root string, c Config, open func(SCSIPath) (*scsi.Device, func(), error)) (*SCSIStore, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	stageCfg := c
	stageCfg.Backend = "emulator"
	stage, e := Open(root, stageCfg)
	if e != nil {
		return nil, e
	}
	s := &SCSIStore{root: root, cfg: c, stage: stage, open: open, index: scsiIndex{Files: map[string][]string{}}}
	e = read(filepath.Join(root, "scsi-index.json"), &s.index)
	if e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	if s.index.Files == nil {
		return nil, errors.New("invalid SCSI position index")
	}
	if s.index.PendingByDrive == nil {
		s.index.PendingByDrive = map[string]PositionJournal{}
	}
	if s.index.Pending != nil {
		j := *s.index.Pending
		if _, ok := c.SCSI.Drives[j.Drive]; !ok {
			return nil, errors.New("legacy pending journal has unknown drive")
		}
		s.index.PendingByDrive[j.Drive] = j
		s.index.Pending = nil
		if e = s.persist(); e != nil {
			return nil, e
		}
	}
	return s, nil
}
func (s *SCSIStore) persist() error { return save(filepath.Join(s.root, "scsi-index.json"), s.index) }
func (s *SCSIStore) checkpoint(j PositionJournal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j.At = time.Now().UTC()
	if e := save(filepath.Join(s.root, "position-journal", j.Operation+".json"), j); e != nil {
		return e
	}
	s.index.PendingByDrive[j.Drive] = j
	return s.persist()
}
func (s *SCSIStore) device(ctx context.Context, p SCSIPath, kind byte) (*scsi.Device, func(), error) {
	d, close, e := s.open(p)
	if e != nil {
		return nil, nil, e
	}
	if e = d.CheckIdentity(ctx, kind, p.Serial); e != nil {
		close()
		return nil, nil, e
	}
	return d, close, nil
}
func (s *SCSIStore) elements(ctx context.Context) ([]scsi.Element, error) {
	d, close, e := s.device(ctx, s.cfg.SCSI.Changer, 8)
	if e != nil {
		return nil, e
	}
	defer close()
	b, e := d.Elements(ctx, 0, 65535)
	if e != nil {
		return nil, e
	}
	return scsi.ParseElements(b)
}
func (s *SCSIStore) Inventory(ctx context.Context) (Library, error) {
	v, e := s.stage.Inventory(ctx)
	if e != nil {
		return v, e
	}
	v.Backend = "scsi"
	v.Actions = SCSIActions()
	entries, err := os.ReadDir(filepath.Join(s.root, "position-journal"))
	if err != nil && !os.IsNotExist(err) {
		return v, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var j PositionJournal
		if err = read(filepath.Join(s.root, "position-journal", entry.Name()), &j); err != nil {
			return v, err
		}
		v.PositionHistory = append(v.PositionHistory, j)
	}
	sort.Slice(v.PositionHistory, func(i, j int) bool { return v.PositionHistory[i].At.After(v.PositionHistory[j].At) })
	if len(v.PositionHistory) > 20 {
		v.PositionHistory = v.PositionHistory[:20]
	}

	v.Positions = s.pendingPositions()
	if len(v.Positions) > 0 {
		j := v.Positions[0]
		v.Position = &j
	}
	v.Queue = s.queue.snapshot()
	s.robot.Lock()
	elements, e := s.elements(ctx)
	s.robot.Unlock()
	if e != nil {
		return v, e
	}
	for i := range v.Cartridges {
		c := &v.Cartridges[i]
		for _, q := range v.Queue {
			if q.Barcode == c.Barcode && q.State == "running" {
				c.Busy = true
			}
		}
		c.Mounted = false
		c.Present = false
		c.DriveID = ""
		for _, el := range elements {
			if el.Full && el.Barcode == c.Barcode {
				c.Present = !el.Exception
				for id, d := range s.cfg.SCSI.Drives {
					if d.Element == el.Address {
						c.Mounted = true
						c.DriveID = id
					}
				}
			}
		}
		for k := range c.Copies {
			confirmed := false
			for _, id := range s.files(c.Barcode) {
				if id == c.Copies[k].ID {
					confirmed = true
				}
			}
			c.Copies[k].Verified = confirmed
		}
		if s.pendingFor(c.Barcode, "") != nil {
			c.Dirty = true
		}
	}
	for i := range v.Drives {
		d := &v.Drives[i]
		d.Online = false
		for _, el := range elements {
			if el.Address == s.cfg.SCSI.Drives[d.ID].Element && el.Type == 4 {
				d.Online = !el.Exception
				d.Empty = !el.Full
			}
		}
	}
	return v, nil
}
func (s *SCSIStore) position(ctx context.Context, d *scsi.Device, n int32) error {
	if e := d.Rewind(ctx); e != nil {
		return e
	}
	if n > 0 {
		return d.SpaceFiles(ctx, n)
	}
	return nil
}
func (s *SCSIStore) receive(ctx context.Context, d *scsi.Device, c Medium, n int32, path string, progress ...func(int64)) error {
	if e := s.position(ctx, d, n); e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	var target io.Writer = f
	if len(progress) > 0 {
		target = &tapeProgressWriter{Writer: f, report: progress[0]}
	}
	if e = d.ReadFile(ctx, target, c.CapacityBytes); e != nil {
		return e
	}
	return f.Sync()
}
func (s *SCSIStore) execute(ctx context.Context, id string, v model.TapeRequest, r *repository.Repository, report func(model.Result), reservedDrive string) (res model.Result, err error) {
	res = model.Result{Tape: &model.TapeResult{Barcode: v.Barcode}, Phase: "scsi-inventory"}
	if !idPattern.MatchString(id) {
		return res, errors.New("invalid operation ID")
	}
	if e := ValidateRequest(v); e != nil {
		return res, e
	}
	receipt := filepath.Join(s.root, "scsi-operations", id+".json")
	var previous operation
	if e := read(receipt, &previous); e == nil {
		a, _ := json.Marshal(previous.Request)
		b, _ := json.Marshal(v)
		if !bytes.Equal(a, b) {
			return res, errors.New("operation identity conflict")
		}
		if previous.Phase == "complete" {
			return previous.Result, nil
		}
		return res, errors.New("previous SCSI operation incomplete; recover before issuing a new operation")
	} else if !os.IsNotExist(e) {
		return res, e
	}
	if s.pendingFor(v.Barcode, reservedDrive) != nil && v.Action != "recover" {
		return res, errors.New("SCSI operation requires recovery; physical position is not trusted")
	}
	if err = save(receipt, operation{Request: v, Phase: "started"}); err != nil {
		return res, err
	}
	defer func() {
		op := operation{Request: v, Result: res, Phase: "complete"}
		if err != nil {
			op.Phase = "failed"
			op.Error = err.Error()
			res.Verified = false
			op.Result = res
			if pending := s.pendingFor(v.Barcode, reservedDrive); pending != nil && pending.Barcode == v.Barcode {
				j := *pending
				j.Error = err.Error()
				if e := s.checkpoint(j); e != nil {
					err = errors.Join(err, e)
				}
			}
		}
		if e := save(receipt, op); e != nil {
			err = errors.Join(err, e)
		}
	}()
	inv, e := s.stage.Inventory(ctx)
	if e != nil {
		return res, e
	}
	var c Medium
	found := false
	for _, m := range inv.Cartridges {
		if m.Barcode == v.Barcode {
			c = m
			found = true
		}
	}
	if !found {
		return res, errors.New("unknown barcode")
	}
	s.robot.Lock()
	robotLocked := true
	defer func() {
		if robotLocked {
			s.robot.Unlock()
		}
	}()
	elements, e := s.elements(ctx)
	if e != nil {
		return res, e
	}
	var current *scsi.Element
	for i := range elements {
		if elements[i].Full && elements[i].Barcode == v.Barcode {
			if current != nil {
				return res, errors.New("duplicate physical barcode")
			}
			current = &elements[i]
		}
	}
	if current == nil || current.Exception {
		return res, errors.New("cartridge absent or reporting exception")
	}
	driveID := reservedDrive
	for name, p := range s.cfg.SCSI.Drives {
		if current.Address == p.Element {
			driveID = name
		}
	}
	if driveID != reservedDrive {
		return res, errors.New("cartridge changed drive while queued; resubmit after inventory")
	}
	j := PositionJournal{Operation: id, Action: v.Action, Barcode: v.Barcode, Drive: driveID, Phase: "intent"}
	pendingRobot := s.pendingFor(v.Barcode, reservedDrive)
	if v.Action == "recover" && pendingRobot != nil && (pendingRobot.Action == "load" || pendingRobot.Action == "unload") {
		pending := *pendingRobot
		if pending.Barcode != v.Barcode || current.Address != pending.Destination {
			return res, errors.New("robotics outcome unresolved; preserve position journal")
		}
		if e := s.syncMounted(c.Barcode, pending.Drive, pending.Action == "load"); e != nil {
			return res, e
		}
		return s.complete(pending, res)
	}

	if v.Action == "load" || v.Action == "unload" {
		if s.robotPending() {
			return res, errors.New("robotics requires recovery before another move")
		}
		dest := uint16(c.Slot)
		if v.Action == "load" {
			p, ok := s.cfg.SCSI.Drives[driveID]
			if !ok {
				return res, errors.New("configured drive required")
			}
			dest = p.Element
		}
		if v.Action == "load" && current.Type != 2 {
			return res, errors.New("load source must be storage slot")
		}
		if v.Action == "unload" && (current.Type != 4 || s.cfg.SCSI.Drives[driveID].Element != current.Address) {
			return res, errors.New("unload source must be configured drive")
		}
		j.Source = current.Address
		j.Destination = dest
		empty := false
		for _, el := range elements {
			if el.Address == dest && !el.Full && !el.Exception {
				empty = true
			}
		}
		if !empty {
			return res, errors.New("destination not empty")
		}
		if e = s.checkpoint(j); e != nil {
			return res, e
		}
		robot, close, e := s.device(ctx, s.cfg.SCSI.Changer, 8)
		if e != nil {
			return res, e
		}
		e = robot.Move(ctx, s.cfg.SCSI.Transport, current.Address, dest)
		close()
		if e != nil {
			return res, e
		}
		after, e := s.elements(ctx)
		if e != nil {
			return res, e
		}
		ok := false
		for _, el := range after {
			if el.Address == dest && el.Full && el.Barcode == v.Barcode && !el.Exception {
				ok = true
			}
		}
		if !ok {
			return res, errors.New("robotics outcome not confirmed")
		}
		s.stage.mu.Lock()
		m, _ := s.stage.medium(v.Barcode)
		m.Mounted = v.Action == "load"
		m.DriveID = ""
		if m.Mounted {
			m.DriveID = driveID
		}
		for i := range s.stage.state.Drives {
			d := &s.stage.state.Drives[i]
			d.Empty = true
			for _, m := range s.stage.state.Cartridges {
				if m.DriveID == d.ID && m.Mounted {
					d.Empty = false
				}
			}
		}
		e = s.stage.persist()
		s.stage.mu.Unlock()
		if e != nil {
			return res, e
		}
		return s.complete(j, res)
	}
	p, ok := s.cfg.SCSI.Drives[driveID]
	if !ok || current.Address != p.Element {
		return res, errors.New("cartridge must be loaded in configured drive")
	}
	s.robot.Unlock()
	robotLocked = false
	d, close, e := s.device(ctx, p, 1)
	if e != nil {
		return res, e
	}
	defer close()
	if e = d.CheckVariableMode(ctx, v.Action == "write" || v.Action == "label" || v.Action == "erase" || v.Action == "recycle"); e != nil {
		return res, e
	}
	s.stage.mu.Lock()
	m, _ := s.stage.medium(v.Barcode)
	m.Mounted = true
	m.DriveID = driveID
	s.stage.mu.Unlock()
	switch v.Action {
	case "import":
		if e = s.checkpoint(j); e != nil {
			return res, e
		}
		if e = s.importPhysical(ctx, d, c, j, report); e != nil {
			return res, e
		}
		return s.complete(j, res)
	case "erase", "recycle":
		if !c.Known || c.Dirty || c.WORM || c.WriteProtected {
			return res, errors.New("unknown, dirty or protected medium")
		}
		if e = d.CheckRewritable(ctx); e != nil {
			return res, e
		}
		// Re-read every physical archive, including previously unknown trailing files.
		if e = s.checkpoint(j); e != nil {
			return res, e
		}
		j.Phase = "preflight"
		if e = s.importPhysical(ctx, d, c, j, report); e != nil {
			return res, e
		}
		latest, _ := s.stage.Inventory(ctx)
		for _, m := range latest.Cartridges {
			if m.Barcode == c.Barcode {
				c = m
			}
		}
		guard := c.Cartridge
		guard.Mounted = false
		if e = CanRecycle(guard, s.stage.now()); e != nil {
			if pe := s.clearPending(j); pe != nil {
				return res, errors.Join(e, pe)
			}
			return res, e
		}
		j.NewLabel = &VolumeLabel{Barcode: c.Barcode, VolumeID: model.ID(), Label: c.Label}
		j.EraseMode = v.EraseMode
		j.Phase = "erase-intent"
		if e = s.checkpoint(j); e != nil {
			return res, e
		}
		res.Phase = "scsi-erase"
		report(res)
		if e = s.position(ctx, d, 0); e != nil {
			return res, e
		}
		if e = d.Erase(ctx, v.Action == "erase" && v.EraseMode == "long"); e != nil {
			return res, e
		}
		if e = s.finishErase(ctx, d, c, j); e != nil {
			return res, e
		}
		return s.complete(j, res)
	case "label":
		if c.Known || len(s.files(c.Barcode)) > 0 {
			return res, errors.New("physical relabel of recorded media is forbidden")
		}
		if e = s.position(ctx, d, 0); e != nil {
			return res, e
		}
		var probe bytes.Buffer
		e = d.ReadFile(ctx, &probe, 1<<20)
		var sense *scsi.SenseError
		if !errors.As(e, &sense) || sense.Key != 8 || probe.Len() != 0 {
			return res, errors.New("blank medium not confirmed")
		}
		if e = s.checkpoint(j); e != nil {
			return res, e
		}
		if e = s.stage.Control(ctx, v); e != nil {
			return res, e
		}
		label, e := s.stage.volumeLabel(Medium{Cartridge: Cartridge{Barcode: c.Barcode}})
		if e != nil {
			return res, e
		}
		payload, _ := json.Marshal(struct {
			Magic string      `json:"magic"`
			Label VolumeLabel `json:"label"`
		}{"SRKSCSI1", label})
		if e = s.checkpoint(j); e != nil {
			return res, e
		}
		if e = s.position(ctx, d, 0); e != nil {
			return res, e
		}
		if e = d.WriteFile(ctx, bytes.NewReader(payload)); e != nil {
			return res, e
		}
		if e = s.position(ctx, d, 0); e != nil {
			return res, e
		}
		var got bytes.Buffer
		if e = d.ReadFile(ctx, &got, 1<<20); e != nil {
			return res, e
		}
		if !bytes.Equal(payload, got.Bytes()) {
			return res, errors.New("label readback mismatch")
		}
		if e = s.setFiles(c.Barcode, []string{}); e != nil {
			return res, e
		}
		return s.complete(j, res)
	case "write":
		if !c.Known || c.Dirty {
			return res, errors.New("import/recovery required before append")
		}
		if e = s.checkLabel(ctx, d, c); e != nil {
			return res, e
		}
		if s.files(c.Barcode) == nil {
			return res, errors.New("physical index missing; import required")
		}
		res, e = s.stage.Execute(ctx, id, v, r, func(v model.Result) { v.Verified = false; v.Phase = "scsi-staging"; v.ProgressPercent = nil; report(v) })
		if e != nil {
			return res, e
		}
		res.Verified = false
		res.ProgressPercent = nil
		res.Phase = "scsi-write"
		report(res)
		j.File = int32(len(s.files(c.Barcode)) + 1)
		j.CopyID = id
		j.Phase = "write-intent"
		if e = s.checkpoint(j); e != nil {
			return res, e
		}
		if e = s.position(ctx, d, j.File); e != nil {
			return res, e
		}
		f, e := os.Open(filepath.Join(s.stage.volumeDir(c.Barcode), id+".srktape"))
		if e != nil {
			return res, e
		}
		info, statErr := f.Stat()
		if statErr != nil {
			f.Close()
			return res, statErr
		}
		e = d.WriteFileProgress(ctx, f, tapeByteProgress(c.Barcode, "scsi-write", info.Size(), report))
		f.Close()
		if e != nil {
			return res, e
		}
		res.Phase = "scsi-readback"
		report(res)
		j.Phase = "verify"
		if e = s.checkpoint(j); e != nil {
			return res, e
		}
		if e = s.verifyCopy(ctx, d, c, j.File, id, report); e != nil {
			return res, e
		}
		if e = s.setFiles(c.Barcode, append(s.files(c.Barcode), id)); e != nil {
			return res, e
		}
		return s.complete(j, res)
	case "recover":
		pending := s.pendingFor(v.Barcode, reservedDrive)
		if pending == nil {
			return res, errors.New("no pending SCSI operation")
		}
		if pending.Barcode != v.Barcode || pending.Drive != driveID {
			return res, errors.New("load the original recovery cartridge/drive")
		}
		if pending.Action == "import" || ((pending.Action == "erase" || pending.Action == "recycle") && pending.NewLabel == nil) {
			if e = s.importPhysical(ctx, d, c, *pending, report); e != nil {
				return res, e
			}
			return s.complete(*pending, res)
		}
		if pending.Action == "erase" || pending.Action == "recycle" {
			if pending.NewLabel == nil || !idPattern.MatchString(pending.NewLabel.VolumeID) {
				return res, errors.New("invalid erase journal")
			}
			if c.WORM || c.WriteProtected {
				return res, errors.New("protected medium")
			}
			if e = d.CheckRewritable(ctx); e != nil {
				return res, e
			}
			if e = s.finishErase(ctx, d, c, *pending); e != nil {
				return res, e
			}
			return s.complete(*pending, res)
		}
		if pending.Action == "label" {
			if e = s.checkLabel(ctx, d, c); e != nil {
				return res, e
			}
			if e = s.setFiles(c.Barcode, []string{}); e != nil {
				return res, e
			}
			return s.complete(*pending, res)
		}
		if pending.Action != "write" {
			return res, errors.New("recovery requires physical import for non-write intents")
		}
		if e = s.verifyCopy(ctx, d, c, pending.File, pending.CopyID, report); e != nil {
			return res, fmt.Errorf("partial tape file remains protected: %w", e)
		}
		if e = s.checkLabel(ctx, d, c); e != nil {
			return res, e
		}
		files := s.files(c.Barcode)
		already := int(pending.File) == len(files) && len(files) > 0 && files[len(files)-1] == pending.CopyID
		if !already && int(pending.File) != len(files)+1 {
			return res, errors.New("position index conflict")
		}
		if !already {
			if e = s.setFiles(c.Barcode, append(s.files(c.Barcode), pending.CopyID)); e != nil {
				return res, e
			}
		}
		res.Tape.CopyID = pending.CopyID
		j = *pending
		j.Phase = "recovered"
		return s.complete(j, res)
	case "restore", "verify":
		if e = s.checkLabel(ctx, d, c); e != nil {
			return res, e
		}
		files := s.files(c.Barcode)
		if files == nil {
			return res, errors.New("physical catalog unavailable")
		}
		if v.Action == "verify" {
			for i, copyID := range files {
				if e = s.verifyCopy(ctx, d, c, int32(i+1), copyID, report); e != nil {
					return res, e
				}
			}
			res.Verified = true
			return res, nil
		}
		for i, copyID := range files {
			if copyID == v.CopyID {
				if e = s.verifyCopy(ctx, d, c, int32(i+1), copyID, report); e != nil {
					return res, e
				}
				return s.stage.Execute(ctx, id, v, r, report)
			}
		}
		return res, errors.New("copy not present on this tape")
	default:
		return res, errors.New("action unavailable on SCSI backend; no physical command issued")
	}
}
func (s *SCSIStore) verifyCopy(ctx context.Context, d *scsi.Device, c Medium, n int32, id string, report func(model.Result)) error {
	tmp := filepath.Join(s.root, "scsi-read-"+c.Barcode+".partial")
	defer os.Remove(tmp)
	var total int64
	for _, cp := range c.Copies {
		if cp.ID == id {
			total = cp.Bytes
		}
	}
	if info, e := os.Stat(filepath.Join(s.stage.volumeDir(c.Barcode), id+".srktape")); e == nil {
		total = info.Size()
	}
	if e := s.receive(ctx, d, c, n, tmp, tapeByteProgress(c.Barcode, "scsi-readback", total, report)); e != nil {
		return e
	}
	h, e := parseArchive(ctx, tmp, nil, func(int64, int64) {})
	if e != nil {
		return e
	}
	label, e := s.stage.volumeLabel(c)
	if e != nil {
		return e
	}
	if h.Copy.ID != id || h.Barcode != c.Barcode || h.VolumeID != label.VolumeID {
		return errors.New("physical copy identity mismatch")
	}
	if e = os.Rename(tmp, filepath.Join(s.stage.volumeDir(c.Barcode), id+".srktape")); e != nil {
		return e
	}
	dir, e := os.Open(s.stage.volumeDir(c.Barcode))
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *SCSIStore) complete(j PositionJournal, res model.Result) (model.Result, error) {
	j.Phase = "confirmed"
	if e := s.checkpoint(j); e != nil {
		return res, e
	}
	if e := s.clearPending(j); e != nil {
		return res, e
	}
	res.Verified = true
	res.Phase = "complete"
	p := 100.
	res.ProgressPercent = &p
	return res, nil
}

func (s *SCSIStore) checkLabel(ctx context.Context, d *scsi.Device, c Medium) error {
	if e := s.position(ctx, d, 0); e != nil {
		return e
	}
	var buf bytes.Buffer
	if e := d.ReadFile(ctx, &buf, 1<<20); e != nil {
		return e
	}
	var physical struct {
		Magic string      `json:"magic"`
		Label VolumeLabel `json:"label"`
	}
	if e := json.Unmarshal(buf.Bytes(), &physical); e != nil {
		return e
	}
	expected, e := s.stage.volumeLabel(c)
	if e != nil {
		return e
	}
	if physical.Magic != "SRKSCSI1" || physical.Label.Barcode != c.Barcode || physical.Label.VolumeID != expected.VolumeID {
		return errors.New("physical volume label mismatch")
	}
	return nil
}
func (s *SCSIStore) syncMounted(barcode, drive string, mounted bool) error {
	s.stage.mu.Lock()
	defer s.stage.mu.Unlock()
	m, e := s.stage.medium(barcode)
	if e != nil {
		return e
	}
	m.Mounted = mounted
	m.DriveID = ""
	if mounted {
		m.DriveID = drive
	}
	for i := range s.stage.state.Drives {
		d := &s.stage.state.Drives[i]
		d.Empty = true
		for _, m := range s.stage.state.Cartridges {
			if m.Mounted && m.DriveID == d.ID {
				d.Empty = false
			}
		}
	}
	return s.stage.persist()
}

func SCSIActions() []string {
	return []string{"load", "unload", "label", "write", "verify", "restore", "recover", "import", "erase", "recycle"}
}
