// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

var namespaceRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

type RestoreTarget struct {
	Profile, Source, Namespace string
	Start                      bool
	Verification               bool
}

func ParseRestoreTarget(raw string) (RestoreTarget, error) {
	var t RestoreTarget
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "kubernetes" || u.User != nil || u.Fragment != "" || u.Port() != "" || !nameRE.MatchString(u.Host) || len(u.Host) > 64 {
		return t, errors.New("destination requires kubernetes://PROFILE/SOURCE_NAMESPACE/NEW_NAMESPACE?start=false")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 {
		return t, errors.New("source and new namespace required")
	}
	for _, n := range parts {
		if len(n) > 63 || !namespaceRE.MatchString(n) {
			return t, errors.New("invalid namespace")
		}
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil {
		return t, e
	}
	for k, v := range q {
		if k != "start" || len(v) != 1 || (v[0] != "true" && v[0] != "false") {
			return t, errors.New("only start=true|false is supported")
		}
	}
	if parts[0] == parts[1] || strings.HasPrefix(parts[1], "kube-") || parts[1] == "default" {
		return t, errors.New("a new non-system namespace is required")
	}
	return RestoreTarget{Profile: u.Host, Source: parts[0], Namespace: parts[1], Start: q.Get("start") == "true"}, nil
}

type restoreObject struct {
	object   map[string]any
	resource resource
	name     string
	rank     int
}
type restorePlan struct {
	objects   []restoreObject
	pvcs      []restoreObject
	volumes   map[string]Volume
	namespace map[string]any
}

func cleanMetadata(o map[string]any, ns, run string) {
	old := obj(o["metadata"])
	labels := obj(old["labels"])
	if labels == nil {
		labels = map[string]any{}
	}
	delete(labels, "kubernetes.io/metadata.name")
	labels["srk.gdata/restore-run"] = run
	annotations := obj(old["annotations"])
	for k := range annotations {
		if k == "kubectl.kubernetes.io/last-applied-configuration" || strings.HasPrefix(k, "pv.kubernetes.io/") || strings.HasPrefix(k, "volume.kubernetes.io/") || strings.HasPrefix(k, "volume.beta.kubernetes.io/") {
			delete(annotations, k)
		}
	}
	m := map[string]any{"name": old["name"], "labels": labels}
	if len(annotations) > 0 {
		m["annotations"] = annotations
	}
	if ns != "" {
		m["namespace"] = ns
	}
	o["metadata"] = m
	delete(o, "status")
}
func resourceFor(o map[string]any) (resource, bool) {
	for _, r := range append(append([]resource{}, resources...), resource{"/api/v1", "secrets", "Secret"}) {
		version := strings.TrimPrefix(strings.TrimPrefix(r.base, "/apis/"), "/api/")
		if str(o["kind"]) == r.kind && str(o["apiVersion"]) == version {
			return r, true
		}
	}
	return resource{}, false
}
func prepareRestore(p Profile, t RestoreTarget, run, dir string) (restorePlan, error) {
	plan := restorePlan{volumes: map[string]Volume{}}
	var used int64
	readJSON := func(name string, out any) error {
		f, e := os.Open(filepath.Join(dir, name))
		if e != nil {
			return e
		}
		defer f.Close()
		info, e := f.Stat()
		if e != nil {
			return e
		}
		if info.Size() > p.MaxBytes-used {
			return errors.New("restore staging quota exceeded")
		}
		used += info.Size()
		return json.NewDecoder(f).Decode(out)
	}
	var idx Index
	if e := readJSON("kubernetes-index.json", &idx); e != nil {
		return plan, e
	}
	if idx.Version != 1 || idx.Objects < 1 || idx.Objects > 10000 || !slices.Contains(idx.Namespaces, t.Source) {
		return plan, errors.New("source namespace not in backup index")
	}
	for _, v := range idx.Volumes {
		if v.Namespace != t.Source {
			continue
		}
		u, e := url.Parse(v.Handle)
		if e != nil || u.Scheme != "bak" || u.Host == "" || strings.TrimPrefix(u.Path, "/") == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !nameRE.MatchString(v.PVC) {
			return plan, errors.New("invalid Longhorn backup handle")
		}
		if _, ok := plan.volumes[v.PVC]; ok {
			return plan, errors.New("duplicate PVC backup")
		}
		plan.volumes[v.PVC] = v
	}
	seen := map[string]bool{}
	desiredReplicas := map[string]int{}
	for i := 0; i < idx.Objects; i++ {
		var o map[string]any
		if e := readJSON(fmt.Sprintf("object-%06d.json", i), &o); e != nil {
			return plan, e
		}
		m := obj(o["metadata"])
		name := str(m["name"])
		if str(o["kind"]) == "Namespace" && str(o["apiVersion"]) == "v1" {
			if name == t.Source {
				if plan.namespace != nil {
					return plan, errors.New("duplicate namespace")
				}
				plan.namespace = o
			}
			continue
		}
		if str(m["namespace"]) != t.Source {
			continue
		}
		r, ok := resourceFor(o)
		if !ok || !nameRE.MatchString(name) || len(name) > 253 || seen[r.kind+"/"+name] {
			return plan, errors.New("unsupported or duplicate Kubernetes resource")
		}
		seen[r.kind+"/"+name] = true
		cleanMetadata(o, t.Namespace, run)
		spec := obj(o["spec"])
		rank := 1
		switch r.kind {
		case "PersistentVolumeClaim":
			if idx.PVCData != "longhorn-external-backupstore" {
				return plan, errors.New("PVC data is absent; refusing to create empty replacement volumes")
			}
			if _, ok := plan.volumes[name]; !ok {
				return plan, errors.New("PVC has no Longhorn backup")
			}
			if mode := str(spec["volumeMode"]); mode != "" && mode != "Filesystem" && mode != "Block" {
				return plan, errors.New("unsupported PVC volumeMode")
			}
			if spec == nil || obj(obj(spec["resources"])["requests"])["storage"] == nil {
				return plan, errors.New("PVC storage request missing")
			}
			for _, k := range []string{"volumeName", "selector", "dataSource", "dataSourceRef", "volumeAttributesClassName"} {
				delete(spec, k)
			}
			spec["storageClassName"] = p.RestoreStorageClass
			plan.pvcs = append(plan.pvcs, restoreObject{o, r, name, 0})
			continue
		case "Secret":
			if str(o["type"]) == "kubernetes.io/service-account-token" {
				return plan, errors.New("service-account tokens cannot be restored")
			}
		case "ServiceAccount":
			delete(o, "secrets")
		case "Service":
			if spec == nil {
				return plan, errors.New("invalid Service")
			}
			if str(spec["clusterIP"]) != "None" {
				delete(spec, "clusterIP")
				delete(spec, "clusterIPs")
			}
			for _, k := range []string{"ipFamilies", "ipFamilyPolicy", "healthCheckNodePort", "loadBalancerIP", "loadBalancerClass", "allocateLoadBalancerNodePorts", "externalIPs", "externalTrafficPolicy"} {
				delete(spec, k)
			}
			if str(spec["type"]) == "LoadBalancer" || str(spec["type"]) == "NodePort" {
				spec["type"] = "ClusterIP"
			}
			ports, _ := spec["ports"].([]any)
			for _, port := range ports {
				delete(obj(port), "nodePort")
			}
		case "RoleBinding":
			role := obj(o["roleRef"])
			if str(role["kind"]) == "ClusterRole" && !slices.Contains(p.RestoreClusterRoles, str(role["name"])) {
				return plan, errors.New("ClusterRole binding is not allowlisted in restore profile")
			}
			subs, _ := o["subjects"].([]any)
			for _, sub := range subs {
				s := obj(sub)
				if str(s["kind"]) == "ServiceAccount" {
					if str(s["namespace"]) != "" && str(s["namespace"]) != t.Source {
						return plan, errors.New("cross-namespace ServiceAccount binding requires manual restoration")
					}
					s["namespace"] = t.Namespace
				}
			}
			rank = 2
		case "Ingress":
			if t.Verification {
				continue
			}
			if !p.AllowIngressRestore {
				return plan, errors.New("Ingress requires allow_ingress_restore in target profile")
			}
			rank = 4
		case "Deployment", "StatefulSet", "DaemonSet", "CronJob":
			rank = 3
			if r.kind == "StatefulSet" {
				desiredReplicas[name] = number(spec["replicas"], 1)
			}
			if spec == nil {
				return plan, errors.New("workload spec missing")
			}
			if t.Verification {
				template := obj(spec["template"])
				if r.kind == "CronJob" {
					template = obj(obj(obj(spec["jobTemplate"])["spec"])["template"])
				}
				pod := obj(template["spec"])
				if pod["hostNetwork"] == true || pod["hostPID"] == true || pod["hostIPC"] == true {
					return plan, errors.New("verification refuses host namespaces")
				}
				vols, _ := pod["volumes"].([]any)
				for _, v := range vols {
					if obj(v)["hostPath"] != nil {
						return plan, errors.New("verification refuses hostPath volumes")
					}
				}
				for _, key := range []string{"containers", "initContainers"} {
					containers, _ := pod[key].([]any)
					for _, v := range containers {
						if obj(obj(v)["securityContext"])["privileged"] == true {
							return plan, errors.New("verification refuses privileged containers")
						}
					}
				}
			}
			if t.Verification && r.kind == "CronJob" {
				spec["suspend"] = true
			}
			if !t.Start {
				switch r.kind {
				case "Deployment", "StatefulSet":
					spec["replicas"] = 0
				case "CronJob":
					spec["suspend"] = true
				case "DaemonSet":
					template := obj(spec["template"])
					pod := obj(template["spec"])
					if pod == nil {
						return plan, errors.New("DaemonSet pod spec missing")
					}
					aff := obj(pod["affinity"])
					if aff == nil {
						aff = map[string]any{}
						pod["affinity"] = aff
					}
					aff["nodeAffinity"] = map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{map[string]any{"key": "srk.gdata/restore-paused", "operator": "Exists"}, map[string]any{"key": "srk.gdata/restore-paused", "operator": "DoesNotExist"}}}}}}
				}
			}
			if r.kind == "StatefulSet" {
				templates, _ := spec["volumeClaimTemplates"].([]any)
				for _, v := range templates {
					claim := obj(v)
					cleanMetadata(claim, "", run)
					cs := obj(claim["spec"])
					delete(cs, "volumeName")
					delete(cs, "dataSource")
					delete(cs, "dataSourceRef")
					cs["storageClassName"] = p.RestoreStorageClass
				}
				spec["persistentVolumeClaimRetentionPolicy"] = map[string]any{"whenDeleted": "Retain", "whenScaled": "Retain"}
			}
		}
		plan.objects = append(plan.objects, restoreObject{o, r, name, rank})
	}
	if plan.namespace == nil {
		return plan, errors.New("source namespace object missing")
	}
	if len(plan.volumes) != len(plan.pvcs) {
		return plan, errors.New("PVC backup index does not match objects")
	}
	// Refuse missing persistent data before creating any target resources.
	for _, o := range plan.objects {
		if o.rank != 3 {
			continue
		}
		spec := obj(o.object["spec"])
		template := obj(spec["template"])
		if o.resource.kind == "CronJob" {
			template = obj(obj(obj(spec["jobTemplate"])["spec"])["template"])
		}
		if tm := obj(template["metadata"]); tm != nil {
			for _, k := range []string{"namespace", "uid", "resourceVersion", "creationTimestamp", "managedFields", "ownerReferences", "finalizers", "generation"} {
				delete(tm, k)
			}
		}
		pod := obj(template["spec"])
		if pod == nil {
			return plan, errors.New("workload pod template missing")
		}
		vols, _ := pod["volumes"].([]any)
		for _, v := range vols {
			claim := str(obj(obj(v)["persistentVolumeClaim"])["claimName"])
			if claim != "" {
				if _, ok := plan.volumes[claim]; !ok {
					return plan, errors.New("workload references a PVC without a backup")
				}
			}
		}
		if o.resource.kind == "StatefulSet" {
			replicas := desiredReplicas[o.name]
			if replicas < 0 || replicas > 10000 {
				return plan, errors.New("invalid StatefulSet replicas")
			}
			templates, _ := spec["volumeClaimTemplates"].([]any)
			start := number(obj(spec["ordinals"])["start"], 0)
			for _, v := range templates {
				prefix := str(obj(obj(v)["metadata"])["name"])
				for n := start; n < start+replicas; n++ {
					if _, ok := plan.volumes[fmt.Sprintf("%s-%s-%d", prefix, o.name, n)]; !ok {
						return plan, errors.New("StatefulSet claim lacks a backup")
					}
				}
			}
		}
	}
	sort.SliceStable(plan.objects, func(i, j int) bool { return plan.objects[i].rank < plan.objects[j].rank })
	return plan, nil
}
func number(v any, def int) int {
	f, ok := v.(float64)
	if !ok {
		n, ok := v.(int)
		if ok {
			return n
		}
		return def
	}
	return int(f)
}
func restoreName(run, pvc string) string {
	sum := sha256.Sum256([]byte(pvc))
	return "srk-r-" + run + "-" + hex.EncodeToString(sum[:6])
}

