// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package tape

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var barcodePattern = regexp.MustCompile(`^[A-Z0-9_-]{1,32}$`)
var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Config struct {
	SCSI       *SCSIConfig  `json:"scsi,omitempty"`
	Backend    string       `json:"backend"`
	LibraryID  string       `json:"library_id"`
	Cartridges []MediumSpec `json:"cartridges"`
	Drives     []string     `json:"drives"`
}
type MediumSpec struct {
	Barcode        string `json:"barcode"`
	Slot           int    `json:"slot"`
	Generation     int    `json:"generation"`
	CapacityBytes  int64  `json:"capacity_bytes"`
	WORM           bool   `json:"worm,omitempty"`
	WriteProtected bool   `json:"write_protected,omitempty"`
}

func (c Config) Validate() error {
	if c.Backend == "scsi" {
		if c.SCSI == nil {
			return errors.New("SCSI configuration required")
		}
		if e := c.SCSI.Validate(c); e != nil {
			return e
		}
	}

	if (c.Backend != "emulator" && c.Backend != "scsi") || !barcodePattern.MatchString(c.LibraryID) || len(c.Drives) < 1 || len(c.Drives) > 16 || len(c.Cartridges) < 1 || len(c.Cartridges) > 1000 {
		return errors.New("explicit emulator backend, library, drives and media required")
	}
	seen := map[string]bool{}
	slots := map[int]bool{}
	for _, d := range c.Drives {
		if !barcodePattern.MatchString(d) || seen[d] {
			return errors.New("invalid/duplicate drive")
		}
		seen[d] = true
	}
	for _, v := range c.Cartridges {
		if !barcodePattern.MatchString(v.Barcode) || seen[v.Barcode] || slots[v.Slot] || v.Slot < 1 || v.Generation < 8 || v.Generation > 9 || v.CapacityBytes < 1<<20 {
			return errors.New("invalid cartridge geometry or duplicate barcode/slot")
		}
		seen[v.Barcode] = true
		slots[v.Slot] = true
	}
	return nil
}

type VolumeLabel struct {
	Policies map[string]Copy `json:"policies,omitempty"`
	Barcode  string          `json:"barcode"`
	VolumeID string          `json:"volume_id"`
	Label    string          `json:"label"`
}
type Medium struct {
	Present bool `json:"present"`
	Cartridge
	Slot          int    `json:"slot"`
	DriveID       string `json:"drive_id,omitempty"`
	VolumeID      string `json:"volume_id,omitempty"`
	Label         string `json:"label,omitempty"`
	CapacityBytes int64  `json:"capacity_bytes"`
	UsedBytes     int64  `json:"used_bytes"`
	Busy          bool   `json:"busy"`
	Erasing       bool   `json:"erasing"`
	Dirty         bool   `json:"dirty"`
}
type Library struct {
	Pools             []Pool            `json:"pools,omitempty"`
	Positions         []PositionJournal `json:"positions,omitempty"`
	Queue             []QueueEntry      `json:"queue,omitempty"`
	PositionHistory   []PositionJournal `json:"position_history,omitempty"`
	Actions           []string          `json:"actions,omitempty"`
	Position          *PositionJournal  `json:"position,omitempty"`
	LibraryID         string            `json:"library_id"`
	Backend           string            `json:"backend"`
	HardwareQualified bool              `json:"hardware_qualified"`
	ObservedAt        time.Time         `json:"observed_at"`
	Cartridges        []Medium          `json:"cartridges"`
	Drives            []Drive           `json:"drives"`
}
type Store struct {
	mu     sync.Mutex
	root   string
	cfg    Config
	state  Library
	failed error
	now    func() time.Time
}

