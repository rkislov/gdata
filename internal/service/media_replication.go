// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func (m *Media) replicationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/replication-manifests/{id}", func(w http.ResponseWriter, r *http.Request) {
		g, e := m.authorize(r)
		if e != nil || g.Permission != "read" || g.SnapshotID != r.PathValue("id") {
			Fail(w, 403, errors.New("snapshot read grant required"))
			return
		}
		if _, e = m.Repo.Manifest(g.SnapshotID); e != nil {
			Fail(w, 422, e)
			return
		}
		f, e := os.Open(filepath.Join(m.Repo.Root, "snapshots", g.SnapshotID+".json"))
		if e != nil {
			Fail(w, 404, e)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-SRK-Repository", m.Repo.Info.ID)
		_, _ = io.Copy(w, f)
	})
	mux.HandleFunc("GET /v1/replications/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager", "admin", "auditor"); e != nil {
			Fail(w, 403, e)
			return
		}
		m.replicationMu.Lock()
		res, ok := m.replications[r.PathValue("id")]
		m.replicationMu.Unlock()
		if !ok {
			manifest, e := m.Repo.Manifest(r.PathValue("id"))
			if e != nil || manifest.Result.Replica == nil {
				Fail(w, 404, errors.New("replication not found"))
				return
			}
			res = manifest.Result
		}
		JSON(w, 200, res)
	})
	mux.HandleFunc("POST /v1/replications/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager"); e != nil {
			Fail(w, 403, e)
			return
		}
		var v model.ReplicationTransfer
		if e := Decode(w, r, &v); e != nil {
			Fail(w, 400, e)
			return
		}
		if v.RunID != r.PathValue("id") || !davID.MatchString(v.RunID) || !davID.MatchString(v.Replication.SourceSnapshotID) || !davID.MatchString(v.Replication.SourceRunID) || !davID.MatchString(v.Replication.CopyRunID) || v.Replication.TargetMediaID != m.Config.ID || v.Replication.SourceMediaID == "" || v.SourceRepositoryID == "" || v.Replication.SourceMediaID == m.Config.ID || !ValidEndpoint(v.SourceURL) || v.Replication.RetentionDays < 1 || v.Replication.RetentionDays > 36500 || len(v.SourceManifestHash) != 64 || v.SourceRepositoryID == m.Repo.Info.ID {
			Fail(w, 400, errors.New("invalid independent replication request"))
			return
		}
		g, e := identity.Verify(v.SourceGrant, m.Key)
		if e != nil || g.Subject != m.Config.ID || g.Audience != v.Replication.SourceMediaID || g.Permission != "read" || g.RunID != v.RunID || g.SnapshotID != v.Replication.SourceSnapshotID || g.Expires != v.LeaseExpiresAt.Unix() {
			Fail(w, 403, errors.New("invalid source replication grant"))
			return
		}
		m.replicationMu.Lock()
		if m.replications == nil {
			m.replications = map[string]model.Result{}
		}
		if _, exists := m.replications[v.RunID]; exists {
			m.replicationMu.Unlock()
			Fail(w, 409, errors.New("replication already active"))
			return
		}
		m.replications[v.RunID] = model.Result{Phase: "replication-manifest"}
		m.replicationMu.Unlock()
		defer func() { m.replicationMu.Lock(); delete(m.replications, v.RunID); m.replicationMu.Unlock() }()
		report := func(res model.Result) {
			m.replicationMu.Lock()
			m.replications[v.RunID] = res
			m.replicationMu.Unlock()
		}
		ctx, cancel := context.WithDeadline(r.Context(), v.LeaseExpiresAt)
		defer cancel()
		res, e := m.replicate(ctx, v, report)
		if e != nil {
			_ = m.Repo.RecordAudit(model.RepositoryAudit{ID: model.ID(), StartedAt: time.Now().UTC(), Action: "replication.failed", Object: v.RunID, Actor: "manager", Phase: "failed", Error: e.Error()})
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, res)
	})
}
func (m *Media) replicate(ctx context.Context, v model.ReplicationTransfer, report func(model.Result)) (model.Result, error) {
	var res model.Result
	if old, e := m.Repo.Manifest(v.RunID); e == nil {
		receipt := old.Result.Replica
		if receipt == nil || receipt.SourceRunID != v.Replication.SourceRunID || receipt.SourceRepositoryID != v.SourceRepositoryID || receipt.TargetMediaID != m.Config.ID || receipt.TargetSnapshotID != v.RunID || receipt.SourceMediaID != v.Replication.SourceMediaID || receipt.SourceSnapshotID != v.Replication.SourceSnapshotID || receipt.SourceManifestHash != v.SourceManifestHash || receipt.TargetRunID != v.Replication.CopyRunID || receipt.RetentionDays != v.Replication.RetentionDays {
			return res, errors.New("existing snapshot belongs to another replication")
		}
		if e = m.Repo.Verify(v.RunID); e != nil {
			return res, e
		}
		return old.Result, nil
	}
	client, e := identity.Client(m.Config, identity.Peer{Role: "media", ID: v.Replication.SourceMediaID})
	if e != nil {
		return res, e
	}
	defer client.CloseIdleConnections()
	headers := http.Header{"Authorization": {"Bearer " + v.SourceGrant}}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = Call(cleanup, client, "POST", v.SourceURL+"/v1/operations/"+v.RunID+"/release", nil, nil, headers)
	}()
	request, e := http.NewRequestWithContext(ctx, "GET", v.SourceURL+"/v1/replication-manifests/"+v.Replication.SourceSnapshotID, nil)
	if e != nil {
		return res, e
	}
	request.Header = headers
	response, e := client.Do(request)
	if e != nil {
		return res, e
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, model.MaxManifestBytes+1024))
	response.Body.Close()
	if e != nil || response.StatusCode != 200 || response.Header.Get("X-SRK-Repository") != v.SourceRepositoryID || repository.Hash(raw) != v.SourceManifestHash {
		return res, errors.New("source manifest integrity or repository identity mismatch")
	}
	var envelope struct {
		Checksum string          `json:"checksum"`
		Data     json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || repository.Hash(envelope.Data) != envelope.Checksum {
		return res, errors.New("source manifest checksum mismatch")
	}
	var source model.Manifest
	if json.Unmarshal(envelope.Data, &source) != nil || repository.Validate(source) != nil || source.ID != v.Replication.SourceSnapshotID {
		return res, errors.New("invalid source manifest")
	}
	if e = m.Repo.BeginLease(model.Lease{ID: v.RunID, Subject: m.Config.ID, Kind: "write", ExpiresAt: v.LeaseExpiresAt}); e != nil {
		return res, e
	}
	defer m.Repo.EndLease(v.RunID)
	lengths := map[string]int64{}
	order := []string{}
	var logical int64
	for _, file := range source.Files {
		if file.Kind != "file" {
			continue
		}
		if file.Size > int64(^uint64(0)>>1)-logical {
			return res, errors.New("logical size overflow")
		}
		logical += file.Size
		for i, hash := range file.Chunks {
			size := min(int64(repository.ChunkSize), file.Size-int64(i)*repository.ChunkSize)
			if previous, seen := lengths[hash]; seen {
				if previous != size {
					return res, errors.New("inconsistent source chunk sizes")
				}
			} else {
				lengths[hash] = size
				order = append(order, hash)
				res.TotalBytes += size
			}
		}
	}
	res.Phase = "replication-transfer"
	res.ProgressScope = "bytes"
	res.LogicalBytes = 0
	res.Files = source.Result.Files
	res.Consistency = source.Consistency
	res.EffectiveMode = "full"
	res.Chunks = len(order)
	replicationProgress(&res)
	report(res)
	for _, hash := range order {
		if e = ctx.Err(); e != nil {
			return res, e
		}
		// Fetch every source block, even when deduplicated on target: source corruption
		// must never be hidden by an existing good target block.
		req, _ := http.NewRequestWithContext(ctx, "GET", v.SourceURL+"/v1/chunks/"+hash, nil)
		req.Header = headers
		response, e = client.Do(req)
		if e != nil {
			return res, e
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, repository.ChunkSize+1))
		response.Body.Close()
		if readErr != nil || response.StatusCode != 200 || int64(len(data)) != lengths[hash] || repository.Hash(data) != hash {
			return res, fmt.Errorf("source block SHA-256 or size mismatch: %s", hash)
		}
		existing, existingErr := m.Repo.Get(hash)
		if existingErr != nil {
			if !os.IsNotExist(existingErr) {
				return res, fmt.Errorf("target block integrity failure: %w", existingErr)
			}
			if e = m.Repo.Put(hash, data); e != nil {
				return res, e
			}
			res.UploadedBytes += int64(len(data))
		} else if int64(len(existing)) != lengths[hash] {
			return res, errors.New("target block size mismatch")
		}
		res.ProgressBytes += int64(len(data))
		res.LogicalBytes += int64(len(data))
		replicationProgress(&res)
		report(res)
	}
	res.Phase = "replication-validation"
	res.ProgressPercent = nil
	res.ProgressScope = "operation"
	report(res)
	now := time.Now().UTC()
	expires := now.AddDate(0, 0, v.Replication.RetentionDays)
	capturedAt := source.CreatedAt
	if source.Result.Replica != nil {
		capturedAt = source.Result.Replica.CapturedAt
	}
	counts := res
	res = source.Result
	res.Replica = &model.Replica{RetentionDays: v.Replication.RetentionDays, SourceRunID: v.Replication.SourceRunID, SourceMediaID: v.Replication.SourceMediaID, SourceSnapshotID: source.ID, SourceRepositoryID: v.SourceRepositoryID, SourceManifestHash: v.SourceManifestHash, CapturedAt: capturedAt, TargetMediaID: m.Config.ID, TargetRunID: v.Replication.CopyRunID, TargetSnapshotID: v.RunID, ExpiresAt: expires}
	// Use counters from this transfer, rather than the original backup.
	res.UploadedBytes = counts.UploadedBytes
	res.LogicalBytes = logical
	res.TotalBytes = counts.TotalBytes
	res.ProgressBytes = counts.ProgressBytes
	res.Chunks = len(order)
	res.EffectiveMode = "full"
	res.RepositoryID = m.Repo.Info.ID
	res.Verified = true
	res.Phase = "complete"
	completeProgress(&res)
	source.ID = v.RunID
	source.JobID = v.Replication.CopyRunID
	source.Base = ""
	source.Mode = "full"
	source.CreatedAt = now
	source.ExpiresAt = expires
	source.Result = res
	if e = m.Repo.CommitLeaseContext(ctx, source, v.RunID); e != nil {
		return res, e
	}
	_ = m.Repo.RecordAudit(model.RepositoryAudit{ID: model.ID(), StartedAt: now, Actor: "manager", Action: "replication.committed", Object: v.RunID, Phase: "committed"})
	return res, nil
}

func replicationProgress(res *model.Result) {
	percent := 0.0
	if res.TotalBytes > 0 {
		percent = 100 * float64(res.ProgressBytes) / float64(res.TotalBytes)
	}
	if percent > 99 {
		percent = 99
	}
	res.ProgressPercent = &percent
}
