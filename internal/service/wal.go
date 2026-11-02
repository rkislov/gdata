// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
)

type postgresControl struct {
	SystemID    string
	SegmentSize int64
	Timeline    int
	State       string
}

func readPostgresControl(ctx context.Context, p config.PostgreSQLProfile, data string) (postgresControl, error) {
	var result postgresControl
	cmd := exec.CommandContext(ctx, filepath.Join(p.BinDir, "pg_controldata"), "-D", data)
	cmd.Env = postgresEnvironment(p)
	b, e := cmd.Output()
	if e != nil {
		return result, errors.New("cannot inspect PostgreSQL control data")
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	result.SystemID = values["Database system identifier"]
	id, err := strconv.ParseUint(result.SystemID, 10, 64)
	if err != nil || id == 0 {
		return result, errors.New("invalid PostgreSQL system identifier")
	}
	result.SegmentSize, e = strconv.ParseInt(values["Bytes per WAL segment"], 10, 64)
	if e != nil || !repository.WALSegmentSize(result.SegmentSize) {
		return result, errors.New("invalid PostgreSQL WAL geometry")
	}
	result.Timeline, _ = strconv.Atoi(values["Latest checkpoint's TimeLineID"])
	result.State = values["Database cluster state"]
	return result, nil
}
func (m *Media) walReader(r *http.Request, archive string) error {
	if e := Diagnostic(r, m.Config); e == nil {
		return nil
	}
	g, e := m.authorize(r)
	if e != nil {
		return e
	}
	if g.Permission != "read" || g.ArchiveID != archive {
		return errors.New("WAL read permission does not match archive")
	}
	base, e := m.Repo.Manifest(g.SnapshotID)
	if e != nil {
		return e
	}
	if base.PostgreSQL == nil || base.PostgreSQL.ArchiveID != archive {
		return errors.New("WAL archive does not match base backup")
	}
	return nil
}
func (m *Media) walRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/wal/{archive}/calendar", func(w http.ResponseWriter, r *http.Request) {
		if e := m.walReader(r, r.PathValue("archive")); e != nil {
			Fail(w, 403, e)
			return
		}
		v, e := m.Repo.WALCalendar(r.PathValue("archive"))
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, v)
	})
	mux.HandleFunc("POST /v1/wal/{archive}/pins/{id}", func(w http.ResponseWriter, r *http.Request) {
		peer, e := Role(r, "agent")
		if e != nil {
			Fail(w, 403, e)
			return
		}
		var req struct {
			Profile   string    `json:"profile"`
			SystemID  string    `json:"system_id"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if e = Decode(w, r, &req); e != nil {
			Fail(w, 400, e)
			return
		}
		if repository.WALArchiveID(peer.ID, req.Profile, req.SystemID) != r.PathValue("archive") {
			Fail(w, 403, errors.New("WAL pin namespace mismatch"))
			return
		}
		e = m.Repo.PinWAL(model.WALPin{ID: r.PathValue("id"), ArchiveID: r.PathValue("archive"), AgentID: peer.ID, ExpiresAt: req.ExpiresAt})
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, map[string]bool{"pinned": true})
	})
	mux.HandleFunc("DELETE /v1/wal/{archive}/pins/{id}", func(w http.ResponseWriter, r *http.Request) {
		peer, e := Role(r, "agent")
		if e != nil {
			Fail(w, 403, e)
			return
		}
		e = m.Repo.ReleaseWALPin(r.PathValue("archive"), r.PathValue("id"), peer.ID)
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, map[string]bool{"released": true})
	})
	mux.HandleFunc("GET /v1/wal/{archive}/gc", func(w http.ResponseWriter, r *http.Request) {
		if e := Diagnostic(r, m.Config); e != nil {
			Fail(w, 403, e)
			return
		}
		before, e := time.Parse(time.RFC3339Nano, r.URL.Query().Get("before"))
		if e != nil {
			Fail(w, 400, e)
			return
		}
		v, e := m.Repo.PreviewWALGC(r.PathValue("archive"), before.UTC(), r.URL.Query().Get("allow_orphan") == "true")
		if e != nil {
			maintenanceError(w, e)
			return
		}
		JSON(w, 200, v)
	})
	mux.HandleFunc("POST /v1/wal/{archive}/gc", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager", "admin"); e != nil {
			Fail(w, 403, e)
			return
		}
		var req struct {
			OperationID string          `json:"operation_id"`
			Plan        model.WALGCPlan `json:"plan"`
		}
		if e := Decode(w, r, &req); e != nil {
			Fail(w, 400, e)
			return
		}
		if req.Plan.ArchiveID != r.PathValue("archive") {
			Fail(w, 400, errors.New("WAL plan archive mismatch"))
			return
		}
		var v model.MaintenanceResult
		e := m.audited(r, "wal.gc", req.Plan.ArchiveID, func() error { var err error; v, err = m.Repo.ApplyWALGC(req.Plan, req.OperationID); return err })
		if e != nil {
			maintenanceError(w, e)
			return
		}
		JSON(w, 200, v)
	})
	mux.HandleFunc("PUT /v1/wal/{archive}/{name}", func(w http.ResponseWriter, r *http.Request) {
		peer, e := Role(r, "agent")
		if e != nil {
			Fail(w, 403, e)
			return
		}
		var meta model.WALRecord
		if e = json.Unmarshal([]byte(r.Header.Get("X-SRK-WAL")), &meta); e != nil {
			Fail(w, 400, errors.New("WAL metadata required"))
			return
		}
		if meta.AgentID != peer.ID || meta.ArchiveID != r.PathValue("archive") || meta.Name != r.PathValue("name") {
			Fail(w, 403, errors.New("WAL writer identity mismatch"))
			return
		}
		var v model.WALRecord
		e = m.audited(r, "wal.archive", meta.ArchiveID+"/"+meta.Name, func() error { var err error; v, err = m.Repo.PutWAL(meta, r.Body); return err })
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, v)
	})
	mux.HandleFunc("GET /v1/wal/{archive}", func(w http.ResponseWriter, r *http.Request) {
		if e := m.walReader(r, r.PathValue("archive")); e != nil {
			Fail(w, 403, e)
			return
		}
		v, e := m.Repo.WALInventory(r.PathValue("archive"))
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, v)
	})
	mux.HandleFunc("GET /v1/wal/{archive}/{name}", func(w http.ResponseWriter, r *http.Request) {
		if e := m.walReader(r, r.PathValue("archive")); e != nil {
			Fail(w, 403, e)
			return
		}
		f, meta, e := m.Repo.OpenWAL(r.PathValue("archive"), r.PathValue("name"))
		if e != nil {
			Fail(w, 422, e)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-SRK-SHA256", meta.SHA256)
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Bytes, 10))
		_, _ = io.Copy(w, f)
	})
}

type walArchiveStatus struct {
	MediaID     string    `json:"media_id"`
	At          time.Time `json:"at"`
	LastSuccess time.Time `json:"last_success"`
	Name        string    `json:"name"`
	ArchiveID   string    `json:"archive_id"`
	Error       string    `json:"error,omitempty"`
}

func (a *Agent) walStatus() map[string]walArchiveStatus {
	out := map[string]walArchiveStatus{}
	for name, p := range a.Config.PostgreSQL {
		if p.WALArchive != nil {
			var v walArchiveStatus
			b, e := os.ReadFile(filepath.Join(p.StagingDir, "wal-status-"+name+".json"))
			if e == nil {
				_ = json.Unmarshal(b, &v)
			}
			v.MediaID = p.WALArchive.MediaID
			out[name] = v
		}
	}
	return out
}
func (a *Agent) walRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/postgresql/{profile}/archive", func(w http.ResponseWriter, r *http.Request) {
		peer, e := Role(r, "wal", "admin")
		if e != nil || peer.Role == "wal" && peer.ID != a.Config.ID+"-wal" {
			Fail(w, 403, errors.New("WAL archive identity is not authorized"))
			return
		}
		var req struct {
			Name string `json:"name"`
			File string `json:"file"`
		}
		if e = Decode(w, r, &req); e != nil {
			Fail(w, 400, e)
			return
		}
		v, e := a.archiveWAL(r.Context(), r.PathValue("profile"), req.File, req.Name)
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, v)
	})
}
func (a *Agent) archiveWAL(ctx context.Context, profile, file, name string) (result model.WALRecord, err error) {
	p, ok := a.Config.PostgreSQL[profile]
	if !ok || p.WALArchive == nil {
		return result, errors.New("WAL archive profile is not configured")
	}
	if !repository.WALName(name) {
		return result, errors.New("invalid WAL archive name")
	}
	expected := filepath.Join(p.WALArchive.DataDir, "pg_wal", name)
	if filepath.Clean(file) != filepath.Join("pg_wal", name) && filepath.Clean(file) != expected {
		return result, errors.New("WAL source must match the configured cluster pg_wal path")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	state := a.walStatus()[profile]
	state.At = time.Now().UTC()
	state.Name = name
	defer func() {
		if err != nil {
			state.Error = err.Error()
		} else {
			state.Error = ""
			state.LastSuccess = time.Now().UTC()
			state.ArchiveID = result.ArchiveID
		}
		b, _ := json.Marshal(state)
		if e := repository.Atomic(filepath.Join(p.StagingDir, "wal-status-"+profile+".json"), b, 0600); e != nil && err == nil {
			err = errors.New("WAL stored but local status could not be persisted; retry is safe")
		}
	}()
	control, e := readPostgresControl(ctx, p, p.WALArchive.DataDir)
	if e != nil {
		return result, e
	}
	version, e := os.ReadFile(filepath.Join(p.WALArchive.DataDir, "PG_VERSION"))
	if e != nil {
		return result, e
	}
	major, e := strconv.Atoi(strings.TrimSpace(string(version)))
	if e != nil {
		return result, e
	}
	if e = postgresTools(ctx, p, major); e != nil {
		return result, e
	}
	root, e := os.OpenRoot(filepath.Join(p.WALArchive.DataDir, "pg_wal"))
	if e != nil {
		return result, e
	}
	defer root.Close()
	fd, e := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return result, e
	}
	defer fd.Close()
	info, e := fd.Stat()
	if e != nil {
		return result, e
	}
	if !info.Mode().IsRegular() {
		return result, errors.New("WAL must be a regular file")
	}
	meta := model.WALRecord{ArchiveID: repository.WALArchiveID(a.Config.ID, profile, control.SystemID), AgentID: a.Config.ID, Profile: profile, SystemID: control.SystemID, Major: major, SegmentSize: control.SegmentSize, Name: name}
	inspectWALTimes(ctx, p, &meta)
	cl, e := identity.Client(a.Config, identity.Peer{Role: "media", ID: p.WALArchive.MediaID})
	if e != nil {
		return result, e
	}
	defer cl.CloseIdleConnections()
	cl.Transport.(*http.Transport).ResponseHeaderTimeout = 0
	req, e := http.NewRequestWithContext(ctx, "PUT", p.WALArchive.MediaURL+"/v1/wal/"+meta.ArchiveID+"/"+name, fd)
	if e != nil {
		return result, e
	}
	req.ContentLength = info.Size()
	b, _ := json.Marshal(meta)
	req.Header.Set("X-SRK-WAL", string(b))
	req.Header.Set("Content-Type", "application/octet-stream")
	response, e := cl.Do(req)
	if e != nil {
		return result, e
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return result, fmt.Errorf("media rejected WAL archive (HTTP %d)", response.StatusCode)
	}
	if e = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); e != nil {
		return result, e
	}
	if result.ArchiveID != meta.ArchiveID || result.Name != name || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(result.SHA256) {
		return result, errors.New("invalid media WAL acknowledgement")
	}
	return result, nil
}
