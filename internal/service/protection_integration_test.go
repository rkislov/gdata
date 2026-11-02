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
	"gdata/srk/internal/protection"
	"gdata/srk/internal/testcloud"
	"golang.org/x/crypto/bcrypt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProtectionLiveInventoryReplicationAndRestore(t *testing.T) {
	e := newVMEnvironment(t)
	target, _ := addReplicaMedia(t, e)
	backup := e.backup(t, testcloud.ImageVM)
	if backup.Status != "success" {
		t.Fatal(backup.Error)
	}
	var replicated model.Run
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+backup.ID+"/replicate", map[string]any{"target_media_id": target.Config.ID, "retention_days": 30}, &replicated); err != nil {
		t.Fatal(err)
	}
	if done := e.wait(t, replicated.ID); done.Status != "success" {
		t.Fatal(done.Error)
	}
	var restore model.Run
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+backup.ID+"/restore", map[string]any{"vm_restore": model.VMRestoreRequest{Name: "protection-test", Profile: "lab", FlavorID: "small", Networks: []string{testcloud.Network}}}, &restore); err != nil {
		t.Fatal(err)
	}
	if done := e.wait(t, restore.ID); done.Status != "success" {
		t.Fatal(done.Error)
	}
	settings := model.ProtectionSettings{Sites: []model.ProtectionSite{{ID: "main", Name: "Main", DisasterDomain: "main"}, {ID: "remote", Name: "Remote", DisasterDomain: "remote"}}, Storages: []model.ProtectionStorage{
		{ID: "primary", Name: "Primary", Kind: "repository", MediumType: "disk", SiteID: "main", FailureDomain: "array-main", MediaID: e.media.Config.ID, RepositoryID: e.media.Repo.Info.ID},
		{ID: "replica", Name: "Replica", Kind: "repository", MediumType: "flash", SiteID: "remote", FailureDomain: "array-remote", MediaID: target.Config.ID, RepositoryID: target.Repo.Info.ID},
	}, Policies: []model.ProtectionPolicy{{JobID: backup.JobID, SourceSiteID: "main", SourceFailureDomain: "vm-storage", SourceMediumType: "disk", RPOSeconds: 3600, RestoreMaxAgeSeconds: 86400}}}
	stale := settings
	if err := Call(context.Background(), e.admin, "PUT", e.url+"/v1/protection/settings", settings, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Version != 1 {
		t.Fatal(settings)
	}
	if err := Call(context.Background(), e.admin, "PUT", e.url+"/v1/protection/settings", stale, nil); err == nil {
		t.Fatal("stale settings overwritten")
	}
	var report struct {
		Cards []protection.Card `json:"cards"`
	}
	if err := Call(context.Background(), e.admin, "GET", e.url+"/v1/protection", nil, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Cards) != 1 || report.Cards[0].State != "protected" || report.Cards[0].BackupCopies != 2 {
		t.Fatal("independent copies not recognized", report)
	}
	// Scoped users inherit replicas through the original job, but only on visible sites.
	user := model.User{Username: "protection-reader", Role: "auditor", AllSites: true, JobIDs: []string{backup.JobID}}
	hash, _ := bcrypt.GenerateFromPassword([]byte("protection-test-password"), bcrypt.MinCost)
	user, err := e.manager.DB.PutUser(context.Background(), user, hash)
	if err != nil {
		t.Fatal(err)
	}
	personal := e.manager.Config
	dir := filepath.Dir(personal.Cert)
	if err = identity.Issue(dir, personal.Installation, "user", user.Username); err != nil {
		t.Fatal(err)
	}
	personal.Role = "user"
	personal.ID = user.Username
	personal.Cert = filepath.Join(dir, user.Username+".crt")
	personal.Key = filepath.Join(dir, user.Username+".key")
	client, err := identity.Client(personal)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	raw := accessCall(t, client, "GET", e.url+"/v1/protection", nil, 200)
	if err = json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Cards) != 1 || report.Cards[0].BackupCopies != 2 || !report.Cards[0].Remote {
		t.Fatalf("owned replica missing: %s", raw)
	}
	user.AllSites = false
	user.SiteIDs = []string{"main"}
	if _, err = e.manager.DB.PutUser(context.Background(), user, nil); err != nil {
		t.Fatal(err)
	}
	raw = accessCall(t, client, "GET", e.url+"/v1/protection", nil, 200)
	if bytes.Contains(raw, []byte(target.Config.ID)) || bytes.Contains(raw, []byte(target.Repo.Info.ID)) || bytes.Contains(raw, []byte(replicated.ID)) {
		t.Fatalf("hidden replica leaked: %s", raw)
	}
	if err = json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Cards) != 1 || report.Cards[0].BackupCopies != 1 || report.Cards[0].Remote {
		t.Fatalf("hidden replica counted: %s", raw)
	}
	// Expiration is read from actual media inventory even before manager retention synchronizes.
	expired := time.Now().Add(-time.Nanosecond)
	if _, err := target.Repo.UpdatePolicy(replicated.ID, model.SnapshotPolicy{ExpiresAt: &expired, Reason: "integration test"}); err != nil {
		t.Fatal(err)
	}
	if err := Call(context.Background(), e.admin, "GET", e.url+"/v1/protection", nil, &report); err != nil {
		t.Fatal(err)
	}
	if report.Cards[0].State == "protected" || report.Cards[0].Remote {
		t.Fatal("expired remote copy counted", report)
	}
	if marker := os.Getenv("SRK_PROTECTION_UI_MARKER"); marker != "" {
		future := time.Now().Add(24 * time.Hour)
		if _, err := target.Repo.UpdatePolicy(replicated.ID, model.SnapshotPolicy{ExpiresAt: &future, Reason: "UI fixture"}); err != nil {
			t.Fatal(err)
		}
		cert, err := tls.LoadX509KeyPair(os.Getenv("SRK_PROTECTION_UI_CERT"), os.Getenv("SRK_PROTECTION_UI_KEY"))
		if err != nil {
			t.Fatal(err)
		}
		ui := httptest.NewUnstartedServer(e.manager.Handler())
		ui.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
		ui.StartTLS()
		defer ui.Close()
		if err = os.WriteFile(marker, []byte(ui.URL), 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(marker)
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(marker); os.IsNotExist(err) {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

}
