// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
)

func TestPITRTargetValidation(t *testing.T) {
	base := &model.PostgreSQLBackup{ArchiveID: "archive", SystemID: "1234", SegmentSize: 1 << 20, FinishedAt: time.Now().Add(-time.Hour), WALRanges: []model.PostgreSQLWALRange{{Timeline: 1, StartLSN: "0/100028"}}}
	if e := validatePITR(&model.PITRRequest{TargetTime: time.Now().Add(-time.Minute)}, base); e != nil {
		t.Fatal(e)
	}
	for _, at := range []time.Time{{}, time.Now().Add(time.Hour), base.FinishedAt.Add(-time.Second)} {
		if validatePITR(&model.PITRRequest{TargetTime: at}, base) == nil {
			t.Fatal("invalid recovery target accepted")
		}
	}
	if validatePITR(&model.PITRRequest{TargetTime: time.Now().Add(-time.Minute)}, nil) == nil {
		t.Fatal("PITR without a WAL-linked base accepted")
	}
}
func TestServiceShutdownCancelsActiveRequest(t *testing.T) {
	cfg := configs(t)
	ac := cfg["agent"]
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	ac.Listen = listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	canceled := make(chan struct{})
	done := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(canceled) })
	go func() { done <- Serve(ctx, ac, handler) }()
	cl, e := identity.Client(cfg["admin"], identity.Peer{Role: "agent"})
	if e != nil {
		t.Fatal(e)
	}
	defer cl.CloseIdleConnections()
	requestCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	go func() {
		for requestCtx.Err() == nil {
			req, _ := http.NewRequestWithContext(requestCtx, "GET", "https://"+ac.Listen+"/work", nil)
			r, e := cl.Do(req)
			if e == nil {
				r.Body.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("service did not accept a request")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the active request")
	}
	select {
	case e = <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("service did not shut down")
	}
}
func linkedPITRPoint(t *testing.T, m *Media) model.Manifest {
	t.Helper()
	at := time.Now().UTC().Add(-time.Hour)
	pg := &model.PostgreSQLBackup{Profile: "production", SystemID: "1234", SegmentSize: 1 << 20, ArchiveID: repository.WALArchiveID("agent-test", "production", "1234"), Major: 16, StartedAt: at.Add(-time.Minute), FinishedAt: at, NativeVerified: true, WALRanges: []model.PostgreSQLWALRange{{Timeline: 1, StartLSN: "0/100028", EndLSN: "0/100100"}}}
	point := model.Manifest{ID: model.ID(), JobID: model.ID(), Version: 1, Source: "postgresql://production", CreatedAt: at, ExpiresAt: time.Now().Add(time.Hour), Mode: "full", Consistency: "postgresql-physical", PostgreSQL: pg, Files: []model.File{}, Result: model.Result{PostgreSQL: pg, Verified: true, Consistency: "postgresql-physical"}}
	if e := m.Repo.Commit(point); e != nil {
		t.Fatal(e)
	}
	return point
}
func TestPITRRequestPersistsTargetAndRejectsIdempotencyConflict(t *testing.T) {
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	server := tlsServer(t, cfg["manager"], m.Handler())
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	defer media.Repo.Close()
	ms := tlsServer(t, cfg["media"], media.Handler())
	point := linkedPITRPoint(t, media)
	ctx := context.Background()
	if e = m.DB.PutNode(ctx, model.Node{ID: cfg["media"].ID, Role: "media", URL: ms.URL, LastSeen: time.Now().UTC()}); e != nil {
		t.Fatal(e)
	}
	job := model.Job{ID: point.JobID, Type: "postgresql", AgentID: cfg["agent"].ID, MediaID: cfg["media"].ID, Source: point.Source, Mode: "full", RetentionDays: 30}
	run := model.Run{ID: point.ID, JobID: job.ID, Job: job, Kind: "backup", Status: "success", CreatedAt: point.CreatedAt, Result: &point.Result}
	if _, e = m.DB.Enqueue(ctx, run, ""); e != nil {
		t.Fatal(e)
	}
	cl, e := identity.Client(cfg["admin"], identity.Peer{Role: "manager"})
	if e != nil {
		t.Fatal(e)
	}
	defer cl.CloseIdleConnections()
	header := http.Header{"Idempotency-Key": []string{"same-pitr"}}
	target := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	request := map[string]any{"destination": "/restore/new", "pitr": &model.PITRRequest{TargetTime: target}}
	var restored model.Run
	endpoint := server.URL + "/v1/runs/" + run.ID + "/restore"
	if e = Call(ctx, cl, "POST", endpoint, request, &restored, header); e != nil {
		t.Fatal(e)
	}
	if restored.PITR == nil || !restored.PITR.TargetTime.Equal(target) {
		t.Fatal("PITR request was not retained")
	}
	request["pitr"] = &model.PITRRequest{TargetTime: target.Add(-time.Second)}
	if e = Call(ctx, cl, "POST", endpoint, request, &restored, header); e == nil {
		t.Fatal("different PITR target reused an idempotency key")
	}
}
func TestWALReadGrantIsBoundToBaseArchive(t *testing.T) {
	cfg := configs(t)
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	defer media.Repo.Close()
	ms := tlsServer(t, cfg["media"], media.Handler())
	point := linkedPITRPoint(t, media)
	cl, e := identity.Client(cfg["agent"], identity.Peer{Role: "media"})
	if e != nil {
		t.Fatal(e)
	}
	defer cl.CloseIdleConnections()
	key, _ := os.ReadFile(cfg["media"].CapabilityKeyFile)
	grant := identity.Sign(identity.Grant{Subject: cfg["agent"].ID, RunID: model.ID(), SnapshotID: point.ID, ArchiveID: point.PostgreSQL.ArchiveID, Permission: "read", Expires: time.Now().Add(time.Hour).Unix()}, key)
	header := http.Header{"Authorization": []string{"Bearer " + grant}}
	var records []model.WALRecord
	if e = Call(context.Background(), cl, "GET", ms.URL+"/v1/wal/"+point.PostgreSQL.ArchiveID, nil, &records, header); e != nil {
		t.Fatal(e)
	}
	other := repository.WALArchiveID("other", "production", "1234")
	if e = Call(context.Background(), cl, "GET", ms.URL+"/v1/wal/"+url.PathEscape(other), nil, &records, header); e == nil {
		t.Fatal("read grant could access another WAL archive")
	}
	auditID := model.ID()
	if e = media.Repo.RecordAudit(model.RepositoryAudit{ID: auditID, Actor: "manager/test", Action: "gc", StartedAt: time.Now().UTC(), Phase: "success"}); e != nil {
		t.Fatal(e)
	}
	data := make([]byte, 1<<20)
	binary.LittleEndian.PutUint16(data[2:4], 2)
	binary.LittleEndian.PutUint32(data[4:8], 1)
	binary.LittleEndian.PutUint64(data[8:16], 1<<20)
	binary.LittleEndian.PutUint64(data[24:32], 1234)
	binary.LittleEndian.PutUint32(data[32:36], 1<<20)
	wal := model.WALRecord{ArchiveID: point.PostgreSQL.ArchiveID, AgentID: cfg["agent"].ID, Profile: "production", SystemID: "1234", Major: 16, SegmentSize: 1 << 20, Name: "000000010000000000000001"}
	b, _ := json.Marshal(wal)
	req, _ := http.NewRequest("PUT", ms.URL+"/v1/wal/"+wal.ArchiveID+"/"+wal.Name, bytes.NewReader(data))
	req.Header.Set("X-SRK-WAL", string(b))
	req.Header.Set("X-Operation-ID", auditID)
	response, e := cl.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("WAL upload failed", response.StatusCode)
	}
	audits, e := media.Repo.Audits()
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, a := range audits {
		if a.ID == auditID {
			found = true
			if a.Actor != "manager/test" || a.Action != "gc" {
				t.Fatal("WAL writer overwrote management audit")
			}
		}
	}
	if !found {
		t.Fatal("management audit disappeared")
	}
}
func TestWALCertificateCannotUseOtherAgentEndpoints(t *testing.T) {
	cfg := configs(t)
	ac := cfg["agent"]
	pki := filepath.Dir(ac.CA)
	if e := identity.Issue(pki, "test", "wal", ac.ID+"-wal"); e != nil {
		t.Fatal(e)
	}
	c := cfg["admin"]
	c.Role = "wal"
	c.ID = ac.ID + "-wal"
	c.Cert = filepath.Join(pki, c.ID+".crt")
	c.Key = filepath.Join(pki, c.ID+".key")
	a, e := NewAgent(ac)
	if e != nil {
		t.Fatal(e)
	}
	server := tlsServer(t, ac, a.Handler())
	cl, e := identity.Client(c, identity.Peer{Role: "agent"})
	if e != nil {
		t.Fatal(e)
	}
	defer cl.CloseIdleConnections()
	r, e := cl.Get(server.URL + "/v1/status")
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatal("WAL certificate obtained general agent diagnostics")
	}
	var out any
	if e = Call(context.Background(), cl, "POST", server.URL+"/v1/execute", model.Execute{}, &out); e == nil {
		t.Fatal("WAL certificate could execute backup")
	}
}
