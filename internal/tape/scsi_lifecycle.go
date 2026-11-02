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
	"gdata/srk/internal/tape/scsi"
	"os"
	"path/filepath"
)

type physicalLabel struct {
	Magic string      `json:"magic"`
	Label VolumeLabel `json:"label"`
}

func blankEnd(err error) bool {
	var s *scsi.SenseError
	return errors.As(err, &s) && s.Key == 8 && !s.EOM && !s.ILI && !s.Filemark && s.ASC == 0 && (s.ASCQ == 0 || s.ASCQ == 5)
}
func (s *SCSIStore) readLabel(ctx context.Context, d *scsi.Device, c Medium) (VolumeLabel, error) {
	var v physicalLabel
	if e := s.position(ctx, d, 0); e != nil {
		return v.Label, e
	}
	var b bytes.Buffer
	if e := d.ReadFile(ctx, &b, 1<<20); e != nil {
		return v.Label, e
	}
	if e := json.Unmarshal(b.Bytes(), &v); e != nil {
		return v.Label, e
	}
	if v.Magic != "SRKSCSI1" || v.Label.Barcode != c.Barcode || !idPattern.MatchString(v.Label.VolumeID) || v.Label.Label == "" {
		return v.Label, errors.New("invalid physical volume label")
	}
	if c.VolumeID != "" && c.VolumeID != v.Label.VolumeID {
		return v.Label, errors.New("physical volume changed")
	}
	return v.Label, nil
}

// Full read through EOD: a truncated archive or unrecognised file cannot be
// mistaken for the end of a catalog. Publication occurs only after the scan.
func (s *SCSIStore) importPhysical(ctx context.Context, d *scsi.Device, c Medium, j PositionJournal, report func(model.Result)) error {
	label, e := s.readLabel(ctx, d, c)
	if e != nil {
		return e
	}
	tmp, e := os.MkdirTemp(s.root, "scsi-import-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(tmp)
	copies := []Copy{}
	files := []string{}
	seen := map[string]bool{}
	var used int64
	for n := int32(1); ; n++ {
		if n > 100000 {
			return errors.New("physical catalog exceeds file limit")
		}
		path := filepath.Join(tmp, "read.partial")
		e = s.receive(ctx, d, c, n, path)
		info, statErr := os.Stat(path)
		if blankEnd(e) && statErr == nil && info.Size() == 0 {
			break
		}
		if e != nil {
			return fmt.Errorf("physical file %d: %w", n, e)
		}
		h, e := parseArchive(ctx, path, nil, func(int64, int64) {})
		if e != nil {
			return e
		}
		if h.VolumeID != label.VolumeID || h.Barcode != c.Barcode || seen[h.Copy.ID] {
			return errors.New("conflicting physical archive identity")
		}
		seen[h.Copy.ID] = true
		// Preserve stronger local policy when the same volume is reimported.
		for _, old := range c.Copies {
			if old.ID == h.Copy.ID {
				h.Copy.Hold = h.Copy.Hold || old.Hold
				if old.ExpiresAt.After(h.Copy.ExpiresAt) {
					h.Copy.ExpiresAt = old.ExpiresAt
				}
			}
		}
		if policy, ok := label.Policies[h.Copy.ID]; ok {
			h.Copy.Hold = h.Copy.Hold || policy.Hold
			if policy.ExpiresAt.After(h.Copy.ExpiresAt) {
				h.Copy.ExpiresAt = policy.ExpiresAt
			}
		}
		if h.Copy.Bytes > c.CapacityBytes-used {
			return errors.New("physical catalog exceeds configured capacity")
		}
		used += h.Copy.Bytes
		copies = append(copies, h.Copy)
		files = append(files, h.Copy.ID)
		if e = os.Rename(path, filepath.Join(tmp, h.Copy.ID+".srktape")); e != nil {
			return e
		}
		report(model.Result{Phase: "scsi-import-readback", ProgressBytes: used, Files: len(copies), ProgressScope: "physical-catalog-scan", Tape: &model.TapeResult{Barcode: c.Barcode, CopyID: h.Copy.ID}})
	}
	j.Phase = "import-publish"
	if e = s.checkpoint(j); e != nil {
		return e
	}
	// Only service-owned staging files are replaced; physical media is read-only.
	dir := s.stage.volumeDir(c.Barcode)
	entries, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if e = os.Remove(filepath.Join(dir, entry.Name())); e != nil {
			return e
		}
	}
	label.Policies = map[string]Copy{}
	for _, copy := range copies {
		label.Policies[copy.ID] = copy
	}
	for _, id := range files {
		if e = os.Rename(filepath.Join(tmp, id+".srktape"), filepath.Join(dir, id+".srktape")); e != nil {
			return e
		}
	}
	if e = save(filepath.Join(dir, "label.json"), label); e != nil {
		return e
	}
	if e = s.stage.finish(c.Barcode, true, used, copies, &label); e != nil {
		return e
	}
	return s.setFiles(c.Barcode, files)
}

func (s *SCSIStore) finishErase(ctx context.Context, d *scsi.Device, c Medium, j PositionJournal) error {
	// This recovery path never repeats ERASE. Blank BOT or the exact replacement
	// label is required; unknown partially erased contents remain blocked.
	if e := d.Ready(ctx); e != nil {
		return e
	}
	if e := s.position(ctx, d, 0); e != nil {
		return e
	}
	var b bytes.Buffer
	e := d.ReadFile(ctx, &b, 1<<20)
	if blankEnd(e) && b.Len() == 0 {
		payload, _ := json.Marshal(physicalLabel{Magic: "SRKSCSI1", Label: *j.NewLabel})
		if e = s.position(ctx, d, 0); e != nil {
			return e
		}
		if e = d.WriteFile(ctx, bytes.NewReader(payload)); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	clone := c
	clone.VolumeID = j.NewLabel.VolumeID
	got, e := s.readLabel(ctx, d, clone)
	if e != nil {
		return e
	}
	if got.VolumeID != j.NewLabel.VolumeID || got.Label != j.NewLabel.Label {
		return errors.New("erase outcome not confirmed")
	}
	// Ensure there is no trailing file after the new label.
	path := filepath.Join(s.root, "erase-check-"+c.Barcode+".partial")
	defer os.Remove(path)
	e = s.receive(ctx, d, c, 1, path)
	info, se := os.Stat(path)
	if !blankEnd(e) || se != nil || info.Size() != 0 {
		return errors.New("erase end of data not confirmed")
	}
	dir := s.stage.volumeDir(c.Barcode)
	entries, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if e = os.Remove(filepath.Join(dir, entry.Name())); e != nil {
			return e
		}
	}
	if e = save(filepath.Join(dir, "label.json"), j.NewLabel); e != nil {
		return e
	}
	if e = s.stage.finish(c.Barcode, true, 0, []Copy{}, j.NewLabel); e != nil {
		return e
	}
	return s.setFiles(c.Barcode, []string{})
}
