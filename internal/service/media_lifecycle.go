// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"net/http"
	"strings"
	"time"
)

func maintenanceError(w http.ResponseWriter, e error) {
	status := 422
	if errors.Is(e, repository.ErrPlanStale) {
		status = 409
	}
	Fail(w, status, e)
}
func (m *Media) maintenanceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/repository/audit", func(w http.ResponseWriter, r *http.Request) {
		if e := Diagnostic(r, m.Config); e != nil {
			Fail(w, 403, e)
			return
		}
		a, e := m.Repo.Audits()
		if e != nil {
			maintenanceError(w, e)
			return
		}
		JSON(w, 200, a)
	})
	mux.HandleFunc("GET /v1/repository/snapshots", func(w http.ResponseWriter, r *http.Request) {
		if e := Diagnostic(r, m.Config); e != nil {
			Fail(w, 403, e)
			return
		}
		v, e := m.Repo.Inventory(r.URL.Query().Get("verify") == "true")
		if e != nil {
			maintenanceError(w, e)
			return
		}
		JSON(w, 200, map[string]any{"repository": m.Repo.Info, "snapshots": v})
	})
	mux.HandleFunc("GET /v1/repository/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		if e := Diagnostic(r, m.Config); e != nil {
			Fail(w, 403, e)
			return
		}
		v, e := m.Repo.Snapshot(r.PathValue("id"), false)
		if e != nil {
			maintenanceError(w, e)
			return
		}
		JSON(w, 200, v)
	})
	mux.HandleFunc("POST /v1/repository/snapshots/{id}/policy", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager", "admin"); e != nil {
			Fail(w, 403, e)
			return
		}
		var p model.SnapshotPolicy
		if e := Decode(w, r, &p); e != nil {
			Fail(w, 400, e)
			return
		}
		var v model.SnapshotInfo
		e := m.audited(r, "policy", r.PathValue("id"), func() error { var err error; v, err = m.Repo.UpdatePolicy(r.PathValue("id"), p); return err })
		if e != nil {
			maintenanceError(w, e)
			return
		}
		JSON(w, 200, v)
	})
	mux.HandleFunc("GET /v1/repository/gc", func(w http.ResponseWriter, r *http.Request) {
		if e := Diagnostic(r, m.Config); e != nil {
			Fail(w, 403, e)
			return
		}
		v, e := m.Repo.PreviewGC()
		if e != nil {
			maintenanceError(w, e)
			return
		}
		JSON(w, 200, v)
	})
	mux.HandleFunc("POST /v1/repository/gc", func(w http.ResponseWriter, r *http.Request) {
		peer, e := Role(r, "manager", "admin")
		if e != nil {
			Fail(w, 403, e)
			return
		}
		var req struct {
			OperationID string        `json:"operation_id"`
			Plan        *model.GCPlan `json:"plan,omitempty"`
			Automatic   bool          `json:"automatic,omitempty"`
		}
		if e = Decode(w, r, &req); e != nil {
			Fail(w, 400, e)
			return
		}
		var report model.MaintenanceResult
		if !(req.Automatic && peer.Role == "manager") && req.Plan == nil {
			Fail(w, 400, errors.New("cleanup preview and operation ID required"))
			return
		}
		e = m.audited(r, "gc", m.Repo.Info.ID, func() error {
			var err error
			if req.Automatic && peer.Role == "manager" {
				report, err = m.Repo.AutomaticGC(req.OperationID)
			} else {
				report, err = m.Repo.ApplyGC(*req.Plan, req.OperationID)
			}
			return err
		})
		if e != nil {
			maintenanceError(w, e)
			return
		}
		JSON(w, 200, report)
	})
	mux.HandleFunc("POST /v1/repository/import", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager", "admin"); e != nil {
			Fail(w, 403, e)
			return
		}
		var req struct {
			SnapshotIDs   []string `json:"snapshot_ids"`
			RetentionDays int      `json:"retention_days"`
		}
		if e := Decode(w, r, &req); e != nil {
			Fail(w, 400, e)
			return
		}
		var v []model.SnapshotInfo
		e := m.audited(r, "import", strings.Join(req.SnapshotIDs, ","), func() error {
			var err error
			v, err = m.Repo.PrepareImport(req.SnapshotIDs, req.RetentionDays)
			return err
		})
		if e != nil {
			maintenanceError(w, e)
			return
		}
		JSON(w, 200, map[string]any{"repository": m.Repo.Info, "snapshots": v})
	})
}
func (m *Media) audited(r *http.Request, action, object string, fn func() error) error {
	roles := []string{"manager", "admin"}
	if action == "wal.archive" {
		roles = []string{"agent"}
	}
	peer, e := Role(r, roles...)
	if e != nil {
		return e
	}
	id := r.Header.Get("X-Operation-ID")
	if action == "wal.archive" {
		id = model.ID()
	}
	if id == "" {
		id = model.ID()
	}
	a := model.RepositoryAudit{ID: id, Actor: peer.Role + "/" + peer.ID, Action: action, Object: object, StartedAt: time.Now().UTC(), Phase: "started"}
	if e = m.Repo.RecordAudit(a); e != nil {
		return e
	}
	e = fn()
	finished := time.Now().UTC()
	a.FinishedAt = &finished
	a.Phase = "success"
	if e != nil {
		a.Phase = "error"
		a.Error = e.Error()
	}
	if ae := m.Repo.RecordAudit(a); ae != nil && e == nil {
		return fmt.Errorf("operation completed but audit finalization failed: %w", ae)
	}
	return e
}
