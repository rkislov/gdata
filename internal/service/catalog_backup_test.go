// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"gdata/srk/internal/catalogbackup"
	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCatalogBackupDisasterRecovery(t *testing.T) {
	ctx := context.Background()
	cfg := configs(t)
	root := t.TempDir()
	keyFile := filepath.Join(root, "recovery.key")
	if e := catalogbackup.GenerateKey(keyFile); e != nil {
		t.Fatal(e)
	}
	mc := cfg["manager"]
	mc.PersonalCAKeyFile = filepath.Join(filepath.Dir(mc.CA), "ca.key")
	mc.CatalogBackup = &config.CatalogBackupConfig{Directory: filepath.Join(root, "backups"), KeyFile: keyFile}
	m := managerTest(t, mc)
	t.Cleanup(m.CatalogBackups.Close)
	if duplicate, err := catalogbackup.OpenStore(ctx, m.DB, m.Config); err == nil {
		duplicate.Close()
		t.Fatal("concurrent storage writer accepted")
	}
	server := tlsServer(t, mc, m.Handler())
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
	hash, _ := bcrypt.GenerateFromPassword([]byte("catalog-recovery-test-password"), bcrypt.MinCost)
	user := model.User{Username: "recovery-user", Name: "Before snapshot", Role: "operator", AllJobs: true, AllSites: true, RestoreSiteIDs: []string{"remote"}, Permissions: []string{"restore", "verify"}}
	user, e = m.DB.PutUser(ctx, user, hash)
	if e != nil {
		t.Fatal(e)
	}
	key := filepath.Join(root, "user.key")
	csr := filepath.Join(root, "user.csr")
	crt := filepath.Join(root, "user.crt")
	if e = identity.CreatePersonalCSR(key, csr); e != nil {
		t.Fatal(e)
	}
	csrBytes, _ := os.ReadFile(csr)
	var issued struct {
		Certificate string                    `json:"certificate"`
		Record      model.PersonalCertificate `json:"record"`
	}
	if e = json.Unmarshal(accessCall(t, admin, "POST", server.URL+"/v1/users/recovery-user/certificates", map[string]any{"csr": string(csrBytes), "days": 1, "version": 0}, 201), &issued); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(crt, []byte(issued.Certificate), 0600); e != nil {
		t.Fatal(e)
	}
	personal := personalClient(t, cfg["admin"], user.Username, key, crt)
	job := model.Job{ID: model.ID(), Name: "scheduled", Enabled: true, NextRun: time.Now().Add(time.Hour), IntervalSeconds: 3600, CreatedAt: time.Now()}
	if e = m.DB.PutJob(ctx, job); e != nil {
		t.Fatal(e)
	}
	run := model.Run{ID: model.ID(), JobID: job.ID, Job: job, Kind: "backup", Status: "queued", CreatedAt: time.Now()}
	if _, e = m.DB.Enqueue(ctx, run, model.ID()); e != nil {
		t.Fatal(e)
	}
	accessCall(t, auditor, "GET", server.URL+"/v1/catalog-backups", nil, 403)
	accessCall(t, personal, "POST", server.URL+"/v1/catalog-backups", nil, 403)
	var entry catalogbackup.Entry
	if e = json.Unmarshal(accessCall(t, admin, "POST", server.URL+"/v1/catalog-backups", nil, 202), &entry); e != nil {
		t.Fatal(e)
	}
	eventually(t, func() bool { list := m.CatalogBackups.List(); return len(list) > 0 && list[0].State != "running" })
	entry = m.CatalogBackups.List()[0]
	if entry.State != "success" {
		t.Fatal(entry)
	}
	raw := accessCall(t, admin, "GET", server.URL+"/v1/catalog-backups/"+entry.ID+"/download", nil, 200)
	if bytes.Contains(raw, hash) || bytes.Contains(raw, []byte("PRIVATE KEY")) {
		t.Fatal("unencrypted credentials")
	}
	accessCall(t, admin, "POST", server.URL+"/v1/catalog-backups/"+entry.ID+"/verify", nil, 200)
	accessCall(t, auditor, "GET", server.URL+"/v1/catalog-backups/"+entry.ID+"/download", nil, 403)
	accessCall(t, admin, "PUT", server.URL+"/v1/catalog-backups/policy", catalogbackup.Policy{IntervalSeconds: 86400}, 200)
	accessCall(t, admin, "PUT", server.URL+"/v1/catalog-backups/policy", catalogbackup.Policy{IntervalSeconds: 60}, 409)
	recoveryKey, _ := catalogbackup.ReadKey(keyFile)
	// Update a later table after the snapshot has begun. Export must retain its earlier value.
	var snapshot bytes.Buffer
	var once sync.Once
	_, e = catalogbackup.Export(ctx, m.DB, m.Config, recoveryKey, &snapshot, model.ID(), func(_ string, _, _ int64) {
		once.Do(func() {
			u := user
			u.Name = "After snapshot"
			if _, err := m.DB.PutUser(ctx, u, nil); err != nil {
				t.Error(err)
			}
		})
	})
	if e != nil {
		t.Fatal(e)
	}
	raw = snapshot.Bytes()
	schema := "restore_" + model.ID()
	o := catalogbackup.RestoreOptions{DatabaseURL: m.Config.DatabaseURL, Schema: schema, Installation: mc.Installation, Directory: filepath.Join(root, "restored")}
	if _, e = catalogbackup.Restore(ctx, bytes.NewReader(raw), recoveryKey, o); e == nil || !strings.Contains(e.Error(), "active manager") {
		t.Fatalf("restore ignored live manager: %v", e)
	}
	// Simulate complete manager loss; the restore command needs only the archive, key and new DB URL.
	server.Close()
	m.CatalogBackups.Close()
	m.DB.Close()
	conn, e := pgx.Connect(ctx, m.Config.DatabaseURL)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	t.Cleanup(func() {
		c, e := pgx.Connect(context.Background(), m.Config.DatabaseURL)
		if e == nil {
			defer c.Close(context.Background())
			_, _ = c.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		}
	})
	bad := append([]byte{}, raw...)
	bad[len(bad)-1] ^= 1
	if _, e = catalogbackup.Restore(ctx, bytes.NewReader(bad), recoveryKey, o); e == nil {
		t.Fatal("corrupt restore accepted")
	}
	var exists bool
	if e = conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)", schema).Scan(&exists); e != nil || exists {
		t.Fatal("failed restore left a schema", exists, e)
	}
	if _, e = os.Stat(o.Directory); !os.IsNotExist(e) {
		t.Fatal("failed restore left recovery material")
	}
	wrong := o
	wrong.Installation = "foreign"
	if _, e = catalogbackup.Restore(ctx, bytes.NewReader(raw), recoveryKey, wrong); e == nil {
		t.Fatal("foreign installation accepted")
	}
	result, e := catalogbackup.Restore(ctx, bytes.NewReader(raw), recoveryKey, o)
	if e != nil {
		t.Fatal(e)
	}
	if !result.ReviewRequired {
		t.Fatal("recovery not gated")
	}
	if _, e = catalogbackup.Restore(ctx, bytes.NewReader(raw), recoveryKey, o); e == nil {
		t.Fatal("existing schema overwritten")
	}
	restoredCfg, e := config.Load(result.ConfigFile)
	if e != nil {
		t.Fatal(e)
	}
	if restoredCfg.RetentionIntervalSeconds != 0 || restoredCfg.CatalogBackup != nil {
		t.Fatal("unsafe automatic recovery settings")
	}
	recovered, e := NewManager(ctx, restoredCfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(recovered.DB.Close)
	rs := tlsServer(t, restoredCfg, recovered.Handler())
	u, gotHash, e := recovered.DB.User(ctx, user.Username)
	if e != nil || u.Name != "Before snapshot" || u.RestoreSiteIDs[0] != "remote" || !bytes.Equal(gotHash, hash) {
		t.Fatal("permissions/password/snapshot mismatch", e, u)
	}
	certs, e := recovered.DB.CertificateState(ctx, user.Username)
	if e != nil || certs.Version != 1 || len(certs.Certificates) != 1 || certs.Certificates[0].Fingerprint != issued.Record.Fingerprint {
		t.Fatal("certificate registry lost", e)
	}
	j, e := recovered.DB.Job(ctx, job.ID)
	if e != nil || j.Enabled {
		t.Fatal("schedule restarted", e)
	}
	r, e := recovered.DB.Run(ctx, run.ID)
	if e != nil || r.Status != "error" {
		t.Fatal("old queue replayed", e)
	}
	accessCall(t, personal, "GET", rs.URL+"/v1/me", nil, 401)
	accessCall(t, admin, "POST", rs.URL+"/v1/jobs/"+job.ID+"/run", nil, 403)
	// Reconcile certificate revocations since backup before removing the recovery gate.
	accessCall(t, admin, "POST", rs.URL+"/v1/users/recovery-user/certificates/revoke", map[string]any{"serial": issued.Record.Serial, "version": 1}, 200)
	accessCall(t, admin, "POST", rs.URL+"/v1/catalog-backups/recovery/acknowledge", map[string]string{"archive_id": result.Manifest.ID, "confirm_installation": "wrong"}, 400)
	accessCall(t, admin, "POST", rs.URL+"/v1/catalog-backups/recovery/acknowledge", map[string]string{"archive_id": result.Manifest.ID, "confirm_installation": mc.Installation}, 200)
	accessCall(t, personal, "GET", rs.URL+"/v1/me", nil, 401)
	state, e := recovered.DB.RecoveryState(ctx)
	if e != nil || state.Required || len(state.PausedJobs) != 1 || state.PausedJobs[0] != job.ID {
		t.Fatal("acknowledgement not persisted")
	}
	if _, e = recovered.DB.Jobs(ctx); e != nil {
		t.Fatal(e)
	}
}

