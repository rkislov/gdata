// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CheckRestore uses the same verified restore path, with a separate result and
// cleanup receipt. A successful API restore alone is not an application check.
func CheckRestore(ctx context.Context, p Profile, t RestoreTarget, run, dir string, policy model.KubernetesCheckPolicy, snapshot string, report func(model.KubernetesRestore, *model.KubernetesCheckResult)) (status model.KubernetesRestore, result model.KubernetesCheckResult, err error) {
	result = model.KubernetesCheckResult{PolicyID: policy.ID, SnapshotID: snapshot, Namespace: t.Namespace, StartedAt: time.Now().UTC(), State: "restoring", Cleanup: "pending", Checks: []model.KubernetesProbeResult{}}
	lastPhase := ""
	emit := func() {
		phase := "check-" + result.State + "-" + result.Cleanup
		if result.State != "restoring" && phase != lastPhase {
			status.Events = append(status.Events, model.KubernetesRestoreEvent{At: time.Now().UTC().Format(time.RFC3339Nano), Phase: phase, Resource: t.Namespace})
			if len(status.Events) > 200 {
				status.Events = status.Events[len(status.Events)-200:]
			}
			lastPhase = phase
		}

		if report != nil {
			copy := result
			copy.Checks = append([]model.KubernetesProbeResult{}, result.Checks...)
			report(status, &copy)
		}
	}
	if e := policy.Validate(); e != nil {
		return status, result, e
	}
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(run) || !p.AllowVerification || !p.AllowRestore || policy.NamespacePrefix != p.RestoreNamespacePrefix+"check-" || t.Namespace != policy.NamespacePrefix+run || t.Source != policy.SourceNamespace || t.Profile != policy.Profile {
		return status, result, errors.New("verification not allowed by target profile or namespace prefix")
	}
	if e := p.Validate(); e != nil {
		return status, result, e
	}
	if e := os.MkdirAll(filepath.Join(p.StagingDir, "restore-journals"), 0700); e != nil {
		return status, result, e
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(policy.TimeoutSeconds)*time.Second)
	defer cancel()
	defer func() {
		result.State = "passed"
		if err != nil {
			result.State = "failed"
			result.Error = err.Error()
		}
		if err != nil && policy.KeepOnFailure {
			result.Cleanup = "retained"
			emit()
		} else {
			result.Cleanup = "running"
			emit()
			cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 180*time.Second)
			ce := cleanupCheck(cleanupCtx, p, t, run)
			stop()
			if ce != nil {
				result.Cleanup = "failed"
				result.State = "failed"
				err = errors.Join(err, fmt.Errorf("cleanup: %w", ce))
				result.Error = err.Error()
			} else {
				result.Cleanup = "complete"
			}
		}
		now := time.Now().UTC()
		result.FinishedAt = &now
		result.DurationSeconds = now.Sub(result.StartedAt).Seconds()
		b, _ := json.Marshal(result)
		if e := repository.Atomic(filepath.Join(p.StagingDir, "restore-journals", run+".check.json"), b, 0600); e != nil {
			err = errors.Join(err, e)
			result.State = "failed"
			result.Error = err.Error()
		}
		emit()
	}()
	t.Start = true
	t.Verification = true
	// Automatic cleanup relies on the CSI provisioner reclaiming only new PVs.
	c, e := newClient(p)
	if e != nil {
		return status, result, e
	}
	defer c.http.CloseIdleConnections()
	if p.RestoreStorageClass != "" {
		sc, _, e := c.call(checkCtx, "GET", "/apis/storage.k8s.io/v1/storageclasses/"+p.RestoreStorageClass, nil)
		if e != nil {
			return status, result, e
		}
		if str(sc["reclaimPolicy"]) != "Delete" {
			return status, result, errors.New("verification requires Delete reclaimPolicy on the dedicated restore StorageClass")
		}
	}
	status, err = Restore(checkCtx, p, t, run, dir, func(st model.KubernetesRestore) { status = st; emit() })
	if err != nil {
		return
	}
	result.State = "checking"
	emit()
	journal, e := readRestoreJournal(p, run)
	if e != nil {
		return status, result, e
	}
	checkIdentity := func() error { return namespaceIdentity(checkCtx, c, t.Namespace, run, journal.NamespaceUID) }
	err = waitRestore(checkCtx, func() (bool, error) {
		if e := checkIdentity(); e != nil {
			return false, e
		}
		list, _, e := c.call(checkCtx, "GET", "/api/v1/namespaces/"+t.Namespace+"/pods?limit=1000", nil)
		if e != nil {
			return false, e
		}
		if str(obj(list["metadata"])["continue"]) != "" {
			return false, errors.New("pod count exceeds verification limit")
		}
		items, _ := list["items"].([]any)
		ready := 0
		pending := 0
		for _, item := range items {
			pod := obj(item)
			phase := str(obj(pod["status"])["phase"])
			if phase == "Succeeded" {
				continue
			}
			if phase == "Failed" {
				return false, errors.New("application pod failed")
			}
			ok := false
			conditions, _ := obj(pod["status"])["conditions"].([]any)
			for _, cond := range conditions {
				if str(obj(cond)["type"]) == "Ready" && str(obj(cond)["status"]) == "True" {
					ok = true
				}
			}
			if phase == "Running" && ok && obj(pod["metadata"])["deletionTimestamp"] == nil {
				ready++
			} else {
				pending++
			}
		}
		result.ReadyPods = ready
		return ready > 0 && pending == 0, nil
	})
	result.Checks = append(result.Checks, model.KubernetesProbeResult{Name: "Pod readiness", Passed: err == nil, Detail: fmt.Sprintf("%d ready pods", result.ReadyPods)})
	emit()
	if err != nil {
		return
	}
	for _, h := range policy.HTTP {
		err = waitRestore(checkCtx, func() (bool, error) {
			if e := checkIdentity(); e != nil {
				return false, e
			}
			service, _, e := c.call(checkCtx, "GET", "/api/v1/namespaces/"+t.Namespace+"/services/"+h.Service, nil)
			if e != nil {
				return false, e
			}
			spec := obj(service["spec"])
			if str(spec["type"]) == "ExternalName" || len(obj(spec["selector"])) == 0 || str(obj(service["metadata"])["namespace"]) != t.Namespace {
				return false, errors.New("HTTP checks require a selector-backed Service in the test namespace")
			}
			portFound := false
			ports, _ := spec["ports"].([]any)
			for _, port := range ports {
				if number(obj(port)["port"], 0) == h.Port {
					portFound = true
				}
			}
			if !portFound {
				return false, errors.New("HTTP check port not exposed by Service")
			}
			path := "/api/v1/namespaces/" + t.Namespace + "/services/" + url.PathEscape(fmt.Sprintf("http:%s:%d", h.Service, h.Port)) + "/proxy"
			for _, segment := range strings.Split(h.Path, "/") {
				if segment != "" {
					path += "/" + url.PathEscape(segment)
				}
			}
			if strings.HasSuffix(h.Path, "/") {
				path += "/"
			}
			req, e := http.NewRequestWithContext(checkCtx, "GET", strings.TrimRight(p.Endpoint, "/")+path, nil)
			if e != nil {
				return false, e
			}
			token, e := os.ReadFile(p.TokenFile)
			if e != nil {
				return false, e
			}
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
			response, e := c.http.Do(req)
			if e != nil {
				if checkCtx.Err() != nil {
					return false, checkCtx.Err()
				}
				return false, nil
			}
			body, re := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
			response.Body.Close()
			if re != nil || len(body) > 1<<20 {
				return false, errors.New("HTTP probe response invalid or too large")
			}
			return response.StatusCode == h.ExpectedStatus && strings.Contains(string(body), h.Contains), nil
		})
		detail := "expected HTTP status and body matched"
		if err != nil {
			detail = "HTTP check did not pass before timeout or API error"
		}
		result.Checks = append(result.Checks, model.KubernetesProbeResult{Name: fmt.Sprintf("HTTP %s:%d%s", h.Service, h.Port, h.Path), Passed: err == nil, Detail: detail})
		emit()
		if err != nil {
			return
		}
	}
	return
}
func readRestoreJournal(p Profile, run string) (restoreJournal, error) {
	var j restoreJournal
	b, e := os.ReadFile(filepath.Join(p.StagingDir, "restore-journals", run+".json"))
	if e != nil {
		return j, e
	}
	e = json.Unmarshal(b, &j)
	return j, e
}
func namespaceIdentity(ctx context.Context, c *client, ns, run, uid string) error {
	o, _, e := c.call(ctx, "GET", "/api/v1/namespaces/"+ns, nil)
	if e != nil {
		return e
	}
	m := obj(o["metadata"])
	if uid == "" || str(m["uid"]) != uid || str(obj(m["labels"])["srk.gdata/restore-run"]) != run {
		return errors.New("namespace ownership changed; refusing operation")
	}
	return nil
}
func cleanupCheck(ctx context.Context, p Profile, t RestoreTarget, run string) error {
	j, e := readRestoreJournal(p, run)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if j.NamespaceUID == "" {
		c, e := newClient(p)
		if e != nil {
			return e
		}
		defer c.http.CloseIdleConnections()
		_, code, e := c.call(ctx, "GET", "/api/v1/namespaces/"+t.Namespace, nil)
		if code == 404 {
			return nil
		}
		if e != nil {
			return e
		}
		return errors.New("namespace creation outcome uncertain; manual inspection required")
	}
	if j.Target.Namespace != t.Namespace || !strings.HasPrefix(t.Namespace, p.RestoreNamespacePrefix+"check-") {
		return errors.New("cleanup journal target mismatch")
	}
	c, e := newClient(p)
	if e != nil {
		return e
	}
	defer c.http.CloseIdleConnections()
	ns, code, e := c.call(ctx, "GET", "/api/v1/namespaces/"+t.Namespace, nil)
	if e != nil && code != 404 {
		return e
	}
	var pvs []string
	if code != 404 {
		if e = namespaceIdentity(ctx, c, t.Namespace, run, j.NamespaceUID); e != nil {
			return e
		}
		pvcs, _, e := c.call(ctx, "GET", "/api/v1/namespaces/"+t.Namespace+"/persistentvolumeclaims?limit=1000", nil)
		if e != nil {
			return e
		}
		if str(obj(pvcs["metadata"])["continue"]) != "" {
			return errors.New("cleanup PVC limit exceeded")
		}
		items, _ := pvcs["items"].([]any)
		for _, item := range items {
			pvc := obj(item)
			meta := obj(pvc["metadata"])
			if str(obj(meta["labels"])["srk.gdata/restore-run"]) != run {
				return errors.New("foreign PVC in verification namespace")
			}
			pvName := str(obj(pvc["spec"])["volumeName"])
			if pvName == "" {
				continue
			}
			pv, _, e := c.call(ctx, "GET", "/api/v1/persistentvolumes/"+pvName, nil)
			if e != nil {
				return e
			}
			spec := obj(pv["spec"])
			ref := obj(spec["claimRef"])
			if str(ref["uid"]) != str(meta["uid"]) || str(ref["namespace"]) != t.Namespace || str(spec["persistentVolumeReclaimPolicy"]) != "Delete" || str(obj(spec["csi"])["driver"]) != "driver.longhorn.io" {
				return errors.New("PVC/PV ownership or reclaim policy unsafe")
			}
			pvs = append(pvs, pvName)
		}
		// Persist PVs before namespace deletion so interrupted cleanup can be diagnosed.
		b, _ := json.Marshal(pvs)
		if e = repository.Atomic(filepath.Join(p.StagingDir, "restore-journals", run+".cleanup-pvs.json"), b, 0600); e != nil {
			return e
		}
		meta := obj(ns["metadata"])
		_, _, e = c.call(ctx, "DELETE", "/api/v1/namespaces/"+t.Namespace, map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]any{"uid": j.NamespaceUID, "resourceVersion": meta["resourceVersion"]}, "propagationPolicy": "Foreground"})
		if e != nil {
			return e
		}
	} else {
		b, re := os.ReadFile(filepath.Join(p.StagingDir, "restore-journals", run+".cleanup-pvs.json"))
		if re == nil {
			if e = json.Unmarshal(b, &pvs); e != nil {
				return e
			}
		} else if !os.IsNotExist(re) {
			return re
		}
	}
	if e = waitRestore(ctx, func() (bool, error) {
		_, code, e := c.call(ctx, "GET", "/api/v1/namespaces/"+t.Namespace, nil)
		if code == 404 {
			return true, nil
		}
		return false, e
	}); e != nil {
		return e
	}
	for _, name := range pvs {
		if e = waitRestore(ctx, func() (bool, error) {
			_, code, e := c.call(ctx, "GET", "/api/v1/persistentvolumes/"+name, nil)
			if code == 404 {
				return true, nil
			}
			return false, e
		}); e != nil {
			return e
		}
	}
	// CSI snapshots use Retain. Delete metadata only after PVCs and namespace are gone.
	base := "/apis/snapshot.storage.k8s.io/v1/volumesnapshotcontents"
	list, _, e := c.call(ctx, "GET", base+"?limit=1000&labelSelector="+url.QueryEscape("srk.gdata/restore-run="+run), nil)
	if e != nil {
		return e
	}
	if str(obj(list["metadata"])["continue"]) != "" {
		return errors.New("cleanup snapshot content limit exceeded")
	}
	items, _ := list["items"].([]any)
	for _, item := range items {
		o := obj(item)
		meta := obj(o["metadata"])
		spec := obj(o["spec"])
		ref := obj(spec["volumeSnapshotRef"])
		if str(obj(meta["labels"])["srk.gdata/restore-run"]) != run || str(ref["namespace"]) != t.Namespace || str(spec["deletionPolicy"]) != "Retain" || str(spec["driver"]) != "driver.longhorn.io" || !strings.HasPrefix(str(meta["name"]), "srk-r-"+run+"-") || str(meta["uid"]) == "" {
			return errors.New("snapshot content ownership/policy mismatch")
		}
		path := base + "/" + str(meta["name"])
		if _, _, e = c.call(ctx, "DELETE", path, map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]any{"uid": meta["uid"], "resourceVersion": meta["resourceVersion"]}}); e != nil {
			return e
		}
		if e = waitRestore(ctx, func() (bool, error) {
			_, code, e := c.call(ctx, "GET", path, nil)
			if code == 404 {
				return true, nil
			}
			return false, e
		}); e != nil {
			return e
		}
	}
	return nil
}
