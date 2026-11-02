// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"errors"
	"gdata/srk/internal/catalogbackup"
	"net/http"
	"time"
)

func (m *Manager) catalogBackupRoutes(mux *http.ServeMux) {
	m.catalogMaintenanceRoutes(mux)
	mux.HandleFunc("GET /v1/catalog-backups", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		state, e := m.DB.RecoveryState(r.Context())
		if e != nil {
			Fail(w, 503, errors.New("catalog unavailable"))
			return
		}
		out := map[string]any{"enabled": m.CatalogBackups != nil, "recovery": state, "installation": m.Config.Installation, "backups": []catalogbackup.Entry{}}
		if m.CatalogBackups != nil {
			out["backups"] = m.CatalogBackups.List()
			out["policy"] = m.CatalogBackups.Policy()
			out["operations"] = m.CatalogBackups.Operations()
			out["drill_enabled"] = m.Config.CatalogBackup.DrillDatabaseURLFile != ""
		}
		JSON(w, 200, out)
	}))
	mux.HandleFunc("POST /v1/catalog-backups", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		if m.CatalogBackups == nil {
			Fail(w, 409, errors.New("catalog backup storage/key not configured"))
			return
		}
		if e := m.DB.Audit(r.Context(), actor, "catalog.backup.requested", "catalog", "requested"); e != nil {
			Fail(w, 503, errors.New("audit unavailable"))
			return
		}
		v, e := m.CatalogBackups.Start(actor)
		if e != nil {
			Fail(w, 409, e)
			return
		}
		JSON(w, 202, v)
	}))
	mux.HandleFunc("PUT /v1/catalog-backups/policy", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		if m.CatalogBackups == nil {
			Fail(w, 409, errors.New("catalog backup not configured"))
			return
		}
		var v catalogbackup.Policy
		if e := Decode(w, r, &v); e != nil {
			Fail(w, 400, e)
			return
		}
		if v.TargetMediaID != "" {
			node, e := m.DB.Node(r.Context(), v.TargetMediaID)
			if e != nil || node.Role != "media" {
				Fail(w, 400, errors.New("registered target media required"))
				return
			}
		}
		if e := m.CatalogBackups.SetPolicy(v); e != nil {
			Fail(w, 409, e)
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "catalog.backup.policy", "catalog", "updated")
		JSON(w, 200, m.CatalogBackups.Policy())
	}))
	mux.HandleFunc("POST /v1/catalog-backups/{id}/verify", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		if m.CatalogBackups == nil {
			Fail(w, 409, errors.New("catalog backup not configured"))
			return
		}
		v, e := m.CatalogBackups.Verify(r.Context(), r.PathValue("id"))
		if e != nil {
			_ = m.DB.Audit(r.Context(), actor, "catalog.backup.verify", r.PathValue("id"), "failed")
			Fail(w, 422, errors.New("archive verification failed"))
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "catalog.backup.verify", v.ID, "success")
		JSON(w, 200, v)
	}))
	mux.HandleFunc("GET /v1/catalog-backups/{id}/download", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		if m.CatalogBackups == nil {
			Fail(w, 404, errors.New("catalog backup not configured"))
			return
		}
		f, e := m.CatalogBackups.Archive(r.PathValue("id"))
		if e != nil {
			Fail(w, 404, e)
			return
		}
		defer f.Close()
		if e = m.DB.Audit(r.Context(), actor, "catalog.backup.download", r.PathValue("id"), "requested"); e != nil {
			Fail(w, 503, errors.New("audit unavailable"))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+r.PathValue("id")+".srkcat\"")
		http.ServeContent(w, r, "catalog.srkcat", time.Time{}, f)
	}))
	mux.HandleFunc("POST /v1/catalog-backups/recovery/acknowledge", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		var v struct {
			ArchiveID string `json:"archive_id"`
			Confirm   string `json:"confirm_installation"`
		}
		if e := Decode(w, r, &v); e != nil {
			Fail(w, 400, e)
			return
		}
		if v.Confirm != m.Config.Installation {
			Fail(w, 400, errors.New("installation confirmation required"))
			return
		}
		if e := m.DB.AcknowledgeRecovery(r.Context(), v.ArchiveID, actor); e != nil {
			Fail(w, 409, e)
			return
		}
		state, e := m.DB.RecoveryState(r.Context())
		respond(w, state, e)
	}))
}
