// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
)

func managerTest(t *testing.T, c config.Config) *Manager {
	t.Helper()
	dsn := os.Getenv("SRK_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SRK_TEST_DATABASE_URL to a dedicated PostgreSQL test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, e := pgx.Connect(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := "test_" + model.ID()
	if _, e = conn.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		_ = conn.Close(ctx)
	})
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	c.DatabaseURL = u.String()
	c.PasswordFile = filepath.Join(t.TempDir(), "password")
	if e = os.WriteFile(c.PasswordFile, []byte("integration-test-password-only"), 0600); e != nil {
		t.Fatal(e)
	}
	m, e := NewManager(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(m.DB.Close)
	return m
}
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not reached before deadline")
}
func startScheduler(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.scheduler(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("scheduler did not stop")
		}
	})
}
func TestManagerPostgresEndToEndAndAuthorization(t *testing.T) {
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	manager := tlsServer(t, cfg["manager"], m.Handler())
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { media.Repo.Close() })
	ms := tlsServer(t, cfg["media"], media.Handler())
	source := t.TempDir()
	restore := t.TempDir()
	if e = os.WriteFile(filepath.Join(source, "hello.txt"), []byte("сквозное восстановление"), 0640); e != nil {
		t.Fatal(e)
	}
	ac := cfg["agent"]
	ac.AllowedRoots = []string{source}
	ac.RestoreRoots = []string{restore}
	a, e := NewAgent(ac)
	if e != nil {
		t.Fatal(e)
	}
	as := tlsServer(t, ac, a.Handler())
	for _, n := range []model.Node{{ID: ac.ID, Role: "agent", URL: as.URL}, {ID: cfg["media"].ID, Role: "media", URL: ms.URL}} {
		cl, e := identity.Client(cfg[n.Role], identity.Peer{Role: "manager"})
		if e != nil {
			t.Fatal(e)
		}
		if e = Call(context.Background(), cl, "POST", manager.URL+"/v1/nodes/heartbeat", n, nil); e != nil {
			t.Fatal(e)
		}
	}
	cl, e := identity.Client(cfg["admin"], identity.Peer{Role: "manager"})
	if e != nil {
		t.Fatal(e)
	}
	job := model.Job{Name: "Сквозное задание", AgentID: ac.ID, MediaID: cfg["media"].ID, Source: source, Mode: "incremental", RetentionDays: 30}
	if e = Call(context.Background(), cl, "POST", manager.URL+"/v1/jobs", job, &job); e != nil {
		t.Fatal(e)
	}
	auditor, e := identity.Client(cfg["auditor"], identity.Peer{Role: "manager"})
	if e != nil {
		t.Fatal(e)
	}
	if e = Call(context.Background(), auditor, "POST", manager.URL+"/v1/jobs", job, new(any)); e == nil {
		t.Fatal("auditor created job")
	}
	startScheduler(t, m)
	var run model.Run
	if e = Call(context.Background(), cl, "POST", manager.URL+"/v1/jobs/"+job.ID+"/run", nil, &run); e != nil {
		t.Fatal(e)
	}
	eventually(t, func() bool {
		v, e := m.DB.Run(context.Background(), run.ID)
		if e == nil && v.Status == "error" {
			t.Fatalf("backup failed: %s", v.Error)
		}
		return e == nil && v.Status == "success"
	})
	var restored model.Run
	dest := filepath.Join(restore, "restored")
	if e = Call(context.Background(), cl, "POST", manager.URL+"/v1/runs/"+run.ID+"/restore", map[string]string{"destination": dest}, &restored); e != nil {
		t.Fatal(e)
	}
	eventually(t, func() bool {
		v, e := m.DB.Run(context.Background(), restored.ID)
		if e == nil && v.Status == "error" {
			t.Fatalf("restore failed: %s", v.Error)
		}
		return e == nil && v.Status == "success"
	})
	b, e := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if e != nil || string(b) != "сквозное восстановление" {
		t.Fatal("restored content differs")
	}
	response, e := cl.Get(manager.URL + "/v1/reports/operations.xlsx")
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" {
		t.Fatal("XLSX export failed")
	}
	audit, e := m.DB.Audits(context.Background())
	if e != nil || len(audit) < 5 {
		t.Fatal("audit trail missing")
	}
}
func TestPersistentQueueParallelLimitsAndCancellation(t *testing.T) {
	cfg := configs(t)
	c := cfg["manager"]
	c.MaxParallel = 2
	c.PerAgent = 2
	c.PerMedia = 2
	m := managerTest(t, c)
	var active, peak atomic.Int32
	release := make(chan struct{}, 3)
	worker := tlsServer(t, cfg["agent"], http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var execution model.Execute
		if e := Decode(w, r, &execution); e != nil {
			Fail(w, 400, e)
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		select {
		case <-release:
			JSON(w, 200, model.Result{Verified: true})
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release) })
	now := time.Now().UTC()
	for _, n := range []model.Node{{ID: cfg["agent"].ID, Role: "agent", URL: worker.URL, LastSeen: now}, {ID: cfg["media"].ID, Role: "media", URL: "https://localhost:1", LastSeen: now}} {
		if e := m.DB.PutNode(context.Background(), n); e != nil {
			t.Fatal(e)
		}
	}
	ids := []string{}
	for i := 0; i < 3; i++ {
		j := model.Job{ID: model.ID(), AgentID: cfg["agent"].ID, MediaID: cfg["media"].ID, Source: fmt.Sprintf("/source/%d", i), Mode: "full", RetentionDays: 1}
		r := model.Run{ID: model.ID(), JobID: j.ID, Job: j, Kind: "backup", Status: "queued", CreatedAt: now.Add(time.Duration(i) * time.Millisecond)}
		if _, e := m.DB.Enqueue(context.Background(), r, ""); e != nil {
			t.Fatal(e)
		}
		ids = append(ids, r.ID)
	}
	startScheduler(t, m)
	eventually(t, func() bool { return active.Load() == 2 })
	r, e := m.DB.Run(context.Background(), ids[2])
	if e != nil || r.Status != "queued" {
		t.Fatalf("third job not queued: %+v %v", r, e)
	}
	release <- struct{}{}
	eventually(t, func() bool { v, e := m.DB.Run(context.Background(), ids[2]); return e == nil && v.Status == "running" })
	if peak.Load() != 2 {
		t.Fatalf("parallel cap violated: %d", peak.Load())
	}
	m.mu.Lock()
	cancel := m.active[ids[2]]
	m.mu.Unlock()
	if cancel == nil {
		t.Fatal("operation cancel missing")
	}
	cancel()
	eventually(t, func() bool {
		v, e := m.DB.Run(context.Background(), ids[2])
		return e == nil && v.Status == "cancelled"
	})
	release <- struct{}{}
}
func TestCatalogLockIdempotencyAndSchedule(t *testing.T) {
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	if second, e := NewManager(context.Background(), m.Config); e == nil {
		second.DB.Close()
		t.Fatal("second active manager allowed")
	}
	j := model.Job{ID: model.ID(), Name: "daily", Mode: "full", Enabled: true, IntervalSeconds: 86400, NextRun: time.Now().Add(-48 * time.Hour), CreatedAt: time.Now()}
	if e := m.DB.PutJob(context.Background(), j); e != nil {
		t.Fatal(e)
	}
	if e := m.DB.Schedule(context.Background(), time.Now()); e != nil {
		t.Fatal(e)
	}
	runs, e := m.DB.Runs(context.Background(), 10)
	if e != nil || len(runs) != 1 {
		t.Fatalf("missed intervals created wrong queue: %v %v", runs, e)
	}
	if e := m.DB.Schedule(context.Background(), time.Now()); e != nil {
		t.Fatal(e)
	}
	again, e := m.DB.Runs(context.Background(), 10)
	if e != nil || len(again) != 1 {
		t.Fatal("schedule duplicated run")
	}
	if e := m.DB.Schedule(context.Background(), time.Now().Add(48*time.Hour)); e != nil {
		t.Fatal(e)
	}
	bounded, e := m.DB.Runs(context.Background(), 10)
	if e != nil || len(bounded) != 1 {
		t.Fatal("scheduler accumulated duplicate pending launches")
	}
	run := model.Run{ID: model.ID(), JobID: j.ID, Job: j, Kind: "backup", Status: "queued", CreatedAt: time.Now()}
	first, e := m.DB.Enqueue(context.Background(), run, "stable-key")
	if e != nil {
		t.Fatal(e)
	}
	run.ID = model.ID()
	second, e := m.DB.Enqueue(context.Background(), run, "stable-key")
	if e != nil || second.ID != first.ID {
		t.Fatal("idempotency failed")
	}
	run.Kind = "restore"
	if _, e = m.DB.Enqueue(context.Background(), run, "stable-key"); e == nil {
		t.Fatal("idempotency payload conflict accepted")
	}
}
func TestBrowserLoginRequiresOriginAndSecureCookie(t *testing.T) {
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	server := tlsServer(t, cfg["manager"], m.Handler())
	tc, e := identity.Config(cfg["admin"], false)
	if e != nil {
		t.Fatal(e)
	}
	tc.Certificates = nil
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: tc}, Timeout: 3 * time.Second}
	b, _ := json.Marshal(map[string]string{"password": "integration-test-password-only"})
	req, _ := http.NewRequest("POST", server.URL+"/v1/login", bytes.NewReader(b))
	req.Header.Set("Origin", "https://attacker.invalid")
	res, e := cl.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("cross-origin login accepted")
	}
	req, _ = http.NewRequest("POST", server.URL+"/v1/login", bytes.NewReader(b))
	req.Header.Set("Origin", server.URL)
	res, e = cl.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal("valid login failed")
	}
	cookies := res.Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("insecure session cookie")
	}
	req, _ = http.NewRequest("POST", server.URL+"/v1/logout", nil)
	req.AddCookie(cookies[0])
	res, e = cl.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("state change without origin accepted")
	}
}
func TestReportsCoverFullFilteredHistoryAndFuture(t *testing.T) {
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	server := tlsServer(t, cfg["manager"], m.Handler())
	cl, e := identity.Client(cfg["admin"], identity.Peer{Role: "manager"})
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	j := model.Job{ID: model.ID(), Name: "=1+1 report-test", Source: "/report-data", Mode: "full", Enabled: true, IntervalSeconds: 3600, RetentionDays: 30, NextRun: now.Add(time.Hour), CreatedAt: now}
	if e = m.DB.PutJob(context.Background(), j); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 205; i++ {
		r := model.Run{ID: model.ID(), JobID: j.ID, Job: j, Kind: "backup", Status: "success", CreatedAt: now.Add(-time.Duration(i) * time.Minute)}
		if i == 0 {
			r.Job.Type = "postgresql"
			r.Result = &model.Result{Consistency: "postgresql-physical", PostgreSQL: &model.PostgreSQLBackup{Major: 16, NativeVerified: true, WALRanges: []model.PostgreSQLWALRange{{Timeline: 1, StartLSN: "0/100", EndLSN: "0/200"}}}}
		}
		if _, e = m.DB.Enqueue(context.Background(), r, ""); e != nil {
			t.Fatal(e)
		}
	}
	var page []model.Run
	if e = Call(context.Background(), cl, "GET", server.URL+"/v1/runs?limit=1&scope=history&search=report-test", nil, &page); e != nil || len(page) != 1 {
		t.Fatalf("pagination failed: %d %v", len(page), e)
	}
	csvRows := func(q string) [][]string {
		t.Helper()
		r, e := cl.Get(server.URL + "/v1/reports/operations.csv?" + q)
		if e != nil {
			t.Fatal(e)
		}
		defer r.Body.Close()
		rows, e := csv.NewReader(r.Body).ReadAll()
		if e != nil || r.StatusCode != 200 {
			t.Fatalf("CSV failed: %d %v", r.StatusCode, e)
		}
		return rows
	}
	rows := csvRows("scope=history&search=report-test&limit=1")
	if len(rows) != 206 {
		t.Fatalf("export truncated to UI pagination: %d", len(rows))
	}
	if len(rows[0]) != 30 || rows[1][21] != "postgresql" || rows[1][22] != "16" || rows[1][23] != "true" || !strings.Contains(rows[1][24], "0/200") || rows[1][25] != "postgresql-physical" {
		t.Fatal("PostgreSQL metadata is missing from report")
	}
	if !strings.HasPrefix(rows[1][1], "'=") {
		t.Fatal("CSV formula guard absent")
	}
	future := csvRows("scope=upcoming&search=report-test")
	if len(future) != 2 || future[1][3] != "planned" || future[1][4] != "" || future[1][5] == "" {
		t.Fatalf("future activity missing planned time: %v", future)
	}
	if e = Call(context.Background(), cl, "GET", server.URL+"/v1/runs?search="+url.QueryEscape("' OR 1=1"), nil, &page); e != nil || len(page) != 0 {
		t.Fatal("search was not treated as literal data")
	}
}
