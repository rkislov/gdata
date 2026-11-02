// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/kubernetes"
	"gdata/srk/internal/model"
	"gdata/srk/internal/testcloud"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestKubernetesManagerRestoreProgressHTTPS(t *testing.T) {
	cfg := configs(t)
	manager := managerTest(t, cfg["manager"])
	ms := tlsServer(t, cfg["manager"], manager.Handler())
	cloud := testcloud.NewKubernetes()
	cloud.VerifyApplication = true
	server := httptest.NewTLSServer(cloud.Handler())
	defer server.Close()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	ca, token := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
	os.WriteFile(token, []byte("kubernetes-fixture-token"), 0600)
	ac := cfg["agent"]
	ac.ManagerURL = ms.URL
	ac.Kubernetes = map[string]kubernetes.Profile{"lab": {Endpoint: server.URL, CAFile: ca, TokenFile: token, StagingDir: dir, Namespaces: []string{"demo"}, LonghornSnapshotClass: "lh-backup", MaxBytes: 1 << 20, TimeoutSeconds: 10, AllowRestore: true, AllowVerification: true, RestoreNamespacePrefix: "restore-", RestoreStorageClass: "longhorn"}}
	agent, err := NewAgent(ac)
	if err != nil {
		t.Fatal(err)
	}
	as := tlsServer(t, ac, agent.Handler())
	media, err := NewMedia(cfg["media"])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { media.Captures.Close(); media.Repo.Close() })
	ds := tlsServer(t, cfg["media"], media.Handler())
	for _, n := range []model.Node{{ID: ac.ID, Role: "agent", URL: as.URL, Status: agent.status()}, {ID: cfg["media"].ID, Role: "media", URL: ds.URL}} {
		client, e := identity.Client(cfg[n.Role], identity.Peer{Role: "manager"})
		if e != nil {
			t.Fatal(e)
		}
		if e = Call(context.Background(), client, "POST", ms.URL+"/v1/nodes/heartbeat", n, nil); e != nil {
			t.Fatal(e)
		}
	}
	admin, err := identity.Client(cfg["admin"], identity.Peer{Role: "manager"})
	if err != nil {
		t.Fatal(err)
	}
	job := model.Job{Name: "Kubernetes restore", Type: "longhorn", AgentID: ac.ID, MediaID: cfg["media"].ID, Source: "longhorn://lab", Mode: "full", RetentionDays: 30}
	if err = Call(context.Background(), admin, "POST", ms.URL+"/v1/jobs", job, &job); err != nil {
		t.Fatal(err)
	}
	startScheduler(t, manager)
	var backup model.Run
	if err = Call(context.Background(), admin, "POST", ms.URL+"/v1/jobs/"+job.ID+"/run", nil, &backup); err != nil {
		t.Fatal(err)
	}
	wait := func(id string) model.Run {
		var r model.Run
		eventually(t, func() bool {
			var e error
			r, e = manager.DB.Run(context.Background(), id)
			return e == nil && (r.Status == "success" || r.Status == "error")
		})
		return r
	}
	backup = wait(backup.ID)
	if backup.Status != "success" {
		t.Fatal(backup.Error)
	}
	var restore model.Run
	if err = Call(context.Background(), admin, "POST", ms.URL+"/v1/runs/"+backup.ID+"/restore", map[string]string{"destination": "kubernetes://lab/demo/restore-manager?start=true"}, &restore); err != nil {
		t.Fatal(err)
	}
	restore = wait(restore.ID)
	if restore.Status != "success" || restore.Result == nil || restore.Result.KubernetesRestore == nil || restore.Result.KubernetesRestore.Volumes != 1 || !restore.Result.Verified {
		t.Fatal("restore", restore.Status, restore.Error, restore.Result)
	}
	events, err := manager.DB.RunEvents(context.Background(), restore.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Action == "kubernetes.restore.progress" {
			found = true
		}
	}
	if !found {
		t.Fatal("progress audit missing", events)
	}
	// A completed run cannot be overwritten by a late heartbeat.
	before := restore.Result.Phase
	client, _ := identity.Client(ac, identity.Peer{Role: "manager"})
	stale := *restore.Result
	stale.Phase = "stale-progress"
	var response map[string]bool
	if err = Call(context.Background(), client, "POST", ms.URL+"/v1/runs/"+restore.ID+"/kubernetes-progress", stale, &response); err != nil {
		t.Fatal(err)
	}
	if response["accepted"] {
		t.Fatal("late progress accepted")
	}
	final, _ := manager.DB.Run(context.Background(), restore.ID)
	if final.Result.Phase != before || !final.Result.Verified {
		t.Fatal("late progress changed terminal result")
	}
	// An administrator certificate is not an agent identity.
	if err = Call(context.Background(), admin, "POST", ms.URL+"/v1/runs/"+restore.ID+"/kubernetes-progress", stale, nil); err == nil {
		t.Fatal("non-agent progress accepted")
	}

	// Schedule the latest available backup and validate application + cleanup report.
	policy := model.KubernetesCheckPolicy{JobID: job.ID, Profile: "lab", SourceNamespace: "demo", NamespacePrefix: "restore-check-", IntervalSeconds: 86400, TimeoutSeconds: 30, Enabled: true, HTTP: []model.KubernetesHTTPCheck{{Service: "app", Port: 8080, Path: "/health", ExpectedStatus: 200, Contains: "healthy"}}}
	if err = Call(context.Background(), admin, "POST", ms.URL+"/v1/kubernetes-checks", policy, &policy); err != nil {
		t.Fatal(err)
	}
	var checked *model.Run
	eventually(t, func() bool {
		checked, err = manager.DB.LatestKubernetesCheck(context.Background(), policy.ID)
		return err == nil && checked != nil && (checked.Status == "success" || checked.Status == "error")
	})
	if checked.Status != "success" || checked.Result == nil || checked.Result.KubernetesCheck == nil || checked.Result.KubernetesCheck.State != "passed" || checked.Result.KubernetesCheck.Cleanup != "complete" || len(checked.Result.KubernetesCheck.Checks) != 2 {
		t.Fatal("scheduled check", checked.Status, checked.Error, checked.Result)
	}
	if checked.SnapshotID != backup.ID {
		t.Fatal("wrong verification source", checked.SnapshotID, backup.ID)
	}
	if cloud.Restored("/api/v1/namespaces/"+checked.Result.KubernetesCheck.Namespace) != nil {
		t.Fatal("test namespace retained after success")
	}
	// Due policy while an operation is queued must not produce duplicate checks.
	manager.operationMu.Lock()
	policy.NextRun = time.Now().Add(-time.Second)
	if err = manager.DB.PutKubernetesCheck(context.Background(), policy); err != nil {
		manager.operationMu.Unlock()
		t.Fatal(err)
	}
	if err = manager.DB.ScheduleKubernetesChecks(context.Background(), time.Now()); err != nil {
		manager.operationMu.Unlock()
		t.Fatal(err)
	}
	queued, _ := manager.DB.LatestKubernetesCheck(context.Background(), policy.ID)
	policy.NextRun = time.Now().Add(-time.Second)
	if err = manager.DB.PutKubernetesCheck(context.Background(), policy); err != nil {
		manager.operationMu.Unlock()
		t.Fatal(err)
	}
	if err = manager.DB.ScheduleKubernetesChecks(context.Background(), time.Now()); err != nil {
		manager.operationMu.Unlock()
		t.Fatal(err)
	}
	same, _ := manager.DB.LatestKubernetesCheck(context.Background(), policy.ID)
	if queued == nil || same == nil || queued.ID != same.ID {
		manager.operationMu.Unlock()
		t.Fatal("overlapping verification scheduled")
	}
	policy.Enabled = false
	if err = manager.DB.PutKubernetesCheck(context.Background(), policy); err != nil {
		manager.operationMu.Unlock()
		t.Fatal(err)
	}
	manager.operationMu.Unlock()
	policy.SourceNamespace = "absent"
	policy.Enabled = true
	if err = Call(context.Background(), admin, "POST", ms.URL+"/v1/kubernetes-checks", policy, &policy); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		r, e := manager.DB.LatestKubernetesCheck(context.Background(), policy.ID)
		return e == nil && r != nil && r.Status == "error" && r.Result.KubernetesCheck.Cleanup == "not-needed"
	})
	policy.Enabled = false
	if err = Call(context.Background(), admin, "POST", ms.URL+"/v1/kubernetes-checks", policy, &policy); err != nil {
		t.Fatal(err)
	}
	// Optional interactive UI verification keeps this isolated test schema alive.
	if marker := os.Getenv("SRK_KUBERNETES_UI_MARKER"); marker != "" {
		uiURL := ms.URL
		if certFile := os.Getenv("SRK_KUBERNETES_UI_CERT"); certFile != "" {
			cert, err := tls.LoadX509KeyPair(certFile, os.Getenv("SRK_KUBERNETES_UI_KEY"))
			if err != nil {
				t.Fatal(err)
			}
			ui := httptest.NewUnstartedServer(manager.Handler())
			ui.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
			ui.StartTLS()
			defer ui.Close()
			uiURL = ui.URL
		}
		if err := os.WriteFile(marker, []byte(uiURL), 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(marker)
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			for _, n := range []model.Node{{ID: ac.ID, Role: "agent", URL: as.URL, Status: agent.status(), LastSeen: time.Now()}, {ID: cfg["media"].ID, Role: "media", URL: ds.URL, LastSeen: time.Now()}} {
				if err := manager.DB.PutNode(context.Background(), n); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(marker); os.IsNotExist(err) {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

}
