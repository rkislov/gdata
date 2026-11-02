// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"encoding/json"
	"gdata/srk/internal/config"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var walCommitLine = regexp.MustCompile(`len \(rec/tot\):\s*[0-9]+/\s*([0-9]+).*lsn: ([0-9A-F]+)/([0-9A-F]+),.*desc: COMMIT(?:_PREPARED)?(?: [0-9]+:)? ([0-9-]+ [0-9:.]+) UTC`)

type walTimeWriter struct {
	pending     []byte
	first, last *time.Time
	bad         bool
	start, end  uint64
}

func (w *walTimeWriter) Write(b []byte) (int, error) {
	for _, c := range b {
		if c == '\n' {
			m := walCommitLine.FindSubmatch(w.pending)
			if strings.Contains(string(w.pending), "desc: COMMIT") && len(m) != 5 {
				w.bad = true
			}
			if len(m) == 5 {
				length, _ := strconv.ParseUint(string(m[1]), 10, 64)
				hi, _ := strconv.ParseUint(string(m[2]), 16, 32)
				lo, _ := strconv.ParseUint(string(m[3]), 16, 32)
				lsn := hi<<32 | lo
				if lsn >= w.start && lsn < w.end && length < 1<<30 && lsn+length+(length/8192+2)*40 < w.end {
					at, e := time.Parse("2006-01-02 15:04:05.999999", string(m[4]))
					if e != nil {
						w.bad = true
					}
					if e == nil {
						if w.last != nil && at.Before(*w.last) {
							w.bad = true
						}
						if w.first == nil {
							v := at
							w.first = &v
						}
						v := at
						w.last = &v
					}
				}
			}
			w.pending = w.pending[:0]
		} else if len(w.pending) < 65536 {
			w.pending = append(w.pending, c)
		}
	}
	return len(b), nil
}
func inspectWALTimes(ctx context.Context, p config.PostgreSQLProfile, meta *model.WALRecord) {
	if len(meta.Name) != 24 {
		return
	}
	start, e := repository.WALPosition(meta.Name, meta.SegmentSize)
	if e != nil {
		return
	}
	writer := &walTimeWriter{start: start, end: start + uint64(meta.SegmentSize)}
	cmd := exec.CommandContext(ctx, filepath.Join(p.BinDir, "pg_waldump"), "--rmgr=Transaction", filepath.Join(p.WALArchive.DataDir, "pg_wal", meta.Name))
	env := postgresEnvironment(p)
	filtered := []string{}
	for _, v := range env {
		if !strings.HasPrefix(v, "TZ=") {
			filtered = append(filtered, v)
		}
	}
	cmd.Env = append(filtered, "TZ=UTC")
	cmd.Stdout = writer
	if e = cmd.Run(); e == nil && !writer.bad {
		meta.TimeBoundsVerified = true
		meta.FirstCommitAt = writer.first
		meta.LastCommitAt = writer.last
	}
}
func (a *Agent) pinBackupWAL(ctx context.Context, v model.Execute, p config.PostgreSQLProfile, profile, system string) (func(), error) {
	archive := repository.WALArchiveID(a.Config.ID, profile, system)
	release := func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		r, e := a.media(c, v, "DELETE", "/v1/wal/"+archive+"/pins/"+v.RunID, nil)
		if e == nil {
			r.Body.Close()
		}
	}
	b, e := json.Marshal(map[string]any{"profile": profile, "system_id": system, "expires_at": time.Now().UTC().Add(time.Duration(p.TimeoutSeconds)*time.Second + 10*time.Minute)})
	if e != nil {
		return release, e
	}
	r, e := a.media(ctx, v, "POST", "/v1/wal/"+archive+"/pins/"+v.RunID, b)
	if e == nil {
		r.Body.Close()
	}
	return release, e
}
