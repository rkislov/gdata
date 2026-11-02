// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
)

func configs(t *testing.T) map[string]config.Config {
	t.Helper()
	root := t.TempDir()
	pki := filepath.Join(root, "pki")
	if e := identity.InitCA(pki, "test"); e != nil {
		t.Fatal(e)
	}
	rev := filepath.Join(root, "revoked.json")
	if e := os.WriteFile(rev, []byte("[]"), 0600); e != nil {
		t.Fatal(e)
	}
	key := filepath.Join(root, "cap.key")
	if e := os.WriteFile(key, bytes.Repeat([]byte("k"), 32), 0600); e != nil {
		t.Fatal(e)
	}
	out := map[string]config.Config{}
	for _, role := range []string{"manager", "agent", "media", "admin", "auditor", "proxy"} {
		if e := identity.Issue(pki, "test", role, role+"-test"); e != nil {
			t.Fatal(e)
		}
		out[role] = config.Config{Role: role, ID: role + "-test", Installation: "test", CA: filepath.Join(pki, "ca.crt"), Cert: filepath.Join(pki, role+"-test.crt"), Key: filepath.Join(pki, role+"-test.key"), Revoked: rev, CapabilityKeyFile: key, Repository: filepath.Join(root, "repository"), MaxParallel: 2, PerAgent: 2, PerMedia: 2, Development: true}
	}
	return out
}
func tlsServer(t *testing.T, c config.Config, h http.Handler) *httptest.Server {
	t.Helper()
	s := httptest.NewUnstartedServer(Secure(c, h))
	tc, e := identity.Config(c, true)
	if e != nil {
		t.Fatal(e)
	}
	s.TLS = tc
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}
func TestAgentRoundTripDedupIncrementAndConfinement(t *testing.T) {
	cfg := configs(t)
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { media.Repo.Close() })
	server := tlsServer(t, cfg["media"], media.Handler())
	source := t.TempDir()
	restore := t.TempDir()
	data := bytes.Repeat([]byte("0123456789abcdef"), 300000)
	if e = os.WriteFile(filepath.Join(source, "данные.bin"), data, 0640); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(source, "empty"), nil, 0644); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(filepath.Join(source, "dir"), 0750); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink("../данные.bin", filepath.Join(source, "dir", "link")); e != nil {
		t.Fatal(e)
	}
	ac := cfg["agent"]
	ac.AllowedRoots = []string{source}
	ac.RestoreRoots = []string{restore}
	a, e := NewAgent(ac)
	if e != nil {
		t.Fatal(e)
	}
	key := bytes.Repeat([]byte("k"), 32)
	j := model.Job{ID: model.ID(), Name: "round-trip", AgentID: ac.ID, MediaID: cfg["media"].ID, Source: source, Mode: "incremental", RetentionDays: 30}
	execution := func(base string) model.Execute {
		v := model.Execute{RunID: model.ID(), Kind: "backup", Job: j, MediaURL: server.URL, Base: base}
		v.Grant = identity.Sign(identity.Grant{Subject: ac.ID, RunID: v.RunID, Base: base, Permission: "write", Expires: time.Now().Add(time.Hour).Unix()}, key)
		return v
	}
	first := execution("")
	res, e := a.Backup(context.Background(), first)
	if e != nil {
		t.Fatal(e)
	}
	if !res.Verified || res.UploadedBytes != int64(len(data)) || res.EffectiveMode != "full" {
		t.Fatalf("bad first result: %+v", res)
	}
	if res.ProgressPercent == nil || *res.ProgressPercent != 100 || res.TotalBytes != int64(len(data)) {
		t.Fatalf("completed backup progress or size estimate incorrect: %+v", res)
	}
	second := execution(first.RunID)
	res, e = a.Backup(context.Background(), second)
	if e != nil {
		t.Fatal(e)
	}
	if res.UploadedBytes != 0 || res.ChangedFiles != 0 || res.EffectiveMode != "incremental" {
		t.Fatalf("unchanged increment: %+v", res)
	}
	if e = os.Remove(filepath.Join(source, "empty")); e != nil {
		t.Fatal(e)
	}
	third := execution(second.RunID)
	res, e = a.Backup(context.Background(), third)
	if e != nil {
		t.Fatal(e)
	}
	if res.ChangedFiles < 1 {
		t.Fatal("deletion not detected")
	}
	dest := filepath.Join(restore, "recovered")
	v := model.Execute{RunID: model.ID(), Kind: "restore", Job: j, MediaURL: server.URL, SnapshotID: third.RunID, Destination: dest}
	v.Grant = identity.Sign(identity.Grant{Subject: ac.ID, RunID: v.RunID, SnapshotID: third.RunID, Permission: "read", Expires: time.Now().Add(time.Hour).Unix()}, key)
	if _, e = a.Restore(context.Background(), v); e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(filepath.Join(dest, "данные.bin"))
	if e != nil || !bytes.Equal(got, data) {
		t.Fatal("restored data differs")
	}
	info, e := os.Stat(filepath.Join(dest, "данные.bin"))
	if e != nil || info.Mode().Perm() != 0640 {
		t.Fatal("restored permission bits differ")
	}
	link, e := os.Readlink(filepath.Join(dest, "dir", "link"))
	if e != nil || link != "../данные.bin" {
		t.Fatal("internal symlink differs")
	}
	if _, e = os.Stat(filepath.Join(dest, "empty")); !os.IsNotExist(e) {
		t.Fatal("deleted file restored")
	}
	if _, e = a.Restore(context.Background(), v); e == nil {
		t.Fatal("existing destination overwritten")
	}
	v.Destination = t.TempDir()
	if _, e = a.Restore(context.Background(), v); e == nil {
		t.Fatal("restore escaped allowlist")
	}
	req, _ := http.NewRequest("GET", server.URL+"/v1/chunks/"+strings.Repeat("0", 64), nil)
	req.Header.Set("Authorization", "Bearer "+v.Grant)
	resp, e := a.Client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("ungranted chunk status %d", resp.StatusCode)
	}
}
func TestMTLSRejectsMissingWrongRoleAndRevokedCertificates(t *testing.T) {
	cfg := configs(t)
	m, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { m.Repo.Close() })
	s := tlsServer(t, cfg["media"], m.Handler())
	noCertTLS, e := identity.Config(cfg["admin"], false)
	if e != nil {
		t.Fatal(e)
	}
	noCertTLS.Certificates = nil
	noCert := &http.Client{Transport: &http.Transport{TLSClientConfig: noCertTLS}, Timeout: time.Second}
	if resp, e := noCert.Get(s.URL + "/v1/status"); e == nil {
		resp.Body.Close()
		t.Fatal("client without certificate accepted")
	}
	cl, e := identity.Client(cfg["admin"], identity.Peer{Role: "media", ID: cfg["media"].ID})
	if e != nil {
		t.Fatal(e)
	}
	if e = Call(context.Background(), cl, "GET", s.URL+"/v1/status", nil, new(any)); e != nil {
		t.Fatal(e)
	}
	wrong, e := identity.Client(cfg["admin"], identity.Peer{Role: "agent"})
	if e != nil {
		t.Fatal(e)
	}
	if e = Call(context.Background(), wrong, "GET", s.URL+"/v1/status", nil, new(any)); e == nil {
		t.Fatal("wrong server role accepted")
	}
	pair, e := tls.LoadX509KeyPair(cfg["admin"].Cert, cfg["admin"].Key)
	if e != nil {
		t.Fatal(e)
	}
	leaf, e := x509.ParseCertificate(pair.Certificate[0])
	if e != nil {
		t.Fatal(e)
	}
	rev, _ := json.Marshal([]string{leaf.SerialNumber.String()})
	if e = os.WriteFile(cfg["media"].Revoked, rev, 0600); e != nil {
		t.Fatal(e)
	}
	// Reuse the established connection: request middleware must recheck revocation.
	if e = Call(context.Background(), cl, "GET", s.URL+"/v1/status", nil, new(any)); e == nil {
		t.Fatal("revoked established session accepted")
	}
}
func TestResolveAllowedRejectsSibling(t *testing.T) {
	root := t.TempDir()
	if _, _, e := ResolveAllowed(root+"-other", []string{root}); e == nil {
		t.Fatal("sibling prefix accepted")
	}
	if _, _, e := ResolveAllowed("relative", []string{root}); e == nil {
		t.Fatal("relative path accepted")
	}
	if _, _, e := ResolveAllowed(filepath.Join(root, "child"), []string{root}); e != nil {
		t.Fatal(e)
	}
}
