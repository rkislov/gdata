// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"github.com/jackc/pgx/v5"
)

func TestContinuousWALAndPITRStopsAtRequestedTime(t *testing.T) { testContinuousWALAndPITR(t, false) }
func TestReplicatedContinuousWALAndRemotePITR(t *testing.T)     { testContinuousWALAndPITR(t, true) }
func testContinuousWALAndPITR(t *testing.T, remote bool) {
	bin := os.Getenv("SRK_TEST_PG_BIN")
	srk := os.Getenv("SRK_TEST_SRK_BIN")
	if bin == "" || srk == "" {
		t.Skip("set SRK_TEST_PG_BIN and SRK_TEST_SRK_BIN for native WAL/PITR integration")
	}
	if os.Geteuid() == 0 {
		t.Skip("PostgreSQL requires a non-root test user")
	}
	if remote && os.Getenv("SRK_TEST_DATABASE_URL") == "" {
		t.Skip("remote PITR test requires isolated catalog database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	base := t.TempDir()
	primary := filepath.Join(base, "primary")
	staging := filepath.Join(base, "staging")
	if e := os.Mkdir(staging, 0700); e != nil {
		t.Fatal(e)
	}
	pgTestCommand(t, bin, "initdb", "-D", primary, "--username=srk_test", "--auth-local=trust", "--auth-host=trust", "--no-locale", "--encoding=UTF8", "--wal-segsize=1")
	cfg := configs(t)
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	defer media.Repo.Close()
	ms := tlsServer(t, cfg["media"], media.Handler())
	ac := cfg["agent"]
	ac.RestoreRoots = []string{base}
	profile := config.PostgreSQLProfile{Host: "127.0.0.1", Port: 5432, User: "srk_test", Database: "postgres", SSLMode: "disable", BinDir: bin, StagingDir: staging, TimeoutSeconds: 120, WALArchive: &config.WALArchiveConfig{DataDir: primary, MediaID: cfg["media"].ID, MediaURL: ms.URL}}
	ac.PostgreSQL = map[string]config.PostgreSQLProfile{"test": profile}
	a, e := NewAgent(ac)
	if e != nil {
		t.Fatal(e)
	}
	as := tlsServer(t, ac, a.Handler())
	pki := filepath.Dir(ac.CA)
	walID := ac.ID + "-wal"
	if e = identity.Issue(pki, "test", "wal", walID); e != nil {
		t.Fatal(e)
	}
	wc := config.Config{Role: "wal", ID: walID, Installation: "test", CA: ac.CA, Cert: filepath.Join(pki, walID+".crt"), Key: filepath.Join(pki, walID+".key"), URL: as.URL, Revoked: ac.Revoked, MaxParallel: 1, PerAgent: 1, PerMedia: 1}
	walConfig := filepath.Join(base, "wal-client.json")
	b, _ := json.Marshal(wc)
	if e = os.WriteFile(walConfig, b, 0600); e != nil {
		t.Fatal(e)
	}
	archiveCommand := shellLiteral(srk) + " --config " + shellLiteral(walConfig) + " --timeout 60s agent wal archive test --file " + shellLiteral("%p") + " --name " + shellLiteral("%f")
	f, e := os.OpenFile(filepath.Join(primary, "postgresql.conf"), os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.WriteString("\narchive_mode = on\narchive_timeout = '60s'\narchive_command = " + postgresLiteral(archiveCommand) + "\n")
	f.Close()
	if e != nil {
		t.Fatal(e)
	}
	// pgTestStart disables archive_mode for recovered copies; the source is explicitly reloaded below.
	port := pgTestStart(t, bin, primary)
	profile.Port = port
	ac.PostgreSQL["test"] = profile
	a.Config = ac
	conn, e := pgx.Connect(ctx, fmt.Sprintf("postgres://srk_test@127.0.0.1:%d/postgres?sslmode=disable", port))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(context.Background())
	// archive_mode is a startup parameter, so start this fixture with it enabled via pg_ctl restart.
	pgTestCommand(t, bin, "pg_ctl", "-D", primary, "-l", filepath.Join(base, "source-archive.log"), "-m", "fast", "-w", "restart", "-o", fmt.Sprintf("-h 127.0.0.1 -p %d -k '' -c archive_mode=on", port))
	conn.Close(context.Background())
	conn, e = pgx.Connect(ctx, fmt.Sprintf("postgres://srk_test@127.0.0.1:%d/postgres?sslmode=disable", port))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(context.Background())
	if _, e = conn.Exec(ctx, "CREATE TABLE pitr_probe(value text NOT NULL); INSERT INTO pitr_probe VALUES ('in-base')"); e != nil {
		t.Fatal(e)
	}
	j := model.Job{ID: model.ID(), Type: "postgresql", Name: "PITR", Source: "postgresql://test", AgentID: ac.ID, MediaID: cfg["media"].ID, Mode: "full", RetentionDays: 30}
	key := bytes.Repeat([]byte("k"), 32)
	backup := model.Execute{RunID: model.ID(), Kind: "backup", Job: j, MediaURL: ms.URL}
	backup.Grant = identity.Sign(identity.Grant{Subject: ac.ID, RunID: backup.RunID, Permission: "write", Expires: time.Now().Add(time.Hour).Unix()}, key)
	result, e := a.Backup(ctx, backup)
	if e != nil {
		t.Fatal(e)
	}
	meta := result.PostgreSQL
	if meta == nil || meta.ArchiveID == "" || meta.SystemID == "" || meta.SegmentSize != 1<<20 {
		t.Fatal("base is not linked to its WAL archive")
	}
	recoveryMedia, recoveryURL, recoverySnapshot := media, ms.URL, backup.RunID
	var replicationEnv *vmEnvironment
	var walPolicy model.WALReplicationPolicy
	if remote {
		manager := managerTest(t, cfg["manager"])
		managerServer := tlsServer(t, cfg["manager"], manager.Handler())
		admin, err := identity.Client(cfg["admin"], identity.Peer{Role: "manager"})
		if err != nil {
			t.Fatal(err)
		}
		defer admin.CloseIdleConnections()
		replicationEnv = &vmEnvironment{manager: manager, media: media, proxy: a, admin: admin, url: managerServer.URL}
		recoveryMedia, recoveryURL = addReplicaMedia(t, replicationEnv)
		if err = manager.DB.PutNode(ctx, model.Node{ID: media.Config.ID, Role: "media", URL: ms.URL, LastSeen: time.Now()}); err != nil {
			t.Fatal(err)
		}
		hb, hbCancel := context.WithCancel(context.Background())
		defer hbCancel()
		go func() {
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-hb.Done():
					return
				case <-tick.C:
					_ = manager.DB.PutNode(hb, model.Node{ID: media.Config.ID, Role: "media", URL: ms.URL, LastSeen: time.Now()})
				}
			}
		}()
		if err = manager.DB.PutJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		source := model.Run{ID: backup.RunID, JobID: j.ID, Job: j, Kind: "backup", Status: "success", CreatedAt: time.Now(), Result: &result}
		if _, err = manager.DB.Enqueue(ctx, source, ""); err != nil {
			t.Fatal(err)
		}
		startScheduler(t, manager)
		var op model.Run
		if err = Call(ctx, admin, "POST", managerServer.URL+"/v1/runs/"+source.ID+"/replicate", map[string]any{"target_media_id": recoveryMedia.Config.ID, "retention_days": 45, "include_wal": true}, &op); err != nil {
			t.Fatal(err)
		}
		copied := replicationEnv.wait(t, op.ID)
		if copied.Status != "success" {
			t.Fatal(copied.Error)
		}
		recoverySnapshot = copied.ID
		policies, err := manager.DB.WALPolicies(ctx)
		if err != nil || len(policies) != 1 {
			t.Fatal("native WAL policy missing", err)
		}
		walPolicy = policies[0]
	}
	if _, e = conn.Exec(ctx, "INSERT INTO pitr_probe VALUES ('before-target')"); e != nil {
		t.Fatal(e)
	}
	var target time.Time
	if e = conn.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&target); e != nil {
		t.Fatal(e)
	}
	time.Sleep(30 * time.Millisecond)
	if _, e = conn.Exec(ctx, "INSERT INTO pitr_probe VALUES ('after-target')"); e != nil {
		t.Fatal(e)
	}
	var closed string
	if e = conn.QueryRow(ctx, "SELECT pg_walfile_name(pg_current_wal_lsn())").Scan(&closed); e != nil {
		t.Fatal(e)
	}
	if _, e = conn.Exec(ctx, "SELECT pg_switch_wal()"); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(20 * time.Second)
	archived := false
	for time.Now().Before(deadline) {
		records, e := media.Repo.WALInventory(meta.ArchiveID)
		if e != nil {
			t.Fatal(e)
		}
		for _, r := range records {
			if r.Name == closed {
				archived = true
			}
		}
		if archived {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !archived {
		var fail string
		_ = conn.QueryRow(ctx, "SELECT coalesce(last_failed_wal,'') FROM pg_stat_archiver").Scan(&fail)
		t.Fatalf("native archive_command failed to store WAL %s (last failed %s); status %+v", closed, fail, a.walStatus())
	}
	if remote {
		if err := Call(ctx, replicationEnv.admin, "POST", replicationEnv.url+"/v1/wal-replication-policies/"+walPolicy.ID+"/run", nil, nil); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(40 * time.Second)
		for time.Now().Before(deadline) {
			f, _, err := recoveryMedia.Repo.OpenWAL(meta.ArchiveID, closed)
			if err == nil {
				f.Close()
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		latest, err := replicationEnv.manager.DB.LatestWALReplication(ctx, walPolicy.ID)
		if err != nil || latest == nil {
			t.Fatal("native WAL transfer not scheduled", err)
		}
		done := replicationEnv.wait(t, latest.ID)
		if done.Status != "success" {
			t.Fatal(done.Error)
		}
		if err = Call(ctx, replicationEnv.admin, "POST", replicationEnv.url+"/v1/wal-replication-policies/"+walPolicy.ID+"/enabled", map[string]bool{"enabled": false}, nil); err != nil {
			t.Fatal(err)
		}
		// The remote recovery must have no dependency on the original WAL files.
		if err = os.Rename(filepath.Join(media.Repo.Root, "wal", meta.ArchiveID), filepath.Join(base, "source-wal-unavailable")); err != nil {
			t.Fatal(err)
		}
	}
	calendar, err := recoveryMedia.Repo.WALCalendar(meta.ArchiveID)
	if err != nil || len(calendar.Windows) != 1 || calendar.Windows[0].ToExclusive == nil || !calendar.Windows[0].ToExclusive.After(target) {
		t.Fatalf("native WAL calendar did not cover transaction target: %+v, %v", calendar, err)
	}
	restore := model.Execute{RunID: model.ID(), Kind: "restore", Job: j, MediaURL: recoveryURL, SnapshotID: recoverySnapshot, Destination: filepath.Join(base, "pitr-restored"), PITR: &model.PITRRequest{TargetTime: target}}
	restore.Job.MediaID = recoveryMedia.Config.ID
	restore.Grant = identity.Sign(identity.Grant{ArchiveID: meta.ArchiveID, Subject: ac.ID, RunID: restore.RunID, SnapshotID: recoverySnapshot, Permission: "read", Expires: time.Now().Add(time.Hour).Unix()}, key)
	rr, e := a.Restore(ctx, restore)
	if e != nil {
		log, _ := os.ReadFile(filepath.Join(restore.Destination, "srk-pitr.log"))
		if len(log) > 16000 {
			log = log[len(log)-16000:]
		}
		t.Fatalf("%v\n%s", e, log)
	}
	if !rr.Verified || rr.PITR == nil || !rr.PITR.Recovered || !rr.PITR.TargetTime.Equal(target) {
		t.Fatal("PITR target was not confirmed")
	}
	// Enable loopback trust only in this isolated test copy, after SRK has completed recovery.
	if e = os.WriteFile(filepath.Join(restore.Destination, "pg_hba.conf"), []byte("host all all 127.0.0.1/32 trust\n"), 0600); e != nil {
		t.Fatal(e)
	}
	recoveredPort := pgTestStart(t, bin, restore.Destination)
	recovered, e := pgx.Connect(ctx, fmt.Sprintf("postgres://srk_test@127.0.0.1:%d/postgres?sslmode=disable", recoveredPort))
	if e != nil {
		t.Fatal(e)
	}
	defer recovered.Close(context.Background())
	var before, after int
	if e = recovered.QueryRow(ctx, "SELECT count(*) FILTER(WHERE value='before-target'), count(*) FILTER(WHERE value='after-target') FROM pitr_probe").Scan(&before, &after); e != nil || before != 1 || after != 0 {
		t.Fatalf("incorrect PITR state: before=%d after=%d error=%v", before, after, e)
	}
	var original int
	if e = conn.QueryRow(ctx, "SELECT count(*) FROM pitr_probe WHERE value='after-target'").Scan(&original); e != nil || original != 1 {
		t.Fatal("source data changed during PITR")
	}
	// A target beyond all available commits must fail instead of returning the end of the archive as success.
	unreachable := restore
	unreachable.RunID = model.ID()
	unreachable.Destination = filepath.Join(base, "unreachable")
	unreachable.PITR = nil
	unreachable.Grant = identity.Sign(identity.Grant{Subject: ac.ID, RunID: unreachable.RunID, SnapshotID: recoverySnapshot, Permission: "read", Expires: time.Now().Add(time.Hour).Unix()}, key)
	if _, e = a.Restore(ctx, unreachable); e != nil {
		t.Fatal(e)
	}
	beyond := &model.PITRRequest{TargetTime: time.Now().Add(-time.Microsecond)}
	if e = runPITRRecovery(ctx, profile, unreachable.Destination, filepath.Join(restore.Destination, "srk_wal_archive"), beyond, meta); e == nil {
		t.Fatal("unreachable recovery target was reported as successful")
	}
	failureLog, _ := os.ReadFile(filepath.Join(unreachable.Destination, "srk-pitr.log"))
	if !strings.Contains(string(failureLog), "recovery ended before configured recovery target was reached") {
		t.Fatalf("unexpected unreachable-target failure: %v\n%s", e, failureLog)
	}
	t.Logf("PITR target %s confirmed with %d archived segments", target.Format(time.RFC3339Nano), rr.PITR.Segments)
}
