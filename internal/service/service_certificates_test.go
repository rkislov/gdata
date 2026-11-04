// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"gdata/srk/internal/catalogbackup"
	"gdata/srk/internal/model"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
)

func shortServiceCertificate(t *testing.T, c config.Config, d time.Duration) {
	t.Helper()
	pair, e := tls.LoadX509KeyPair(c.Cert, c.Key)
	if e != nil {
		t.Fatal(e)
	}
	leaf, e := x509.ParseCertificate(pair.Certificate[0])
	if e != nil {
		t.Fatal(e)
	}
	issuer, e := tls.LoadX509KeyPair(c.CA, filepath.Join(filepath.Dir(c.CA), "ca.key"))
	if e != nil {
		t.Fatal(e)
	}
	ca, e := x509.ParseCertificate(issuer.Certificate[0])
	if e != nil {
		t.Fatal(e)
	}
	leaf.NotAfter = time.Now().Add(d)
	der, e := x509.CreateCertificate(rand.Reader, leaf, ca, leaf.PublicKey, issuer.PrivateKey)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(c.Cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); e != nil {
		t.Fatal(e)
	}
}
func rotatingConfig(t *testing.T, c config.Config) config.Config {
	t.Helper()
	c.ServiceCertificates = &config.ServiceCertificateConfig{Directory: t.TempDir(), AutoRenew: true, RenewBeforeHours: 1, LifetimeHours: 4}
	if err := os.Chmod(c.ServiceCertificates.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	shortServiceCertificate(t, c, 30*time.Minute)
	return c
}
func requestSerial(t *testing.T, cl *http.Client, url string) string {
	t.Helper()
	res, e := cl.Get(url)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	return res.TLS.PeerCertificates[0].SerialNumber.String()
}
func TestServiceCertificatesRotateLiveAndRetainStreams(t *testing.T) {
	cfg := configs(t)
	mc := rotatingConfig(t, cfg["manager"])
	mc.ServiceCAKeyFile = filepath.Join(filepath.Dir(mc.CA), "ca.key")
	m := managerTest(t, mc)
	managerServer := tlsServer(t, mc, m.Handler())
	m.Config.URL = managerServer.URL
	ac := rotatingConfig(t, cfg["agent"])
	ac.ManagerURL = managerServer.URL
	started := make(chan struct{})
	finish := make(chan struct{})
	release := sync.OnceFunc(func() { close(finish) })
	defer release()
	agentServer := tlsServer(t, ac, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			w.Write([]byte("before\n"))
			w.(http.Flusher).Flush()
			close(started)
			<-finish
			w.Write([]byte("after\n"))
			return
		}
		JSON(w, 200, map[string]string{"client_serial": r.TLS.PeerCertificates[0].SerialNumber.String()})
	}))
	admin, e := identity.Client(cfg["admin"])
	if e != nil {
		t.Fatal(e)
	}
	defer admin.CloseIdleConnections()
	oldAgent := requestSerial(t, admin, agentServer.URL)
	res, e := admin.Get(agentServer.URL + "/stream")
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	<-started
	oldManager := requestSerial(t, admin, managerServer.URL+"/v1/status")
	var clientBefore map[string]string
	if e = Call(context.Background(), m.Client, "GET", agentServer.URL, nil, &clientBefore); e != nil {
		t.Fatal(e)
	}
	// Workers rotate automatically while the agent's streaming response is held open.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); certificateLifecycle(ctx, ac) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(8 * time.Second)
	for identity.CertificateInfo(ac).Serial == oldAgent && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if identity.CertificateInfo(ac).Serial == oldAgent {
		t.Fatal("automatic rotation did not run")
	}
	admin.CloseIdleConnections()
	newAgent := requestSerial(t, admin, agentServer.URL)
	if newAgent == oldAgent {
		t.Fatal("running TLS server retained old certificate")
	}
	release()
	body, e := io.ReadAll(res.Body)
	if e != nil || string(body) != "before\nafter\n" {
		t.Fatalf("stream interrupted: %q %v", body, e)
	}
	if e = renewServiceCertificate(context.Background(), m.Config, true); e != nil {
		t.Fatal(e)
	}
	admin.CloseIdleConnections()
	newManager := requestSerial(t, admin, managerServer.URL+"/v1/status")
	if newManager == oldManager {
		t.Fatal("manager server did not rotate")
	}
	var clientAfter map[string]string
	if e = Call(context.Background(), m.Client, "GET", agentServer.URL, nil, &clientAfter); e != nil {
		t.Fatal(e)
	}
	if clientAfter["client_serial"] != newManager || clientBefore["client_serial"] == clientAfter["client_serial"] {
		t.Fatalf("cached client did not rotate: %v -> %v", clientBefore, clientAfter)
	}
	// Restarted clients resolve the same atomically persisted generation.
	restarted, e := identity.Config(ac, true)
	if e != nil {
		t.Fatal(e)
	}
	pair, e := restarted.GetCertificate(nil)
	if e != nil || pair.Leaf.SerialNumber.String() != newAgent {
		t.Fatal("restart lost active generation", e)
	}
}
func TestServiceCertificateRetriesAndAuthorization(t *testing.T) {
	cfg := configs(t)
	mc := cfg["manager"]
	mc.ServiceCAKeyFile = filepath.Join(filepath.Dir(mc.CA), "ca.key")
	m := managerTest(t, mc)
	server := tlsServer(t, mc, m.Handler())
	ac := rotatingConfig(t, cfg["agent"])
	cl, e := identity.Client(ac)
	if e != nil {
		t.Fatal(e)
	}
	defer cl.CloseIdleConnections()
	csr, key, e := identity.PendingCSR(ac)
	if e != nil {
		t.Fatal(e)
	}
	var first struct {
		Certificate string `json:"certificate"`
	}
	var retry struct {
		Certificate string `json:"certificate"`
	}
	in := map[string]any{"csr": csr, "lifetime_hours": 4}
	if e = Call(context.Background(), cl, "POST", server.URL+"/v1/service-certificates/renew", in, &first); e != nil {
		t.Fatal(e)
	}
	// Lost response: old credential + same durable key retrieves byte-identical issuance.
	if e = Call(context.Background(), cl, "POST", server.URL+"/v1/service-certificates/renew", in, &retry); e != nil || retry.Certificate != first.Certificate {
		t.Fatal("retry issued another generation", e)
	}
	other := ac
	other.ServiceCertificates = &config.ServiceCertificateConfig{Directory: t.TempDir()}
	different, _, e := identity.PendingCSR(other)
	if e != nil {
		t.Fatal(e)
	}
	accessCall(t, cl, "POST", server.URL+"/v1/service-certificates/renew", map[string]any{"csr": different, "lifetime_hours": 4}, 409)
	if e = identity.InstallCertificate(ac, first.Certificate, key); e != nil {
		t.Fatal(e)
	}
	// A caller cannot rename its identity or expand SANs through its CSR.
	b, _ := pem.Decode([]byte(first.Certificate))
	leaf, _ := x509.ParseCertificate(b.Bytes)
	peer, e := identity.Parse(leaf)
	if e != nil || peer.ID != ac.ID || peer.Role != ac.Role || leaf.DNSNames[0] != "localhost" {
		t.Fatal("service identity changed")
	}
	for _, role := range []string{"admin", "auditor"} {
		user, e := identity.Client(cfg[role])
		if e != nil {
			t.Fatal(e)
		}
		accessCall(t, user, "POST", server.URL+"/v1/service-certificates/renew", in, 403)
		user.CloseIdleConnections()
	}
	admin, _ := identity.Client(cfg["admin"])
	defer admin.CloseIdleConnections()
	auditor, _ := identity.Client(cfg["auditor"])
	defer auditor.CloseIdleConnections()
	accessCall(t, auditor, "GET", server.URL+"/v1/service-certificates", nil, 403)
	var inventory certificateInventory
	if e = json.Unmarshal(accessCall(t, admin, "GET", server.URL+"/v1/service-certificates", nil, 200), &inventory); e != nil || !inventory.IssuerEnabled {
		t.Fatal("inventory", e)
	}
	if len(inventory.Certificates) < 2 {
		t.Fatal("manager/CA inventory missing")
	}
}
func TestServiceRenewalFailureKeepsCurrentCertificate(t *testing.T) {
	cfg := configs(t)
	ac := rotatingConfig(t, cfg["agent"])
	old := identity.CertificateInfo(ac).Serial
	ac.ManagerURL = "https://127.0.0.1:1"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e := RotateServiceCertificate(ctx, ac); e == nil {
		t.Fatal("unavailable issuer accepted")
	}
	if identity.CertificateInfo(ac).Serial != old {
		t.Fatal("failed renewal replaced active credential")
	}
	if identity.CertificateInfo(ac).RenewalError == "" {
		t.Fatal("failure was not reported")
	}
	csr, key, e := identity.PendingCSR(ac)
	if e != nil {
		t.Fatal(e)
	}
	again, keyAgain, e := identity.PendingCSR(ac)
	if e != nil || !strings.Contains(csr, "CERTIFICATE REQUEST") || !strings.Contains(again, "CERTIFICATE REQUEST") || string(key) != string(keyAgain) {
		t.Fatal("retry lost its pending key")
	}
	issuer := filepath.Join(filepath.Dir(ac.CA), "ca.key")
	certBytes, _, _ := identity.ActivePEM(ac)
	b, _ := pem.Decode(certBytes)
	leaf, _ := x509.ParseCertificate(b.Bytes)
	cert, _, e := identity.SignServiceCSR(ac.CA, issuer, leaf, csr, 4)
	if e != nil {
		t.Fatal(e)
	}
	wrong := ac
	wrong.ID = "someone-else"
	if identity.InstallCertificate(wrong, cert, key) == nil {
		t.Fatal("foreign identity installed")
	}
	if identity.CertificateInfo(ac).Serial != old {
		t.Fatal("invalid installation replaced current credential")
	}
	// Administrative expired credentials do not bypass TLS by invoking the renewal worker.
	shortServiceCertificate(t, ac, -time.Minute)
	if e = RotateServiceCertificate(ctx, ac); e == nil {
		t.Fatal("expired certificate renewed")
	}
}
func TestCertificateAlertThresholdsAndDeduplication(t *testing.T) {
	now := time.Now()
	for _, v := range []struct {
		left time.Duration
		want string
	}{{-time.Second, "expired"}, {time.Hour, "critical"}, {8 * 24 * time.Hour, "expiring"}, {31 * 24 * time.Hour, "healthy"}} {
		if got := certificateState(now.Add(v.left), now); got != v.want {
			t.Fatalf("%s: %s", v.want, got)
		}
	}
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	for i := 0; i < 2; i++ {
		fresh, e := m.DB.NotifyCertificate(context.Background(), "alert-test", "agent/a", "critical")
		if e != nil || fresh != (i == 0) {
			t.Fatal("alert deduplication", fresh, e)
		}
	}
}

