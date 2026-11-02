// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package testcloud

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func km(v any) map[string]any { m, _ := v.(map[string]any); return m }
func ks(v any) string         { s, _ := v.(string); return s }

// Caller holds the fixture mutex; this is a contract simulator, not a CSI engine.
func (k *KubernetesCloud) restoreAPI(w http.ResponseWriter, r *http.Request) bool {
	if k.RestoreObjects == nil {
		k.RestoreObjects = map[string]map[string]any{}
	}
	path := r.URL.Path
	reply := func(v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }
	if path == "/apis/storage.k8s.io/v1/storageclasses/longhorn" {
		reply(map[string]any{"provisioner": "driver.longhorn.io", "volumeBindingMode": "Immediate", "reclaimPolicy": "Delete"})
		return true
	}
	if strings.Contains(path, "/namespaces/restore-") && strings.Contains(path, "/proxy") {
		if k.ProbeFail {
			http.Error(w, "not healthy", 503)
		} else {
			w.Write([]byte("healthy fixture"))
		}
		return true
	}
	if r.Method == "GET" && strings.HasSuffix(path, "/pods") && strings.Contains(path, "/namespaces/restore-") {
		ready := "True"
		if k.NoReadyPods {
			ready = "False"
		}
		items := []any{}
		ns := strings.Split(strings.TrimPrefix(path, "/api/v1/namespaces/"), "/")[0]
		for _, o := range k.RestoreObjects {
			if ks(o["kind"]) == "Deployment" && ks(km(o["metadata"])["namespace"]) == ns {
				items = append(items, map[string]any{"metadata": map[string]any{"name": "app-pod", "namespace": ns}, "status": map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": ready}}}})
			}
		}
		reply(map[string]any{"items": items})
		return true
	}
	if r.Method == "GET" && (strings.HasSuffix(path, "/persistentvolumeclaims") && strings.Contains(path, "/namespaces/restore-") || path == "/apis/snapshot.storage.k8s.io/v1/volumesnapshotcontents") {
		items := []any{}
		for key, o := range k.RestoreObjects {
			if strings.HasPrefix(key, path+"/") {
				if selector := r.URL.Query().Get("labelSelector"); selector != "" && selector != "srk.gdata/restore-run="+ks(km(km(o["metadata"])["labels"])["srk.gdata/restore-run"]) {
					continue
				}
				if k.ForeignContent && ks(o["kind"]) == "VolumeSnapshotContent" {
					km(o["spec"])["deletionPolicy"] = "Delete"
				}
				items = append(items, o)
			}
		}
		reply(map[string]any{"items": items})
		return true
	}
	if r.Method == "DELETE" {
		if k.CleanupDenied {
			http.Error(w, "denied", 403)
			return true
		}
		o := k.RestoreObjects[path]
		if o == nil {
			http.NotFound(w, r)
			return true
		}
		var options map[string]any
		json.NewDecoder(r.Body).Decode(&options)
		pre := km(options["preconditions"])
		meta := km(o["metadata"])
		if ks(pre["uid"]) == "" || ks(pre["uid"]) != ks(meta["uid"]) || ks(pre["resourceVersion"]) != ks(meta["resourceVersion"]) {
			http.Error(w, "precondition failed", 409)
			return true
		}
		if ks(o["kind"]) == "Namespace" {
			ns := ks(meta["name"])
			for key, child := range k.RestoreObjects {
				if ks(km(child["metadata"])["namespace"]) == ns || ks(km(km(child["spec"])["claimRef"])["namespace"]) == ns {
					delete(k.RestoreObjects, key)
				}
			}
		}
		delete(k.RestoreObjects, path)
		k.RestoreLog = append(k.RestoreLog, "DELETE "+ks(o["kind"])+"/"+ks(meta["name"]))
		reply(map[string]any{"kind": "Status", "status": "Success"})
		return true
	}
	if r.Method == "GET" {
		if o, ok := k.RestoreObjects[path]; ok {
			if k.ReplaceNamespace && strings.HasPrefix(path, "/api/v1/namespaces/restore-") && !strings.Contains(strings.TrimPrefix(path, "/api/v1/namespaces/"), "/") {
				km(o["metadata"])["uid"] = "replacement-namespace"
			}
			if ks(o["kind"]) == "VolumeSnapshot" {
				name := ks(km(o["spec"])["source"].(map[string]any)["volumeSnapshotContentName"])
				st := map[string]any{"readyToUse": k.RestoreObjects["/apis/snapshot.storage.k8s.io/v1/volumesnapshotcontents/"+name] != nil, "boundVolumeSnapshotContentName": name}
				if k.MissingBackup {
					st["error"] = map[string]any{"message": "backup missing"}
				}
				o["status"] = st
			}
			reply(o)
			return true
		}
	}
	isTarget := strings.Contains(path, "/namespaces/restore-") || path == "/api/v1/namespaces" || path == "/apis/snapshot.storage.k8s.io/v1/volumesnapshotcontents"
	if !isTarget {
		return false
	}
	if r.Method != "POST" && r.Method != "PUT" {
		http.NotFound(w, r)
		return true
	}
	var o map[string]any
	if json.NewDecoder(r.Body).Decode(&o) != nil {
		http.Error(w, "bad JSON", 400)
		return true
	}
	meta := km(o["metadata"])
	name := ks(meta["name"])
	kind := ks(o["kind"])
	key := path + "/" + name
	if r.Method == "PUT" {
		key = path
	}
	if kind == k.RestoreFailure {
		http.Error(w, "fixture failure", 500)
		return true
	}
	if _, ok := k.RestoreObjects[key]; ok && r.Method == "POST" {
		http.Error(w, "exists", 409)
		return true
	}
	k.RestoreCounter++
	meta["uid"] = fmt.Sprintf("restore-uid-%d", k.RestoreCounter)
	meta["resourceVersion"] = "1"
	meta["generation"] = 1
	switch kind {
	case "Namespace":
		def := map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": "default", "namespace": name, "uid": "default-" + name, "resourceVersion": "1"}}
		k.RestoreObjects["/api/v1/namespaces/"+name+"/serviceaccounts/default"] = def
	case "VolumeSnapshotContent":
		o["status"] = map[string]any{"readyToUse": !k.MissingBackup, "snapshotHandle": km(km(o["spec"])["source"])["snapshotHandle"]}
		if k.MissingBackup {
			km(o["status"])["error"] = map[string]any{"message": "missing"}
		}
	case "PersistentVolumeClaim":
		ns := ks(meta["namespace"])
		pv := "srk-pv-" + name + "-" + ns
		spec := km(o["spec"])
		spec["volumeName"] = pv
		o["status"] = map[string]any{"phase": "Bound"}
		if k.PendingPVC {
			km(o["status"])["phase"] = "Pending"
		}
		k.RestoreObjects["/api/v1/persistentvolumes/"+pv] = map[string]any{"kind": "PersistentVolume", "metadata": map[string]any{"name": pv}, "spec": map[string]any{"persistentVolumeReclaimPolicy": "Delete", "claimRef": map[string]any{"uid": meta["uid"], "namespace": ns, "name": name}, "csi": map[string]any{"driver": "driver.longhorn.io", "volumeHandle": "restored-" + pv}}}
	case "Deployment", "StatefulSet", "DaemonSet":
		n := km(o["spec"])["replicas"]
		if n == nil {
			n = 1
		}
		if k.WorkloadNotReady {
			n = 0
		}
		o["status"] = map[string]any{"observedGeneration": 1, "updatedReplicas": n, "readyReplicas": n, "availableReplicas": n, "desiredNumberScheduled": 1, "numberReady": n, "updatedNumberScheduled": n}
	}
	k.RestoreObjects[key] = o
	k.RestoreLog = append(k.RestoreLog, r.Method+" "+kind+"/"+name)
	reply(o)
	return true
}

// Snapshot returns a deep copy for assertions without racing active HTTP handlers.
func (k *KubernetesCloud) Restored(path string) map[string]any {
	k.mu.Lock()
	defer k.mu.Unlock()
	b, _ := json.Marshal(k.RestoreObjects[path])
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}
func (k *KubernetesCloud) RestoreActions() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string{}, k.RestoreLog...)
}
