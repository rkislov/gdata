// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seedWALReplicationBase(t *testing.T, e *vmEnvironment) model.Run {
	t.Helper()
	finish := time.Now().UTC().Add(-time.Hour)
	pg := &model.PostgreSQLBackup{SystemID: "123456", SegmentSize: 1 << 20, Profile: "production", Major: 16, StartedAt: finish.Add(-time.Minute), FinishedAt: finish, NativeVerified: true, WALRanges: []model.PostgreSQLWALRange{{Timeline: 1, StartLSN: "0/200028", EndLSN: "0/300000"}}}
	pg.ArchiveID = repository.WALArchiveID("agent-test", pg.Profile, pg.SystemID)
	manifest := model.Manifest{Version: 1, ID: model.ID(), JobID: model.ID(), Source: "postgresql://production", Mode: "full", CreatedAt: finish, ExpiresAt: finish.Add(24 * time.Hour), Consistency: "postgresql-physical", PostgreSQL: pg, Result: model.Result{PostgreSQL: pg, Consistency: "postgresql-physical", EffectiveMode: "full"}, Files: []model.File{}}
	if err := e.media.Repo.Commit(manifest); err != nil {
		t.Fatal(err)
	}
	manifest, _ = e.media.Repo.Manifest(manifest.ID)
	job := model.Job{ID: manifest.JobID, Type: "postgresql", Name: "WAL replication fixture", AgentID: "agent-test", MediaID: e.media.Config.ID, Source: manifest.Source, Mode: "full", RetentionDays: 1}
	if err := e.manager.DB.PutJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	run := model.Run{ID: manifest.ID, JobID: job.ID, Job: job, Kind: "backup", Status: "success", CreatedAt: finish, Result: &manifest.Result}
	if _, err := e.manager.DB.Enqueue(context.Background(), run, ""); err != nil {
		t.Fatal(err)
	}
	return run
}
func putWALReplicationSegment(t *testing.T, media *Media, pg *model.PostgreSQLBackup, number int) model.WALRecord {
	t.Helper()
	name := fmt.Sprintf("0000000100000000%08X", number)
	record := model.WALRecord{AgentID: "agent-test", Profile: pg.Profile, SystemID: pg.SystemID, Major: pg.Major, SegmentSize: pg.SegmentSize, ArchiveID: pg.ArchiveID, Name: name, TimeBoundsVerified: true}
	at := pg.FinishedAt.Add(time.Duration(number) * 10 * time.Second)
	record.FirstCommitAt = &at
	record.LastCommitAt = &at
	data := make([]byte, pg.SegmentSize)
	binary.LittleEndian.PutUint16(data[2:4], 2)
	binary.LittleEndian.PutUint32(data[4:8], 1)
	binary.LittleEndian.PutUint64(data[8:16], uint64(number)*uint64(pg.SegmentSize))
	binary.LittleEndian.PutUint64(data[24:32], 123456)
	binary.LittleEndian.PutUint32(data[32:36], uint32(pg.SegmentSize))
	data[99] = byte(number)
	stored, err := media.Repo.PutWAL(record, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return stored
}
func waitWALReplication(t *testing.T, e *vmEnvironment, policy, previous string) model.Run {
	t.Helper()
	var run *model.Run
	eventually(t, func() bool {
		latest, err := e.manager.DB.LatestWALReplication(context.Background(), policy)
		if err != nil || latest == nil || latest.ID == previous {
			return false
		}
		run = latest
		return true
	})
	return e.wait(t, run.ID)
}
func TestWALReplicationContinuousGapsRetentionAndCorruption(t *testing.T) {
	e := newVMEnvironment(t)
	target, _ := addReplicaMedia(t, e)
	base := seedWALReplicationBase(t, e)
	pg := base.Result.PostgreSQL
	second := putWALReplicationSegment(t, e.media, pg, 2)
	putWALReplicationSegment(t, e.media, pg, 4)
	history := second
	history.Name = "00000001.history"
	history.TimeBoundsVerified = false
	history.FirstCommitAt = nil
	history.LastCommitAt = nil
	if _, err := e.media.Repo.PutWAL(history, bytes.NewReader([]byte("1\t0/200000\tfixture\n"))); err != nil {
		t.Fatal(err)
	}
	var transfer model.Run
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+base.ID+"/replicate", map[string]any{"target_media_id": target.Config.ID, "retention_days": 45, "include_wal": true}, &transfer); err != nil {
		t.Fatal(err)
	}
	copied := e.wait(t, transfer.ID)
	if copied.Status != "success" {
		t.Fatal(copied.Error)
	}
	policies, err := e.manager.DB.WALPolicies(context.Background())
	if err != nil || len(policies) != 1 || !policies[0].Enabled {
		t.Fatal("automatic continuous WAL policy missing", err, policies)
	}
	policy := policies[0]
	first := waitWALReplication(t, e, policy.ID, "")
	if first.Status != "success" || first.Result.WALReplication == nil || first.Result.WALReplication.Copied != 3 {
		t.Fatal("initial WAL sync", first.Error)
	}
	lag, err := e.media.Repo.WALReceiverLag(policy.ID)
	if err != nil || !lag.SourceProtected || lag.PendingFiles == nil || *lag.PendingFiles != 0 || lag.LastAcknowledgedAt == nil {
		t.Fatalf("successful sync missing source ACKs: %+v %v", lag, err)
	}
	calendar, err := target.Repo.WALCalendar(pg.ArchiveID)
	if err != nil || len(calendar.Windows) != 1 || calendar.Windows[0].Status != "gap" {
		t.Fatal("missing segment hidden", err, calendar)
	}
	f, stored, err := target.Repo.OpenWAL(pg.ArchiveID, second.Name)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if stored.SHA256 != second.SHA256 || !stored.ArchivedAt.Equal(second.ArchivedAt) || !sameWALTimeEvidence(stored, second) {
		t.Fatal("producer WAL evidence changed")
	}
	// The source's base may expire or be removed while its archive keeps growing.
	if err = os.Remove(filepath.Join(e.media.Repo.Root, "snapshots", base.ID+".json")); err != nil {
		t.Fatal(err)
	}
	putWALReplicationSegment(t, e.media, pg, 3)
	if err = Call(context.Background(), e.admin, "POST", e.url+"/v1/wal-replication-policies/"+policy.ID+"/run", nil, nil); err != nil {
		t.Fatal(err)
	}
	next := waitWALReplication(t, e, policy.ID, first.ID)
	if next.Status != "success" || next.Result.WALReplication.Copied != 1 {
		t.Fatal("continued delivery after source base deletion", next.Error)
	}
	calendar, err = target.Repo.WALCalendar(pg.ArchiveID)
	if err != nil || calendar.Windows[0].Status != "candidate" || calendar.Windows[0].ToExclusive == nil {
		t.Fatal("gap not repaired", err, calendar)
	}
	for _, media := range []*Media{e.media, target} {
		plan, err := media.Repo.PreviewWALGC(pg.ArchiveID, time.Now().Add(-48*time.Hour), true)
		if err != nil || plan.ActivePins != 0 {
			t.Fatal("WAL replication pin leaked", err)
		}
	}
	fifth := putWALReplicationSegment(t, e.media, pg, 5)
	if err = os.WriteFile(filepath.Join(e.media.Repo.Root, "wal", pg.ArchiveID, fifth.Name), []byte("CORRUPT"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = Call(context.Background(), e.admin, "POST", e.url+"/v1/wal-replication-policies/"+policy.ID+"/run", nil, nil); err != nil {
		t.Fatal(err)
	}
	failed := waitWALReplication(t, e, policy.ID, next.ID)
	if failed.Status != "error" {
		t.Fatal("corrupted WAL accepted", failed.Status)
	}
	records, _ := target.Repo.WALInventory(pg.ArchiveID)
	for _, r := range records {
		if r.Name == fifth.Name {
			t.Fatal("corrupt WAL metadata published")
		}
	}
	if err = Call(context.Background(), e.admin, "POST", e.url+"/v1/wal-replication-policies/"+policy.ID+"/enabled", map[string]bool{"enabled": false}, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := e.manager.DB.WALPolicy(context.Background(), policy.ID)
	if err != nil || saved.Enabled {
		t.Fatal("pause not persisted", err)
	}
	if err = e.manager.DB.ScheduleWALReplications(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	latest, _ := e.manager.DB.LatestWALReplication(context.Background(), policy.ID)
	if latest.ID != failed.ID {
		t.Fatal("paused policy enqueued")
	}
	lag, err = e.media.Repo.WALReceiverLag(policy.ID)
	if err != nil || !lag.SourceProtected || *lag.PendingFiles != 1 || *lag.PendingBytes != fifth.Bytes {
		t.Fatalf("pause/corruption lost WAL protection: %+v %v", lag, err)
	}
	missing := policy
	missing.SourceMediaID = "not-registered"
	unknown := e.manager.sourceWALLag(context.Background(), missing)
	if unknown.State != "source_unavailable" || unknown.PendingFiles != nil || unknown.PendingBytes != nil {
		t.Fatalf("offline source reported zero lag: %+v", unknown)
	}
	releaseURL := e.url + "/v1/wal-replication-policies/" + policy.ID + "/release-protection"
	if err = Call(context.Background(), e.admin, "POST", releaseURL, map[string]any{"reason": "retire fixture"}, nil); err == nil {
		t.Fatal("released without explicit confirmation")
	}
	if err = Call(context.Background(), e.admin, "POST", releaseURL, map[string]any{"reason": "retire fixture", "confirm": true}, nil); err != nil {
		t.Fatal(err)
	}
	lag, err = e.media.Repo.WALReceiverLag(policy.ID)
	if err != nil || lag.SourceProtected || lag.State != "released" || lag.PendingFiles != nil {
		t.Fatalf("release state: %+v %v", lag, err)
	}
}

func TestWALReplicationCancellationPinsAndArchiveScope(t *testing.T) {
	e := newVMEnvironment(t)
	target, targetURL := addReplicaMedia(t, e)
	source := seedWALReplicationBase(t, e)
	pg := source.Result.PostgreSQL
	putWALReplicationSegment(t, e.media, pg, 2)
	var transfer model.Run
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+source.ID+"/replicate", map[string]any{"target_media_id": target.Config.ID, "retention_days": 30}, &transfer); err != nil {
		t.Fatal(err)
	}
	copied := e.wait(t, transfer.ID)
	if copied.Status != "success" {
		t.Fatal(copied.Error)
	}
	entered := make(chan struct{}, 1)
	handler := e.media.Handler()
	slow := tlsServer(t, e.media.Config, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if repository.WALName(filepath.Base(r.URL.Path)) {
			entered <- struct{}{}
			<-r.Context().Done()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	policy := model.WALReplicationPolicy{ID: model.ID(), SourceMediaID: e.media.Config.ID, SourceRepositoryID: e.media.Repo.Info.ID, TargetMediaID: target.Config.ID, TargetRepositoryID: target.Repo.Info.ID, TargetSnapshotID: copied.ID, TargetRunID: copied.Replication.CopyRunID, ArchiveID: pg.ArchiveID, IntervalSeconds: 30, Enabled: true}
	receiver, registerErr := e.manager.ensureWALReceiver(context.Background(), policy, false)
	if registerErr != nil {
		t.Fatal(registerErr)
	}
	id := model.ID()
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	grant := identity.Grant{WALPolicyID: policy.ID, WALGeneration: receiver.Generation, WALTargetRepositoryID: target.Repo.Info.ID, Audience: e.media.Config.ID, Subject: target.Config.ID, RunID: id, Permission: "wal-replication-read", ArchiveID: pg.ArchiveID, Expires: until.Unix()}
	body := model.WALReplicationTransfer{AckGeneration: receiver.Generation, RunID: id, Policy: policy, SourceURL: slow.URL, SourceGrant: identity.Sign(grant, e.manager.Key), ExpiresAt: until}
	managerClient, err := identity.Client(e.manager.Config, identity.Peer{Role: "media", ID: target.Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer managerClient.CloseIdleConnections()
	mediaClient, err := identity.Client(target.Config, identity.Peer{Role: "media", ID: e.media.Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer mediaClient.CloseIdleConnections()
	headers := http.Header{"Authorization": {"Bearer " + body.SourceGrant}}
	if err = Call(context.Background(), mediaClient, "GET", slow.URL+"/v1/wal-replication-source/"+strings.Repeat("a", 64), nil, new(any), headers); err == nil {
		t.Fatal("archive scope bypass")
	}
	if err = Call(context.Background(), mediaClient, "GET", slow.URL+"/v1/snapshots/"+source.ID, nil, new(any), headers); err == nil {
		t.Fatal("archive grant used for base snapshot")
	}
	forbiddenClient, clientErr := identity.Client(target.Config, identity.Peer{Role: "media", ID: target.Config.ID})
	if clientErr != nil {
		t.Fatal(clientErr)
	}
	defer forbiddenClient.CloseIdleConnections()
	if err = Call(context.Background(), forbiddenClient, "POST", targetURL+"/v1/wal-replications/"+id, body, new(model.Result)); err == nil {
		t.Fatal("media started manager-only transfer")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Call(ctx, managerClient, "POST", targetURL+"/v1/wal-replications/"+id, body, new(model.Result))
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("WAL download not reached")
	}
	for _, media := range []*Media{e.media, target} {
		p, err := media.Repo.PreviewWALGC(pg.ArchiveID, time.Now().Add(-48*time.Hour), true)
		if err != nil || p.ActivePins != 1 || len(p.Blocked) == 0 {
			t.Fatal("active WAL transfer not protected against cleanup", err, p)
		}
	}
	cancel()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("cancel ignored")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not finish")
	}
	eventually(t, func() bool {
		target.replicationMu.Lock()
		n := len(target.replications)
		target.replicationMu.Unlock()
		return n == 0
	})
	records, err := target.Repo.WALInventory(pg.ArchiveID)
	if err != nil || len(records) != 0 {
		t.Fatal("cancelled WAL published", err)
	}
	eventually(t, func() bool {
		for _, media := range []*Media{e.media, target} {
			p, err := media.Repo.PreviewWALGC(pg.ArchiveID, time.Now().Add(-48*time.Hour), true)
			if err != nil || p.ActivePins != 0 {
				return false
			}
		}
		return true
	})
}

func TestWALReplicationVerifiesRetainedDestinationOnlySegments(t *testing.T) {
	e := newVMEnvironment(t)
	target, _ := addReplicaMedia(t, e)
	base := seedWALReplicationBase(t, e)
	record := putWALReplicationSegment(t, e.media, base.Result.PostgreSQL, 2)
	var copy model.Run
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+base.ID+"/replicate", map[string]any{"target_media_id": target.Config.ID, "retention_days": 30, "include_wal": true}, &copy); err != nil {
		t.Fatal(err)
	}
	copied := e.wait(t, copy.ID)
	if copied.Status != "success" {
		t.Fatal(copied.Error)
	}
	policies, err := e.manager.DB.WALPolicies(context.Background())
	if err != nil || len(policies) != 1 {
		t.Fatal(err)
	}
	p := policies[0]
	first := waitWALReplication(t, e, p.ID, "")
	if first.Status != "success" {
		t.Fatal(first.Error)
	}
	sourcePath := filepath.Join(e.media.Repo.Root, "wal", record.ArchiveID, record.Name)
	if err = os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(sourcePath + ".json"); err != nil {
		t.Fatal(err)
	}
	if err = Call(context.Background(), e.admin, "POST", e.url+"/v1/wal-replication-policies/"+p.ID+"/run", nil, nil); err != nil {
		t.Fatal(err)
	}
	retained := waitWALReplication(t, e, p.ID, first.ID)
	if retained.Status != "success" || retained.Result.WALReplication.Reused != 1 {
		t.Fatal("source cleanup mirrored or destination-only file skipped", retained.Error)
	}
	if err = os.WriteFile(filepath.Join(target.Repo.Root, "wal", record.ArchiveID, record.Name), []byte("corruption"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = Call(context.Background(), e.admin, "POST", e.url+"/v1/wal-replication-policies/"+p.ID+"/run", nil, nil); err != nil {
		t.Fatal(err)
	}
	failed := waitWALReplication(t, e, p.ID, retained.ID)
	if failed.Status != "error" {
		t.Fatal("destination-only corruption hidden")
	}
}
