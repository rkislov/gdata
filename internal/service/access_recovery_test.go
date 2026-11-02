// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/testcloud"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func accessCall(t *testing.T, cl *http.Client, method, url string, body any, status int) []byte {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, url, bytes.NewReader(b))
	res, e := cl.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	if res.StatusCode != status {
		t.Fatalf("%s %s: %d expected %d: %s", method, url, res.StatusCode, status, out)
	}
	return out
}
func personalClient(t *testing.T, c config.Config, name, key, cert string) *http.Client {
	t.Helper()
	c.Role = "user"
	c.ID = name
	c.Key = key
	c.Cert = cert
	cl, e := identity.Client(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(cl.CloseIdleConnections)
	return cl
}
func TestManagedCertificatesRotationRevocation(t *testing.T) {
	cfg := configs(t)
	mc := cfg["manager"]
	mc.PersonalCAKeyFile = filepath.Join(filepath.Dir(mc.CA), "ca.key")
	m := managerTest(t, mc)
	server := tlsServer(t, mc, m.Handler())
	admin, e := identity.Client(cfg["admin"])
	if e != nil {
		t.Fatal(e)
	}
	defer admin.CloseIdleConnections()
	user := model.User{Username: "rotating", Role: "operator", AllJobs: true, AllSites: true}
	if _, e = m.DB.PutUser(context.Background(), user, []byte("unused-test-password-hash")); e != nil {
		t.Fatal(e)
	}
	dir := filepath.Dir(mc.CA)
	if e = identity.Issue(dir, mc.Installation, "user", user.Username); e != nil {
		t.Fatal(e)
	}
	legacy := personalClient(t, cfg["admin"], user.Username, filepath.Join(dir, user.Username+".key"), filepath.Join(dir, user.Username+".crt"))
	accessCall(t, legacy, "GET", server.URL+"/v1/me", nil, 200)
	endpoint := server.URL + "/v1/users/rotating/certificates"
	issue := func(version, overlap int) (*http.Client, model.PersonalCertificate, string) {
		t.Helper()
		d := t.TempDir()
		key := filepath.Join(d, "user.key")
		csr := filepath.Join(d, "user.csr")
		crt := filepath.Join(d, "user.crt")
		if e := identity.CreatePersonalCSR(key, csr); e != nil {
			t.Fatal(e)
		}
		b, _ := os.ReadFile(csr)
		raw := accessCall(t, admin, "POST", endpoint, map[string]any{"csr": string(b), "days": 30, "version": version, "overlap_hours": overlap}, 201)
		var response struct {
			Certificate string                    `json:"certificate"`
			Record      model.PersonalCertificate `json:"record"`
		}
		if e := json.Unmarshal(raw, &response); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(crt, []byte(response.Certificate), 0600); e != nil {
			t.Fatal(e)
		}
		block, _ := pem.Decode([]byte(response.Certificate))
		leaf, e := x509.ParseCertificate(block.Bytes)
		if e != nil {
			t.Fatal(e)
		}
		peer, e := identity.Parse(leaf)
		if e != nil || peer.Role != "user" || peer.ID != "rotating" || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
			t.Fatal("incorrect certificate identity or usage")
		}
		return personalClient(t, cfg["admin"], "rotating", key, crt), response.Record, string(b)
	}
	first, c1, csr1 := issue(0, 1)
	accessCall(t, legacy, "GET", server.URL+"/v1/me", nil, 401)
	accessCall(t, first, "GET", server.URL+"/v1/me", nil, 200)
	accessCall(t, first, "POST", endpoint, map[string]any{"csr": csr1, "days": 30, "version": 1}, 403)
	accessCall(t, admin, "POST", endpoint, map[string]any{"csr": csr1, "days": 30, "version": 1}, 409) // fresh key mandatory
	accessCall(t, admin, "POST", endpoint, map[string]any{"csr": "invalid", "days": 30, "version": 1}, 400)
	second, c2, _ := issue(1, 1)
	accessCall(t, first, "GET", server.URL+"/v1/me", nil, 200)
	accessCall(t, second, "GET", server.URL+"/v1/me", nil, 200)
	accessCall(t, admin, "POST", endpoint+"/revoke", map[string]any{"serial": c1.Serial, "version": 1}, 409)
	accessCall(t, admin, "POST", endpoint+"/revoke", map[string]any{"serial": c1.Serial, "version": 2}, 200)
	// The client reuses its TLS transport: revocation is enforced per request.
	accessCall(t, first, "GET", server.URL+"/v1/me", nil, 401)
	accessCall(t, second, "GET", server.URL+"/v1/me", nil, 200)
	var state model.CertificateState
	if e = json.Unmarshal(accessCall(t, admin, "GET", endpoint, nil, 200), &state); e != nil {
		t.Fatal(e)
	}
	if state.Version != 3 || !state.Managed || len(state.Certificates) != 2 {
		t.Fatal(state)
	}
	for _, c := range state.Certificates {
		if c.Serial == c2.Serial && c.Certificate == "" {
			t.Fatal("issued certificate cannot be recovered after lost response")
		}
	}
	third, _, _ := issue(3, 0)
	accessCall(t, second, "GET", server.URL+"/v1/me", nil, 401)
	accessCall(t, third, "GET", server.URL+"/v1/me", nil, 200)
	u, _, e := m.DB.User(context.Background(), user.Username)
	if e != nil {
		t.Fatal(e)
	}
	u.Disabled = true
	if _, e = m.DB.PutUser(context.Background(), u, nil); e != nil {
		t.Fatal(e)
	}
	accessCall(t, third, "GET", server.URL+"/v1/me", nil, 401)
}
func TestScopedRestoreTargetEndToEnd(t *testing.T) {
	env := newVMEnvironment(t)
	backup := env.backup(t, testcloud.ImageVM)
	if backup.Status != "success" {
		t.Fatal(backup.Error)
	}
	ctx := context.Background()
	settings := model.ProtectionSettings{Sites: []model.ProtectionSite{{ID: "source", Name: "Source", DisasterDomain: "source"}, {ID: "recovery", Name: "Recovery", DisasterDomain: "recovery"}, {ID: "forbidden", Name: "Forbidden", DisasterDomain: "forbidden"}}, Policies: []model.ProtectionPolicy{{JobID: backup.JobID, SourceSiteID: "source", SourceMediumType: "disk", SourceFailureDomain: "source", RPOSeconds: 3600, RestoreMaxAgeSeconds: 86400}}, RestoreTargets: []model.RestoreTarget{{ID: "recovery", Name: "Approved", SiteID: "recovery", NodeID: env.proxy.Config.ID, Kind: "openstack", Profile: "lab", VM: &model.VMRestoreRequest{FlavorID: "small", Networks: []string{testcloud.Network}}}}}
	if _, e := env.manager.DB.PutProtectionSettings(ctx, settings); e != nil {
		t.Fatal(e)
	}
	user := model.User{Username: "restore-user", Role: "operator", JobIDs: []string{backup.JobID}, SiteIDs: []string{"source"}, RestoreSiteIDs: []string{"recovery"}, Permissions: []string{"restore"}}
	user, e := env.manager.DB.PutUser(ctx, user, []byte("unused-test-password-hash"))
	if e != nil {
		t.Fatal(e)
	}
	c := env.proxy.Config
	dir := filepath.Dir(c.CA)
	if e = identity.Issue(dir, c.Installation, "user", user.Username); e != nil {
		t.Fatal(e)
	}
	cl := personalClient(t, c, user.Username, filepath.Join(dir, user.Username+".key"), filepath.Join(dir, user.Username+".crt"))
	endpoint := env.url + "/v1/runs/" + backup.ID + "/restore"
	accessCall(t, cl, "POST", endpoint, map[string]any{"vm_restore": model.VMRestoreRequest{Name: "missing-binding"}}, 403)
	accessCall(t, cl, "POST", endpoint, map[string]any{"restore_target_id": "unknown", "vm_restore": model.VMRestoreRequest{Name: "unknown"}}, 403)
	var targets []model.RestoreTarget
	if e = json.Unmarshal(accessCall(t, cl, "GET", env.url+"/v1/restore-targets", nil, 200), &targets); e != nil || len(targets) != 1 {
		t.Fatal(targets, e)
	}
	// Submitted placement/profile/proxy cannot override the fixed target template.
	raw := accessCall(t, cl, "POST", endpoint, map[string]any{"restore_target_id": "recovery", "vm_restore": model.VMRestoreRequest{Name: "scoped-restore", ProxyID: "foreign", Profile: "foreign", FlavorID: "foreign", Networks: []string{"foreign"}}}, 202)
	var run model.Run
	if e = json.Unmarshal(raw, &run); e != nil {
		t.Fatal(e)
	}
	if run.RestoreSiteID != "recovery" || run.Job.AgentID != env.proxy.Config.ID || run.VMRestore.Profile != "lab" || run.VMRestore.FlavorID != "small" || run.VMRestore.Networks[0] != testcloud.Network {
		t.Fatal("target binding bypass", run)
	}
	if done := env.wait(t, run.ID); done.Status != "success" {
		t.Fatal(done.Error)
	}
	user.RestoreSiteIDs = []string{"forbidden"}
	user, e = env.manager.DB.PutUser(ctx, user, nil)
	if e != nil {
		t.Fatal(e)
	}
	accessCall(t, cl, "POST", endpoint, map[string]any{"restore_target_id": "recovery", "vm_restore": model.VMRestoreRequest{Name: "forbidden"}}, 403)
	accessCall(t, cl, "GET", env.url+"/v1/users/restore-user/certificates", nil, 403)
	user.AllRestoreSites = true
	user.SiteIDs = []string{"forbidden"}
	if _, e = env.manager.DB.PutUser(ctx, user, nil); e != nil {
		t.Fatal(e)
	}
	accessCall(t, cl, "POST", endpoint, map[string]any{"restore_target_id": "recovery", "vm_restore": model.VMRestoreRequest{Name: "foreign-source"}}, 403)
}
func TestRestoreProfileAndFilesystemBindings(t *testing.T) {
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	ctx := context.Background()
	if e := m.DB.PutNode(ctx, model.Node{ID: "target-agent", Role: "agent", LastSeen: time.Now()}); e != nil {
		t.Fatal(e)
	}
	p := model.ProtectionSettings{Sites: []model.ProtectionSite{{ID: "target", Name: "Target", DisasterDomain: "target"}}}
	for _, kind := range []string{"files", "postgresql", "s3", "mysql", "kubernetes", "longhorn"} {
		profile := "lab"
		if kind == "files" || kind == "postgresql" {
			profile = ""
		}
		p.RestoreTargets = append(p.RestoreTargets, model.RestoreTarget{ID: kind, SiteID: "target", NodeID: "target-agent", Kind: kind, Profile: profile})
	}
	if _, e := m.DB.PutProtectionSettings(ctx, p); e != nil {
		t.Fatal(e)
	}
	r := withPrincipal(httptest.NewRequest("POST", "/", nil), model.User{Role: "operator", RestoreSiteIDs: []string{"target"}})
	for _, tc := range []struct {
		kind, dest string
		allowed    bool
	}{{"files", "/safe/new", true}, {"files", "/safe/../other", false}, {"postgresql", "/pg/new", true}, {"s3", "s3://lab/new", true}, {"s3", "s3://other/new", false}, {"mysql", "mysql://lab/newdb", true}, {"mysql", "mysql://lab@other/newdb", false}, {"kubernetes", "kubernetes://lab/old/new?start=false", true}, {"longhorn", "kubernetes://other/old/new", false}} {
		_, _, e := m.resolveRestoreTarget(r, tc.kind, model.Run{Job: model.Job{Type: tc.kind}}, tc.dest, nil)
		if (e == nil) != tc.allowed {
			t.Fatalf("%s %s: %v", tc.kind, tc.dest, e)
		}
	}
}
