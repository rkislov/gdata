// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package kubernetes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

type Profile struct {
	AllowVerification      bool     `json:"allow_verification,omitempty"`
	AllowRestore           bool     `json:"allow_restore,omitempty"`
	RestoreNamespacePrefix string   `json:"restore_namespace_prefix,omitempty"`
	RestoreStorageClass    string   `json:"restore_storage_class,omitempty"`
	RestoreClusterRoles    []string `json:"restore_cluster_roles,omitempty"`
	AllowIngressRestore    bool     `json:"allow_ingress_restore,omitempty"`

	Endpoint              string   `json:"endpoint"`
	CAFile                string   `json:"ca_file"`
	TokenFile             string   `json:"token_file"`
	StagingDir            string   `json:"staging_dir"`
	Namespaces            []string `json:"namespaces"`
	IncludeSecrets        bool     `json:"include_secrets,omitempty"`
	LonghornSnapshotClass string   `json:"longhorn_snapshot_class,omitempty"`
	MaxBytes              int64    `json:"max_bytes"`
	TimeoutSeconds        int      `json:"timeout_seconds"`
}
type Volume struct {
	Namespace string `json:"namespace"`
	PVC       string `json:"pvc"`
	PV        string `json:"pv"`
	Snapshot  string `json:"snapshot"`
	Content   string `json:"content"`
	Handle    string `json:"handle"`
}
type Index struct {
	Version         int       `json:"version"`
	Namespaces      []string  `json:"namespaces"`
	Objects         int       `json:"objects"`
	IncludesSecrets bool      `json:"includes_secrets"`
	PVCData         string    `json:"pvc_data"`
	Volumes         []Volume  `json:"volumes"`
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at"`
}

func (p Profile) Validate() error {
	if p.AllowVerification && (!p.AllowRestore || len(p.RestoreNamespacePrefix) > 24) {
		return errors.New("verification requires allow_restore and prefix up to 24 characters")
	}
	if p.AllowRestore && (len(p.RestoreNamespacePrefix) < 3 || len(p.RestoreNamespacePrefix) > 50 || !regexp.MustCompile(`^[a-z][a-z0-9-]*-$`).MatchString(p.RestoreNamespacePrefix)) {
		return errors.New("restore requires a namespace prefix ending in -, length 3..50")
	}
	if p.RestoreStorageClass != "" && (!nameRE.MatchString(p.RestoreStorageClass) || len(p.RestoreStorageClass) > 253) {
		return errors.New("invalid restore storage class")
	}

	u, e := url.Parse(p.Endpoint)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("Kubernetes requires an HTTPS API origin")
	}
	if p.MaxBytes < 1 || p.MaxBytes > 1<<30 || p.TimeoutSeconds < 1 || p.TimeoutSeconds > 43200 || len(p.Namespaces) == 0 || len(p.Namespaces) > 100 {
		return errors.New("Kubernetes requires namespaces, max_bytes <= 1 GiB and timeout_seconds")
	}
	seen := map[string]bool{}
	for _, n := range p.Namespaces {
		if !nameRE.MatchString(n) || len(n) > 63 || seen[n] {
			return errors.New("invalid or duplicate namespace")
		}
		seen[n] = true
	}
	if p.LonghornSnapshotClass != "" && (!nameRE.MatchString(p.LonghornSnapshotClass) || len(p.LonghornSnapshotClass) > 253) {
		return errors.New("invalid snapshot class")
	}
	for _, f := range []string{p.TokenFile, p.CAFile} {
		st, e := os.Lstat(f)
		if e != nil || !filepath.IsAbs(f) || !st.Mode().IsRegular() {
			return errors.New("regular absolute Kubernetes credentials/CA file required")
		}
		if f == p.TokenFile && st.Mode().Perm()&0077 != 0 {
			return errors.New("Kubernetes token file must be private")
		}
	}
	st, e := os.Lstat(p.StagingDir)
	if e != nil || !filepath.IsAbs(p.StagingDir) || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return errors.New("private Kubernetes staging directory required")
	}
	return nil
}

type client struct {
	p     Profile
	http  *http.Client
	bytes int64
}

func newClient(p Profile) (*client, error) {
	if e := p.Validate(); e != nil {
		return nil, e
	}
	pem, e := os.ReadFile(p.CAFile)
	if e != nil {
		return nil, e
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("invalid Kubernetes CA")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return &client{p: p, http: &http.Client{Transport: tr, Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("Kubernetes redirects are forbidden") }}}, nil
}
func (c *client) call(ctx context.Context, method, path string, body any) (map[string]any, int, error) {
	var in io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return nil, 0, e
		}
		in = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.p.Endpoint, "/")+path, in)
	if e != nil {
		return nil, 0, e
	}
	token, e := os.ReadFile(c.p.TokenFile)
	if e != nil {
		return nil, 0, e
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, e := c.http.Do(req)
	if e != nil {
		return nil, 0, errors.New("Kubernetes API transport failed")
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 16<<20+1))
	if e != nil || len(raw) > 16<<20 {
		return nil, resp.StatusCode, errors.New("Kubernetes API response exceeds limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("Kubernetes API %s returned HTTP %d", method, resp.StatusCode)
	}
	var out map[string]any
	if e = json.Unmarshal(raw, &out); e != nil {
		return nil, resp.StatusCode, errors.New("invalid Kubernetes JSON response")
	}
	return out, resp.StatusCode, nil
}
func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func str(v any) string         { s, _ := v.(string); return s }
func (c *client) save(dir, path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	if int64(len(b)) > c.p.MaxBytes-c.bytes {
		return errors.New("Kubernetes staging quota exceeded")
	}
	c.bytes += int64(len(b))
	return os.WriteFile(filepath.Join(dir, path), b, 0600)
}

