// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"gdata/srk/internal/config"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServiceCSRCannotEscalateNamesAndRoles(t *testing.T) {
	dir := t.TempDir()
	if e := InitCA(dir, "test"); e != nil {
		t.Fatal(e)
	}
	if e := Issue(dir, "test", "agent", "a", "localhost"); e != nil {
		t.Fatal(e)
	}
	pair, e := tls.LoadX509KeyPair(filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key"))
	if e != nil {
		t.Fatal(e)
	}
	peer, _ := x509.ParseCertificate(pair.Certificate[0])
	peer.NotAfter = time.Now().Add(time.Hour)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, e := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "manager/other"}, DNSNames: []string{"attacker.example"}, URIs: []*url.URL{{Scheme: "spiffe", Host: "srk", Path: "/foreign/manager/other"}}}, key)
	if e != nil {
		t.Fatal(e)
	}
	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	_, leaf, e := SignServiceCSR(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), peer, csr, 4)
	if e != nil {
		t.Fatal(e)
	}
	p, e := Parse(leaf)
	if e != nil || p.Role != "agent" || p.ID != "a" || p.Installation != "test" || len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "localhost" {
		t.Fatal("CSR escalated identity or hostnames")
	}
	reused, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, pair.PrivateKey)
	if _, _, e = SignServiceCSR(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), peer, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: reused})), 4); e == nil {
		t.Fatal("key reuse accepted")
	}
	c := config.Config{Role: "agent", ID: "a", Installation: "test", CA: filepath.Join(dir, "ca.crt"), Cert: filepath.Join(dir, "a.crt"), Key: filepath.Join(dir, "a.key"), ServiceCertificates: &config.ServiceCertificateConfig{Directory: filepath.Join(dir, "rotation")}}
	unlock, e := RotationLock(c)
	if e != nil {
		t.Fatal(e)
	}
	defer unlock()
	if other, e := RotationLock(c); e == nil {
		other()
		t.Fatal("two rotation workers acquired the same store")
	}
	_, pending, e := PendingCSR(c)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = ActivePEM(c); e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(filepath.Join(c.ServiceCertificates.Directory, "pending.key"), 0644); e != nil {
		t.Fatal(e)
	}
	if _, _, e = PendingCSR(c); e == nil {
		t.Fatal("readable pending key accepted")
	}
	_ = pending
}
