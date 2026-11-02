// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package kubernetes_test

import (
	"context"
	"encoding/json"
	"fmt"
	"gdata/srk/internal/kubernetes"
	"gdata/srk/internal/model"
	"gdata/srk/internal/testcloud"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func restoreFixture(t *testing.T) (*testcloud.KubernetesCloud, kubernetes.Profile, string) {
	cloud, p := fixture(t)
	dir := t.TempDir()
	_, e := kubernetes.Backup(context.Background(), p, model.ID(), dir, nil)
	if e != nil {
		t.Fatal(e)
	}
	p.AllowRestore = true
	p.RestoreNamespacePrefix = "restore-"
	p.RestoreStorageClass = "longhorn"
	addRestoreObjects(t, dir,
		`{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"default","namespace":"demo","uid":"old-sa"},"secrets":[{"name":"old-token"}]}`,
		`{"apiVersion":"v1","kind":"Service","metadata":{"name":"app","namespace":"demo","uid":"old-service","ownerReferences":[{"uid":"old-owner"}]},"spec":{"type":"NodePort","clusterIP":"10.1.1.1","clusterIPs":["10.1.1.1"],"ports":[{"port":80,"nodePort":30080}],"selector":{"app":"app"}}}`,
		`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"app","namespace":"demo","uid":"old-app","resourceVersion":"999"},"spec":{"replicas":2,"selector":{"matchLabels":{"app":"app"}},"template":{"metadata":{"labels":{"app":"app"},"uid":"old-template"},"spec":{"containers":[{"name":"app","image":"test/app:1"}],"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data"}}]}}},"status":{"readyReplicas":2}}`,
		`{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"db","namespace":"demo"},"spec":{"replicas":1,"serviceName":"app","selector":{"matchLabels":{"app":"db"}},"template":{"metadata":{"labels":{"app":"db"}},"spec":{"containers":[{"name":"db","image":"test/db:1"}],"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data"}}]}}}}`,
		`{"apiVersion":"batch/v1","kind":"CronJob","metadata":{"name":"daily","namespace":"demo"},"spec":{"schedule":"0 1 * * *","jobTemplate":{"spec":{"template":{"spec":{"restartPolicy":"Never","containers":[{"name":"task","image":"test/task:1"}]}}}}}}`,
		`{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"monitor","namespace":"demo"},"spec":{"selector":{"matchLabels":{"app":"monitor"}},"template":{"metadata":{"labels":{"app":"monitor"}},"spec":{"containers":[{"name":"monitor","image":"test/monitor:1"}]}}}}`)
	return cloud, p, dir
}
func addRestoreObjects(t *testing.T, dir string, objects ...string) {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(dir, "kubernetes-index.json"))
	if e != nil {
		t.Fatal(e)
	}
	var idx kubernetes.Index
	if e = json.Unmarshal(b, &idx); e != nil {
		t.Fatal(e)
	}
	for _, o := range objects {
		if !json.Valid([]byte(o)) {
			t.Fatal("invalid fixture JSON")
		}
		if e = os.WriteFile(filepath.Join(dir, fmt.Sprintf("object-%06d.json", idx.Objects)), []byte(o), 0600); e != nil {
			t.Fatal(e)
		}
		idx.Objects++
	}
	b, _ = json.Marshal(idx)
	if e = os.WriteFile(filepath.Join(dir, "kubernetes-index.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
}
func TestRestoreNewNamespaceLonghornPreparedAndStart(t *testing.T) {
	for _, start := range []bool{false, true} {
		t.Run(fmt.Sprint(start), func(t *testing.T) {
			cloud, p, dir := restoreFixture(t)
			run := model.ID()
			target := kubernetes.RestoreTarget{Profile: "lab", Source: "demo", Namespace: "restore-app", Start: start}
			status, e := kubernetes.Restore(context.Background(), p, target, run, dir, nil)
			if e != nil {
				t.Fatal(e)
			}
			want := "prepared"
			if start {
				want = "complete"
			}
			if status.State != want || status.Volumes != 1 || status.Objects != status.TotalObjects {
				t.Fatal(status)
			}
			ns := cloud.Restored("/api/v1/namespaces/restore-app")
			if ns == nil {
				t.Fatal("namespace not created")
			}
			pvc := cloud.Restored("/api/v1/namespaces/restore-app/persistentvolumeclaims/data")
			ps := pvc["spec"].(map[string]any)
			if ps["volumeName"] == "pv-data" || ps["dataSource"] == nil || ps["storageClassName"] != "longhorn" {
				t.Fatal("original PV reused", pvc)
			}
			app := cloud.Restored("/apis/apps/v1/namespaces/restore-app/deployments/app")
			meta := app["metadata"].(map[string]any)
			if meta["uid"] == "old-app" || meta["namespace"] != "restore-app" || meta["ownerReferences"] != nil {
				t.Fatal("old identity retained")
			}
			wantReplicas := 0.
			if start {
				wantReplicas = 2
			}
			if app["spec"].(map[string]any)["replicas"] != wantReplicas {
				t.Fatal("workload start policy")
			}
			svc := cloud.Restored("/api/v1/namespaces/restore-app/services/app")
			ss := svc["spec"].(map[string]any)
			if ss["type"] != "ClusterIP" || ss["clusterIP"] != nil {
				t.Fatal("original service address retained")
			}
			actions := len(cloud.RestoreActions())
			again, e := kubernetes.Restore(context.Background(), p, target, run, dir, nil)
			if e != nil || again.State != want || len(cloud.RestoreActions()) != actions {
				t.Fatal("completed replay issued writes", e)
			}
			if start {
				createdDB, waitApp := -1, -1
				for i, ev := range status.Events {
					if ev.Phase == "resource-created" && ev.Resource == "StatefulSet/db" {
						createdDB = i
					}
					if ev.Phase == "workload-wait" && ev.Resource == "Deployment/app" {
						waitApp = i
					}
				}
				if createdDB < 0 || waitApp < createdDB {
					t.Fatal("waited before all controllers created")
				}
			}
			if _, e = kubernetes.Restore(context.Background(), p, target, model.ID(), dir, nil); e == nil || len(cloud.RestoreActions()) != actions {
				t.Fatal("existing namespace overwritten")
			}
		})
	}
}
func TestRestoreFailuresRetainJournalAndNeverCreateEmptyPVC(t *testing.T) {
	for _, mode := range []string{"backup-missing", "pvc-timeout", "namespace-replaced", "api-failure", "workload-timeout"} {
		t.Run(mode, func(t *testing.T) {
			cloud, p, dir := restoreFixture(t)
			switch mode {
			case "backup-missing":
				cloud.MissingBackup = true
			case "pvc-timeout":
				cloud.PendingPVC = true
			case "namespace-replaced":
				cloud.ReplaceNamespace = true
			case "api-failure":
				cloud.RestoreFailure = "Deployment"
			case "workload-timeout":
				cloud.WorkloadNotReady = true
			}
			ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancel()
			run := model.ID()
			target := kubernetes.RestoreTarget{Profile: "lab", Source: "demo", Namespace: "restore-failed", Start: true}
			status, e := kubernetes.Restore(ctx, p, target, run, dir, nil)
			if e == nil || status.State != "failed" {
				t.Fatal("unconfirmed restore succeeded", status, e)
			}
			actions := cloud.RestoreActions()
			if mode == "backup-missing" {
				for _, action := range actions {
					if strings.Contains(action, "PersistentVolumeClaim/") {
						t.Fatal("PVC created without backup")
					}
				}
			}
			if _, e = os.Stat(filepath.Join(p.StagingDir, "restore-journals", run+".json")); e != nil {
				t.Fatal(e)
			}
			if _, e = kubernetes.Restore(context.Background(), p, target, run, dir, nil); e == nil || len(cloud.RestoreActions()) != len(actions) {
				t.Fatal("incomplete restore replayed")
			}
		})
	}
}
func TestRestorePreflightProtectsNamespaceAndMissingVolumes(t *testing.T) {
	cloud, p, dir := restoreFixture(t)
	pvcFile := filepath.Join(dir, "kubernetes-index.json")
	b, _ := os.ReadFile(pvcFile)
	var idx kubernetes.Index
	json.Unmarshal(b, &idx)
	idx.PVCData = "not-included"
	idx.Volumes = nil
	b, _ = json.Marshal(idx)
	os.WriteFile(pvcFile, b, 0600)
	_, e := kubernetes.Restore(context.Background(), p, kubernetes.RestoreTarget{Source: "demo", Namespace: "restore-empty"}, model.ID(), dir, nil)
	if e == nil || len(cloud.RestoreActions()) != 0 {
		t.Fatal("metadata-only PVC silently restored empty")
	}
	for _, raw := range []string{"kubernetes://lab/demo/demo", "kubernetes://lab/demo/default", "kubernetes://lab/demo/kube-system", "kubernetes://lab/demo/restore-a?start=yes", "kubernetes://user:password@lab/demo/restore-a"} {
		if _, e = kubernetes.ParseRestoreTarget(raw); e == nil {
			t.Fatal("unsafe destination", raw)
		}
	}
}
