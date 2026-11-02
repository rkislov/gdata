// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"errors"
	"gdata/srk/internal/model"
	"net/http"
	"time"
)

func (m *Media) walAckRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/wal-receivers/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager"); e != nil {
			Fail(w, 403, e)
			return
		}
		var req struct {
			Receiver   model.WALReceiver `json:"receiver"`
			Reactivate bool              `json:"reactivate"`
		}
		if e := Decode(w, r, &req); e != nil {
			Fail(w, 400, e)
			return
		}
		if req.Receiver.ID != r.PathValue("id") {
			Fail(w, 400, errors.New("WAL receiver identity mismatch"))
			return
		}
		v, e := m.Repo.RegisterWALReceiver(req.Receiver, req.Reactivate)
		if e != nil {
			Fail(w, 409, e)
			return
		}
		JSON(w, 200, v)
	})
	mux.HandleFunc("GET /v1/wal-receivers/{id}/lag", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager", "admin", "auditor"); e != nil {
			Fail(w, 403, e)
			return
		}
		v, e := m.Repo.WALReceiverLag(r.PathValue("id"))
		if e != nil {
			Fail(w, 409, e)
			return
		}
		JSON(w, 200, v)
	})
	mux.HandleFunc("POST /v1/wal-receivers/{id}/release", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager"); e != nil {
			Fail(w, 403, e)
			return
		}
		var request struct {
			Reason  string `json:"reason"`
			Confirm bool   `json:"confirm"`
		}
		if e := Decode(w, r, &request); e != nil {
			Fail(w, 400, e)
			return
		}
		if !request.Confirm {
			Fail(w, 400, errors.New("explicit confirmation required to abandon unacknowledged WAL"))
			return
		}
		if e := m.Repo.ReleaseWALReceiver(r.PathValue("id"), request.Reason); e != nil {
			Fail(w, 409, e)
			return
		}
		JSON(w, 200, map[string]bool{"released": true})
	})
	mux.HandleFunc("GET /v1/wal-replication-source/{archive}/receivers/{id}/pending", func(w http.ResponseWriter, r *http.Request) {
		g, e := m.walReplicationRead(r, r.PathValue("archive"))
		if e != nil || g.WALPolicyID != r.PathValue("id") {
			Fail(w, 403, errors.New("WAL receiver outside capability"))
			return
		}
		records, v, e := m.Repo.WALReceiverPending(g.WALPolicyID)
		if e != nil || v.ArchiveID != g.ArchiveID || v.Generation != g.WALGeneration || v.TargetMediaID != g.Subject || v.TargetRepositoryID != g.WALTargetRepositoryID {
			Fail(w, 409, errors.New("WAL receiver generation or repository mismatch"))
			return
		}
		names := []string{}
		for _, record := range records {
			names = append(names, record.Name)
		}
		JSON(w, 200, names)
	})
	mux.HandleFunc("GET /v1/wal-replication-source/{archive}/receivers/{id}/lag", func(w http.ResponseWriter, r *http.Request) {
		g, e := m.walReplicationRead(r, r.PathValue("archive"))
		if e != nil || g.WALPolicyID != r.PathValue("id") {
			Fail(w, 403, errors.New("WAL lag outside capability"))
			return
		}
		v, e := m.Repo.WALReceiver(g.WALPolicyID)
		if e != nil || v.Generation != g.WALGeneration || v.TargetRepositoryID != g.WALTargetRepositoryID || v.TargetMediaID != g.Subject || v.ArchiveID != g.ArchiveID {
			Fail(w, 409, errors.New("WAL receiver lag identity mismatch"))
			return
		}
		lag, e := m.Repo.WALReceiverLag(g.WALPolicyID)
		if e != nil {
			Fail(w, 500, e)
			return
		}
		JSON(w, 200, lag)
	})
	mux.HandleFunc("POST /v1/wal-replication-source/{archive}/receivers/{id}/ack", func(w http.ResponseWriter, r *http.Request) {
		g, e := m.walReplicationRead(r, r.PathValue("archive"))
		if e != nil || g.WALPolicyID != r.PathValue("id") {
			Fail(w, 403, errors.New("WAL acknowledgement outside capability"))
			return
		}
		var request model.WALReceiverAck
		if e = Decode(w, r, &request); e != nil {
			Fail(w, 400, e)
			return
		}
		if request.Generation != g.WALGeneration || request.TargetRepositoryID != g.WALTargetRepositoryID {
			Fail(w, 403, errors.New("WAL acknowledgement generation mismatch"))
			return
		}
		v, e := m.Repo.WALReceiver(g.WALPolicyID)
		if e != nil || v.ArchiveID != g.ArchiveID {
			Fail(w, 409, errors.New("WAL receiver archive mismatch"))
			return
		}
		if e = m.Repo.AcknowledgeWAL(g.WALPolicyID, g.Subject, request); e != nil {
			Fail(w, 409, e)
			return
		}
		JSON(w, 200, map[string]bool{"acknowledged": true})
	})
}
func (m *Manager) ensureWALReceiver(ctx context.Context, p model.WALReplicationPolicy, reactivate bool) (model.WALReceiver, error) {
	source, client, e := m.mediaClient(ctx, p.SourceMediaID)
	if e != nil {
		return model.WALReceiver{}, e
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	v := model.WALReceiver{ID: p.ID, ArchiveID: p.ArchiveID, SourceRepositoryID: p.SourceRepositoryID, TargetMediaID: p.TargetMediaID, TargetRepositoryID: p.TargetRepositoryID}
	var result model.WALReceiver
	e = Call(ctx, client, "POST", source.URL+"/v1/wal-receivers/"+p.ID, map[string]any{"receiver": v, "reactivate": reactivate}, &result)
	if e == nil && (result.ID != p.ID || result.ArchiveID != p.ArchiveID || result.Generation == "" || result.TargetMediaID != p.TargetMediaID || result.TargetRepositoryID != p.TargetRepositoryID || result.SourceRepositoryID != p.SourceRepositoryID || result.Released) {
		e = errors.New("source WAL receiver receipt mismatch")
	}
	return result, e
}
func (m *Manager) sourceWALLag(ctx context.Context, p model.WALReplicationPolicy) model.WALReplicationLag {
	lag := model.WALReplicationLag{State: "source_unavailable", ObservedAt: time.Now().UTC()}
	target, e := m.DB.Node(ctx, p.TargetMediaID)
	online := e == nil && target.Role == "media" && time.Since(target.LastSeen) < 30*time.Second
	source, client, e := m.mediaClient(ctx, p.SourceMediaID)
	if e == nil {
		defer client.CloseIdleConnections()
		poll, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		e = Call(poll, client, "GET", source.URL+"/v1/wal-receivers/"+p.ID+"/lag", nil, &lag)
	}
	if e != nil {
		lag.State = "source_unavailable"
		lag.SourceProtected = false
		lag.PendingBytes = nil
		lag.PendingFiles = nil
		lag.PendingAgeSeconds = nil
		lag.Error = e.Error()
	}
	lag.TargetOnline = online
	return lag
}
func (m *Manager) restoreWALReceivers(ctx context.Context) {
	policies, e := m.DB.WALPolicies(ctx)
	if e != nil {
		return
	}
	for _, p := range policies {
		if ctx.Err() != nil {
			return
		}
		_, _ = m.ensureWALReceiver(ctx, p, false)
	}
}
