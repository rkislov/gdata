// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gdata/srk/internal/config"
	"gdata/srk/internal/model"
)

func TestPostgreSQLArchiveConfinement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
	}{
		{"parent", []*tar.Header{{Name: "../escaped", Typeflag: tar.TypeReg}}},
		{"absolute", []*tar.Header{{Name: "/escaped", Typeflag: tar.TypeReg}}},
		{"link", []*tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}}},
		{"hardlink", []*tar.Header{{Name: "link", Typeflag: tar.TypeLink, Linkname: "data"}}},
		{"device", []*tar.Header{{Name: "device", Typeflag: tar.TypeFifo}}},
		{"duplicate", []*tar.Header{{Name: "data", Typeflag: tar.TypeReg}, {Name: "data", Typeflag: tar.TypeReg}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			tw := tar.NewWriter(&b)
			for _, h := range tc.headers {
				if e := tw.WriteHeader(h); e != nil {
					t.Fatal(e)
				}
			}
			if e := tw.Close(); e != nil {
				t.Fatal(e)
			}
			archive := filepath.Join(t.TempDir(), "base.tar")
			if e := os.WriteFile(archive, b.Bytes(), 0600); e != nil {
				t.Fatal(e)
			}
			if e := extractPostgreSQLTar(context.Background(), archive, t.TempDir(), 0); e == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}
func TestPostgreSQLProgressStream(t *testing.T) {
	var reports []float64
	writer := &postgresProgressWriter{report: func(done, total int64, percent float64) {
		if done > total || total < 1 {
			t.Fatal("invalid progress counters")
		}
		reports = append(reports, percent)
	}}
	for _, part := range []string{"tool diagnostic with a secret\n", " 1024/40", "96 kB (25%), 1/1 tablespace\r", "4096/4096 kB (100%), 1/1 tablespace\n", strings.Repeat("x", 10000), "\n"} {
		if _, e := writer.Write([]byte(part)); e != nil {
			t.Fatal(e)
		}
	}
	if len(reports) != 2 || reports[0] != 25 || reports[1] != 99 || len(writer.pending) != 0 {
		t.Fatalf("invalid or unbounded native progress parsing: %v", reports)
	}
}
func TestPostgreSQLCommandCancellationAndCredentialIsolation(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'secret-should-not-escape' >&2\nsleep 30\n"
	if e := os.WriteFile(filepath.Join(dir, "pg_basebackup"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PGPASSWORD", "secret-should-not-escape")
	t.Setenv("PGSERVICE", "unexpected-service")
	p := config.PostgreSQLProfile{BinDir: dir, StagingDir: dir, Host: "127.0.0.1", Port: 55432, User: "backup", Database: "postgres", SSLMode: "disable"}
	for _, v := range postgresEnvironment(p) {
		if strings.HasPrefix(v, "PGPASSWORD=") || strings.HasPrefix(v, "PGSERVICE=") {
			t.Fatal("inherited PostgreSQL credentials or service")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	e := postgresCommand(ctx, p, "pg_basebackup")
	if e == nil || strings.Contains(e.Error(), "secret-should-not-escape") || time.Since(started) > 3*time.Second {
		t.Fatalf("cancellation or redaction failed: %v", e)
	}
}
func TestPostgreSQLJobValidation(t *testing.T) {
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	ctx := context.Background()
	a := model.Node{ID: "pg-agent", Role: "agent", Status: map[string]any{"postgresql_profiles": []string{"production"}}}
	if e := m.DB.PutNode(ctx, a); e != nil {
		t.Fatal(e)
	}
	if e := m.DB.PutNode(ctx, model.Node{ID: "media", Role: "media"}); e != nil {
		t.Fatal(e)
	}
	j := model.Job{Type: "postgresql", Name: "hot", Source: "postgresql://production", Mode: "full", AgentID: a.ID, MediaID: "media", RetentionDays: 30}
	if e := m.validateJob(ctx, j); e != nil {
		t.Fatal(e)
	}
	point := model.SnapshotInfo{ID: model.ID(), JobID: model.ID(), Source: j.Source, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(7 * 24 * time.Hour), Verified: true, Hold: true, Result: model.Result{PostgreSQL: &model.PostgreSQLBackup{Profile: "production", Major: 16, NativeVerified: true}}}
	if _, e := m.DB.RegisterImport(ctx, "media", a.ID, model.ID(), []model.SnapshotInfo{point}); e != nil {
		t.Fatal(e)
	}
	jobs, e := m.DB.Jobs(ctx)
	if e != nil || len(jobs) != 1 || jobs[0].Type != "postgresql" || jobs[0].Enabled {
		t.Fatal("PostgreSQL import lost source type or scheduled an imported job", e)
	}
	for _, mutate := range []func(*model.Job){func(j *model.Job) { j.Mode = "incremental" }, func(j *model.Job) { j.Source = "postgresql://unknown" }, func(j *model.Job) { j.Source = "postgresql://user:password@host/db" }, func(j *model.Job) { j.Type = "unknown" }} {
		bad := j
		mutate(&bad)
		if m.validateJob(ctx, bad) == nil {
			t.Fatalf("invalid PostgreSQL job accepted: %+v", bad)
		}
	}
}
