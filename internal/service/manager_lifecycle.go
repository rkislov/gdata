// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"errors"
	"fmt"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"net/http"
	"net/url"
	"time"
)

type inventoryResponse struct {
	Repository model.RepositoryInfo `json:"repository"`
	Snapshots  []model.SnapshotInfo `json:"snapshots"`
}

func (m *Manager) mediaClient(ctx context.Context, id string) (model.Node, *http.Client, error) {
	n, e := m.DB.Node(ctx, id)
	if e != nil || n.Role != "media" {
		return n, nil, errors.New("registered media server required")
	}
	if time.Since(n.LastSeen) > 30*time.Second {
		return n, nil, errors.New("media server is offline")
	}
	cl, e := identity.Client(m.Config, identity.Peer{Role: "media", ID: n.ID})
	if e == nil {
		cl.Transport.(*http.Transport).ResponseHeaderTimeout = 0
	}
	return n, cl, e
}
func (m *Manager) maintenanceRoutes(mux *http.ServeMux) {
	for _, action := range []string{"calendar", "gc"} {
		mux.HandleFunc("GET /v1/media/{media}/wal/{archive}/"+action, m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
			n, cl, e := m.mediaClient(r.Context(), r.PathValue("media"))
			if e != nil {
				Fail(w, 409, e)
				return
			}
			defer cl.CloseIdleConnections()
			var result any
			e = Call(r.Context(), cl, "GET", n.URL+"/v1/wal/"+url.PathEscape(r.PathValue("archive"))+"/"+action+"?"+r.URL.RawQuery, nil, &result)
			if e != nil {
				Fail(w, 422, e)
				return
			}
			JSON(w, 200, result)
		}))
	}
	mux.HandleFunc("GET /v1/media/{media}/wal/{archive}", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		n, cl, e := m.mediaClient(r.Context(), r.PathValue("media"))
		if e != nil {
			Fail(w, 409, e)
			return
		}
		defer cl.CloseIdleConnections()
		var records []model.WALRecord
		e = Call(r.Context(), cl, "GET", n.URL+"/v1/wal/"+url.PathEscape(r.PathValue("archive")), nil, &records)
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, records)
	}))
	mux.HandleFunc("GET /v1/media/{media}/snapshots", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		n, cl, e := m.mediaClient(r.Context(), r.PathValue("media"))
		if e != nil {
			Fail(w, 409, e)
			return
		}
		defer cl.CloseIdleConnections()
		var v inventoryResponse
		verify := r.URL.Query().Get("verify")
		if !currentUser(r).Global() {
			verify = "false"
		}
		e = Call(r.Context(), cl, "GET", n.URL+"/v1/repository/snapshots?verify="+verify, nil, &v)
		if e != nil {
			Fail(w, 422, errors.New("repository inventory unavailable"))
			return
		}
		if !currentUser(r).Global() {
			v, e = m.scopedRepository(r.Context(), currentUser(r), r.PathValue("media"), v)
			if e != nil {
				Fail(w, 503, errors.New("scoped inventory unavailable"))
				return
			}
		}
		JSON(w, 200, v)
	}))
	mux.HandleFunc("GET /v1/media/{media}/gc", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		n, cl, e := m.mediaClient(r.Context(), r.PathValue("media"))
		if e != nil {
			Fail(w, 409, e)
			return
		}
		defer cl.CloseIdleConnections()
		var p model.GCPlan
		if e = Call(r.Context(), cl, "GET", n.URL+"/v1/repository/gc", nil, &p); e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, p)
	}))
	for _, kind := range []string{"gc", "import", "policy", "wal-gc"} {
		mux.HandleFunc("POST /v1/media/{media}/"+kind, m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
			var o model.MaintenanceOptions
			if e := Decode(w, r, &o); e != nil {
				Fail(w, 400, e)
				return
			}
			o.MediaID = r.PathValue("media")
			if _, e := m.DB.Node(r.Context(), o.MediaID); e != nil {
				Fail(w, 404, errors.New("media server not found"))
				return
			}
			if o.Automatic {
				Fail(w, 400, errors.New("automatic retention is configured on the manager"))
				return
			}
			switch kind {
			case "wal-gc":
				if o.WALPlan == nil || o.WALPlan.Token == "" {
					Fail(w, 400, errors.New("reviewed WAL cleanup plan required"))
					return
				}
			case "gc":
				if o.Plan == nil || o.Plan.Token == "" {
					Fail(w, 400, errors.New("cleanup preview required"))
					return
				}
			case "import":
				if len(o.SnapshotIDs) == 0 || len(o.SnapshotIDs) > 1000 || o.RetentionDays < 1 || o.RetentionDays > 36500 {
					Fail(w, 400, errors.New("select snapshots and a retention period"))
					return
				}
				n, e := m.DB.Node(r.Context(), o.AgentID)
				if e != nil || (n.Role != "agent" && n.Role != "proxy") {
					Fail(w, 400, errors.New("select a registered restore agent"))
					return
				}
			case "policy":
				if o.Policy == nil || o.SnapshotID == "" {
					Fail(w, 400, errors.New("snapshot and policy required"))
					return
				}
			}
			run, e := m.enqueueMaintenance(r.Context(), kind, o, kind+"/"+o.MediaID+"/"+idempotency(r))
			if e != nil {
				Fail(w, 409, e)
				return
			}
			_ = m.DB.Audit(r.Context(), actor, kind+".enqueue", run.ID, "success")
			JSON(w, 202, run)
		}))
	}
}
func (m *Manager) enqueueMaintenance(ctx context.Context, kind string, o model.MaintenanceOptions, key string) (model.Run, error) {
	names := map[string]string{"gc": "Очистка хранилища", "import": "Импорт репозитория", "policy": "Изменение хранения", "wal-gc": "Очистка WAL"}
	j := model.Job{ID: "repository/" + o.MediaID, Name: names[kind] + ": " + o.MediaID, MediaID: o.MediaID, Source: "repository://" + o.MediaID}
	run := model.Run{ID: model.ID(), JobID: j.ID, Job: j, Kind: kind, Status: "queued", CreatedAt: time.Now().UTC(), Options: &o}
	return m.DB.Enqueue(ctx, run, key)
}
func (m *Manager) executeMaintenance(ctx context.Context, r *model.Run) error {
	o := r.Options
	if o == nil {
		return errors.New("maintenance options absent")
	}
	n, cl, e := m.mediaClient(ctx, o.MediaID)
	if e != nil {
		return e
	}
	defer cl.CloseIdleConnections()
	var report model.MaintenanceResult
	headers := http.Header{"X-Operation-ID": []string{r.ID}}
	switch r.Kind {
	case "wal-gc":
		if o.WALPlan == nil {
			return errors.New("WAL cleanup plan missing")
		}
		req := map[string]any{"operation_id": r.ID, "plan": o.WALPlan}
		if e = Call(ctx, cl, "POST", n.URL+"/v1/wal/"+o.WALPlan.ArchiveID+"/gc", req, &report, headers); e != nil {
			return e
		}
	case "gc":
		req := struct {
			OperationID string        `json:"operation_id"`
			Plan        *model.GCPlan `json:"plan,omitempty"`
			Automatic   bool          `json:"automatic,omitempty"`
		}{r.ID, o.Plan, o.Automatic}
		if e = Call(ctx, cl, "POST", n.URL+"/v1/repository/gc", req, &report, headers); e != nil {
			return e
		}
		if e = m.DB.MarkDeleted(ctx, n.ID, report.RemovedSnapshots); e != nil {
			return e
		}
	case "policy":
		var point model.SnapshotInfo
		if e = Call(ctx, cl, "POST", n.URL+"/v1/repository/snapshots/"+url.PathEscape(o.SnapshotID)+"/policy", o.Policy, &point, headers); e != nil {
			return e
		}
		if e = m.DB.SnapshotPolicy(ctx, n.ID, point); e != nil {
			return e
		}
		report = model.MaintenanceResult{OperationID: r.ID, RepositoryID: point.Result.RepositoryID, At: time.Now().UTC()}
	case "import":
		var discovered inventoryResponse
		req := struct {
			SnapshotIDs   []string `json:"snapshot_ids"`
			RetentionDays int      `json:"retention_days"`
		}{o.SnapshotIDs, o.RetentionDays}
		if e = Call(ctx, cl, "POST", n.URL+"/v1/repository/import", req, &discovered, headers); e != nil {
			return e
		}
		target, e := m.DB.Node(ctx, o.AgentID)
		if e != nil {
			return e
		}
		for _, snapshot := range discovered.Snapshots {
			expected := "agent"
			if snapshot.Result.VM != nil {
				expected = "proxy"
			}
			if target.Role != expected {
				return errors.New("import target role does not match the selected backup sources; import VM and agent backups separately")
			}
		}
		report, e = m.DB.RegisterImport(ctx, n.ID, o.AgentID, discovered.Repository.ID, discovered.Snapshots)
		if e != nil {
			return e
		}
		report.OperationID = r.ID
	default:
		return errors.New("unknown maintenance operation")
	}
	r.Maintenance = &report
	return nil
}
func (m *Manager) scheduleRetention(ctx context.Context, now time.Time) error {
	interval := m.Config.RetentionIntervalSeconds
	if interval == 0 {
		return nil
	}
	bucket := now.Unix() / int64(interval)
	nodes, e := m.DB.Nodes(ctx)
	if e != nil {
		return e
	}
	for _, n := range nodes {
		if n.Role != "media" || time.Since(n.LastSeen) > 30*time.Second {
			continue
		}
		if m.retentionBuckets[n.ID] == bucket {
			continue
		}
		repoID, _ := n.Status["repository_id"].(string)
		known, e := m.DB.KnowsRepository(ctx, n.ID, repoID)
		if e != nil {
			return e
		}
		if !known {
			continue
		}
		o := model.MaintenanceOptions{MediaID: n.ID, Automatic: true}
		if _, e = m.enqueueMaintenance(ctx, "gc", o, fmt.Sprintf("retention/%s/%d", n.ID, bucket)); e != nil {
			return e
		}
		m.retentionBuckets[n.ID] = bucket
	}
	return nil
}