func TestCatalogBackupScheduleAndRestart(t *testing.T) {
	cfg := configs(t)
	root := t.TempDir()
	key := filepath.Join(root, "recovery.key")
	if e := catalogbackup.GenerateKey(key); e != nil {
		t.Fatal(e)
	}
	mc := cfg["manager"]
	mc.CatalogBackup = &config.CatalogBackupConfig{Directory: filepath.Join(root, "backups"), KeyFile: key}
	m := managerTest(t, mc)
	t.Cleanup(m.CatalogBackups.Close)
	if e := m.CatalogBackups.SetPolicy(catalogbackup.Policy{IntervalSeconds: 60}); e != nil {
		t.Fatal(e)
	}
	m.CatalogBackups.Run()
	eventually(t, func() bool { v := m.CatalogBackups.List(); return len(v) == 1 && v[0].State == "success" })
	id := m.CatalogBackups.List()[0].ID
	m.CatalogBackups.Close()
	reopened, e := catalogbackup.OpenStore(context.Background(), m.DB, m.Config)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	if reopened.Policy().Version != 1 || reopened.Policy().IntervalSeconds != 60 || len(reopened.List()) != 1 || reopened.List()[0].ID != id {
		t.Fatal("schedule or metadata lost across restart")
	}
	archive := filepath.Join(mc.CatalogBackup.Directory, id+".srkcat")
	raw, e := os.ReadFile(archive)
	if e != nil {
		t.Fatal(e)
	}
	raw[len(raw)-1] ^= 1
	if e = os.WriteFile(archive, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = reopened.Verify(context.Background(), id); e == nil || reopened.List()[0].State != "corrupt" {
		t.Fatal("damaged archive reported as healthy")
	}
	if f, e := reopened.Archive(id); e == nil {
		f.Close()
		t.Fatal("corrupt archive offered for download")
	}
}
