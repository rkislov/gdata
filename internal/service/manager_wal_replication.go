// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"errors"
	"fmt"
	"gdata/srk/internal/catalog"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"net/http"
	"time"
)

func (m *Manager) walReplicationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/wal-replication-policies", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		policies, e := m.DB.WALPolicies(r.Context())
		if e != nil {
			Fail(w, 500, e)
			return
		}
		type row struct {
			Policy  model.WALReplicationPolicy `json:"policy"`
			LastRun *model.Run                 `json:"last_run,omitempty"`
			Lag     model.WALReplicationLag    `json:"lag"`
		}
		out := []row{}
		for _, p := range policies {
			last, e := m.DB.LatestWALReplication(r.Context(), p.ID)
			if e != nil {
				Fail(w, 500, e)
				return
			}
			out = append(out, row{p, last, m.sourceWALLag(r.Context(), p)})
		}
		JSON(w, 200, out)
	}))
	mux.HandleFunc("POST /v1/runs/{id}/wal-replication", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		var request struct {
			IntervalSeconds int   `json:"interval_seconds"`
			Enabled         *bool `json:"enabled,omitempty"`
		}
		if e := Decode(w, r, &request); e != nil {
			Fail(w, 400, e)
			return
		}
		if request.IntervalSeconds == 0 {
			request.IntervalSeconds = 30
		}
		if request.IntervalSeconds < 30 || request.IntervalSeconds > 3600 {
			Fail(w, 400, errors.New("WAL polling interval must be 30–3600 seconds"))
			return
		}
		point, e := m.DB.Run(r.Context(), r.PathValue("id"))
		if e != nil || point.Status != "success" || point.Kind != "backup" || point.Result == nil || point.Result.Replica == nil || point.Result.PostgreSQL == nil || !repository.ValidWALArchiveID(point.Result.PostgreSQL.ArchiveID) {
			Fail(w, 409, errors.New("replicated PostgreSQL base with WAL archive identity required"))
			return
		}
		origin := point.Result.Replica
		target, client, e := m.mediaClient(r.Context(), point.Job.MediaID)
		if e != nil {
			Fail(w, 409, e)
			return
		}
		defer client.CloseIdleConnections()
		var info model.SnapshotInfo
		if e = Call(r.Context(), client, "GET", target.URL+"/v1/repository/snapshots/"+point.SnapshotID, nil, &info); e != nil || !info.Hold && !time.Now().Before(info.ExpiresAt) {
			Fail(w, 409, errors.New("target base unavailable or retention expired"))
			return
		}
		now := time.Now().UTC()
		enabled := request.Enabled == nil || *request.Enabled
		p := model.WALReplicationPolicy{SourceMediaID: origin.SourceMediaID, SourceRepositoryID: origin.SourceRepositoryID, TargetMediaID: target.ID, TargetRepositoryID: point.Result.RepositoryID, TargetRunID: point.ID, TargetSnapshotID: point.SnapshotID, ArchiveID: point.Result.PostgreSQL.ArchiveID, Enabled: enabled, IntervalSeconds: request.IntervalSeconds, NextRun: now, CreatedAt: now}
		p.ID = catalog.WALPolicyID(p)
		m.operationMu.Lock()
		defer m.operationMu.Unlock()
		if _, e = m.ensureWALReceiver(r.Context(), p, p.Enabled); e != nil {
			Fail(w, 409, e)
			return
		}
		if e = m.DB.PutWALPolicy(r.Context(), p); e != nil {
			Fail(w, 500, e)
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "wal.replication.policy", p.ID, fmt.Sprintf("%s → %s; enabled=%t; every %ds", p.SourceMediaID, p.TargetMediaID, p.Enabled, p.IntervalSeconds))
		JSON(w, 201, p)
	}))
	mux.HandleFunc("POST /v1/wal-replication-policies/{id}/release-protection", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		var req struct {
			Reason  string `json:"reason"`
			Confirm bool   `json:"confirm"`
		}
		if e := Decode(w, r, &req); e != nil {
			Fail(w, 400, e)
			return
		}
		if !req.Confirm || len(req.Reason) == 0 || len(req.Reason) > 500 {
			Fail(w, 400, errors.New("confirmation and reason required to abandon pending WAL"))
			return
		}
		m.operationMu.Lock()
		defer m.operationMu.Unlock()
		p, e := m.DB.WALPolicy(r.Context(), r.PathValue("id"))
		if e != nil {
			Fail(w, 404, e)
			return
		}
		if p.Enabled {
			Fail(w, 409, errors.New("pause WAL policy before releasing protection"))
			return
		}
		last, e := m.DB.LatestWALReplication(r.Context(), p.ID)
		if e != nil {
			Fail(w, 500, e)
			return
		}
		if last != nil && (last.Status == "running" || last.Status == "queued") {
			Fail(w, 409, errors.New("wait for active WAL replication to stop"))
			return
		}
		source, client, e := m.mediaClient(r.Context(), p.SourceMediaID)
		if e != nil {
			Fail(w, 409, e)
			return
		}
		defer client.CloseIdleConnections()
		if e = Call(r.Context(), client, "POST", source.URL+"/v1/wal-receivers/"+p.ID+"/release", req, nil); e != nil {
			Fail(w, 409, e)
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "wal.replication.protection.released", p.ID, req.Reason)
		JSON(w, 200, map[string]bool{"released": true})
	}))
	for _, action := range []string{"enabled", "run"} {
		mux.HandleFunc("POST /v1/wal-replication-policies/{id}/"+action, m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
			m.operationMu.Lock()
			defer m.operationMu.Unlock()
			p, e := m.DB.WALPolicy(r.Context(), r.PathValue("id"))
			if e != nil {
				Fail(w, 404, e)
				return
			}
			if action == "enabled" {
				var req struct {
					Enabled bool `json:"enabled"`
				}
				if e = Decode(w, r, &req); e != nil {
					Fail(w, 400, e)
					return
				}
				p.Enabled = req.Enabled
			} else if !p.Enabled {
				Fail(w, 409, errors.New("enable WAL policy before syncing"))
				return
			}
			p.NextRun = time.Now().UTC()
			if p.Enabled {
				if _, e = m.ensureWALReceiver(r.Context(), p, action == "enabled"); e != nil {
					Fail(w, 409, e)
					return
				}
			}
			if e = m.DB.PutWALPolicy(r.Context(), p); e != nil {
				Fail(w, 500, e)
				return
			}
			_ = m.DB.Audit(r.Context(), actor, "wal.replication."+action, p.ID, fmt.Sprint(p.Enabled))
			JSON(w, 202, p)
		}))
	}
}
func (m *Manager) executeWALReplication(ctx context.Context, r model.Run) (model.Result, error) {
	p := *r.WALReplication
	receiver, e := m.ensureWALReceiver(ctx, p, false)
	if e != nil {
		return model.Result{}, e
	}
	source, sc, e := m.mediaClient(ctx, p.SourceMediaID)
	if e != nil {
		return model.Result{}, e
	}
	sc.CloseIdleConnections()
	target, client, e := m.mediaClient(ctx, p.TargetMediaID)
	if e != nil {
		return model.Result{}, e
	}
	defer client.CloseIdleConnections()
	until := time.Now().Add(11*time.Hour + 59*time.Minute).UTC().Truncate(time.Second)
	grant := identity.Sign(identity.Grant{WALPolicyID: p.ID, WALGeneration: receiver.Generation, WALTargetRepositoryID: p.TargetRepositoryID, Audience: source.ID, Subject: target.ID, RunID: r.ID, Permission: "wal-replication-read", ArchiveID: p.ArchiveID, Expires: until.Unix()}, m.Key)
	transfer := model.WALReplicationTransfer{AckGeneration: receiver.Generation, RunID: r.ID, Policy: p, SourceURL: source.URL, SourceGrant: grant, ExpiresAt: until}
	type outcome struct {
		res model.Result
		err error
	}
	done := make(chan outcome, 1)
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	_ = m.DB.Audit(ctx, "scheduler", "wal.replication.started", r.ID, p.SourceMediaID+" → "+p.TargetMediaID)
	go func() {
		var res model.Result
		e := Call(requestCtx, client, "POST", target.URL+"/v1/wal-replications/"+r.ID, transfer, &res)
		done <- outcome{res, e}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := ""
	for {
		select {
		case result := <-done:
			if result.err != nil {
				return result.res, result.err
			}
			receipt := result.res.WALReplication
			if !result.res.Verified || receipt == nil || receipt.PolicyID != p.ID || receipt.ArchiveID != p.ArchiveID || receipt.SourceMediaID != p.SourceMediaID || receipt.TargetMediaID != p.TargetMediaID || receipt.Calendar == nil {
				return result.res, errors.New("invalid WAL replication result")
			}
			_ = m.DB.Audit(ctx, "media/"+p.TargetMediaID, "wal.replication.verified", r.ID, fmt.Sprintf("copied=%d reused=%d; inspect destination PITR calendar for gaps", receipt.Copied, receipt.Reused))
			return result.res, nil
		case <-ctx.Done():
			return model.Result{}, ctx.Err()
		case <-tick.C:
			poll, stop := context.WithTimeout(ctx, 3*time.Second)
			var res model.Result
			if Call(poll, client, "GET", target.URL+"/v1/replications/"+r.ID, nil, &res) == nil {
				res.Verified = false
				_ = m.DB.WALReplicationProgress(ctx, r.ID, res)
				key := res.Phase
				if res.ProgressPercent != nil {
					key += fmt.Sprintf("/%d%%", int(*res.ProgressPercent)/5*5)
				}
				if key != last {
					_ = m.DB.Audit(ctx, "media/"+p.TargetMediaID, "wal.replication.progress", r.ID, key)
					last = key
				}
			}
			stop()
		}
	}
}
