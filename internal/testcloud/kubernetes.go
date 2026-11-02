// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package testcloud

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// KubernetesCloud models only the published API contracts used by SRK.
type KubernetesCloud struct {
	RestoreObjects                                                           map[string]map[string]any
	RestoreLog                                                               []string
	RestoreFailure                                                           string
	MissingBackup, PendingPVC, ReplaceNamespace, WorkloadNotReady            bool
	ProbeFail, NoReadyPods, CleanupDenied, ForeignContent, VerifyApplication bool
	RestoreCounter                                                           int

	mu                                                                    sync.Mutex
	Snapshots                                                             map[string]map[string]any
	SnapshotCreates, SecretReads                                          int
	WrongClass, SnapshotError, NeverReady, WrongNamespace, PaginationLoop bool
}

func NewKubernetes() *KubernetesCloud {
	return &KubernetesCloud{Snapshots: map[string]map[string]any{}}
}
func (k *KubernetesCloud) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer kubernetes-fixture-token" {
			http.Error(w, "unauthorized", 401)
			return
		}
		reply := func(v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }
		p := r.URL.Path
		if k.restoreAPI(w, r) {
			return
		}
		if p == "/apis/snapshot.storage.k8s.io/v1/volumesnapshotclasses/lh-backup" {
			typ := "bak"
			if k.WrongClass {
				typ = "snap"
			}
			reply(map[string]any{"driver": "driver.longhorn.io", "deletionPolicy": "Retain", "parameters": map[string]any{"type": typ}})
			return
		}
		if p == "/api/v1/namespaces/demo" {
			reply(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "demo", "uid": "ns-uid"}})
			return
		}
		if p == "/api/v1/persistentvolumes/pv-data" {
			reply(map[string]any{"metadata": map[string]any{"name": "pv-data"}, "spec": map[string]any{"claimRef": map[string]any{"uid": "pvc-uid"}, "csi": map[string]any{"driver": "driver.longhorn.io", "volumeHandle": "lh-volume"}}})
			return
		}
		snapBase := "/apis/snapshot.storage.k8s.io/v1/namespaces/demo/volumesnapshots"
		if p == snapBase && r.Method == "POST" {
			var snap map[string]any
			if json.NewDecoder(r.Body).Decode(&snap) != nil {
				http.Error(w, "bad JSON", 400)
				return
			}
			meta := snap["metadata"].(map[string]any)
			name := meta["name"].(string)
			if _, ok := k.Snapshots[name]; ok {
				http.Error(w, "exists", 409)
				return
			}
			meta["uid"] = "snap-" + name
			status := map[string]any{"readyToUse": !k.NeverReady, "boundVolumeSnapshotContentName": "content-" + name}
			if k.SnapshotError {
				status["error"] = map[string]any{"message": "fixture failure"}
			}
			snap["status"] = status
			k.Snapshots[name] = snap
			k.SnapshotCreates++
			reply(snap)
			return
		}
		if strings.HasPrefix(p, snapBase+"/") {
			if s, ok := k.Snapshots[strings.TrimPrefix(p, snapBase+"/")]; ok {
				reply(s)
				return
			}
			http.NotFound(w, r)
			return
		}
		contentBase := "/apis/snapshot.storage.k8s.io/v1/volumesnapshotcontents/content-"
		if strings.HasPrefix(p, contentBase) {
			name := strings.TrimPrefix(p, contentBase)
			reply(map[string]any{"spec": map[string]any{"driver": "driver.longhorn.io", "deletionPolicy": "Retain", "source": map[string]any{"volumeHandle": "lh-volume"}, "volumeSnapshotRef": map[string]any{"name": name, "namespace": "demo", "uid": "snap-" + name}}, "status": map[string]any{"readyToUse": true, "snapshotHandle": "bak://lh-volume/backup-" + name}})
			return
		}
		if r.Method == "GET" && strings.Contains(p, "/namespaces/demo/") {
			ns := "demo"
			if k.WrongNamespace {
				ns = "other"
			}
			items := []any{}
			cont := ""
			if strings.HasSuffix(p, "/persistentvolumeclaims") {
				items = append(items, map[string]any{"metadata": map[string]any{"name": "data", "namespace": ns, "uid": "pvc-uid"}, "spec": map[string]any{"volumeName": "pv-data", "accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "1Gi"}}}, "status": map[string]any{"phase": "Bound"}})
			}
			if strings.HasSuffix(p, "/configmaps") {
				if r.URL.Query().Get("continue") == "" {
					cont = "page-2"
					items = append(items, map[string]any{"metadata": map[string]any{"name": "settings", "namespace": ns}, "data": map[string]any{"key": "value"}})
				} else {
					items = append(items, map[string]any{"metadata": map[string]any{"name": "settings2", "namespace": ns}, "data": map[string]any{"key": "value2"}})
				}
				if k.PaginationLoop {
					cont = "page-2"
				}
			}
			if k.VerifyApplication && strings.HasSuffix(p, "/deployments") {
				items = append(items, map[string]any{"metadata": map[string]any{"name": "app", "namespace": ns}, "spec": map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": map[string]any{"app": "test"}}, "template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": "test"}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "app", "image": "fixture:1"}}}}}})
			}
			if k.VerifyApplication && strings.HasSuffix(p, "/services") {
				items = append(items, map[string]any{"metadata": map[string]any{"name": "app", "namespace": ns}, "spec": map[string]any{"type": "ClusterIP", "selector": map[string]any{"app": "test"}, "ports": []any{map[string]any{"port": 8080}}}})
			}
			if strings.HasSuffix(p, "/secrets") {
				k.SecretReads++
				items = append(items, map[string]any{"type": "kubernetes.io/service-account-token", "metadata": map[string]any{"name": "token", "namespace": ns}})
			}
			reply(map[string]any{"items": items, "metadata": map[string]any{"continue": cont}})
			return
		}
		http.NotFound(w, r)
	})
}
