// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/testcloud"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func addReplicaMedia(t *testing.T, e *vmEnvironment) (*Media, string) {
	t.Helper()
	cfg := e.media.Config
	cfg.ID = "media-replica"
	pki := filepath.Dir(cfg.CA)
	if err := identity.Issue(pki, cfg.Installation, "media", cfg.ID); err != nil {
		t.Fatal(err)
	}
	cfg.Cert = filepath.Join(pki, cfg.ID+".crt")
	cfg.Key = filepath.Join(pki, cfg.ID+".key")
	cfg.Repository = t.TempDir()
	media, err := NewMedia(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { media.Captures.Close(); media.Repo.Close() })
	server := tlsServer(t, cfg, media.Handler())
	if err = e.manager.DB.PutNode(context.Background(), model.Node{ID: cfg.ID, Role: "media", URL: server.URL, LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				_ = e.manager.DB.PutNode(ctx, model.Node{ID: cfg.ID, Role: "media", URL: server.URL, LastSeen: time.Now()})
			}
		}
	}()
	return media, server.URL
}
func TestReplicationIndependentRetentionDedupAndRestore(t *testing.T) {
	e := newVMEnvironment(t)
	target, _ := addReplicaMedia(t, e)
	backup := e.backup(t, testcloud.ImageVM)
	if backup.Status != "success" {
		t.Fatal(backup.Error)
	}
	sourceBefore, err := e.media.Repo.Snapshot(backup.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	req := map[string]any{"target_media_id": target.Config.ID, "retention_days": 45}
	headers := http.Header{"Idempotency-Key": {"replication-test"}}
	var run, again model.Run
	endpoint := e.url + "/v1/runs/" + backup.ID + "/replicate"
	if err = Call(context.Background(), e.admin, "POST", endpoint, req, &run, headers); err != nil {
		t.Fatal(err)
	}
	if err = Call(context.Background(), e.admin, "POST", endpoint, req, &again, headers); err != nil || run.ID != again.ID {
		t.Fatal("not idempotent", err)
	}
	done := e.wait(t, run.ID)
	if done.Status != "success" || done.Result == nil || !done.Result.Verified || done.Result.Replica == nil {
		t.Fatalf("replication: %s %s", done.Status, done.Error)
	}
	replica, err := e.manager.DB.Run(context.Background(), done.Replication.CopyRunID)
	if err != nil || replica.Kind != "backup" || replica.Job.MediaID != target.Config.ID || replica.SnapshotID != done.ID {
		t.Fatal("replica not registered as recovery point", err)
	}
	manifest, err := target.Repo.Manifest(replica.SnapshotID)
	if err != nil || manifest.Base != "" || manifest.Mode != "full" || manifest.JobID == backup.JobID {
		t.Fatal("replica retains source dependencies", err)
	}
	if manifest.ExpiresAt.Before(time.Now().Add(44*24*time.Hour)) || manifest.ExpiresAt.After(time.Now().Add(46*24*time.Hour)) {
		t.Fatal("target retention incorrect")
	}
	sourceAfter, _ := e.media.Repo.Snapshot(backup.ID, false)
	if !sourceBefore.ExpiresAt.Equal(sourceAfter.ExpiresAt) {
		t.Fatal("source retention changed")
	}
	req["retention_days"] = 46
	if err = Call(context.Background(), e.admin, "POST", endpoint, req, &again, headers); err == nil {
		t.Fatal("idempotency conflict ignored")
	}
	req["retention_days"] = 45
	if err = Call(context.Background(), e.admin, "POST", endpoint, req, &again); err != nil {
		t.Fatal(err)
	}
	second := e.wait(t, again.ID)
	if second.Status != "success" || second.Result.UploadedBytes != 0 {
		t.Fatalf("target dedup: %s %s", second.Status, second.Error)
	}
	// Remove the complete source point and its blocks; recovery must use only target.
	for _, file := range manifest.Files {
		for _, hash := range file.Chunks {
			os.Remove(filepath.Join(e.media.Repo.Root, "chunks", hash[:2], hash))
		}
	}
	os.Remove(filepath.Join(e.media.Repo.Root, "snapshots", backup.ID+".json"))
	var restore model.Run
	if err = Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+replica.ID+"/restore", map[string]any{"vm_restore": model.VMRestoreRequest{Name: "restored-from-replica", Profile: "lab", FlavorID: "small", Networks: []string{testcloud.Network}}}, &restore); err != nil {
		t.Fatal(err)
	}
	restored := e.wait(t, restore.ID)
	if restored.Status != "success" {
		t.Fatal("independent restore", restored.Error)
	}
	var reverse model.Run
	if err = Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+replica.ID+"/replicate", map[string]any{"target_media_id": e.media.Config.ID, "retention_days": 10}, &reverse); err != nil {
		t.Fatal(err)
	}
	reverse = e.wait(t, reverse.ID)
	if reverse.Status != "success" || !reverse.Result.Replica.CapturedAt.Equal(done.Result.Replica.CapturedAt) {
		t.Fatal("replica-of-replica lost capture time", reverse.Error)
	}
	if _, err = e.manager.DB.Pool.Exec(context.Background(), "DELETE FROM srk_runs WHERE id=$1", replica.ID); err != nil {
		t.Fatal(err)
	}
	done.Status = "error"
	done.Error = "simulated lost acknowledgement"
	if err = e.manager.DB.SaveRun(context.Background(), done); err != nil {
		t.Fatal(err)
	}
	var reconciled model.Run
	if err = Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+done.ID+"/reconcile-replication", nil, &reconciled); err != nil || reconciled.Status != "success" {
		t.Fatal("durable receipt reconciliation failed", err)
	}
	if _, err = e.manager.DB.Run(context.Background(), replica.ID); err != nil {
		t.Fatal("reconciliation did not register recovery point", err)
	}
	events, err := e.manager.DB.RunEvents(context.Background(), done.ID)
	if err != nil || len(events) < 2 {
		t.Fatal("replication journal missing", err)
	}
}

