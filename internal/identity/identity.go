// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gdata/srk/internal/config"
)

type Peer struct{ Role, ID, Installation string }

func Parse(cert *x509.Certificate) (Peer, error) {
	for _, u := range cert.URIs {
		p := strings.Split(strings.Trim(u.Path, "/"), "/")
		if u.Scheme == "spiffe" && u.Host == "srk" && len(p) == 3 {
			return Peer{Installation: p[0], Role: p[1], ID: p[2]}, nil
		}
	}
	return Peer{}, errors.New("missing SRK certificate identity")
}
func Request(r *http.Request) (Peer, error) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		return Peer{}, errors.New("client certificate required")
	}
	return Parse(r.TLS.PeerCertificates[0])
}
func Config(c config.Config, server bool) (*tls.Config, error) {
	pair, e := tls.LoadX509KeyPair(c.Cert, c.Key)
	if e != nil {
		return nil, e
	}
	ca, e := os.ReadFile(c.CA)
	if e != nil {
		return nil, e
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid CA")
	}
	leaf, e := x509.ParseCertificate(pair.Certificate[0])
	if e != nil {
		return nil, e
	}
	intermediates := x509.NewCertPool()
	for _, raw := range pair.Certificate[1:] {
		cert, e := x509.ParseCertificate(raw)
		if e != nil {
			return nil, e
		}
		intermediates.AddCert(cert)
	}
	if _, e = leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); e != nil {
		return nil, fmt.Errorf("invalid local certificate: %w", e)
	}
	self, e := Parse(leaf)
	if e != nil || self.ID != c.ID || self.Role != c.Role || self.Installation != c.Installation {
		return nil, errors.New("configuration and certificate identity mismatch")
	}
	t := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, RootCAs: pool, ClientCAs: pool}
	if server {
		t.ClientAuth = tls.RequireAndVerifyClientCert
		if c.Role == "manager" || c.Role == "media" && c.RescueCaptureEnabled {
			t.ClientAuth = tls.VerifyClientCertIfGiven
		}
	}
	t.VerifyConnection = func(s tls.ConnectionState) error {
		if len(s.PeerCertificates) == 0 {
			if server && (c.Role == "manager" || c.Role == "media" && c.RescueCaptureEnabled) {
				return nil
			}
			return errors.New("peer certificate absent")
		}
		p, e := Parse(s.PeerCertificates[0])
		if e != nil {
			return e
		}
		if p.Installation != c.Installation {
			return errors.New("foreign installation")
		}
		if !server && p.Role != "manager" && p.Role != "media" && p.Role != "agent" && p.Role != "proxy" {
			return errors.New("invalid server role")
		}
		if c.Revoked != "" {
			b, e := os.ReadFile(c.Revoked)
			if e != nil {
				return fmt.Errorf("read revocation list: %w", e)
			}
			var list []string
			if e = json.Unmarshal(b, &list); e != nil {
				return e
			}
			for _, serial := range list {
				if serial == s.PeerCertificates[0].SerialNumber.String() {
					return errors.New("revoked certificate")
				}
			}
		}
		return nil
	}
	return t, nil
}
func Client(c config.Config, expected ...Peer) (*http.Client, error) {
	t, e := Config(c, false)
	if e != nil {
		return nil, e
	}
	if len(expected) > 0 {
		previous := t.VerifyConnection
		want := expected[0]
		t.VerifyConnection = func(s tls.ConnectionState) error {
			if e := previous(s); e != nil {
				return e
			}
			p, e := Parse(s.PeerCertificates[0])
			if e != nil {
				return e
			}
			if p.Role != want.Role || (want.ID != "" && p.ID != want.ID) {
				return errors.New("unexpected server identity")
			}
			return nil
		}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: t, MaxIdleConnsPerHost: 16, ResponseHeaderTimeout: 30 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects prohibited") }}, nil
}
func InitCA(dir, installation string) error {
	if _, e := os.Stat(filepath.Join(dir, "ca.key")); !os.IsNotExist(e) {
		return errors.New("CA exists or cannot be inspected")
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return e
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "SRK " + installation + " root"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, e := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if e != nil {
		return e
	}
	kb, e := x509.MarshalECPrivateKey(key)
	if e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(dir, "ca.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0600); e != nil {
		return e
	}
	return os.WriteFile(filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644)
}
func Issue(dir, installation, role, id string, hosts ...string) error {
	for _, s := range []string{installation, role, id} {
		if s == "" || strings.ContainsAny(s, "/\\ .") {
			return errors.New("identity must contain only simple path-safe segments")
		}
	}
	if _, e := os.Stat(filepath.Join(dir, id+".key")); !os.IsNotExist(e) {
		return errors.New("node key already exists")
	}
	cb, e := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if e != nil {
		return e
	}
	kb, e := os.ReadFile(filepath.Join(dir, "ca.key"))
	if e != nil {
		return e
	}
	cp, _ := pem.Decode(cb)
	kp, _ := pem.Decode(kb)
	if cp == nil || kp == nil {
		return errors.New("invalid CA PEM")
	}
	ca, e := x509.ParseCertificate(cp.Bytes)
	if e != nil {
		return e
	}
	cak, e := x509.ParseECPrivateKey(kp.Bytes)
	if e != nil {
		return e
	}
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return e
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	u, _ := url.Parse("spiffe://srk/" + installation + "/" + role + "/" + id)
	if len(hosts) == 0 {
		hosts = []string{"localhost", "127.0.0.1", "::1"}
	}
	var dns []string
	var ips []net.IP
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			return errors.New("empty certificate host")
		}
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
		} else {
			dns = append(dns, h)
		}
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: role + "/" + id}, URIs: []*url.URL{u}, DNSNames: dns, IPAddresses: ips, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().AddDate(0, 3, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, e := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, cak)
	if e != nil {
		return e
	}
	priv, e := x509.MarshalECPrivateKey(key)
	if e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(dir, id+".key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: priv}), 0600); e != nil {
		return e
	}
	return os.WriteFile(filepath.Join(dir, id+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644)
}

type Grant struct {
	WALPolicyID           string `json:"wal_policy_id,omitempty"`
	WALGeneration         string `json:"wal_generation,omitempty"`
	WALTargetRepositoryID string `json:"wal_target_repository_id,omitempty"`
	Audience              string `json:"audience,omitempty"`
	ArchiveID             string `json:"archive_id,omitempty"`
	Subject               string `json:"subject"`
	RunID                 string `json:"run_id"`
	SnapshotID            string `json:"snapshot_id,omitempty"`
	Base                  string `json:"base,omitempty"`
	Permission            string `json:"permission"`
	Expires               int64  `json:"expires"`
}

func Sign(g Grant, key []byte) string {
	b, _ := json.Marshal(g)
	p := base64.RawURLEncoding.EncodeToString(b)
	h := hmac.New(sha256.New, key)
	h.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func Verify(token string, key []byte) (Grant, error) {
	var g Grant
	p := strings.Split(token, ".")
	if len(p) != 2 {
		return g, errors.New("invalid capability")
	}
	sig, e := base64.RawURLEncoding.DecodeString(p[1])
	if e != nil {
		return g, e
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte(p[0]))
	if !hmac.Equal(sig, h.Sum(nil)) {
		return g, errors.New("invalid capability signature")
	}
	b, e := base64.RawURLEncoding.DecodeString(p[0])
	if e != nil {
		return g, e
	}
	if e = json.Unmarshal(b, &g); e != nil {
		return g, e
	}
	if time.Now().Unix() >= g.Expires {
		return g, errors.New("expired capability")
	}
	return g, nil
}
