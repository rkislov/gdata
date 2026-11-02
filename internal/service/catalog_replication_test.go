// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"encoding/json"
	"gdata/srk/internal/catalogbackup"
	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"github.com/jackc/pgx/v5"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCatalogReplicationAndDrill(t *testing.T) {
	ctx := context.Background()
	cfg := configs(t)
	root := t.TempDir()
	key := filepath.Join(root, "key")
	if e := catalogbackup.GenerateKey(key); e != nil {
		t.Fatal(e)
	}
	drillDSN := os.Getenv("SRK_TEST_DRILL_DATABASE_URL")
	if drillDSN == "" {
		t.Skip("set separate SRK_TEST_DRILL_DATABASE_URL")
	}
	dsnFile := filepath.Join(root, "drill-dsn")
	if e := os.WriteFile(dsnFile, []byte(drillDSN), 0600); e != nil {
		t.Fatal(e)
	}
	mc := cfg["manager"]
	mc.CatalogBackup = &config.CatalogBackupConfig{Directory: filepath.Join(root, "archives"), KeyFile: key, DrillDatabaseURLFile: dsnFile}
	m := managerTest(t, mc)
	defer m.CatalogBackups.Close()
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	defer media.Repo.Close()
	defer media.Captures.Close()
	defer media.CaptureClient.CloseIdleConnections()
	ms := tlsServer(t, cfg["media"], media.Handler())
	server := tlsServer(t, mc, m.Handler())
	if e = m.DB.PutNode(ctx, model.Node{ID: media.Config.ID, Role: "media", URL: ms.URL, LastSeen: time.Now()}); e != nil {
		t.Fatal(e)
	}
	admin, e := identity.Client(cfg["admin"])
	if e != nil {
		t.Fatal(e)
	}
	defer admin.CloseIdleConnections()
	auditor, e := identity.Client(cfg["auditor"])
	if e != nil {
		t.Fatal(e)
	}
	defer auditor.CloseIdleConnections()
	accessCall(t, auditor, "GET", server.URL+"/v1/catalog-backups", nil, 403)
	accessCall(t, admin, "GET", ms.URL+"/v1/catalog-copies", nil, 403)
	var entry catalogbackup.Entry
	if e = json.Unmarshal(accessCall(t, admin, "POST", server.URL+"/v1/catalog-backups", nil, 202), &entry); e != nil {
		t.Fatal(e)
	}
	eventually(t, func() bool { return m.CatalogBackups.List()[0].State == "success" })
	policy := catalogbackup.Policy{Version: 0, TargetMediaID: media.Config.ID, RemoteRetentionDays: 30, LocalRetentionDays: 1, DrillIntervalSeconds: 60}
	accessCall(t, admin, "PUT", server.URL+"/v1/catalog-backups/policy", policy, 200)
	accessCall(t, admin, "PUT", server.URL+"/v1/catalog-backups/policy", policy, 409)
	m.CatalogBackups.Run()
	eventually(t, func() bool {
		for _, op := range m.CatalogBackups.Operations() {
			if op.Kind == "drill" && op.State != "running" {
				if op.State != "success" || !op.Cleanup || op.Manifest == nil {
					t.Fatalf("drill failed: %+v", op)
				}
				return true
			}
		}
		return false
	})
	list, e := media.CatalogCopies.List()
	if e != nil || len(list) != 1 {
		t.Fatal("replica missing", e)
	}
	if list[0].ID != entry.ID || time.Until(list[0].ExpiresAt) < 29*24*time.Hour {
		t.Fatal("invalid remote receipt")
	}
	accessCall(t, admin, "GET", server.URL+"/v1/catalog-backups/remote/"+media.Config.ID+"/copies", nil, 200)
	conn, e := pgx.Connect(ctx, drillDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	var count int
	if e = conn.QueryRow(ctx, "SELECT count(*) FROM pg_namespace WHERE nspname LIKE 'srk_drill_%'").Scan(&count); e != nil || count != 0 {
		t.Fatal("drill schema leaked", count, e)
	}
	// No plaintext recovery materials are written by the drill.
	files, _ := filepath.Glob(filepath.Join(mc.CatalogBackup.Directory, ".drill-*"))
	if len(files) != 0 {
		t.Fatal("drill archive leaked")
	}
	// Remote integrity failure must not produce a successful recovery report.
	path := filepath.Join(media.Repo.Root, "catalog-copies", entry.ID+".srkcat")
	if e = os.WriteFile(path, []byte("corrupted"), 0600); e != nil {
		t.Fatal(e)
	}
	op, e := m.CatalogBackups.StartOperation("drill", entry.ID, media.Config.ID, "test")
	if e != nil {
		t.Fatal(e)
	}
	eventually(t, func() bool {
		ops := m.CatalogBackups.Operations()
		return ops[0].ID == op.ID && ops[0].State == "error"
	})
	// A failed or ambiguous upload can be retried safely with the same archive identity.
	op, e = m.CatalogBackups.StartOperation("replicate", entry.ID, media.Config.ID, "test")
	if e != nil {
		t.Fatal(e)
	}
	eventually(t, func() bool { return m.CatalogBackups.Operations()[0].State != "running" })
	if m.CatalogBackups.Operations()[0].State != "success" {
		t.Fatal("verified retry failed")
	}
	op, e = m.CatalogBackups.StartOperation("drill", entry.ID, media.Config.ID, "test")
	if e != nil {
		t.Fatal(e)
	}
	eventually(t, func() bool {
		return m.CatalogBackups.Operations()[0].ID == op.ID && m.CatalogBackups.Operations()[0].State != "running"
	})
	if m.CatalogBackups.Operations()[0].State != "success" {
		t.Fatal("retry did not repair remote archive")
	}
	// Dedicated-database guard must reject even a schema-qualified production URL.
	if e = os.WriteFile(dsnFile, []byte(m.Config.DatabaseURL), 0600); e != nil {
		t.Fatal(e)
	}
	op, e = m.CatalogBackups.StartOperation("drill", entry.ID, "", "test")
	if e != nil {
		t.Fatal(e)
	}
	eventually(t, func() bool {
		return m.CatalogBackups.Operations()[0].ID == op.ID && m.CatalogBackups.Operations()[0].State == "error"
	})
	if e = conn.QueryRow(ctx, "SELECT count(*) FROM pg_namespace WHERE nspname LIKE 'srk_drill_%'").Scan(&count); e != nil || count != 0 {
		t.Fatal("failed drill leaked schema")
	}
	if _, e = m.DB.RecoveryState(ctx); e != nil {
		t.Fatal("production catalog damaged", e)
	}
}
