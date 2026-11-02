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
	"time"
)

func (m *Media) walReplicationRead(r *http.Request, archive string) (identity.Grant, error) {
	peer, e := Role(r, "media")
	if e != nil {
		return identity.Grant{}, e
	}
	g, e := identity.Verify(bearerToken(r), m.Key)
	if e != nil || g.Subject != peer.ID || g.Audience != m.Config.ID || g.Permission != "wal-replication-read" || g.ArchiveID != archive || !davID.MatchString(g.RunID) {
		return g, errors.New("archive-scoped media WAL capability required")
	}
	return g, nil
}
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	s := r.Header.Get("Authorization")
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		return ""
	}
	return s[len(prefix):]
}
func (m *Media) walReplicationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/wal-replication-source/{archive}/pins/{id}", func(w http.ResponseWriter, r *http.Request) {
		g, e := m.walReplicationRead(r, r.PathValue("archive"))
		if e != nil || g.RunID != r.PathValue("id") {
			Fail(w, 403, errors.New("WAL replication pin outside capability"))
			return
		}
		e = m.Repo.PinWAL(model.WALPin{ID: g.RunID, ArchiveID: g.ArchiveID, AgentID: "media/" + g.Subject, ExpiresAt: time.Unix(g.Expires, 0).UTC()})
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, map[string]string{"repository_id": m.Repo.Info.ID})
	})
	mux.HandleFunc("DELETE /v1/wal-replication-source/{archive}/pins/{id}", func(w http.ResponseWriter, r *http.Request) {
		g, e := m.walReplicationRead(r, r.PathValue("archive"))
		if e != nil || g.RunID != r.PathValue("id") {
			Fail(w, 403, errors.New("WAL pin outside capability"))
			return
		}
		e = m.Repo.ReleaseWALPin(g.ArchiveID, g.RunID, "media/"+g.Subject)
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, map[string]bool{"released": true})
	})
	mux.HandleFunc("GET /v1/wal-replication-source/{archive}", func(w http.ResponseWriter, r *http.Request) {
		if _, e := m.walReplicationRead(r, r.PathValue("archive")); e != nil {
			Fail(w, 403, e)
			return
		}
		records, e := m.Repo.WALInventory(r.PathValue("archive"))
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, records)
	})
	mux.HandleFunc("GET /v1/wal-replication-source/{archive}/{name}", func(w http.ResponseWriter, r *http.Request) {
		if _, e := m.walReplicationRead(r, r.PathValue("archive")); e != nil {
			Fail(w, 403, e)
			return
		}
		f, record, e := m.Repo.OpenWAL(r.PathValue("archive"), r.PathValue("name"))
		if e != nil {
			Fail(w, 422, e)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-SRK-SHA256", record.SHA256)
		_, _ = io.Copy(w, f)
	})
	mux.HandleFunc("POST /v1/wal-replications/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager"); e != nil {
			Fail(w, 403, e)
			return
		}
		var v model.WALReplicationTransfer
		if e := Decode(w, r, &v); e != nil {
			Fail(w, 400, e)
			return
		}
		p := v.Policy
		if !davID.MatchString(v.RunID) || v.RunID != r.PathValue("id") || p.TargetMediaID != m.Config.ID || p.SourceMediaID == "" || p.SourceMediaID == p.TargetMediaID || p.SourceRepositoryID == "" || p.TargetRepositoryID != m.Repo.Info.ID || p.SourceRepositoryID == m.Repo.Info.ID || !davID.MatchString(p.TargetSnapshotID) || !ValidEndpoint(v.SourceURL) {
			Fail(w, 400, errors.New("invalid WAL replication target"))
			return
		}
		g, e := identity.Verify(v.SourceGrant, m.Key)
		if e != nil || g.Subject != m.Config.ID || g.Audience != p.SourceMediaID || g.RunID != v.RunID || g.Permission != "wal-replication-read" || g.ArchiveID != p.ArchiveID || g.Expires != v.ExpiresAt.Unix() || g.WALPolicyID != p.ID || g.WALGeneration != v.AckGeneration || v.AckGeneration == "" || g.WALTargetRepositoryID != m.Repo.Info.ID {
			Fail(w, 403, errors.New("invalid WAL replication grant"))
			return
		}
		m.replicationMu.Lock()
		if m.replications == nil {
			m.replications = map[string]model.Result{}
		}
		if _, ok := m.replications[v.RunID]; ok {
			m.replicationMu.Unlock()
			Fail(w, 409, errors.New("WAL transfer already active"))
			return
		}
		m.replications[v.RunID] = model.Result{Phase: "wal-replication-inventory"}
		m.replicationMu.Unlock()
		defer func() { m.replicationMu.Lock(); delete(m.replications, v.RunID); m.replicationMu.Unlock() }()
		report := func(res model.Result) {
			if res.WALReplication != nil {
				snapshot := *res.WALReplication
				res.WALReplication = &snapshot
			}
			m.replicationMu.Lock()
			m.replications[v.RunID] = res
			m.replicationMu.Unlock()
		}
		ctx, cancel := context.WithDeadline(r.Context(), v.ExpiresAt)
		defer cancel()
		res, e := m.replicateWAL(ctx, v, report)
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, res)
	})
}
func (m *Media) replicateWAL(ctx context.Context, v model.WALReplicationTransfer, report func(model.Result)) (model.Result, error) {
	p := v.Policy
	res := model.Result{Phase: "wal-replication-inventory", WALReplication: &model.WALReplicationResult{PolicyID: p.ID, ArchiveID: p.ArchiveID, SourceMediaID: p.SourceMediaID, TargetMediaID: p.TargetMediaID}}
	points, e := m.Repo.Inventory(false)
	if e != nil {
		return res, e
	}
	anchor := ""
	for _, point := range points {
		if point.Result.PostgreSQL == nil || point.Result.PostgreSQL.ArchiveID != p.ArchiveID || !point.Hold && !time.Now().Before(point.ExpiresAt) {
			continue
		}
		anchor = point.ID
		if anchor == p.TargetSnapshotID {
			break
		}
	}
	if anchor == "" {
		return res, errors.New("no retained target base for WAL replication")
	}
	if e = m.Repo.BeginLease(model.Lease{ID: v.RunID, Subject: m.Config.ID, Kind: "read", SnapshotID: anchor, ExpiresAt: v.ExpiresAt}); e != nil {
		return res, e
	}
	defer m.Repo.EndLease(v.RunID)
	base, e := m.Repo.Manifest(anchor)
	if e != nil {
		return res, e
	}
	pg := base.PostgreSQL
	if pg == nil || pg.ArchiveID != p.ArchiveID {
		return res, errors.New("target base belongs to another WAL archive")
	}
	if e = m.Repo.PinWAL(model.WALPin{ID: v.RunID, ArchiveID: p.ArchiveID, AgentID: "media/" + m.Config.ID, ExpiresAt: v.ExpiresAt}); e != nil {
		return res, e
	}
	defer m.Repo.ReleaseWALPin(p.ArchiveID, v.RunID, "media/"+m.Config.ID)
	client, e := identity.Client(m.Config, identity.Peer{Role: "media", ID: p.SourceMediaID})
	if e != nil {
		return res, e
	}
	defer client.CloseIdleConnections()
	headers := http.Header{"Authorization": {"Bearer " + v.SourceGrant}}
	pinURL := v.SourceURL + "/v1/wal-replication-source/" + p.ArchiveID + "/pins/" + v.RunID
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = Call(cleanup, client, "DELETE", pinURL, nil, nil, headers)
	}()
	var pin map[string]string
	if e = Call(ctx, client, "POST", pinURL, nil, &pin, headers); e != nil {
		return res, e
	}
	if pin["repository_id"] != p.SourceRepositoryID {
		return res, errors.New("source WAL repository identity changed")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", v.SourceURL+"/v1/wal-replication-source/"+p.ArchiveID, nil)
	req.Header = headers
	response, e := client.Do(req)
	if e != nil {
		return res, e
	}
	var records []model.WALRecord
	e = json.NewDecoder(io.LimitReader(response.Body, model.MaxManifestBytes)).Decode(&records)
	response.Body.Close()
	if e != nil || response.StatusCode != 200 {
		return res, errors.New("source WAL inventory unavailable")
	}
	var pendingNames []string
	receiverURL := v.SourceURL + "/v1/wal-replication-source/" + p.ArchiveID + "/receivers/" + p.ID
	if e = Call(ctx, client, "GET", receiverURL+"/pending", nil, &pendingNames, headers); e != nil {
		return res, e
	}
	pending := map[string]bool{}
	for _, name := range pendingNames {
		if !repository.WALName(name) {
			return res, errors.New("invalid pending WAL filename")
		}
		pending[name] = true
	}
	// Include all retained target bases, so older recovery points keep their WAL.
	points, e = m.Repo.Inventory(false)
	if e != nil {
		return res, e
	}
	floor := map[int]uint64{}
	for _, point := range points {
		meta := point.Result.PostgreSQL
		if meta == nil || meta.ArchiveID != p.ArchiveID || !point.Hold && !time.Now().Before(point.ExpiresAt) {
			continue
		}
		pos, err := repository.WALBasePosition(meta)
		if err != nil {
			return res, err
		}
		timeline := meta.WALRanges[0].Timeline
		if old, ok := floor[timeline]; !ok || pos < old {
			floor[timeline] = pos
		}
	}
	existing, e := m.Repo.WALInventory(p.ArchiveID)
	if e != nil {
		return res, e
	}
	byName := map[string]model.WALRecord{}
	for _, r := range existing {
		byName[r.Name] = r
	}
	selected := []model.WALRecord{}
	seen := map[string]bool{}
	for _, record := range records {
		if repository.ValidateWALRecord(record) != nil || record.ArchiveID != p.ArchiveID || record.SystemID != pg.SystemID || record.Major != pg.Major || record.SegmentSize != pg.SegmentSize || record.Profile != pg.Profile || record.Bytes < 1 || record.Bytes > max(pg.SegmentSize, 1<<20) || len(record.SHA256) != 64 || seen[record.Name] {
			return res, errors.New("source WAL identity/geometry inventory mismatch")
		}
		seen[record.Name] = true
		if len(record.Name) == 24 {
			pos, err := repository.WALPosition(record.Name, record.SegmentSize)
			if err != nil {
				return res, err
			}
			var timeline int
			if _, err = fmt.Sscanf(record.Name[:8], "%X", &timeline); err != nil {
				return res, err
			}
			if low, ok := floor[timeline]; ok && pos < low && !pending[record.Name] {
				continue
			}
		}
		selected = append(selected, record)
		res.TotalBytes += record.Bytes
	}
	// Source cleanup is not mirrored. Retained destination-only WAL must still
	// pass integrity checks even when no longer listed by the producer.
	for _, record := range existing {
		if seen[record.Name] {
			continue
		}
		if record.SystemID != pg.SystemID || record.Major != pg.Major || record.SegmentSize != pg.SegmentSize || record.Profile != pg.Profile {
			return res, errors.New("destination-only WAL identity mismatch")
		}
		if len(record.Name) == 24 {
			pos, err := repository.WALPosition(record.Name, record.SegmentSize)
			if err != nil {
				return res, err
			}
			var timeline int
			_, _ = fmt.Sscanf(record.Name[:8], "%X", &timeline)
			if low, ok := floor[timeline]; ok && pos < low {
				continue
			}
		}
		selected = append(selected, record)
		res.TotalBytes += record.Bytes
	}
	// Publish newly arrived segments before rechecking older, already present files.
	ordered := make([]model.WALRecord, 0, len(selected))
	for _, record := range selected {
		if _, ok := byName[record.Name]; !ok {
			ordered = append(ordered, record)
		}
	}
	for _, record := range selected {
		if _, ok := byName[record.Name]; ok {
			ordered = append(ordered, record)
		}
	}
	res.WALReplication.Segments = len(selected)
	res.TotalFiles = len(selected)
	res.Phase = "wal-replication-transfer"
	res.ProgressScope = "wal-replication"
	replicationProgress(&res)
	report(res)
	ackFiles := []model.WALAcknowledgement{}
	acknowledge := func() error {
		request := model.WALReceiverAck{Generation: v.AckGeneration, TargetRepositoryID: m.Repo.Info.ID, Files: ackFiles}
		if err := Call(ctx, client, "POST", receiverURL+"/ack", request, nil, headers); err != nil {
			return err
		}
		ackFiles = nil
		return nil
	}
	for _, record := range ordered {
		if e = ctx.Err(); e != nil {
			return res, e
		}
		if old, ok := byName[record.Name]; ok {
			if old.SHA256 != record.SHA256 || old.Bytes != record.Bytes || old.SystemID != record.SystemID || old.SegmentSize != record.SegmentSize || !sameWALTimeEvidence(old, record) {
				return res, errors.New("WAL filename or transaction-time metadata collision")
			}
			f, _, err := m.Repo.OpenWAL(p.ArchiveID, record.Name)
			if err != nil {
				return res, err
			}
			f.Close()
			res.WALReplication.Reused++
		} else {
			req, _ := http.NewRequestWithContext(ctx, "GET", v.SourceURL+"/v1/wal-replication-source/"+p.ArchiveID+"/"+record.Name, nil)
			req.Header = headers
			response, err := client.Do(req)
			if err != nil {
				return res, err
			}
			if response.StatusCode != 200 || response.Header.Get("X-SRK-SHA256") != record.SHA256 {
				response.Body.Close()
				return res, errors.New("WAL stream checksum metadata mismatch")
			}
			_, err = m.Repo.PutReplicatedWAL(record, io.LimitReader(response.Body, record.Bytes+1))
			response.Body.Close()
			if err != nil {
				return res, err
			}
			res.WALReplication.Copied++
			res.UploadedBytes += record.Bytes
		}
		if seen[record.Name] {
			ackFiles = append(ackFiles, model.WALAcknowledgement{Name: record.Name, SHA256: record.SHA256, Bytes: record.Bytes})
			if len(ackFiles) >= 128 {
				if e = acknowledge(); e != nil {
					return res, e
				}
			}
		}
		res.ProgressBytes += record.Bytes
		res.LogicalBytes += record.Bytes
		res.Files++
		replicationProgress(&res)
		report(res)
	}
	if e = acknowledge(); e != nil {
		return res, e
	}
	var lag model.WALReplicationLag
	if e = Call(ctx, client, "GET", receiverURL+"/lag", nil, &lag, headers); e != nil {
		return res, e
	}
	lag.TargetOnline = true
	res.WALReplication.Lag = &lag
	calendar, e := m.Repo.WALCalendar(p.ArchiveID)
	if e != nil {
		return res, e
	}
	res.WALReplication.Calendar = &calendar
	res.WALReplication.CheckedAt = time.Now().UTC()
	res.Verified = true
	res.RepositoryID = m.Repo.Info.ID
	res.Phase = "complete"
	completeProgress(&res)
	return res, nil
}
func sameWALTimeEvidence(a, b model.WALRecord) bool {
	if a.TimeBoundsVerified != b.TimeBoundsVerified {
		return false
	}
	equal := func(x, y *time.Time) bool {
		if x == nil || y == nil {
			return x == y
		}
		return x.Equal(*y)
	}
	return equal(a.FirstCommitAt, b.FirstCommitAt) && equal(a.LastCommitAt, b.LastCommitAt)
}
