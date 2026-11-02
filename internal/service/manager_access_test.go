// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPersonalAccessScopesAndRevocation(t *testing.T) {
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	srv := tlsServer(t, cfg["manager"], m.Handler())
	ctx := context.Background()
	admin, e := identity.Client(cfg["admin"])
	if e != nil {
		t.Fatal(e)
	}
	defer admin.CloseIdleConnections()
	call := func(cl *http.Client, method, path string, body any, want int) []byte {
		t.Helper()
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(b))
		res, e := cl.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, res.StatusCode, want, out)
		}
		return out
	}
	user := model.User{Username: "alice", Name: "Alice", Role: "operator", AllSites: true, JobIDs: []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, Permissions: []string{"run", "verify", "restore", "erase", "cleanup"}}
	var saved model.User
	create := map[string]any{"username": user.Username, "name": user.Name, "role": user.Role, "all_sites": true, "job_ids": user.JobIDs, "permissions": user.Permissions, "password": "personal-test-password-only"}
	raw := call(admin, "PUT", "/v1/users", create, 200)
	if bytes.Contains(raw, []byte("password")) {
		t.Fatal("password leaked")
	}
	if e = json.Unmarshal(raw, &saved); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "hidden"} {
		j := model.Job{ID: id, Name: id}
		if e = m.DB.PutJob(ctx, j); e != nil {
			t.Fatal(e)
		}
		for n := 0; n < 2; n++ {
			_, e = m.DB.Enqueue(ctx, model.Run{ID: id + string(rune('a'+n)), JobID: id, Job: j, Kind: "backup", Status: "success", CreatedAt: time.Now()}, model.ID())
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	personal := cfg["admin"]
	personal.Role = "user"
	personal.ID = "alice"
	dir := filepath.Dir(personal.Cert)
	if e = identity.Issue(dir, "test", "user", "alice"); e != nil {
		t.Fatal(e)
	}
	personal.Cert = filepath.Join(dir, "alice.crt")
	personal.Key = filepath.Join(dir, "alice.key")
	cl, e := identity.Client(personal)
	if e != nil {
		t.Fatal(e)
	}
	defer cl.CloseIdleConnections()
	for _, path := range []string{"/v1/jobs", "/v1/runs?limit=1", "/v1/reports/operations.csv"} {
		body := call(cl, "GET", path, nil, 200)
		if bytes.Contains(body, []byte("hidden")) || !bytes.Contains(body, []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")) {
			t.Fatalf("scope leak or missing permitted data: %s", body)
		}
	}
	call(cl, "GET", "/v1/runs/hiddena", nil, 403)
	call(cl, "GET", "/v1/runs/hiddena/events", nil, 403)
	call(cl, "POST", "/v1/jobs/hidden/run", nil, 403)
	call(cl, "POST", "/v1/runs/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/restore", map[string]string{"destination": "/tmp"}, 403)
	call(cl, "POST", "/v1/media/test/gc", nil, 403)
	call(cl, "GET", "/v1/users", nil, 403)
	call(cl, "GET", "/v1/media/test/snapshots", nil, 403)
	call(cl, "PUT", "/v1/protection/settings", nil, 403)
	// Site AND job scope; missing mapping is denied, never interpreted as global.
	saved.AllSites = false
	saved.SiteIDs = []string{"east"}
	saved, e = m.DB.PutUser(ctx, saved, nil)
	if e != nil {
		t.Fatal(e)
	}
	if b := call(cl, "GET", "/v1/jobs", nil, 200); strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("unmapped site should be inaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa: %s", b)
	}
	_, e = m.DB.PutProtectionSettings(ctx, model.ProtectionSettings{Sites: []model.ProtectionSite{{ID: "east", Name: "East", DisasterDomain: "east"}}, Policies: []model.ProtectionPolicy{{JobID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SourceSiteID: "east", SourceMediumType: "disk", SourceFailureDomain: "source", RPOSeconds: 86400, RestoreMaxAgeSeconds: 86400}}})
	if e != nil {
		t.Fatal(e)
	}
	call(cl, "GET", "/v1/runs/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil, 200)
	// Global operator permissions remain opt-in; an auditor cannot inherit write grants.
	saved.AllJobs = true
	saved.AllSites = true
	saved.Permissions = []string{}
	saved, e = m.DB.PutUser(ctx, saved, nil)
	if e != nil {
		t.Fatal(e)
	}
	call(cl, "POST", "/v1/media/test/gc", nil, 403)
	call(cl, "POST", "/v1/media/test/tape/operations", map[string]string{"action": "erase"}, 403)
	saved.Permissions = []string{"cleanup"}
	saved, e = m.DB.PutUser(ctx, saved, nil)
	if e != nil {
		t.Fatal(e)
	}
	call(cl, "POST", "/v1/media/test/gc", nil, 400)
	saved.Role = "auditor"
	saved, e = m.DB.PutUser(ctx, saved, nil)
	if e != nil {
		t.Fatal(e)
	}
	call(cl, "POST", "/v1/media/test/gc", nil, 403)
	call(cl, "GET", "/v1/jobs", nil, 200)
	// Personal browser login and immediate session revocation on version change.
	handler := m.Handler()
	login := httptest.NewRequest("POST", "https://localhost/v1/login", strings.NewReader(`{"username":"alice","password":"personal-test-password-only"}`))
	login.Header.Set("Origin", "https://localhost")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, login)
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("session missing")
	}
	me := func(want int) {
		t.Helper()
		r := httptest.NewRequest("GET", "https://localhost/v1/me", nil)
		r.AddCookie(cookies[0])
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("session status %d want %d", w.Code, want)
		}
	}
	me(200)
	stale := saved
	saved.Name = "Renamed"
	saved, e = m.DB.PutUser(ctx, saved, nil)
	if e != nil {
		t.Fatal(e)
	}
	me(401)
	if _, e = m.DB.PutUser(ctx, stale, nil); e == nil {
		t.Fatal("stale write accepted")
	}
	call(cl, "GET", "/v1/me", nil, 200)
	saved.Disabled = true
	if _, e = m.DB.PutUser(ctx, saved, nil); e != nil {
		t.Fatal(e)
	}
	call(cl, "GET", "/v1/me", nil, 401)
}
func TestPermissionAllowlist(t *testing.T) {
	for _, tc := range []struct{ pattern, body, want string }{
		{"POST /v1/runs/{id}/restore", "", "restore"},
		{"POST /v1/media/{media}/tape/operations", `{"action":"erase"}`, "erase"},
		{"POST /v1/media/{media}/tape/operations", `{"action":"release-hold"}`, ""},
		{"POST /v1/media/{media}/removable/{volume}/operations", `{"action":"recycle"}`, "erase"},
		{"POST /v1/future/destructive", "", ""},
	} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
		r.Pattern = tc.pattern
		if p := permissionFor(r); p != tc.want {
			t.Fatalf("%s: %s", tc.pattern, p)
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != tc.body {
			t.Fatal("authorization consumed request body")
		}
	}
}

