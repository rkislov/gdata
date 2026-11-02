// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestPersonalCSRBoundIdentity(t *testing.T) {
	dir := t.TempDir()
	if e := InitCA(dir, "test"); e != nil {
		t.Fatal(e)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	uri, _ := url.Parse("spiffe://srk/foreign/admin/attacker")
	der, e := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "admin/attacker"}, URIs: []*url.URL{uri}, DNSNames: []string{"attacker.example"}}, key)
	if e != nil {
		t.Fatal(e)
	}
	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	_, cert, _, e := SignPersonalCSR(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), "test", "alice", csr, 1)
	if e != nil {
		t.Fatal(e)
	}
	p, e := Parse(cert)
	if e != nil || p.Role != "user" || p.ID != "alice" || p.Installation != "test" || len(cert.DNSNames) != 0 || cert.IsCA || len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatal("CSR escalated identity", cert)
	}
	der[len(der)-1] ^= 1
	bad := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	if _, _, _, e = SignPersonalCSR(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), "test", "alice", bad, 1); e == nil {
		t.Fatal("invalid CSR signature accepted")
	}
	keyPath := filepath.Join(dir, "client.key")
	csrPath := filepath.Join(dir, "client.csr")
	if e = CreatePersonalCSR(keyPath, csrPath); e != nil {
		t.Fatal(e)
	}
	before, _ := os.ReadFile(keyPath)
	if e = CreatePersonalCSR(keyPath, csrPath); e == nil {
		t.Fatal("key overwritten")
	}
	after, _ := os.ReadFile(keyPath)
	if string(before) != string(after) {
		t.Fatal("key changed")
	}
	st, _ := os.Stat(keyPath)
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}