func TestHeartbeatCertificateObservationCannotBeForged(t *testing.T) {
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	server := tlsServer(t, cfg["manager"], m.Handler())
	cl, e := identity.Client(cfg["agent"])
	if e != nil {
		t.Fatal(e)
	}
	defer cl.CloseIdleConnections()
	accessCall(t, cl, "POST", server.URL+"/v1/nodes/heartbeat", map[string]any{
		"id": cfg["agent"].ID, "role": "agent", "url": "https://localhost:8445",
		"certificate": map[string]any{"serial": "forged", "expires_at": "2099-01-01T00:00:00Z"},
		"status":      map[string]any{"certificate": map[string]any{"serial": "forged", "expires_at": "2099-01-01T00:00:00Z"}},
	}, 200)
	cert, _, _ := identity.ActivePEM(cfg["agent"])
	block, _ := pem.Decode(cert)
	leaf, _ := x509.ParseCertificate(block.Bytes)
	inventory, e := m.certificateInventory(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, v := range inventory.Certificates {
		if v.Role == "agent" {
			found = true
			if v.Serial != leaf.SerialNumber.String() || !v.ExpiresAt.Equal(leaf.NotAfter) {
				t.Fatal("heartbeat forged authenticated certificate expiry")
			}
		}
	}
	if !found {
		t.Fatal("heartbeat missing")
	}
}

func TestRotatedManagerBackupKeepsMatchingActiveCredentials(t *testing.T) {
	cfg := configs(t)
	mc := rotatingConfig(t, cfg["manager"])
	mc.ServiceCAKeyFile = filepath.Join(filepath.Dir(mc.CA), "ca.key")
	m := managerTest(t, mc)
	server := tlsServer(t, mc, m.Handler())
	m.Config.URL = server.URL
	csr, key, e := identity.PendingCSR(mc)
	if e != nil {
		t.Fatal(e)
	}
	var issued struct {
		Certificate string `json:"certificate"`
	}
	if e = Call(context.Background(), m.Client, "POST", server.URL+"/v1/service-certificates/renew", map[string]any{"csr": csr, "lifetime_hours": 4}, &issued); e != nil {
		t.Fatal(e)
	}
	encryptionKey := bytes.Repeat([]byte("x"), 32)
	var output bytes.Buffer
	if _, e = catalogbackup.Export(context.Background(), m.DB, m.Config, encryptionKey, &output, model.ID(), nil); e == nil {
		t.Fatal("backup accepted registry generation that was not installed")
	}
	if e = identity.InstallCertificate(mc, issued.Certificate, key); e != nil {
		t.Fatal(e)
	}
	newSerial := identity.CertificateInfo(mc).Serial
	output.Reset()
	if _, e = catalogbackup.Export(context.Background(), m.DB, m.Config, encryptionKey, &output, model.ID(), nil); e != nil {
		t.Fatal(e)
	}
	m.DB.Close()
	schema := "recovered_" + model.ID()
	root := t.TempDir()
	dir := filepath.Join(root, "restored")
	result, e := catalogbackup.Restore(context.Background(), bytes.NewReader(output.Bytes()), encryptionKey, catalogbackup.RestoreOptions{DatabaseURL: os.Getenv("SRK_TEST_DATABASE_URL"), Schema: schema, Installation: "test", Directory: dir})
	if e != nil {
		t.Fatal(e)
	}
	conn, e := pgx.Connect(context.Background(), os.Getenv("SRK_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(context.Background())
	defer conn.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	recovered, e := config.Load(result.ConfigFile)
	if e != nil {
		t.Fatal(e)
	}
	if recovered.ServiceCertificates != nil || recovered.ServiceCAKeyFile == "" {
		t.Fatal("recovery did not pause rotation or restore issuer")
	}
	if got := identity.CertificateInfo(recovered).Serial; got != newSerial {
		t.Fatalf("backup restored bootstrap credential: got %s want %s", got, newSerial)
	}
	if _, e = identity.Config(recovered, true); e != nil {
		t.Fatal(e)
	}
}

func TestServiceCertificateAdministrativeRecovery(t *testing.T) {
	cfg := configs(t)
	mc := cfg["manager"]
	mc.ServiceCAKeyFile = filepath.Join(filepath.Dir(mc.CA), "ca.key")
	m := managerTest(t, mc)
	server := tlsServer(t, mc, m.Handler())
	ac := rotatingConfig(t, cfg["agent"])
	original := ac
	original.ServiceCertificates = nil
	oldClient, e := identity.Client(original)
	if e != nil {
		t.Fatal(e)
	}
	defer oldClient.CloseIdleConnections()
	csr, _, e := identity.PendingCSR(ac)
	if e != nil {
		t.Fatal(e)
	}
	var issued struct {
		Certificate string `json:"certificate"`
	}
	if e = Call(context.Background(), oldClient, "POST", server.URL+"/v1/service-certificates/renew", map[string]any{"csr": csr, "lifetime_hours": 4}, &issued); e != nil {
		t.Fatal(e)
	}
	// Lose the pending key and recover with an independently issued offline pair.
	recovery := ac
	recovery.ServiceCertificates = &config.ServiceCertificateConfig{Directory: t.TempDir()}
	freshCSR, freshKey, e := identity.PendingCSR(recovery)
	if e != nil {
		t.Fatal(e)
	}
	oldBytes, _, _ := identity.ActivePEM(ac)
	block, _ := pem.Decode(oldBytes)
	oldLeaf, _ := x509.ParseCertificate(block.Bytes)
	cert, leaf, e := identity.SignServiceCSR(ac.CA, mc.ServiceCAKeyFile, oldLeaf, freshCSR, 4)
	if e != nil {
		t.Fatal(e)
	}
	block, _ = pem.Decode([]byte(issued.Certificate))
	pendingLeaf, _ := x509.ParseCertificate(block.Bytes)
	admin, _ := identity.Client(cfg["admin"])
	defer admin.CloseIdleConnections()
	auditor, _ := identity.Client(cfg["auditor"])
	defer auditor.CloseIdleConnections()
	request := map[string]any{"certificate": cert, "expected_serial": "stale", "reason": "Recover lost pending private key"}
	accessCall(t, auditor, "POST", server.URL+"/v1/service-certificates/adopt", request, 403)
	accessCall(t, admin, "POST", server.URL+"/v1/service-certificates/adopt", request, 409)
	request["expected_serial"] = pendingLeaf.SerialNumber.String()
	accessCall(t, admin, "POST", server.URL+"/v1/service-certificates/adopt", request, 200)
	accessCall(t, oldClient, "POST", server.URL+"/v1/service-certificates/renew", map[string]any{"csr": freshCSR, "lifetime_hours": 4}, 409)
	shortServiceCertificate(t, ac, -time.Minute)
	if e = identity.RecoverServiceCertificate(ac, cert, freshKey); e != nil {
		t.Fatal(e)
	}
	if got := identity.CertificateInfo(ac).Serial; got != leaf.SerialNumber.String() {
		t.Fatal("offline recovery not activated")
	}
	if _, e = identity.Config(ac, true); e != nil {
		t.Fatal("recovered identity cannot start", e)
	}
	entries, e := m.certificateInventory(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	_ = entries
}
