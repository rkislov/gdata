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
	"golang.org/x/crypto/bcrypt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSharedStorageScopes(t *testing.T) {
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	ctx := context.Background()
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	defer media.Repo.Close()
	defer media.Captures.Close()
	defer media.CaptureClient.CloseIdleConnections()
	ms := tlsServer(t, cfg["media"], media.Handler())
	srv := tlsServer(t, cfg["manager"], m.Handler())
	if e = m.DB.PutNode(ctx, model.Node{ID: media.Config.ID, Role: "media", URL: ms.URL, LastSeen: time.Now(), Status: map[string]any{"secret_metric": 999}}); e != nil {
		t.Fatal(e)
	}
	own := seedPoint(t, media, false)
	other := seedPoint(t, media, false)
	unknown := seedPoint(t, media, false)
	jobs := []model.Job{{ID: own.JobID, Name: "allowed-job", Source: "/allowed", MediaID: media.Config.ID}, {ID: other.JobID, Name: "SECRET-JOB", Source: "/SECRET-PATH", MediaID: media.Config.ID}}
	for i, j := range jobs {
		if e = m.DB.PutJob(ctx, j); e != nil {
			t.Fatal(e)
		}
		pt := []model.Manifest{own, other}[i]
		res := pt.Result
		res.RepositoryID = media.Repo.Info.ID
		res.Verified = true
		_, e = m.DB.Enqueue(ctx, model.Run{ID: pt.ID, JobID: j.ID, Job: j, SnapshotID: pt.ID, Kind: "backup", Status: "success", CreatedAt: pt.CreatedAt, Result: &res}, model.ID())
		if e != nil {
			t.Fatal(e)
		}
	}
	p := model.ProtectionSettings{Sites: []model.ProtectionSite{{ID: "east", Name: "East", DisasterDomain: "east"}, {ID: "secret-site", Name: "SECRET-SITE", DisasterDomain: "secret-site"}}, Storages: []model.ProtectionStorage{{ID: "shared", Name: "Shared", Kind: "repository", MediumType: "disk", SiteID: "east", FailureDomain: "shared", MediaID: media.Config.ID, RepositoryID: media.Repo.Info.ID}, {ID: "secret-store", Name: "SECRET-STORE", Kind: "longhorn", MediumType: "cloud", SiteID: "secret-site", FailureDomain: "secret-store"}}, Policies: []model.ProtectionPolicy{{JobID: own.JobID, SourceSiteID: "east", SourceMediumType: "disk", SourceFailureDomain: "source", RPOSeconds: 86400, RestoreMaxAgeSeconds: 86400, LonghornStorageID: "secret-store"}, {JobID: other.JobID, SourceSiteID: "secret-site", SourceMediumType: "disk", SourceFailureDomain: "secret-source", RPOSeconds: 86400, RestoreMaxAgeSeconds: 86400}}}
	if _, e = m.DB.PutProtectionSettings(ctx, p); e != nil {
		t.Fatal(e)
	}
	createClient := func(name, role string, all bool) *http.Client {
		u := model.User{Username: name, Role: role, AllJobs: all, AllSites: all, JobIDs: []string{own.JobID}, SiteIDs: []string{"east"}, Permissions: []string{"erase", "cleanup", "verify", "restore"}}
		hash, _ := bcrypt.GenerateFromPassword([]byte("scope-fixture-password"), bcrypt.MinCost)
		if _, err := m.DB.PutUser(ctx, u, hash); err != nil {
			t.Fatal(err)
		}
		c := cfg["admin"]
		dir := filepath.Dir(c.Cert)
		if err := identity.Issue(dir, "test", "user", name); err != nil {
			t.Fatal(err)
		}
		c.Role = "user"
		c.ID = name
		c.Cert = filepath.Join(dir, name+".crt")
		c.Key = filepath.Join(dir, name+".key")
		if path := os.Getenv("SRK_STORAGE_UI_CLIENT"); path != "" && name == "scope-operator" {
			c.URL = srv.URL
			b, _ := json.Marshal(c)
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
		}
		cl, err := identity.Client(c)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cl.CloseIdleConnections)
		return cl
	}
	admin := createClient("scope-admin", "admin", false)
	for _, role := range []string{"operator", "auditor"} {
		cl := createClient("scope-"+role, role, false)
		for _, path := range []string{"/v1/storage-view", "/v1/protection", "/v1/protection/settings", "/v1/media/" + media.Config.ID + "/snapshots?verify=true"} {
			raw := accessCall(t, cl, "GET", srv.URL+path, nil, 200)
			for _, secret := range []string{other.ID, other.JobID, unknown.ID, unknown.JobID, "SECRET", "secret-site", "secret-store", "secret_metric", "/lost/original"} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Fatalf("%s leaks %q: %s", path, secret, raw)
				}
			}
			if !bytes.Contains(raw, []byte(own.JobID)) {
				t.Fatalf("missing own job: %s", raw)
			}
			if path == "/v1/protection" {
				var d struct {
					Cards        []protection.Card        `json:"cards"`
					Settings     model.ProtectionSettings `json:"settings"`
					Observations []struct {
						Count int `json:"snapshot_count"`
					} `json:"observations"`
				}
				if e = json.Unmarshal(raw, &d); e != nil {
					t.Fatal(e)
				}
				if len(d.Cards) != 1 || len(d.Cards[0].Copies) != 1 || d.Cards[0].Remote || d.Settings.Version != 0 || d.Observations[0].Count != 1 {
					t.Fatalf("bad scoped aggregates: %s", raw)
				}
			}
		}
		for _, path := range []string{"/v1/media/" + media.Config.ID + "/gc", "/v1/media/" + media.Config.ID + "/tape", "/v1/media/" + media.Config.ID + "/removable", "/v1/media/missing/snapshots"} {
			accessCall(t, cl, "GET", srv.URL+path, nil, 403)
		}
		for _, path := range []string{"/v1/media/" + media.Config.ID + "/gc", "/v1/media/" + media.Config.ID + "/tape/operations"} {
			accessCall(t, cl, "POST", srv.URL+path, map[string]string{"action": "erase"}, 403)
		}
		accessCall(t, cl, "PUT", srv.URL+"/v1/protection/settings", p, 403)
	}
	full := createClient("full-auditor", "auditor", true)
	for _, cl := range []*http.Client{admin, full} {
		raw := accessCall(t, cl, "GET", srv.URL+"/v1/media/"+media.Config.ID+"/snapshots", nil, 200)
		if !bytes.Contains(raw, []byte(unknown.ID)) || !bytes.Contains(raw, []byte(other.ID)) {
			t.Fatal("global inventory lost")
		}
	}
	accessCall(t, full, "PUT", srv.URL+"/v1/protection/settings", p, 403)
	// A replica of a replica remains owned through a hidden intermediate site.
	midJob := model.Job{ID: model.ID(), Name: "SECRET-intermediate", MediaID: "SECRET-media"}
	mid := model.Run{ID: model.ID(), JobID: midJob.ID, Job: midJob, Kind: "backup", Status: "success", CreatedAt: time.Now(), Origin: &model.Origin{OriginalJobID: own.JobID}}
	if _, e = m.DB.Enqueue(ctx, mid, model.ID()); e != nil {
		t.Fatal(e)
	}
	finalPoint := seedPoint(t, media, false)
	finalJob := model.Job{ID: finalPoint.JobID, Name: "replica", MediaID: media.Config.ID}
	res := finalPoint.Result
	res.Verified = true
	res.RepositoryID = media.Repo.Info.ID
	res.Replica = &model.Replica{SourceRunID: mid.ID, CapturedAt: own.CreatedAt}
	finalRun := model.Run{ID: finalPoint.ID, JobID: finalJob.ID, Job: finalJob, Kind: "backup", Status: "success", SnapshotID: finalPoint.ID, CreatedAt: time.Now(), Result: &res, Origin: &model.Origin{OriginalJobID: mid.JobID}}
	if _, e = m.DB.Enqueue(ctx, finalRun, model.ID()); e != nil {
		t.Fatal(e)
	}
	reader := createClient("chain-reader", "auditor", false)
	chainRaw := accessCall(t, reader, "GET", srv.URL+"/v1/media/"+media.Config.ID+"/snapshots", nil, 200)
	if !bytes.Contains(chainRaw, []byte(finalPoint.ID)) || bytes.Contains(chainRaw, []byte("SECRET")) || bytes.Contains(chainRaw, []byte(mid.ID)) {
		t.Fatalf("replica lineage ownership failed: %s", chainRaw)
	}
	if marker := os.Getenv("SRK_STORAGE_UI_MARKER"); marker != "" {
		cert, err := tls.LoadX509KeyPair(os.Getenv("SRK_STORAGE_UI_CERT"), os.Getenv("SRK_STORAGE_UI_KEY"))
		if err != nil {
			t.Fatal(err)
		}
		ui := httptest.NewUnstartedServer(m.Handler())
		ui.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
		ui.StartTLS()
		defer ui.Close()
		if err = os.WriteFile(marker, []byte(ui.URL), 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(marker)
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			if _, err = os.Stat(marker); os.IsNotExist(err) {
				break
			}
			_ = m.DB.PutNode(ctx, model.Node{ID: media.Config.ID, Role: "media", URL: ms.URL, LastSeen: time.Now()})
			time.Sleep(time.Second)
		}
	}
	// A user with no effective intersection gets empty data, never global fallback.
	u, _, _ := m.DB.User(ctx, "scope-auditor")
	u.SiteIDs = []string{"no-site"}
	if _, e = m.DB.PutUser(ctx, u, nil); e != nil {
		t.Fatal(e)
	}
	// Existing certificate must immediately use the updated rights.
	c := cfg["admin"]
	c.Role = "user"
	c.ID = u.Username
	c.Cert = filepath.Join(filepath.Dir(c.Cert), u.Username+".crt")
	c.Key = filepath.Join(filepath.Dir(c.Key), u.Username+".key")
	cl, e := identity.Client(c)
	if e != nil {
		t.Fatal(e)
	}
	defer cl.CloseIdleConnections()
	raw := accessCall(t, cl, "GET", srv.URL+"/v1/storage-view", nil, 200)
	if bytes.Contains(raw, []byte(own.ID)) || bytes.Contains(raw, []byte("Shared")) {
		t.Fatal("empty scope leaked data", string(raw))
	}
	accessCall(t, cl, "GET", srv.URL+"/v1/media/"+media.Config.ID+"/snapshots", nil, 403)
}