type resource struct{ base, plural, kind string }

var resources = []resource{{"/api/v1", "configmaps", "ConfigMap"}, {"/api/v1", "services", "Service"}, {"/api/v1", "serviceaccounts", "ServiceAccount"}, {"/api/v1", "persistentvolumeclaims", "PersistentVolumeClaim"}, {"/apis/apps/v1", "deployments", "Deployment"}, {"/apis/apps/v1", "statefulsets", "StatefulSet"}, {"/apis/apps/v1", "daemonsets", "DaemonSet"}, {"/apis/batch/v1", "cronjobs", "CronJob"}, {"/apis/networking.k8s.io/v1", "ingresses", "Ingress"}, {"/apis/networking.k8s.io/v1", "networkpolicies", "NetworkPolicy"}, {"/apis/rbac.authorization.k8s.io/v1", "roles", "Role"}, {"/apis/rbac.authorization.k8s.io/v1", "rolebindings", "RoleBinding"}}

// Backup captures namespace-scoped desired state. No pods are stopped or scaled.
// Longhorn data stays in its configured backupstore; the index records that dependency.
func Backup(ctx context.Context, p Profile, run, dir string, report func(string, int)) (Index, error) {
	idx := Index{Version: 1, Namespaces: p.Namespaces, IncludesSecrets: p.IncludeSecrets, PVCData: "not-included", StartedAt: time.Now().UTC(), Volumes: []Volume{}}
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(run) {
		return idx, errors.New("invalid backup run ID")
	}
	c, e := newClient(p)
	if e != nil {
		return idx, e
	}
	defer c.http.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	if report == nil {
		report = func(string, int) {}
	}
	var class map[string]any
	if p.LonghornSnapshotClass != "" {
		class, _, e = c.call(ctx, "GET", "/apis/snapshot.storage.k8s.io/v1/volumesnapshotclasses/"+p.LonghornSnapshotClass, nil)
		if e != nil {
			return idx, e
		}
		if str(class["driver"]) != "driver.longhorn.io" || str(class["deletionPolicy"]) != "Retain" || str(obj(class["parameters"])["type"]) != "bak" {
			return idx, errors.New("Longhorn class must use driver.longhorn.io, type=bak and deletionPolicy=Retain")
		}
		idx.PVCData = "longhorn-external-backupstore"
	}
	selected := append([]resource{}, resources...)
	if p.IncludeSecrets {
		selected = append(selected, resource{"/api/v1", "secrets", "Secret"})
	}
	var pvcs []map[string]any
	for _, ns := range p.Namespaces {
		namespace, _, e := c.call(ctx, "GET", "/api/v1/namespaces/"+ns, nil)
		if e != nil {
			return idx, e
		}
		if str(obj(namespace["metadata"])["name"]) != ns {
			return idx, errors.New("namespace identity mismatch")
		}
		if e = c.save(dir, fmt.Sprintf("object-%06d.json", idx.Objects), namespace); e != nil {
			return idx, e
		}
		idx.Objects++
		for _, r := range selected {
			next := ""
			tokens := map[string]bool{}
			for {
				path := r.base + "/namespaces/" + ns + "/" + r.plural + "?limit=200"
				if next != "" {
					path += "&continue=" + url.QueryEscape(next)
				}
				page, _, e := c.call(ctx, "GET", path, nil)
				if e != nil {
					return idx, e
				}
				items, ok := page["items"].([]any)
				if !ok {
					return idx, errors.New("invalid Kubernetes list")
				}
				for _, item := range items {
					o := obj(item)
					if o == nil {
						return idx, errors.New("invalid Kubernetes list item")
					}
					if str(o["kind"]) == "" {
						o["kind"] = r.kind
					}
					if str(o["apiVersion"]) == "" {
						o["apiVersion"] = strings.TrimPrefix(strings.TrimPrefix(r.base, "/apis/"), "/api/")
					}
					m := obj(o["metadata"])
					if str(m["namespace"]) != ns || str(m["name"]) == "" || str(o["kind"]) != r.kind {
						return idx, errors.New("Kubernetes object identity mismatch")
					}
					if idx.Objects >= 10000 {
						return idx, errors.New("Kubernetes object limit exceeded")
					}
					if r.plural == "secrets" && str(o["type"]) == "kubernetes.io/service-account-token" {
						continue
					}
					if r.plural == "persistentvolumeclaims" {
						pvcs = append(pvcs, o)
					}
					if e = c.save(dir, fmt.Sprintf("object-%06d.json", idx.Objects), o); e != nil {
						return idx, e
					}
					idx.Objects++
					report("kubernetes-objects", idx.Objects)
				}
				next = str(obj(page["metadata"])["continue"])
				if next == "" {
					break
				}
				if tokens[next] {
					return idx, errors.New("Kubernetes pagination loop")
				}
				tokens[next] = true
			}
		}
	}
	if class != nil {
		for _, pvc := range pvcs {
			report("longhorn-backup", len(idx.Volumes))
			v, e := c.backupVolume(ctx, run, pvc, p.LonghornSnapshotClass)
			if e != nil {
				return idx, e
			}
			idx.Volumes = append(idx.Volumes, v)
			report("longhorn-backup-ready", len(idx.Volumes))
		}
	}
	idx.FinishedAt = time.Now().UTC()
	e = c.save(dir, "kubernetes-index.json", idx)
	return idx, e
}
func (c *client) backupVolume(ctx context.Context, run string, pvc map[string]any, class string) (Volume, error) {
	meta, spec := obj(pvc["metadata"]), obj(pvc["spec"])
	v := Volume{Namespace: str(meta["namespace"]), PVC: str(meta["name"]), PV: str(spec["volumeName"])}
	if !nameRE.MatchString(v.PV) || str(obj(pvc["status"])["phase"]) != "Bound" {
		return v, errors.New("Longhorn backup requires a Bound PVC")
	}
	pv, _, e := c.call(ctx, "GET", "/api/v1/persistentvolumes/"+v.PV, nil)
	if e != nil {
		return v, e
	}
	pvSpec := obj(pv["spec"])
	ref := obj(pvSpec["claimRef"])
	if str(obj(pvSpec["csi"])["driver"]) != "driver.longhorn.io" || str(ref["uid"]) == "" || str(ref["uid"]) != str(meta["uid"]) {
		return v, errors.New("PVC is not bound to the expected Longhorn PV")
	}
	sum := sha256.Sum256([]byte(v.Namespace + "/" + v.PVC))
	v.Snapshot = "srk-" + run + "-" + hex.EncodeToString(sum[:6])
	path := "/apis/snapshot.storage.k8s.io/v1/namespaces/" + v.Namespace + "/volumesnapshots"
	body := map[string]any{"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot", "metadata": map[string]any{"name": v.Snapshot, "namespace": v.Namespace, "labels": map[string]string{"srk.gdata/run": run}}, "spec": map[string]any{"volumeSnapshotClassName": class, "source": map[string]string{"persistentVolumeClaimName": v.PVC}}}
	snap, code, e := c.call(ctx, "POST", path, body)
	if e != nil && code != 409 {
		return v, e
	}
	for {
		snap, _, e = c.call(ctx, "GET", path+"/"+v.Snapshot, nil)
		if e != nil {
			return v, e
		}
		sm, ss := obj(snap["metadata"]), obj(snap["spec"])
		if str(obj(sm["labels"])["srk.gdata/run"]) != run || str(ss["volumeSnapshotClassName"]) != class || str(obj(ss["source"])["persistentVolumeClaimName"]) != v.PVC {
			return v, errors.New("snapshot ownership conflict")
		}
		status := obj(snap["status"])
		if status["error"] != nil {
			return v, errors.New("Longhorn snapshot reports an error")
		}
		if status["readyToUse"] == true {
			v.Content = str(status["boundVolumeSnapshotContentName"])
			if !nameRE.MatchString(v.Content) {
				return v, errors.New("missing snapshot content")
			}
			content, _, e := c.call(ctx, "GET", "/apis/snapshot.storage.k8s.io/v1/volumesnapshotcontents/"+v.Content, nil)
			if e != nil {
				return v, e
			}
			cs, ct := obj(content["spec"]), obj(content["status"])
			cr := obj(cs["volumeSnapshotRef"])
			v.Handle = str(ct["snapshotHandle"])
			handle, he := url.Parse(v.Handle)
			if he != nil || handle.Scheme != "bak" || handle.Host == "" || len(strings.TrimPrefix(handle.Path, "/")) == 0 || handle.RawQuery != "" || handle.Fragment != "" || str(obj(cs["source"])["volumeHandle"]) != str(obj(pvSpec["csi"])["volumeHandle"]) {
				return v, errors.New("Longhorn backup handle mismatch")
			}
			if str(cs["driver"]) != "driver.longhorn.io" || str(cs["deletionPolicy"]) != "Retain" || ct["readyToUse"] != true || ct["error"] != nil || str(cr["uid"]) == "" || str(cr["uid"]) != str(sm["uid"]) || str(cr["namespace"]) != v.Namespace || str(cr["name"]) != v.Snapshot || !strings.HasPrefix(v.Handle, "bak://") {
				return v, errors.New("Longhorn external backup not confirmed")
			}
			return v, nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return v, ctx.Err()
		case <-timer.C:
		}
	}
}