func TestReplicationRejectsCorruptSourceAndTarget(t *testing.T) {
	for _, corrupt := range []string{"source", "target"} {
		t.Run(corrupt, func(t *testing.T) {
			e := newVMEnvironment(t)
			target, _ := addReplicaMedia(t, e)
			backup := e.backup(t, testcloud.ImageVM)
			if backup.Status != "success" {
				t.Fatal(backup.Error)
			}
			request := map[string]any{"target_media_id": target.Config.ID, "retention_days": 30}
			endpoint := e.url + "/v1/runs/" + backup.ID + "/replicate"
			var run model.Run
			if err := Call(context.Background(), e.admin, "POST", endpoint, request, &run); err != nil {
				t.Fatal(err)
			}
			first := e.wait(t, run.ID)
			if first.Status != "success" {
				t.Fatal(first.Error)
			}
			manifest, _ := e.media.Repo.Manifest(backup.ID)
			hash := manifest.Files[0].Chunks[0]
			root := e.media.Repo.Root
			if corrupt == "target" {
				root = target.Repo.Root
			}
			if err := os.WriteFile(filepath.Join(root, "chunks", hash[:2], hash), []byte("CORRUPTED"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := Call(context.Background(), e.admin, "POST", endpoint, request, &run); err != nil {
				t.Fatal(err)
			}
			failed := e.wait(t, run.ID)
			if failed.Status != "error" {
				t.Fatalf("corrupt %s accepted: %s", corrupt, failed.Status)
			}
			if _, err := target.Repo.Manifest(run.ID); err == nil {
				t.Fatal("corrupt transfer published")
			}
			if _, err := e.manager.DB.Run(context.Background(), failed.Replication.CopyRunID); err == nil {
				t.Fatal("failed transfer registered as usable backup")
			}
			if corrupt == "source" {
				if err := target.Repo.Verify(first.ID); err != nil {
					t.Fatal("existing good replica damaged", err)
				}
			}
		})
	}
}

func TestReplicationCancellationLeasesAndAuthority(t *testing.T) {
	e := newVMEnvironment(t)
	target, targetURL := addReplicaMedia(t, e)
	backup := e.backup(t, testcloud.ImageVM)
	if backup.Status != "success" {
		t.Fatal(backup.Error)
	}
	info, err := e.media.Repo.Snapshot(backup.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	requests := 0
	sourceHandler := e.media.Handler()
	slow := tlsServer(t, e.media.Config, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/chunks/") {
			requests++
			if requests == 1 {
				sourceHandler.ServeHTTP(w, r)
				return
			}
			entered <- struct{}{}
			<-r.Context().Done()
			return
		}
		sourceHandler.ServeHTTP(w, r)
	}))
	runID := model.ID()
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	g := identity.Grant{Subject: target.Config.ID, Audience: e.media.Config.ID, RunID: runID, SnapshotID: backup.ID, Permission: "read", Expires: until.Unix()}
	transfer := model.ReplicationTransfer{RunID: runID, Replication: model.Replication{SourceRunID: backup.ID, SourceMediaID: e.media.Config.ID, SourceSnapshotID: backup.ID, TargetMediaID: target.Config.ID, CopyRunID: model.ID(), RetentionDays: 7}, SourceURL: slow.URL, SourceRepositoryID: e.media.Repo.Info.ID, SourceManifestHash: info.ManifestHash, SourceGrant: identity.Sign(g, e.manager.Key), LeaseExpiresAt: until}
	managerClient, err := identity.Client(e.manager.Config, identity.Peer{Role: "media", ID: target.Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer managerClient.CloseIdleConnections()
	// A normal administrator or another media cannot initiate internal transfers.
	adminClient, err := identity.Client(e.proxy.Config, identity.Peer{Role: "media", ID: target.Config.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer adminClient.CloseIdleConnections()
	if err = Call(context.Background(), adminClient, "POST", targetURL+"/v1/replications/"+runID, transfer, new(model.Result)); err == nil {
		t.Fatal("proxy started replication")
	}
	wrong := transfer
	g.Audience = target.Config.ID
	wrong.SourceGrant = identity.Sign(g, e.manager.Key)
	if err = Call(context.Background(), managerClient, "POST", targetURL+"/v1/replications/"+runID, wrong, new(model.Result)); err == nil {
		t.Fatal("wrong grant audience accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Call(ctx, managerClient, "POST", targetURL+"/v1/replications/"+runID, transfer, new(model.Result))
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("transfer did not reach source blocks")
	}
	target.replicationMu.Lock()
	progress := target.replications[runID]
	target.replicationMu.Unlock()
	if progress.ProgressPercent == nil || *progress.ProgressPercent <= 0 || *progress.ProgressPercent >= 100 || progress.ProgressBytes <= 0 || progress.ProgressBytes >= progress.TotalBytes || progress.LogicalBytes != progress.ProgressBytes {
		t.Fatal("transfer progress does not reflect verified blocks", progress)
	}
	expired := time.Now().Add(-time.Millisecond)
	if _, err = e.media.Repo.UpdatePolicy(backup.ID, model.SnapshotPolicy{ExpiresAt: &expired}); err != nil {
		t.Fatal(err)
	}
	plan, err := e.media.Repo.PreviewGC()
	if err != nil || plan.ActiveReaders != 1 {
		t.Fatal("source not pinned during transfer", err, plan.ActiveReaders)
	}
	for _, id := range plan.Snapshots {
		if id == backup.ID {
			t.Fatal("active replication source selected for cleanup")
		}
	}
	cancel()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("cancelled transfer succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation blocked")
	}
	eventually(t, func() bool {
		target.replicationMu.Lock()
		n := len(target.replications)
		target.replicationMu.Unlock()
		return n == 0
	})
	if _, err = target.Repo.Manifest(runID); err == nil {
		t.Fatal("cancelled transfer published")
	}
	eventually(t, func() bool { p, _ := e.media.Repo.PreviewGC(); return p.ActiveReaders == 0 })
	p, err := target.Repo.PreviewGC()
	if err != nil || p.ActiveWriters != 0 {
		t.Fatal("target write lease leaked", err)
	}
}

func TestReplicationFlattensIncrementalManifest(t *testing.T) {
	e := newVMEnvironment(t)
	target, _ := addReplicaMedia(t, e)
	base := seedPoint(t, e.media, false)
	child := base
	child.ID = model.ID()
	child.Base = base.ID
	child.Mode = "incremental"
	child.Result.EffectiveMode = "incremental"
	if err := e.media.Repo.Commit(child); err != nil {
		t.Fatal(err)
	}
	child, _ = e.media.Repo.Manifest(child.ID)
	job := model.Job{ID: child.JobID, Name: "incremental replica", AgentID: e.proxy.Config.ID, MediaID: e.media.Config.ID, Type: "files", Source: child.Source, Mode: "incremental", RetentionDays: 1}
	if err := e.manager.DB.PutJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	source := model.Run{ID: child.ID, JobID: child.JobID, Job: job, Kind: "backup", Status: "success", CreatedAt: time.Now(), Result: &child.Result}
	if _, err := e.manager.DB.Enqueue(context.Background(), source, ""); err != nil {
		t.Fatal(err)
	}
	var run model.Run
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+source.ID+"/replicate", map[string]any{"target_media_id": target.Config.ID, "retention_days": 7}, &run); err != nil {
		t.Fatal(err)
	}
	done := e.wait(t, run.ID)
	if done.Status != "success" {
		t.Fatal(done.Error)
	}
	replica, err := target.Repo.Manifest(run.ID)
	if err != nil || replica.Base != "" || replica.Mode != "full" || len(replica.Files) != len(child.Files) {
		t.Fatal("incremental replica depends on source base", err)
	}
	if _, err = target.Repo.Manifest(base.ID); err == nil {
		t.Fatal("base dependency copied unnecessarily")
	}
	if err = target.Repo.Verify(replica.ID); err != nil {
		t.Fatal(err)
	}
}