func save(path string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	envelope := struct {
		Checksum string          `json:"checksum"`
		Data     json.RawMessage `json:"data"`
	}{repository.Hash(b), b}
	b, e = json.Marshal(envelope)
	if e != nil {
		return e
	}
	return repository.Atomic(path, b, 0600)
}
func read(path string, v any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	var x struct {
		Checksum string          `json:"checksum"`
		Data     json.RawMessage `json:"data"`
	}
	if json.Unmarshal(b, &x) != nil || repository.Hash(x.Data) != x.Checksum {
		return errors.New("tape metadata checksum mismatch")
	}
	return json.Unmarshal(x.Data, v)
}
func Open(root string, c Config) (*Store, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	if e := os.MkdirAll(root, 0700); e != nil {
		return nil, e
	}
	s := &Store{root: root, cfg: c, now: time.Now}
	e := read(filepath.Join(root, "catalog.json"), &s.state)
	if os.IsNotExist(e) {
		s.state = Library{LibraryID: c.LibraryID, Backend: "emulator", Cartridges: []Medium{}, Drives: []Drive{}}
		for _, v := range c.Cartridges {
			s.state.Cartridges = append(s.state.Cartridges, Medium{Cartridge: Cartridge{Barcode: v.Barcode, Generation: v.Generation, WORM: v.WORM, WriteProtected: v.WriteProtected}, Slot: v.Slot, CapacityBytes: v.CapacityBytes})
			if e = os.MkdirAll(s.volumeDir(v.Barcode), 0700); e != nil {
				return nil, e
			}
		}
		for _, id := range c.Drives {
			s.state.Drives = append(s.state.Drives, Drive{ID: id, Generation: 9, Empty: true, Online: true})
		}
	} else if e != nil {
		return nil, e
	}
	if s.state.LibraryID != c.LibraryID || len(s.state.Cartridges) != len(c.Cartridges) || len(s.state.Drives) != len(c.Drives) {
		return nil, errors.New("tape topology changed; explicit migration required")
	}
	for i := range s.state.Cartridges {
		v := &s.state.Cartridges[i]
		spec := c.Cartridges[i]
		if v.Barcode != spec.Barcode || v.Generation != spec.Generation || v.Slot != spec.Slot || v.CapacityBytes != spec.CapacityBytes {
			return nil, errors.New("cartridge identity changed")
		}
		v.WORM = spec.WORM
		v.WriteProtected = spec.WriteProtected
		// A previous writer leaves Dirty persisted; never infer a successful tape file.
		v.Busy = false
	}
	for i, d := range s.state.Drives {
		if d.ID != c.Drives[i] {
			return nil, errors.New("drive identity changed")
		}
	}
	if e = s.persist(); e != nil {
		return nil, e
	}
	return s, nil
}
func (s *Store) persist() error {
	if s.failed != nil {
		return s.failed
	}
	e := save(filepath.Join(s.root, "catalog.json"), s.state)
	if e != nil {
		s.failed = e
	}
	return e
}
func (s *Store) volumeDir(barcode string) string { return filepath.Join(s.root, "volumes", barcode) }
func (s *Store) medium(barcode string) (*Medium, error) {
	for i := range s.state.Cartridges {
		if s.state.Cartridges[i].Barcode == barcode {
			return &s.state.Cartridges[i], nil
		}
	}
	return nil, errors.New("unknown cartridge barcode")
}
func (s *Store) drive(id string) (*Drive, error) {
	for i := range s.state.Drives {
		if s.state.Drives[i].ID == id {
			return &s.state.Drives[i], nil
		}
	}
	return nil, errors.New("unknown drive")
}
func (s *Store) Inventory(ctx context.Context) (Library, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return Library{}, e
	}
	if s.failed != nil {
		return Library{}, s.failed
	}
	b, _ := json.Marshal(s.state)
	var v Library
	_ = json.Unmarshal(b, &v)
	v.ObservedAt = s.now().UTC()
	return v, nil
}
func ValidateRequest(v model.TapeRequest) error {
	if v.PoolID != "" {
		if !barcodePattern.MatchString(v.PoolID) || v.Action != "write" || v.VolumeID != "" || v.Barcode != "" || v.DriveID != "" {
			return errors.New("pool writes require pool_id without barcode or drive")
		}
		v.Barcode = "POOL"
	}
	if !barcodePattern.MatchString(v.Barcode) {
		return errors.New("barcode required")
	}
	switch v.Action {
	case "label":
		if !utf8.ValidString(v.Label) || strings.TrimSpace(v.Label) == "" || len(v.Label) > 128 || strings.ContainsAny(v.Label, "\r\n\x00") {
			return errors.New("valid volume label required")
		}
		fallthrough
	case "erase", "recycle", "release-hold":
		if v.Confirm != v.Barcode || strings.TrimSpace(v.Reason) == "" || len(v.Reason) > 500 {
			return errors.New("exact barcode confirmation and reason required")
		}
		if v.Action == "release-hold" && !idPattern.MatchString(v.CopyID) {
			return errors.New("copy ID required")
		}
		if v.Action == "erase" && v.EraseMode != "quick" && v.EraseMode != "long" {
			return errors.New("erase_mode must be quick or long")
		}
	case "hold":
		if !idPattern.MatchString(v.CopyID) {
			return errors.New("copy ID required")
		}
	case "load", "unload", "import", "verify", "recover":
	case "write":
		if !idPattern.MatchString(v.SnapshotID) || v.RetentionDays < 1 || v.RetentionDays > 36500 {
			return errors.New("snapshot and retention 1–36500 days required")
		}
	case "restore":
		if !idPattern.MatchString(v.CopyID) || v.RetentionDays < 1 || v.RetentionDays > 36500 {
			return errors.New("copy and retention 1–36500 days required")
		}
	default:
		return errors.New("unsupported tape action")
	}
	return nil
}
func (s *Store) Control(ctx context.Context, v model.TapeRequest) error {
	if e := ValidateRequest(v); e != nil {
		return e
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return s.failed
	}
	c, e := s.medium(v.Barcode)
	if e != nil {
		return e
	}
	if c.Busy {
		return errors.New("cartridge is busy")
	}
	switch v.Action {
	case "load":
		d, e := s.drive(v.DriveID)
		if e != nil {
			return e
		}
		if e = CanLoad(c.Cartridge, *d); e != nil {
			return e
		}
		c.Mounted = true
		c.DriveID = d.ID
		d.Empty = false
	case "unload":
		if !c.Mounted {
			return errors.New("cartridge is not mounted")
		}
		d, e := s.drive(c.DriveID)
		if e != nil {
			return e
		}
		d.Empty = true
		c.Mounted = false
		c.DriveID = ""
	case "hold", "release-hold":
		if !c.Known || c.Dirty || c.WriteProtected || c.WORM {
			return errors.New("cannot change protection on unknown/dirty/protected media")
		}
		label, e := s.volumeLabel(*c)
		if e != nil {
			return e
		}
		if label.Policies == nil {
			label.Policies = map[string]Copy{}
		}
		found := false
		for i := range c.Copies {
			if c.Copies[i].ID == v.CopyID {
				p := c.Copies[i]
				p.Hold = v.Action == "hold"
				label.Policies[p.ID] = p
				found = true
			}
		}
		if !found {
			return errors.New("copy not found")
		}
		if e = save(filepath.Join(s.volumeDir(c.Barcode), "label.json"), label); e != nil {
			s.failed = e
			return e
		}
		for i := range c.Copies {
			if c.Copies[i].ID == v.CopyID {
				c.Copies[i].Hold = v.Action == "hold"
			}
		}
	case "label":
		if c.WriteProtected || c.WORM {
			return errors.New("write-protected/WORM cartridge cannot be relabelled by emulator")
		}
		for _, m := range s.state.Cartridges {
			if m.Barcode != c.Barcode && m.Label == v.Label {
				return errors.New("volume label already used")
			}
		}
		entries, e := os.ReadDir(s.volumeDir(c.Barcode))
		if e != nil {
			return e
		}
		if !c.Known && len(entries) > 0 {
			return errors.New("unknown recorded cartridge; import required before labelling")
		}
		if c.Dirty {
			return errors.New("incomplete tape write; label cannot hide it")
		}
		if c.VolumeID == "" {
			c.VolumeID = model.ID()
		}
		c.Label = v.Label
		c.Known = true
		if c.Copies == nil {
			c.Copies = []Copy{}
		}
		label := VolumeLabel{Barcode: c.Barcode, VolumeID: c.VolumeID, Label: c.Label}
		if len(entries) > 0 {
			var old VolumeLabel
			if e = read(filepath.Join(s.volumeDir(c.Barcode), "label.json"), &old); e != nil {
				return e
			}
			label.Policies = old.Policies
		}
		if e = save(filepath.Join(s.volumeDir(c.Barcode), "label.json"), label); e != nil {
			s.failed = e
			return e
		}
	default:
		return fmt.Errorf("%s requires a streaming operation", v.Action)
	}
	return s.persist()
}

