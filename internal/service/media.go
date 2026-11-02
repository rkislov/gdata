// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gdata/srk/internal/catalogbackup"
	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/removable"
	"gdata/srk/internal/repository"
	"gdata/srk/internal/tape"
)

type Media struct {
	CatalogCopies *catalogbackup.ReplicaStore
	Removable     map[string]*removable.Volume
	Tape          tape.Backend
	replicationMu sync.Mutex
	replications  map[string]model.Result
	DAV           *davStore
	membersMu     sync.Mutex
	members       map[string]*manifestMembership
	statsMu       sync.RWMutex
	cachedStats   map[string]any
	Captures      *repository.CaptureStore
	CaptureClient *http.Client
	Config        config.Config
	Repo          *repository.Repository
	Key           []byte
}

func NewMedia(c config.Config) (*Media, error) {
	r, e := repository.Open(c.Repository)
	if e != nil {
		return nil, e
	}
	r.ReserveBytes = c.ReserveBytes
	k, e := os.ReadFile(c.CapabilityKeyFile)
	if e != nil {
		r.Close()
		return nil, e
	}
	if len(k) < 32 {
		r.Close()
		return nil, errors.New("capability key too short")
	}
	cl, e := identity.Client(c, identity.Peer{Role: "manager"})
	if e != nil {
		r.Close()
		return nil, e
	}
	m := &Media{members: map[string]*manifestMembership{}, Config: c, Repo: r, Key: k, Captures: repository.NewCaptureStore(r), CaptureClient: cl}
	m.CatalogCopies, e = catalogbackup.OpenReplicas(filepath.Join(r.Root, "catalog-copies"))
	if e != nil {
		r.Close()
		return nil, e
	}
	if c.Tape != nil {
		m.Tape, e = tape.OpenBackend(filepath.Join(r.Root, "tape-"+c.Tape.Backend), *c.Tape)
		if e != nil {
			r.Close()
			return nil, e
		}
	}
	m.Removable = map[string]*removable.Volume{}
	for _, v := range c.Removable {
		volume, err := removable.Open(v, filepath.Join(r.Root, "removable-catalog", v.ID+".json"))
		if err != nil {
			r.Close()
			return nil, err
		}
		m.Removable[v.ID] = volume
	}
	m.DAV, e = newDAVStore(m)
	if e != nil {
		r.Close()
		return nil, e
	}
	return m, nil
}
func (m *Media) grant(r *http.Request) (identity.Grant, error) {
	p, e := Role(r, "agent", "proxy", "manager", "media")
	if e != nil {
		return identity.Grant{}, e
	}
	g, e := identity.Verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), m.Key)
	if e != nil {
		return g, e
	}
	if g.Audience != "" && g.Audience != m.Config.ID {
		return g, errors.New("capability audience mismatch")
	}
	if p.Role == "media" && (g.Permission != "read" || g.Audience != m.Config.ID) {
		return g, errors.New("media replication requires audience-bound read capability")
	}
	if p.Role == "manager" && g.Permission != "read" {
		return g, errors.New("manager capabilities are read-only")
	}
	if g.Subject != p.ID {
		return g, errors.New("capability subject mismatch")
	}
	return g, nil
}
func (m *Media) authorize(r *http.Request) (identity.Grant, error) {
	g, e := m.grant(r)
	if e != nil {
		return g, e
	}
	e = m.Repo.BeginLease(model.Lease{ID: g.RunID, Subject: g.Subject, Kind: g.Permission, SnapshotID: g.SnapshotID, Base: g.Base, ExpiresAt: time.Unix(g.Expires, 0).UTC()})
	return g, e
}
func (m *Media) Handler() http.Handler {
	mux := http.NewServeMux()
	m.catalogCopyRoutes(mux)
	m.walAckRoutes(mux)
	m.walReplicationRoutes(mux)
	m.replicationRoutes(mux)
	m.captureRoutes(mux)
	m.davRoutes(mux)
	m.removableRoutes(mux)
	m.tapeRoutes(mux)
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		if e := Diagnostic(r, m.Config); e != nil {
			Fail(w, 403, e)
			return
		}
		s := LocalStatus(m.Config)
		s["rescue_capture_enabled"] = m.Config.RescueCaptureEnabled
		s["webdav"] = m.davStatus()
		v, e := m.Repo.Stats()
		if e != nil {
			Fail(w, 500, e)
			return
		}
		s["repository"] = v
		s["adapters"] = map[string]string{"disk": "ready", "tape": "unconfigured"}
		if m.Tape != nil {
			s["adapters"].(map[string]string)["tape"] = m.Config.Tape.Backend
		}
		JSON(w, 200, s)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if e := Diagnostic(r, m.Config); e != nil {
			Fail(w, 403, e)
			return
		}
		if e := m.Repo.Check(); e != nil {
			Fail(w, 503, e)
			return
		}
		JSON(w, 200, LocalStatus(m.Config))
	})
	mux.HandleFunc("/v1/chunks/{hash}", func(w http.ResponseWriter, r *http.Request) {
		g, e := m.authorize(r)
		if e != nil {
			Fail(w, 403, e)
			return
		}
		h := r.PathValue("hash")
		switch r.Method {
		case "HEAD":
			if g.Permission != "write" {
				Fail(w, 403, errors.New("write grant required"))
				return
			}
			if !m.Repo.Has(h) {
				w.WriteHeader(404)
				return
			}
			w.WriteHeader(200)
		case "PUT":
			if g.Permission != "write" {
				Fail(w, 403, errors.New("write grant required"))
				return
			}
			b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, repository.ChunkSize))
			if e != nil {
				Fail(w, 400, e)
				return
			}
			if e = m.Repo.Put(h, b); e != nil {
				Fail(w, 400, e)
				return
			}
			JSON(w, 201, map[string]bool{"stored": true})
		case "GET":
			if g.Permission != "read" {
				Fail(w, 403, errors.New("read grant required"))
				return
			}
			found, e := m.snapshotContains(g.SnapshotID, h)
			if e != nil {
				Fail(w, 404, e)
				return
			}
			if !found {
				Fail(w, 403, errors.New("chunk is outside granted snapshot"))
				return
			}
			b, e := m.Repo.Get(h)
			if e != nil {
				Fail(w, 500, e)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(b)
		default:
			w.WriteHeader(405)
		}
	})
	mux.HandleFunc("GET /v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		g, e := m.authorize(r)
		if e != nil {
			Fail(w, 403, e)
			return
		}
		id := r.PathValue("id")
		if !(g.Permission == "read" && id == g.SnapshotID || g.Permission == "write" && id == g.Base && g.Base != "") {
			Fail(w, 403, errors.New("snapshot outside grant"))
			return
		}
		v, e := m.Repo.Manifest(id)
		if e != nil {
			Fail(w, 404, e)
			return
		}
		JSON(w, 200, v)
	})
	mux.HandleFunc("POST /v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		g, e := m.authorize(r)
		if e != nil {
			Fail(w, 403, e)
			return
		}
		if g.Permission != "write" || r.PathValue("id") != g.RunID {
			Fail(w, 403, errors.New("snapshot outside grant"))
			return
		}
		var v model.Manifest
		if e = decodeManifest(w, r, &v); e != nil {
			Fail(w, 400, e)
			return
		}
		if v.ID != g.RunID {
			Fail(w, 400, errors.New("snapshot identity mismatch"))
			return
		}
		if e = m.Repo.CommitLease(v, g.RunID); e != nil {
			Fail(w, 409, e)
			return
		}
		JSON(w, 201, map[string]any{"verified": true, "repository_id": m.Repo.Info.ID})
	})
	mux.HandleFunc("POST /v1/snapshots/{id}/verify", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager", "admin"); e != nil {
			g, ge := m.authorize(r)
			peer, pe := Role(r, "proxy")
			_ = peer
			if ge != nil || pe != nil || g.Permission != "read" || g.SnapshotID != r.PathValue("id") {
				Fail(w, 403, errors.New("snapshot verification outside proxy read grant"))
				return
			}
		}
		if e := m.Repo.Verify(r.PathValue("id")); e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, map[string]bool{"verified": true})
	})
	mux.HandleFunc("POST /v1/operations/{id}/release", func(w http.ResponseWriter, r *http.Request) {
		g, e := m.grant(r)
		if e != nil {
			Fail(w, 403, e)
			return
		}
		if r.PathValue("id") != g.RunID {
			Fail(w, 403, errors.New("lease outside grant"))
			return
		}
		if e = m.Repo.EndLease(g.RunID); e != nil {
			Fail(w, 409, e)
			return
		}
		JSON(w, 200, map[string]bool{"released": true})
	})
	m.maintenanceRoutes(mux)
	m.walRoutes(mux)
	return mux
}
func (m *Media) Run(ctx context.Context) error {
	if m.DAV != nil {
		child, cancel := context.WithCancel(ctx)
		defer cancel()
		errors := make(chan error, 2)
		go func() { errors <- m.serveDAV(child) }()
		go func() { errors <- m.runCore(child) }()
		return <-errors
	}
	return m.runCore(ctx)
}
func (m *Media) runCore(ctx context.Context) error {
	defer m.Repo.Close()
	defer m.CaptureClient.CloseIdleConnections()
	defer m.Captures.Close()
	go func() { <-ctx.Done(); m.Captures.Close() }()
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			v, e := m.Repo.Stats()
			if e != nil {
				v = map[string]any{"error": e.Error()}
			}
			m.statsMu.Lock()
			m.cachedStats = v
			m.statsMu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tick.C:
				_ = m.CatalogCopies.GC(now)
			}
		}
	}()
	go Heartbeat(ctx, m.Config, func() map[string]any {
		m.statsMu.RLock()
		cached := m.cachedStats
		out := make(map[string]any, len(cached)+2)
		for k, v := range cached {
			out[k] = v
		}
		m.statsMu.RUnlock()
		out["repository_id"] = m.Repo.Info.ID
		out["rescue_capture_enabled"] = m.Config.RescueCaptureEnabled
		out["webdav"] = m.davStatus()
		return out
	})
	return Serve(ctx, m.Config, m.Handler())
}
func Heartbeat(ctx context.Context, c config.Config, status func() map[string]any) {
	if c.ManagerURL == "" {
		return
	}
	cl, e := identity.Client(c, identity.Peer{Role: "manager"})
	if e != nil {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		reqctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		snapshot := LocalStatus(c)
		for key, value := range status() {
			snapshot[key] = value
		}
		_ = Call(reqctx, cl, "POST", c.ManagerURL+"/v1/nodes/heartbeat", model.Node{ID: c.ID, Role: c.Role, URL: c.URL, Status: snapshot}, nil)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