func TestAccessUIFixture(t *testing.T) {
	marker := os.Getenv("SRK_ACCESS_UI_MARKER")
	if marker == "" {
		t.Skip("optional browser fixture")
	}
	cfg := configs(t)
	mc := cfg["manager"]
	mc.PersonalCAKeyFile = filepath.Join(filepath.Dir(mc.CA), "ca.key")
	m := managerTest(t, mc)
	_, e := m.DB.PutUser(context.Background(), model.User{Username: "operator-demo", Name: "Оператор восстановления", Role: "operator", AllJobs: true, AllSites: true, RestoreSiteIDs: []string{"recovery"}, Permissions: []string{"restore", "verify"}}, []byte("unused-test-password-hash"))
	if e != nil {
		t.Fatal(e)
	}
	if e = m.DB.PutNode(context.Background(), model.Node{ID: "recovery-agent", Role: "agent", LastSeen: time.Now(), Status: map[string]any{}}); e != nil {
		t.Fatal(e)
	}
	_, e = m.DB.PutProtectionSettings(context.Background(), model.ProtectionSettings{Sites: []model.ProtectionSite{{ID: "recovery", Name: "Резервная площадка", DisasterDomain: "remote"}}, RestoreTargets: []model.RestoreTarget{{ID: "files-recovery", Name: "Сервер восстановления", SiteID: "recovery", NodeID: "recovery-agent", Kind: "files"}}})
	if e != nil {
		t.Fatal(e)
	}

	cert, e := tls.LoadX509KeyPair(os.Getenv("SRK_ACCESS_UI_CERT"), os.Getenv("SRK_ACCESS_UI_KEY"))
	if e != nil {
		t.Fatal(e)
	}
	ui := httptest.NewUnstartedServer(m.Handler())
	ui.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
	ui.StartTLS()
	defer ui.Close()
	if e = os.WriteFile(marker, []byte(ui.URL), 0600); e != nil {
		t.Fatal(e)
	}
	defer os.Remove(marker)
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if _, e = os.Stat(marker); os.IsNotExist(e) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
}