// Reserve a mounted cartridge. Persisted Dirty and copy retention precede all writes.
func (s *Store) reserve(v model.TapeRequest, writing bool, copy *Copy) (Medium, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return Medium{}, s.failed
	}
	c, e := s.medium(v.Barcode)
	if e != nil {
		return Medium{}, e
	}
	if c.Busy || !c.Mounted {
		return Medium{}, errors.New("load an idle cartridge first")
	}
	d, e := s.drive(c.DriveID)
	if e != nil || !d.Online {
		return Medium{}, errors.New("drive offline")
	}
	if v.DriveID != "" && c.DriveID != v.DriveID {
		return Medium{}, errors.New("mounted drive mismatch")
	}
	if writing && (!c.Known || c.Dirty || c.WriteProtected || c.WORM || c.Copies == nil) {
		return Medium{}, errors.New("unlabelled, dirty or protected cartridge")
	}
	c.Busy = true
	if writing {
		c.Dirty = true
		if copy != nil {
			c.Copies = append(c.Copies, *copy)
		}
	}
	if e = s.persist(); e != nil {
		return Medium{}, e
	}
	return *c, nil
}
func (s *Store) finish(barcode string, success bool, used int64, copies []Copy, label *VolumeLabel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, e := s.medium(barcode)
	if e != nil {
		return e
	}
	c.Busy = false
	if success {
		c.Dirty = false
		c.Erasing = false
		c.UsedBytes = used
		if copies != nil {
			c.Copies = copies
			c.Known = true
		}
		if label != nil {
			c.VolumeID = label.VolumeID
			c.Label = label.Label
		}
	}
	return s.persist()
}
