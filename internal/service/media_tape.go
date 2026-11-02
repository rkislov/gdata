// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/tape"
	"net/http"
	"time"
)

func (m *Media) tapeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /v1/tape/pools/{pool}", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager"); e != nil {
			Fail(w, 403, e)
			return
		}
		pools, ok := m.Tape.(tape.PoolController)
		if !ok {
			Fail(w, 409, errors.New("tape pools unavailable"))
			return
		}
		var p tape.Pool
		if e := Decode(w, r, &p); e != nil {
			Fail(w, 400, e)
			return
		}
		if p.ID != r.PathValue("pool") {
			Fail(w, 400, errors.New("pool ID mismatch"))
			return
		}
		if e := pools.SetPool(r.Context(), p); e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, p)
	})

	mux.HandleFunc("GET /v1/tape/capabilities", func(w http.ResponseWriter, r *http.Request) {
		if e := Diagnostic(r, m.Config); e != nil {
			Fail(w, 403, e)
			return
		}
		JSON(w, 200, map[string]any{"stage": func() string {
			if m.Tape != nil {
				return m.Config.Tape.Backend
			}
			return "unconfigured"
		}(), "scsi_transport": m.Config.Tape != nil && m.Config.Tape.Backend == "scsi", "hardware_qualified": false, "actions": func() []string {
			if m.Config.Tape != nil && m.Config.Tape.Backend == "scsi" {
				return tape.SCSIActions()
			}
			return nil
		}(), "writes": m.Tape != nil, "inventory": m.Tape != nil})
	})
	mux.HandleFunc("GET /v1/tape", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager", "admin", "auditor"); e != nil {
			Fail(w, 403, e)
			return
		}
		if m.Tape == nil {
			Fail(w, 409, errors.New("tape library is not configured"))
			return
		}
		v, e := m.Tape.Inventory(r.Context())
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, v)
	})
	mux.HandleFunc("GET /v1/tape/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager", "admin", "auditor"); e != nil {
			Fail(w, 403, e)
			return
		}
		m.replicationMu.Lock()
		v, ok := m.replications[r.PathValue("id")]
		m.replicationMu.Unlock()
		if !ok {
			Fail(w, 404, errors.New("tape operation not active"))
			return
		}
		JSON(w, 200, v)
	})
	mux.HandleFunc("POST /v1/tape/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager"); e != nil {
			Fail(w, 403, e)
			return
		}
		if m.Tape == nil {
			Fail(w, 409, errors.New("tape library is not configured"))
			return
		}
		id := r.PathValue("id")
		if !davID.MatchString(id) {
			Fail(w, 400, errors.New("invalid operation ID"))
			return
		}
		var req model.TapeRequest
		if e := Decode(w, r, &req); e != nil {
			Fail(w, 400, e)
			return
		}
		m.replicationMu.Lock()
		if m.replications == nil {
			m.replications = map[string]model.Result{}
		}
		if _, ok := m.replications[id]; ok {
			m.replicationMu.Unlock()
			Fail(w, 409, errors.New("operation already active"))
			return
		}
		m.replications[id] = model.Result{Phase: "tape-start"}
		m.replicationMu.Unlock()
		defer func() { m.replicationMu.Lock(); delete(m.replications, id); m.replicationMu.Unlock() }()
		report := func(v model.Result) {
			if v.Tape != nil {
				p := *v.Tape
				v.Tape = &p
			}
			m.replicationMu.Lock()
			m.replications[id] = v
			m.replicationMu.Unlock()
		}
		object := req.Barcode
		if req.PoolID != "" {
			object = "pool/" + req.PoolID
		}
		audit := model.RepositoryAudit{ID: id, StartedAt: time.Now().UTC(), Action: "tape." + req.Action, Object: object, Actor: "manager", Phase: "started"}
		if e := m.Repo.RecordAudit(audit); e != nil {
			Fail(w, 500, e)
			return
		}
		res, e := m.Tape.Execute(r.Context(), id, req, m.Repo, report)
		at := time.Now().UTC()
		audit.FinishedAt = &at
		audit.Phase = "complete"
		if e != nil {
			audit.Phase = "failed"
			audit.Error = e.Error()
		}
		_ = m.Repo.RecordAudit(audit)
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, res)
	})
}
