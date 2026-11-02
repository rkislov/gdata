// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"github.com/jackc/pgx/v5"
)

func pgTestCommand(t *testing.T, bin, tool string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, filepath.Join(bin, tool), args...)
	command.WaitDelay = 2 * time.Second
	out, e := command.CombinedOutput()
	if e != nil {
		t.Fatalf("%s: %v: %s", tool, e, out)
	}
}
func pgTestStart(t *testing.T, bin, data string) int {
	t.Helper()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	// Override copied configuration, including data_directory and all listening endpoints.
	options := fmt.Sprintf("-h 127.0.0.1 -p %d -k '' -c data_directory=%s -c ssl=off -c archive_mode=off -c restore_command='' -c primary_conninfo='' -c shared_preload_libraries=''", port, data)
	pgTestCommand(t, bin, "pg_ctl", "-D", data, "-l", filepath.Join(filepath.Dir(data), "server-"+strconv.Itoa(port)+".log"), "-o", options, "-w", "start")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out, e := exec.CommandContext(ctx, filepath.Join(bin, "pg_ctl"), "-D", data, "-m", "immediate", "-w", "stop").CombinedOutput()
		if e != nil {
			t.Errorf("stop temporary PostgreSQL: %v %s", e, out)
		}
	})
	return port
}
func TestPostgreSQLHotBackupRestoreWithConcurrentWrites(t *testing.T) {
	bin := os.Getenv("SRK_TEST_PG_BIN")
	if bin == "" {
		t.Skip("set SRK_TEST_PG_BIN to native PostgreSQL 13–18 binary directory")
	}
	if os.Geteuid() == 0 {
		t.Skip("initdb requires a non-root user")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	base := t.TempDir()
	primary := filepath.Join(base, "primary")
	pgTestCommand(t, bin, "initdb", "-D", primary, "--username=srk_test", "--auth-local=trust", "--auth-host=trust", "--no-locale", "--encoding=UTF8")
	port := pgTestStart(t, bin, primary)
	conn, e := pgx.Connect(ctx, fmt.Sprintf("postgres://srk_test@127.0.0.1:%d/postgres?sslmode=disable", port))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(context.Background())
	if _, e = conn.Exec(ctx, "CREATE ROLE srk_backup LOGIN REPLICATION; CREATE TABLE srk_probe(id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, value text NOT NULL); INSERT INTO srk_probe(value) VALUES ('before-backup');"); e != nil {
		t.Fatal(e)
	}
	var started time.Time
	if e = conn.QueryRow(ctx, "SELECT pg_postmaster_start_time()").Scan(&started); e != nil {
		t.Fatal(e)
	}
	writerCtx, stopWriter := context.WithCancel(ctx)
	defer stopWriter()
	writerDone := make(chan struct{})
	var writes atomic.Int64
	var writerError atomic.Value
	go func() {
		defer close(writerDone)
		c, e := pgx.Connect(writerCtx, fmt.Sprintf("postgres://srk_test@127.0.0.1:%d/postgres?sslmode=disable", port))
		if e != nil {
			writerError.Store(e.Error())
			return
		}
		defer c.Close(context.Background())
		for {
			select {
			case <-writerCtx.Done():
				return
			case <-time.After(10 * time.Millisecond):
				if _, e = c.Exec(writerCtx, "INSERT INTO srk_probe(value) VALUES ('during-backup')"); e != nil {
					if writerCtx.Err() == nil {
						writerError.Store(e.Error())
					}
					return
				}
				writes.Add(1)
			}
		}
	}()
	cfg := configs(t)
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	defer media.Repo.Close()
	server := tlsServer(t, cfg["media"], media.Handler())
	staging := filepath.Join(base, "staging")
	if e = os.Mkdir(staging, 0700); e != nil {
		t.Fatal(e)
	}
	ac := cfg["agent"]
	ac.RestoreRoots = []string{base}
	ac.PostgreSQL = map[string]config.PostgreSQLProfile{"test": {Host: "127.0.0.1", Port: port, User: "srk_backup", Database: "postgres", SSLMode: "disable", BinDir: bin, StagingDir: staging, TimeoutSeconds: 60}}
	a, e := NewAgent(ac)
	if e != nil {
		t.Fatal(e)
	}
	j := model.Job{ID: model.ID(), Type: "postgresql", Name: "hot PostgreSQL", Source: "postgresql://test", Mode: "full", AgentID: ac.ID, MediaID: cfg["media"].ID, RetentionDays: 30}
	v := model.Execute{RunID: model.ID(), Kind: "backup", Job: j, MediaURL: server.URL}
	key := bytes.Repeat([]byte("k"), 32)
	v.Grant = identity.Sign(identity.Grant{Subject: ac.ID, RunID: v.RunID, Permission: "write", Expires: time.Now().Add(time.Hour).Unix()}, key)
	res, e := a.Backup(ctx, v)
	if e != nil {
		t.Fatal(e)
	}
	stopWriter()
	<-writerDone
	if writerError.Load() != nil || writes.Load() == 0 {
		t.Fatalf("source was not writable during backup: writes=%d error=%v", writes.Load(), writerError.Load())
	}
	if !res.Verified || res.PostgreSQL == nil || !res.PostgreSQL.NativeVerified || len(res.PostgreSQL.WALRanges) == 0 || res.Consistency != "postgresql-physical" {
		t.Fatalf("invalid PostgreSQL result: %+v", res)
	}
	if res.ProgressPercent == nil || *res.ProgressPercent != 100 {
		t.Fatal("PostgreSQL backup did not report completed progress")
	}
	var after time.Time
	if e = conn.QueryRow(ctx, "SELECT pg_postmaster_start_time()").Scan(&after); e != nil || !after.Equal(started) {
		t.Fatal("source PostgreSQL restarted")
	}
	entries, e := os.ReadDir(staging)
	if e != nil || len(entries) != 0 {
		t.Fatal("staging was not cleaned after successful backup")
	}
	restore := model.Execute{RunID: model.ID(), Kind: "restore", Job: j, MediaURL: server.URL, SnapshotID: v.RunID, Destination: filepath.Join(base, "restored")}
	restore.Grant = identity.Sign(identity.Grant{Subject: ac.ID, RunID: restore.RunID, SnapshotID: v.RunID, Permission: "read", Expires: time.Now().Add(time.Hour).Unix()}, key)
	rr, e := a.Restore(ctx, restore)
	if e != nil {
		t.Fatal(e)
	}
	if rr.PostgreSQL == nil || !rr.Verified {
		t.Fatal("native restore verification missing")
	}
	// Removing a required WAL segment must fail native verification, even with intact data files.
	walDir := filepath.Join(restore.Destination, "pg_wal")
	wal, e := os.ReadDir(walDir)
	if e != nil {
		t.Fatal(e)
	}
	checked := false
	for _, f := range wal {
		if f.Type().IsRegular() && len(f.Name()) == 24 {
			file := filepath.Join(walDir, f.Name())
			b, e := os.ReadFile(file)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.Remove(file); e != nil {
				t.Fatal(e)
			}
			if a.verifyPostgreSQLRestore(ctx, res.PostgreSQL, restore.Destination) == nil {
				t.Fatal("missing recovery WAL was accepted")
			}
			if e = os.WriteFile(file, b, 0600); e != nil {
				t.Fatal(e)
			}
			checked = true
			break
		}
	}
	if !checked {
		t.Fatal("no WAL segment found for negative check")
	}
	restoredPort := pgTestStart(t, bin, restore.Destination)
	recovered, e := pgx.Connect(ctx, fmt.Sprintf("postgres://srk_test@127.0.0.1:%d/postgres?sslmode=disable", restoredPort))
	if e != nil {
		t.Fatal(e)
	}
	defer recovered.Close(context.Background())
	var marker int
	if e = recovered.QueryRow(ctx, "SELECT count(*) FROM srk_probe WHERE value='before-backup'").Scan(&marker); e != nil || marker != 1 {
		t.Fatalf("restored PostgreSQL data missing: %d %v", marker, e)
	}
	// An extra tablespace is detected in tar output; its source directory is never a backup target.
	tablespace := filepath.Join(base, "tablespace")
	if e = os.Mkdir(tablespace, 0700); e != nil {
		t.Fatal(e)
	}
	if _, e = conn.Exec(ctx, "CREATE TABLESPACE extra LOCATION '"+strings.ReplaceAll(tablespace, "'", "''")+"'"); e != nil {
		t.Fatal(e)
	}
	v.RunID = model.ID()
	v.Grant = identity.Sign(identity.Grant{Subject: ac.ID, RunID: v.RunID, Permission: "write", Expires: time.Now().Add(time.Hour).Unix()}, key)
	if _, e = a.Backup(ctx, v); e == nil || !strings.Contains(e.Error(), "tablespaces") {
		t.Fatalf("additional tablespace should be explicitly rejected: %v", e)
	}
	if _, e = media.Repo.Manifest(v.RunID); !os.IsNotExist(e) {
		t.Fatal("failed PostgreSQL backup was published")
	}
	entries, e = os.ReadDir(staging)
	if e != nil || len(entries) != 0 {
		t.Fatal("staging was not cleaned after failure")
	}
	t.Logf("restored PostgreSQL %d, writes during backup: %d, WAL ranges: %+v", res.PostgreSQL.Major, writes.Load(), res.PostgreSQL.WALRanges)
}