type restoreJournal struct {
	Target       RestoreTarget           `json:"target"`
	Fingerprint  string                  `json:"fingerprint"`
	NamespaceUID string                  `json:"namespace_uid,omitempty"`
	Result       model.KubernetesRestore `json:"result"`
	Error        string                  `json:"error,omitempty"`
}

func Restore(ctx context.Context, p Profile, target RestoreTarget, run, dir string, report func(model.KubernetesRestore)) (status model.KubernetesRestore, err error) {
	status = model.KubernetesRestore{Namespace: target.Namespace, SourceNamespace: target.Source, WorkloadsStarted: target.Start, State: "preflight", Events: []model.KubernetesRestoreEvent{}}
	if !p.AllowRestore || !strings.HasPrefix(target.Namespace, p.RestoreNamespacePrefix) || !namespaceRE.MatchString(target.Namespace) || len(target.Namespace) > 63 || target.Source == target.Namespace || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(run) {
		return status, errors.New("restore target not allowed by profile")
	}
	c, e := newClient(p)
	if e != nil {
		return status, e
	}
	defer c.http.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	plan, e := prepareRestore(p, target, run, dir)
	if e != nil {
		return status, e
	}
	status.TotalObjects = 1 + len(plan.objects) + len(plan.pvcs)
	status.TotalVolumes = len(plan.pvcs)
	// Fingerprint covers the entire validated input, including normalization policy.
	raw, e := json.Marshal(struct {
		Target  RestoreTarget
		Profile Profile
		Objects []map[string]any
		Volumes map[string]Volume
	}{Target: target, Profile: p, Objects: func() []map[string]any {
		v := []map[string]any{plan.namespace}
		for _, o := range append(append([]restoreObject{}, plan.objects...), plan.pvcs...) {
			v = append(v, o.object)
		}
		return v
	}(), Volumes: plan.volumes})
	if e != nil {
		return status, e
	}
	fingerprint := repository.Hash(raw)
	journalDir := filepath.Join(p.StagingDir, "restore-journals")
	if e = os.MkdirAll(journalDir, 0700); e != nil {
		return status, e
	}
	journalPath := filepath.Join(journalDir, run+".json")
	lock, e := os.OpenFile(filepath.Join(journalDir, run+".started"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		if !os.IsExist(e) {
			return status, e
		}
		var old restoreJournal
		b, re := os.ReadFile(journalPath)
		if re != nil || json.Unmarshal(b, &old) != nil || old.Fingerprint != fingerprint {
			return status, errors.New("restore run already used or journal unavailable")
		}
		if old.Result.State == "complete" || old.Result.State == "prepared" {
			return old.Result, nil
		}
		return old.Result, errors.New("restore was interrupted; inspect retained namespace and journal, use a new target namespace")
	}
	if e = lock.Sync(); e != nil {
		lock.Close()
		return status, e
	}
	lock.Close()
	journal := restoreJournal{Target: target, Fingerprint: fingerprint}
	saveJournal := func() error {
		journal.Result = status
		b, e := json.Marshal(journal)
		if e != nil {
			return e
		}
		return repository.Atomic(journalPath, b, 0600)
	}
	emit := func(phase, resource string) error {
		status.State = phase
		status.CurrentResource = resource
		event := model.KubernetesRestoreEvent{At: time.Now().UTC().Format(time.RFC3339Nano), Phase: phase, Resource: resource}
		f, e := os.OpenFile(filepath.Join(journalDir, run+".events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		b, _ := json.Marshal(event)
		_, e = f.Write(append(b, '\n'))
		if e == nil {
			e = f.Sync()
		}
		f.Close()
		if e != nil {
			return e
		}
		status.Events = append(status.Events, event)
		if len(status.Events) > 200 {
			status.Events = append([]model.KubernetesRestoreEvent{}, status.Events[len(status.Events)-200:]...)
		}
		if e = saveJournal(); e != nil {
			return e
		}
		if report != nil {
			copy := status
			copy.Events = append([]model.KubernetesRestoreEvent{}, status.Events...)
			report(copy)
		}
		return nil
	}
	defer func() {
		if err != nil {
			journal.Error = err.Error()
			if e := emit("failed", status.CurrentResource); e != nil {
				err = errors.Join(err, e)
			}
		}
	}()
	if e = emit("preflight", target.Namespace); e != nil {
		return status, e
	}
	if _, code, e := c.call(ctx, "GET", "/api/v1/namespaces/"+target.Namespace, nil); e == nil || code != 404 {
		return status, errors.New("target namespace already exists or cannot be checked")
	}
	if len(plan.pvcs) > 0 {
		if p.LonghornSnapshotClass == "" || p.RestoreStorageClass == "" {
			return status, errors.New("restore requires Longhorn snapshot and storage classes")
		}
		class, _, e := c.call(ctx, "GET", "/apis/snapshot.storage.k8s.io/v1/volumesnapshotclasses/"+p.LonghornSnapshotClass, nil)
		if e != nil {
			return status, e
		}
		if str(class["driver"]) != "driver.longhorn.io" || str(class["deletionPolicy"]) != "Retain" || str(obj(class["parameters"])["type"]) != "bak" {
			return status, errors.New("unsafe restore snapshot class")
		}
		sc, _, e := c.call(ctx, "GET", "/apis/storage.k8s.io/v1/storageclasses/"+p.RestoreStorageClass, nil)
		if e != nil {
			return status, e
		}
		if str(sc["provisioner"]) != "driver.longhorn.io" || (str(sc["volumeBindingMode"]) != "" && str(sc["volumeBindingMode"]) != "Immediate") {
			return status, errors.New("restore requires Longhorn storage with Immediate binding")
		}
	}
	cleanMetadata(plan.namespace, "", run)
	obj(plan.namespace["metadata"])["name"] = target.Namespace
	delete(plan.namespace, "spec")
	if e = emit("namespace-create", target.Namespace); e != nil {
		return status, e
	}
	ns, _, e := c.call(ctx, "POST", "/api/v1/namespaces", plan.namespace)
	if e != nil {
		return status, e
	}
	journal.NamespaceUID = str(obj(ns["metadata"])["uid"])
	if journal.NamespaceUID == "" {
		return status, errors.New("namespace UID not confirmed")
	}
	status.Objects++
	checkNamespace := func() error {
		ns, _, e := c.call(ctx, "GET", "/api/v1/namespaces/"+target.Namespace, nil)
		if e != nil {
			return e
		}
		m := obj(ns["metadata"])
		if str(m["uid"]) != journal.NamespaceUID || str(obj(m["labels"])["srk.gdata/restore-run"]) != run || m["deletionTimestamp"] != nil {
			return errors.New("restore namespace identity changed")
		}
		return nil
	}
	create := func(path string, o map[string]any) (map[string]any, error) {
		if e := checkNamespace(); e != nil {
			return nil, e
		}
		out, code, e := c.call(ctx, "POST", path, o)
		// Kubernetes creates the default ServiceAccount itself. Only inside our new,
		// UID-checked namespace may it be replaced with the saved default settings.
		if code == 409 && str(o["kind"]) == "ServiceAccount" && str(obj(o["metadata"])["name"]) == "default" {
			current, _, ge := c.call(ctx, "GET", path+"/default", nil)
			if ge != nil {
				return nil, ge
			}
			meta := obj(current["metadata"])
			if str(meta["uid"]) == "" || str(meta["resourceVersion"]) == "" {
				return nil, errors.New("default ServiceAccount identity missing")
			}
			obj(o["metadata"])["uid"] = meta["uid"]
			obj(o["metadata"])["resourceVersion"] = meta["resourceVersion"]
			out, _, e = c.call(ctx, "PUT", path+"/default", o)
		}
		if e != nil {
			return nil, e
		}
		if str(obj(out["metadata"])["uid"]) == "" {
			return nil, errors.New("created resource UID missing")
		}
		return out, nil
	}
	if e = emit("namespace-created", target.Namespace); e != nil {
		return status, e
	}
	for _, pvc := range plan.pvcs {
		name := restoreName(run, pvc.name)
		handle := plan.volumes[pvc.name].Handle
		snapPath := "/apis/snapshot.storage.k8s.io/v1/namespaces/" + target.Namespace + "/volumesnapshots"
		snap := map[string]any{"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot", "metadata": map[string]any{"name": name, "namespace": target.Namespace, "labels": map[string]any{"srk.gdata/restore-run": run}}, "spec": map[string]any{"volumeSnapshotClassName": p.LonghornSnapshotClass, "source": map[string]any{"volumeSnapshotContentName": name}}}
		if e = emit("longhorn-snapshot-create", pvc.name); e != nil {
			return status, e
		}
		created, e := create(snapPath, snap)
		if e != nil {
			return status, e
		}
		snapUID := str(obj(created["metadata"])["uid"])
		mode := str(obj(pvc.object["spec"])["volumeMode"])
		if mode == "" {
			mode = "Filesystem"
		}
		if mode != "Filesystem" && mode != "Block" {
			return status, errors.New("unsupported PVC volumeMode")
		}
		content := map[string]any{"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshotContent", "metadata": map[string]any{"name": name, "labels": map[string]any{"srk.gdata/restore-run": run}}, "spec": map[string]any{"volumeSnapshotClassName": p.LonghornSnapshotClass, "driver": "driver.longhorn.io", "deletionPolicy": "Retain", "sourceVolumeMode": mode, "source": map[string]any{"snapshotHandle": handle}, "volumeSnapshotRef": map[string]any{"name": name, "namespace": target.Namespace, "uid": snapUID}}}
		contentPath := "/apis/snapshot.storage.k8s.io/v1/volumesnapshotcontents"
		if e = emit("longhorn-content-create", pvc.name); e != nil {
			return status, e
		}
		created, e = create(contentPath, content)
		if e != nil {
			return status, e
		}
		contentUID := str(obj(created["metadata"])["uid"])
		if e = emit("longhorn-backup-wait", pvc.name); e != nil {
			return status, e
		}
		if e = waitRestore(ctx, func() (bool, error) {
			if e := checkNamespace(); e != nil {
				return false, e
			}
			s, _, e := c.call(ctx, "GET", snapPath+"/"+name, nil)
			if e != nil {
				return false, e
			}
			v, _, e := c.call(ctx, "GET", contentPath+"/"+name, nil)
			if e != nil {
				return false, e
			}
			ss, cs := obj(s["status"]), obj(v["status"])
			spec := obj(v["spec"])
			ref := obj(spec["volumeSnapshotRef"])
			if str(obj(s["metadata"])["uid"]) != snapUID || str(obj(v["metadata"])["uid"]) != contentUID || str(ref["uid"]) != snapUID || str(ref["namespace"]) != target.Namespace || str(ref["name"]) != name || str(spec["driver"]) != "driver.longhorn.io" || str(spec["deletionPolicy"]) != "Retain" || str(obj(spec["source"])["snapshotHandle"]) != handle {
				return false, errors.New("snapshot binding identity changed")
			}
			if ss["error"] != nil || cs["error"] != nil {
				return false, errors.New("Longhorn backup unavailable or CSI restoration failed")
			}
			return ss["readyToUse"] == true && cs["readyToUse"] == true && str(ss["boundVolumeSnapshotContentName"]) == name, nil
		}); e != nil {
			return status, e
		}
		spec := obj(pvc.object["spec"])
		spec["dataSource"] = map[string]any{"apiGroup": "snapshot.storage.k8s.io", "kind": "VolumeSnapshot", "name": name}
		path := "/api/v1/namespaces/" + target.Namespace + "/persistentvolumeclaims"
		if e = emit("pvc-create", pvc.name); e != nil {
			return status, e
		}
		created, e = create(path, pvc.object)
		if e != nil {
			return status, e
		}
		pvcUID := str(obj(created["metadata"])["uid"])
		status.Objects++
		if e = emit("pvc-wait", pvc.name); e != nil {
			return status, e
		}
		if e = waitRestore(ctx, func() (bool, error) {
			if e := checkNamespace(); e != nil {
				return false, e
			}
			v, _, e := c.call(ctx, "GET", path+"/"+pvc.name, nil)
			if e != nil {
				return false, e
			}
			if str(obj(v["metadata"])["uid"]) != pvcUID {
				return false, errors.New("PVC identity changed")
			}
			if str(obj(v["status"])["phase"]) == "Lost" {
				return false, errors.New("restored PVC is Lost")
			}
			if str(obj(v["status"])["phase"]) != "Bound" {
				return false, nil
			}
			vs := obj(v["spec"])
			if str(vs["storageClassName"]) != p.RestoreStorageClass || str(obj(vs["dataSource"])["name"]) != name {
				return false, errors.New("PVC restore source changed")
			}
			pvName := str(vs["volumeName"])
			if !nameRE.MatchString(pvName) {
				return false, errors.New("restored PV name missing")
			}
			pv, _, e := c.call(ctx, "GET", "/api/v1/persistentvolumes/"+pvName, nil)
			if e != nil {
				return false, e
			}
			ps := obj(pv["spec"])
			ref := obj(ps["claimRef"])
			native := obj(ps["csi"])
			u, _ := url.Parse(handle)
			if str(ref["uid"]) != pvcUID || str(ref["namespace"]) != target.Namespace || str(ref["name"]) != pvc.name || str(native["driver"]) != "driver.longhorn.io" || str(native["volumeHandle"]) == "" || str(native["volumeHandle"]) == u.Host {
				return false, errors.New("restored Longhorn PV binding is invalid")
			}
			return true, nil
		}); e != nil {
			return status, e
		}
		status.Volumes++
		if e = emit("pvc-bound", pvc.name); e != nil {
			return status, e
		}
	}
	waiters := []func() error{}
	for _, o := range plan.objects {
		if e = emit("resource-create", o.resource.kind+"/"+o.name); e != nil {
			return status, e
		}
		path := o.resource.base + "/namespaces/" + target.Namespace + "/" + o.resource.plural
		created, e := create(path, o.object)
		if e != nil {
			return status, e
		}
		status.Objects++
		if e = emit("resource-created", o.resource.kind+"/"+o.name); e != nil {
			return status, e
		}
		if target.Start && (o.resource.kind == "Deployment" || o.resource.kind == "StatefulSet" || o.resource.kind == "DaemonSet") {
			waiters = append(waiters, func() error {
				if e = emit("workload-wait", o.resource.kind+"/"+o.name); e != nil {
					return e
				}
				uid := str(obj(created["metadata"])["uid"])
				generation := number(obj(created["metadata"])["generation"], 1)
				if e = waitRestore(ctx, func() (bool, error) {
					if e := checkNamespace(); e != nil {
						return false, e
					}
					v, _, e := c.call(ctx, "GET", path+"/"+o.name, nil)
					if e != nil {
						return false, e
					}
					if str(obj(v["metadata"])["uid"]) != uid {
						return false, errors.New("workload identity changed")
					}
					st := obj(v["status"])
					if number(st["observedGeneration"], 0) < generation {
						return false, nil
					}
					want := number(obj(o.object["spec"])["replicas"], 1)
					switch o.resource.kind {
					case "Deployment":
						return number(st["updatedReplicas"], 0) >= want && number(st["availableReplicas"], 0) >= want, nil
					case "StatefulSet":
						return number(st["readyReplicas"], 0) >= want && number(st["updatedReplicas"], 0) >= want, nil
					default:
						want = number(st["desiredNumberScheduled"], 0)
						return want > 0 && number(st["numberReady"], 0) >= want && number(st["updatedNumberScheduled"], 0) >= want, nil
					}
				}); e != nil {
					return e
				}
				if e = emit("workload-ready", o.resource.kind+"/"+o.name); e != nil {
					return e
				}
				return nil
			})
		}
	}
	for _, wait := range waiters {
		if e = wait(); e != nil {
			return status, e
		}
	}
	phase := "prepared"
	if target.Start {
		phase = "complete"
	}
	if e = emit(phase, target.Namespace); e != nil {
		return status, e
	}
	return status, nil
}
func waitRestore(ctx context.Context, fn func() (bool, error)) error {
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		ok, e := fn()
		if e != nil {
			return e
		}
		if ok {
			return nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
