// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func WALPrefix(base *model.PostgreSQLBackup, names []string) (uint64, []model.WALGap, error) {
	start, e := WALBasePosition(base)
	if e != nil {
		return 0, nil, e
	}
	step := uint64(base.SegmentSize)
	timeline := base.WALRanges[0].Timeline
	prefix := fmt.Sprintf("%08X", timeline)
	seen := map[uint64]bool{}
	for _, name := range names {
		if strings.HasSuffix(name, ".history") {
			t, _ := strconv.ParseUint(name[:8], 16, 32)
			if t > uint64(timeline) {
				return start, nil, errors.New("timeline switch requires a new base")
			}
		}
		if len(name) != 24 || !strings.HasPrefix(name, prefix) {
			continue
		}
		p, e := WALPosition(name, base.SegmentSize)
		if e != nil {
			return start, nil, e
		}
		if p >= start {
			seen[p] = true
		}
	}
	positions := []uint64{}
	for p := range seen {
		positions = append(positions, p)
	}
	sort.Slice(positions, func(i, j int) bool { return positions[i] < positions[j] })
	expected := start
	end := start
	gaps := []model.WALGap{}
	broken := false
	for _, p := range positions {
		if p > expected {
			gaps = append(gaps, model.WALGap{Timeline: timeline, From: fmt.Sprintf("%X/%X", expected>>32, expected&0xffffffff), To: fmt.Sprintf("%X/%X", (p-step)>>32, (p-step)&0xffffffff), Count: (p - expected) / step})
			broken = true
		}
		if !broken {
			end = p + step
		}
		expected = p + step
	}
	if len(positions) == 0 {
		gaps = append(gaps, model.WALGap{Timeline: timeline, From: base.WALRanges[0].StartLSN, To: base.WALRanges[0].StartLSN, Count: 1})
	}
	return end, gaps, nil
}
func (r *Repository) WALCalendar(archive string) (model.PITRCalendar, error) {
	r.walMu.RLock()
	defer r.walMu.RUnlock()
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := model.PITRCalendar{ArchiveID: archive, At: time.Now().UTC(), Windows: []model.PITRWindow{}}
	if r.failed != nil {
		return result, r.failed
	}
	records, e := r.walInventory(archive)
	if e != nil {
		return result, e
	}
	result.Records = len(records)
	points, e := r.inventory(false)
	if e != nil {
		return result, e
	}
	names := []string{}
	available := map[string]bool{}
	for _, v := range records {
		info, e := os.Lstat(filepath.Join(r.Root, "wal", archive, v.Name))
		if e == nil && info.Mode().IsRegular() && info.Size() == v.Bytes {
			names = append(names, v.Name)
			available[v.Name] = true
		}
		if v.TimeBoundsVerified {
			result.TimestampedRecords++
		}
	}
	for _, point := range points {
		pg := point.Result.PostgreSQL
		if pg == nil || pg.ArchiveID != archive {
			continue
		}
		w := model.PITRWindow{BaseID: point.ID, CreatedAt: point.CreatedAt, From: pg.FinishedAt, ExpiresAt: point.ExpiresAt, Hold: point.Hold, Status: "unknown", Gaps: []model.WALGap{}, Notes: []string{"Intervals are WAL-derived candidates; native PITR confirms the target. Content checksums are checked during restore."}}
		end, gaps, e := WALPrefix(pg, names)
		w.Gaps = gaps
		w.ContiguousEnd = fmt.Sprintf("%X/%X", end>>32, end&0xffffffff)
		if e != nil {
			w.Status = "timeline_switch"
			w.Notes = append(w.Notes, e.Error())
			result.Windows = append(result.Windows, w)
			continue
		}
		var last *time.Time
		clockBad := false
		unknown := false
		for _, v := range records {
			if len(v.Name) != 24 || !available[v.Name] || v.Name[:8] != fmt.Sprintf("%08X", pg.WALRanges[0].Timeline) {
				continue
			}
			p, _ := WALPosition(v.Name, v.SegmentSize)
			start, _ := WALBasePosition(pg)
			if p < start || p >= end {
				continue
			}
			if v.SystemID != pg.SystemID || v.SegmentSize != pg.SegmentSize {
				clockBad = true
				continue
			}
			if !v.TimeBoundsVerified {
				unknown = true
				continue
			}
			if v.FirstCommitAt != nil && last != nil && v.FirstCommitAt.Before(*last) {
				clockBad = true
			}
			if v.LastCommitAt != nil {
				copy := *v.LastCommitAt
				last = &copy
			}
		}
		if !unknown && !clockBad && last != nil && last.After(w.From) {
			w.ToExclusive = last
			w.Status = "candidate"
		}
		if unknown {
			w.Notes = append(w.Notes, "Some WAL files have no verified transaction timestamps; upload times are not used.")
		}
		if clockBad {
			w.Status = "clock_or_identity_error"
			w.Notes = append(w.Notes, "WAL identity mismatch or non-monotonic commit clock.")
		}
		if len(gaps) > 0 {
			w.Status = "gap"
			w.Notes = append(w.Notes, "Only the continuous prefix before the first gap can be used.")
		}
		if !point.Hold && !result.At.Before(point.ExpiresAt) {
			w.Status = "expired"
		}
		result.Windows = append(result.Windows, w)
	}
	sort.Slice(result.Windows, func(i, j int) bool { return result.Windows[i].From.Before(result.Windows[j].From) })
	return result, nil
}
