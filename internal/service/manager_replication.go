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

func (m *Manager) replicationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/runs/{id}/reconcile-replication", m.wrap(true, func(w http.ResponseWriter, request *http.Request, actor string) {
		run, e := m.DB.Run(request.Context(), request.PathValue("id"))
		if e != nil || run.Replication == nil || run.Status == "queued" {
			Fail(w, 409, errors.New("finished replication required"))
			return
		}
		if run.Status == "running" {
			m.mu.Lock()
			_, active := m.active[run.ID]
			m.mu.Unlock()
			if active {
				Fail(w, 409, errors.New("replication is still active"))
				return
			}
		}
		p := run.Replication
		target, client, e := m.mediaClient(request.Context(), p.TargetMediaID)
		if e != nil {
			Fail(w, 409, e)
			return
		}
		defer client.CloseIdleConnections()
		var res model.Result
		if e = Call(request.Context(), client, "GET", target.URL+"/v1/replications/"+run.ID, nil, &res); e != nil {
			Fail(w, 409, e)
			return
		}
		receipt := res.Replica
		if !res.Verified || receipt == nil || receipt.SourceRunID != p.SourceRunID || receipt.SourceMediaID != p.SourceMediaID || receipt.SourceSnapshotID != p.SourceSnapshotID || receipt.TargetSnapshotID != run.ID || receipt.TargetRunID != p.CopyRunID || receipt.TargetMediaID != p.TargetMediaID || receipt.RetentionDays != p.RetentionDays {
			Fail(w, 409, errors.New("no matching committed replica receipt"))
			return
		}
		if e = Call(request.Context(), client, "POST", target.URL+"/v1/snapshots/"+run.ID+"/verify", nil, nil); e != nil {
			Fail(w, 422, e)
			return
		}
		now := time.Now().UTC()
		run.Status = "success"
		run.Error = ""
		run.Result = &res
		run.FinishedAt = &now
		if e = m.DB.CompleteReplication(request.Context(), run); e != nil {
			Fail(w, 500, e)
			return
		}
		_ = m.DB.Audit(request.Context(), actor, "replication.reconciled", run.ID, "committed independent recovery point registered")
		JSON(w, 200, run)
	}))

	mux.HandleFunc("POST /v1/runs/{id}/replicate", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		var req struct {
			TargetMediaID string `json:"target_media_id"`
			RetentionDays int    `json:"retention_days"`
			IncludeWAL    bool   `json:"include_wal"`
		}
		if e := Decode(w, r, &req); e != nil {
			Fail(w, 400, e)
			return
		}
		if req.RetentionDays < 1 || req.RetentionDays > 36500 {
			Fail(w, 400, errors.New("retention_days must be 1–36500"))
			return
		}
		source, e := m.DB.Run(r.Context(), r.PathValue("id"))
		if e != nil || source.Kind != "backup" || source.Status != "success" || source.SnapshotState == "deleted" {
			Fail(w, 409, errors.New("successful retained backup required"))
			return
		}
		if req.TargetMediaID == source.Job.MediaID {
			Fail(w, 400, errors.New("source and target media servers must differ"))
			return
		}
		target, tc, e := m.mediaClient(r.Context(), req.TargetMediaID)
		if e != nil {
			Fail(w, 409, e)
			return
		}
		tc.CloseIdleConnections()
		sourceNode, sc, e := m.mediaClient(r.Context(), source.Job.MediaID)
		if e != nil {
			Fail(w, 409, e)
			return
		}
		defer sc.CloseIdleConnections()
		physical := source.SnapshotID
		if physical == "" {
			physical = source.ID
		}
		var point model.SnapshotInfo
		if e = Call(r.Context(), sc, "GET", sourceNode.URL+"/v1/repository/snapshots/"+physical, nil, &point); e != nil {
			Fail(w, 409, e)
			return
		}
		if !point.Hold && !time.Now().Before(point.ExpiresAt) {
			Fail(w, 409, errors.New("source retention expired"))
			return
		}
		if req.IncludeWAL && (point.Result.PostgreSQL == nil || !repository.ValidWALArchiveID(point.Result.PostgreSQL.ArchiveID)) {
			Fail(w, 400, errors.New("WAL replication requires a PostgreSQL base linked to an archive"))
			return
		}
		job := source.Job
		job.MediaID = target.ID
		run := model.Run{ID: model.ID(), JobID: source.JobID, Job: job, Kind: "replicate", Status: "queued", SnapshotID: physical, CreatedAt: time.Now().UTC(), Replication: &model.Replication{IncludeWAL: req.IncludeWAL, SourceRunID: source.ID, SourceMediaID: source.Job.MediaID, SourceSnapshotID: physical, TargetMediaID: target.ID, RetentionDays: req.RetentionDays, CopyRunID: model.ID()}}
		run, e = m.DB.Enqueue(r.Context(), run, "replicate/"+source.ID+"/"+idempotency(r))
		if e != nil {
			code := 500
			if errors.Is(e, catalog.ErrIdempotencyConflict) {
				code = 409
			}
			Fail(w, code, e)
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "replication.enqueue", run.ID, fmt.Sprintf("%s → %s; retention %d days", source.Job.MediaID, target.ID, req.RetentionDays))
		JSON(w, 202, run)
	}))
}
func (m *Manager) executeReplication(ctx context.Context, r model.Run) (model.Result, error) {
	p := r.Replication
	source, sc, e := m.mediaClient(ctx, p.SourceMediaID)
	if e != nil {
		return model.Result{}, e
	}
	defer sc.CloseIdleConnections()
	target, tc, e := m.mediaClient(ctx, p.TargetMediaID)
	if e != nil {
		return model.Result{}, e
	}
	defer tc.CloseIdleConnections()
	var info model.SnapshotInfo
	if e = Call(ctx, sc, "GET", source.URL+"/v1/repository/snapshots/"+p.SourceSnapshotID, nil, &info); e != nil {
		return model.Result{}, e
	}
	if !info.Hold && !time.Now().Before(info.ExpiresAt) {
		return model.Result{}, errors.New("source backup retention expired")
	}
	if info.Result.RepositoryID == "" {
		return model.Result{}, errors.New("source repository identity missing")
	}
	if p.IncludeWAL && info.Result.PostgreSQL != nil {
		var status struct {
			Repository struct {
				ID string `json:"repository_id"`
			} `json:"repository"`
		}
		if e = Call(ctx, tc, "GET", target.URL+"/v1/status", nil, &status); e != nil {
			return model.Result{}, e
		}
		policy := model.WALReplicationPolicy{SourceMediaID: source.ID, SourceRepositoryID: info.Result.RepositoryID, TargetMediaID: target.ID, TargetRepositoryID: status.Repository.ID, ArchiveID: info.Result.PostgreSQL.ArchiveID}
		policy.ID = catalog.WALPolicyID(policy)
		if _, e = m.ensureWALReceiver(ctx, policy, true); e != nil {
			return model.Result{}, e
		}
	}
	expiry := time.Now().Add(12 * time.Hour).UTC().Truncate(time.Second)
	_ = m.DB.Audit(ctx, "scheduler", "replication.started", r.ID, source.ID+" → "+target.ID)
	grant := identity.Sign(identity.Grant{Audience: source.ID, Subject: target.ID, RunID: r.ID, SnapshotID: p.SourceSnapshotID, Permission: "read", Expires: expiry.Unix()}, m.Key)
	transfer := model.ReplicationTransfer{RunID: r.ID, Replication: *p, SourceURL: source.URL, SourceRepositoryID: info.Result.RepositoryID, SourceManifestHash: info.ManifestHash, SourceGrant: grant, LeaseExpiresAt: expiry}
	type outcome struct {
		res model.Result
		err error
	}
	done := make(chan outcome, 1)
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		var res model.Result
		err := Call(requestCtx, tc, "POST", target.URL+"/v1/replications/"+r.ID, transfer, &res)
		done <- outcome{res, err}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := ""
	record := func(res model.Result) {
		res.Verified = false
		_ = m.DB.ReplicationProgress(ctx, r.ID, res)
		key := res.Phase
		if res.ProgressPercent != nil {
			key += fmt.Sprintf("/%d%%", int(*res.ProgressPercent)/5*5)
		}
		if key != last {
			_ = m.DB.Audit(ctx, "media/"+target.ID, "replication.progress", r.ID, key)
			last = key
		}
	}
	for {
		select {
		case result := <-done:
			if result.err != nil {
				return result.res, result.err
			}
			if !result.res.Verified || result.res.Replica == nil || result.res.Replica.TargetSnapshotID != r.ID || result.res.Replica.SourceSnapshotID != p.SourceSnapshotID || result.res.Replica.SourceManifestHash != info.ManifestHash || result.res.Replica.TargetRunID != p.CopyRunID || result.res.Replica.TargetMediaID != target.ID {
				return result.res, errors.New("invalid replication receipt")
			}
			_ = m.DB.Audit(ctx, "media/"+target.ID, "replication.verified", r.ID, "target manifest committed after SHA-256 verification")
			return result.res, nil
		case <-ctx.Done():
			return model.Result{}, ctx.Err()
		case <-tick.C:
			poll, pollCancel := context.WithTimeout(ctx, 3*time.Second)
			var res model.Result
			if Call(poll, tc, "GET", target.URL+"/v1/replications/"+r.ID, nil, &res) == nil {
				record(res)
			}
			pollCancel()
		}
	}
}

func dispatchSourceKey(r model.Run) string {
	if r.Tape != nil && r.Tape.PoolID != "" {
		return "tape-pool/" + r.ID
	}
	if r.WALReplication != nil {
		return "wal-replication/" + r.WALReplication.SourceMediaID + "/" + r.WALReplication.TargetMediaID + "/" + r.WALReplication.ArchiveID
	}
	if r.Replication != nil {
		return "replication/" + r.Replication.SourceMediaID + "/" + r.Replication.SourceSnapshotID + "/" + r.Replication.TargetMediaID
	}
	return r.Job.AgentID + "\x00" + r.Job.Source
}