func TestScopedMixedMediaAndLonghorn(t *testing.T) {
	now := time.Now()
	job := model.Job{ID: "owned", Name: "Owned"}
	u := model.User{Role: "operator", JobIDs: []string{"owned"}, SiteIDs: []string{"main"}}
	settings := model.ProtectionSettings{Version: 19, Sites: []model.ProtectionSite{{ID: "main"}, {ID: "SECRET"}}, Storages: []model.ProtectionStorage{{ID: "tape", Kind: "tape", SiteID: "main"}, {ID: "flash", Kind: "flash", SiteID: "main"}, {ID: "SECRET", Kind: "longhorn", SiteID: "SECRET"}}, Policies: []model.ProtectionPolicy{{JobID: "owned", SourceSiteID: "main", LonghornStorageID: "SECRET"}}}
	p := scopeProtectionSettings(u, settings, []model.Job{job})
	copies := []protection.Copy{{JobID: "owned", StorageID: "tape", SnapshotID: "one", MediaID: "shared", Available: true, Verified: true, DataAt: now, Longhorn: true, MetadataOnly: true, Namespaces: []string{"app"}}, {JobID: "owned", StorageID: "flash", SnapshotID: "two", MediaID: "shared", Available: true, Verified: true, DataAt: now, Longhorn: true, MetadataOnly: true}, {JobID: "SECRET", StorageID: "tape", SnapshotID: "SECRET-copy"}, {JobID: "owned", StorageID: "SECRET", SnapshotID: "SECRET-point"}}
	checks := []protection.Restore{{JobID: "owned", MediaID: "shared", SnapshotID: "one", Passed: true, ApplicationCheck: true, Namespace: "app", At: now}, {JobID: "SECRET", SnapshotID: "SECRET-copy", Passed: true}}
	p, jobs, cp, rs := projectProtection(u, p, []model.Job{job}, copies, checks)
	cards := protection.Evaluate(now, jobs, p, cp, rs, false)
	raw, _ := json.Marshal(map[string]any{"settings": p, "cards": cards})
	if bytes.Contains(raw, []byte("SECRET")) || len(cp) != 2 || len(rs) != 1 {
		t.Fatalf("mixed media projection failed: %s", raw)
	}
	if cards[0].BackupCopies != 0 || cards[0].State == "protected" {
		t.Fatal("hidden Longhorn dependency counted as independent data")
	}
}
