// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"gdata/srk/internal/config"
)

func Fingerprint(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func ServiceRole(role string) bool {
	return role == "manager" || role == "media" || role == "agent" || role == "proxy"
}

// ActivePEM reads one atomic bundle, never two independently switched files.
// Existing installations keep using their original certificate and key.
func ActivePEM(c config.Config) ([]byte, []byte, error) {
	if c.ServiceCertificates != nil {
		name := filepath.Join(c.ServiceCertificates.Directory, "current.pem")
		b, e := os.ReadFile(name)
		if e == nil {
			if e = privateFile(name); e != nil {
				return nil, nil, e
			}
			var cert, key []byte
			for len(b) > 0 {
				block, rest := pem.Decode(b)
				if block == nil {
					return nil, nil, errors.New("invalid service certificate bundle")
				}
				b = rest
				if block.Type == "CERTIFICATE" {
					cert = append(cert, pem.EncodeToMemory(block)...)
				} else if block.Type == "PRIVATE KEY" {
					if key != nil {
						return nil, nil, errors.New("multiple private keys")
					}
					key = pem.EncodeToMemory(block)
				} else {
					return nil, nil, errors.New("unexpected bundle material")
				}
			}
			if len(cert) == 0 || len(key) == 0 {
				return nil, nil, errors.New("incomplete certificate bundle")
			}
			return cert, key, nil
		}
		if !os.IsNotExist(e) {
			return nil, nil, e
		}
	}
	if e := privateFile(c.Key); e != nil {
		return nil, nil, e
	}
	cert, e := os.ReadFile(c.Cert)
	if e != nil {
		return nil, nil, e
	}
	key, e := os.ReadFile(c.Key)
	return cert, key, e
}
func privateFile(path string) error {
	st, e := os.Stat(path)
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return errors.New("private certificate material must have mode 0600 or 0400")
	}
	return nil
}

func loadPair(c config.Config, pool *x509.CertPool) (tls.Certificate, error) {
	cert, key, e := ActivePEM(c)
	if e != nil {
		return tls.Certificate{}, e
	}
	pair, e := tls.X509KeyPair(cert, key)
	if e != nil {
		return pair, e
	}
	leaf, e := x509.ParseCertificate(pair.Certificate[0])
	if e != nil {
		return pair, e
	}
	inter := x509.NewCertPool()
	for _, raw := range pair.Certificate[1:] {
		v, e := x509.ParseCertificate(raw)
		if e != nil {
			return pair, e
		}
		inter.AddCert(v)
	}
	if _, e = leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); e != nil {
		return pair, e
	}
	self, e := Parse(leaf)
	if e != nil || self != (Peer{Role: c.Role, ID: c.ID, Installation: c.Installation}) {
		return pair, errors.New("configuration and certificate identity mismatch")
	}
	pair.Leaf = leaf
	return pair, nil
}

// RotationLock prevents two local workers from replacing a pending private key.
func RotationLock(c config.Config) (func(), error) {
	if c.ServiceCertificates == nil {
		return nil, errors.New("service certificate store is not configured")
	}
	dir := c.ServiceCertificates.Directory
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	st, e := os.Stat(dir)
	if e != nil {
		return nil, e
	}
	if st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("certificate directory must have mode 0700")
	}
	f, e := os.OpenFile(filepath.Join(dir, "rotation.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, e
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

func atomicPrivate(name string, b []byte) error {
	f, e := os.CreateTemp(filepath.Dir(name), ".certificate-*")
	if e != nil {
		return e
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(temp, name); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(name))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}

// PendingCSR durably retains a new key across network errors and process crashes.
func PendingCSR(c config.Config) (csr string, key []byte, err error) {
	name := filepath.Join(c.ServiceCertificates.Directory, "pending.key")
	key, err = os.ReadFile(name)
	if os.IsNotExist(err) {
		k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			return "", nil, e
		}
		der, e := x509.MarshalPKCS8PrivateKey(k)
		if e != nil {
			return "", nil, e
		}
		key = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if e = atomicPrivate(name, key); e != nil {
			return "", nil, e
		}
	} else if err != nil {
		return "", nil, err
	}
	if err = privateFile(name); err != nil {
		return "", nil, err
	}
	b, _ := pem.Decode(key)
	if b == nil {
		return "", nil, errors.New("invalid pending key")
	}
	k, e := x509.ParsePKCS8PrivateKey(b.Bytes)
	if e != nil {
		return "", nil, e
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return "", nil, errors.New("invalid pending signer")
	}
	der, e := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, signer)
	if e != nil {
		return "", nil, e
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), key, nil
}

// InstallCertificate validates identity, chain, key and endpoint names before
// one durable rename. TLS callbacks see either complete old or complete new material.
func InstallCertificate(c config.Config, cert string, key []byte) error {
	if c.ServiceCertificates == nil {
		return errors.New("certificate store required")
	}
	pair, e := tls.X509KeyPair([]byte(cert), key)
	if e != nil {
		return e
	}
	leaf, e := x509.ParseCertificate(pair.Certificate[0])
	if e != nil {
		return e
	}
	ca, e := os.ReadFile(c.CA)
	if e != nil {
		return e
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return errors.New("invalid CA")
	}
	inter := x509.NewCertPool()
	for _, raw := range pair.Certificate[1:] {
		v, e := x509.ParseCertificate(raw)
		if e != nil {
			return e
		}
		inter.AddCert(v)
	}
	if _, e = leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); e != nil {
		return e
	}
	if _, e = leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); e != nil {
		return e
	}
	p, e := Parse(leaf)
	if e != nil || p != (Peer{Role: c.Role, ID: c.ID, Installation: c.Installation}) {
		return errors.New("renewed certificate identity mismatch")
	}
	old, e := loadPair(c, pool)
	if e != nil {
		return e
	}
	if Fingerprint(leaf.RawSubjectPublicKeyInfo) == Fingerprint(old.Leaf.RawSubjectPublicKeyInfo) {
		return errors.New("rotation requires a fresh private key")
	}
	if leaf.NotAfter.Before(old.Leaf.NotAfter) {
		return errors.New("renewal must not shorten certificate validity")
	}
	for _, host := range old.Leaf.DNSNames {
		if !slices.Contains(leaf.DNSNames, host) {
			return errors.New("renewal removed an existing DNS name")
		}
	}
	for _, ip := range old.Leaf.IPAddresses {
		if !slices.ContainsFunc(leaf.IPAddresses, ip.Equal) {
			return errors.New("renewal removed an existing IP")
		}
	}
	bundle := append(append([]byte{}, key...), []byte(cert)...)
	return atomicPrivate(filepath.Join(c.ServiceCertificates.Directory, "current.pem"), bundle)
}

func ValidateServiceCSR(peer *x509.Certificate, csr string) (*x509.CertificateRequest, error) {
	if len(csr) > 16384 {
		return nil, errors.New("CSR exceeds 16 KiB")
	}
	block, rest := pem.Decode([]byte(csr))
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(rest) != 0 {
		return nil, errors.New("one PEM CSR required")
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err = req.CheckSignature(); err != nil {
		return nil, err
	}
	k, ok := req.PublicKey.(*ecdsa.PublicKey)
	if !ok || k.Curve != elliptic.P256() {
		return nil, errors.New("service CSR requires P-256")
	}
	if Fingerprint(req.RawSubjectPublicKeyInfo) == Fingerprint(peer.RawSubjectPublicKeyInfo) {
		return nil, errors.New("fresh private key required")
	}
	return req, nil
}

func SignServiceCSR(caPath, keyPath string, peer *x509.Certificate, csr string, hours int) (string, *x509.Certificate, error) {
	fail := func(e error) (string, *x509.Certificate, error) { return "", nil, e }
	p, e := Parse(peer)
	if e != nil || !ServiceRole(p.Role) {
		return fail(errors.New("service identity required"))
	}
	if hours < 2 || hours > 2160 {
		return fail(errors.New("invalid certificate lifetime"))
	}
	req, e := ValidateServiceCSR(peer, csr)
	if e != nil {
		return fail(e)
	}
	cb, e := os.ReadFile(caPath)
	if e != nil {
		return fail(e)
	}
	kb, e := os.ReadFile(keyPath)
	if e != nil {
		return fail(e)
	}
	cp, _ := pem.Decode(cb)
	kp, _ := pem.Decode(kb)
	if cp == nil || kp == nil {
		return fail(errors.New("invalid issuer PEM"))
	}
	ca, e := x509.ParseCertificate(cp.Bytes)
	if e != nil {
		return fail(e)
	}
	var parsed any
	if kp.Type == "EC PRIVATE KEY" {
		parsed, e = x509.ParseECPrivateKey(kp.Bytes)
	} else {
		parsed, e = x509.ParsePKCS8PrivateKey(kp.Bytes)
	}
	if e != nil {
		return fail(e)
	}
	issuer, ok := parsed.(crypto.Signer)
	if !ok {
		return fail(errors.New("issuer key is not a signer"))
	}
	pub, e := x509.MarshalPKIXPublicKey(issuer.Public())
	if e != nil || Fingerprint(pub) != Fingerprint(ca.RawSubjectPublicKeyInfo) || !ca.IsCA || ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		return fail(errors.New("issuer mismatch"))
	}
	now := time.Now().UTC()
	end := now.Add(time.Duration(hours) * time.Hour)
	if end.Before(peer.NotAfter) {
		return fail(errors.New("renewal must not shorten existing validity"))
	}
	if now.Before(ca.NotBefore) || !end.Before(ca.NotAfter) {
		return fail(errors.New("requested lifetime outside issuer validity"))
	}
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 160))
	if e != nil {
		return fail(e)
	}
	u := &url.URL{Scheme: "spiffe", Host: "srk", Path: "/" + p.Installation + "/" + p.Role + "/" + p.ID}
	leaf := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: p.Role + "/" + p.ID}, URIs: []*url.URL{u}, DNSNames: peer.DNSNames, IPAddresses: peer.IPAddresses, NotBefore: now.Add(-time.Minute), NotAfter: end, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	der, e := x509.CreateCertificate(rand.Reader, leaf, ca, req.PublicKey, issuer)
	if e != nil {
		return fail(e)
	}
	leaf, e = x509.ParseCertificate(der)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), leaf, e
}

type CertificateStatus struct {
	Serial        string    `json:"serial,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
	State         string    `json:"state"`
	AutoRenew     bool      `json:"auto_renew"`
	RenewalError  string    `json:"renewal_error,omitempty"`
	LastRenewedAt time.Time `json:"last_renewed_at,omitempty"`
}

func CertificateInfo(c config.Config) CertificateStatus {
	var s CertificateStatus
	if c.ServiceCertificates != nil {
		s.AutoRenew = c.ServiceCertificates.AutoRenew
		b, _ := os.ReadFile(filepath.Join(c.ServiceCertificates.Directory, "status.json"))
		_ = json.Unmarshal(b, &s)
		s.AutoRenew = c.ServiceCertificates.AutoRenew
	}
	s.Serial, s.ExpiresAt = "", time.Time{}
	cert, _, e := ActivePEM(c)
	if e != nil {
		s.State = "unavailable"
		return s
	}
	b, _ := pem.Decode(cert)
	if b == nil {
		s.State = "unavailable"
		return s
	}
	leaf, e := x509.ParseCertificate(b.Bytes)
	if e != nil {
		s.State = "unavailable"
		return s
	}
	s.Serial = leaf.SerialNumber.String()
	s.ExpiresAt = leaf.NotAfter
	s.State = "healthy"
	before, _ := c.CertificateTiming()
	if time.Until(leaf.NotAfter) <= time.Duration(before)*time.Hour {
		s.State = "expiring"
	}
	if time.Now().After(leaf.NotAfter) {
		s.State = "expired"
	}
	if s.RenewalError != "" && s.State == "healthy" {
		s.State = "renewal_failed"
	}
	return s
}
func SaveRenewalStatus(c config.Config, err error) {
	s := CertificateInfo(c)
	if err != nil {
		s.RenewalError = "automatic renewal failed; inspect service logs"
	} else {
		s.RenewalError = ""
		s.LastRenewedAt = time.Now().UTC()
	}
	b, _ := json.Marshal(s)
	_ = atomicPrivate(filepath.Join(c.ServiceCertificates.Directory, "status.json"), b)
}

// ValidateServicePublicCertificate is also used for explicit administrator
// recovery. It accepts public certificates only and never relaxes CA trust.
func ValidateServicePublicCertificate(c config.Config, encoded string) (*x509.Certificate, error) {
	if len(encoded) > 16384 {
		return nil, errors.New("service certificate exceeds 16 KiB")
	}
	var chain []*x509.Certificate
	b := []byte(encoded)
	for len(b) > 0 {
		block, rest := pem.Decode(b)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, errors.New("public certificate PEM required")
		}
		v, e := x509.ParseCertificate(block.Bytes)
		if e != nil {
			return nil, e
		}
		chain = append(chain, v)
		b = rest
	}
	if len(chain) == 0 {
		return nil, errors.New("certificate required")
	}
	leaf := chain[0]
	peer, e := Parse(leaf)
	if e != nil || !ServiceRole(peer.Role) || peer.Installation != c.Installation || leaf.IsCA {
		return nil, errors.New("service certificate identity required")
	}
	ca, e := os.ReadFile(c.CA)
	if e != nil {
		return nil, e
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid CA")
	}
	inter := x509.NewCertPool()
	for _, v := range chain[1:] {
		inter.AddCert(v)
	}
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		if _, e = leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{usage}}); e != nil {
			return nil, e
		}
	}
	if c.Revoked != "" {
		b, e := os.ReadFile(c.Revoked)
		if e != nil {
			return nil, e
		}
		var serials []string
		if e = json.Unmarshal(b, &serials); e != nil {
			return nil, e
		}
		if slices.Contains(serials, leaf.SerialNumber.String()) {
			return nil, errors.New("certificate is revoked")
		}
	}
	return leaf, nil
}

// RecoverServiceCertificate explicitly installs a CA-verified offline recovery
// pair when the previous certificate is expired or its pending key is lost.
func RecoverServiceCertificate(c config.Config, cert string, key []byte) error {
	unlock, e := RotationLock(c)
	if e != nil {
		return e
	}
	defer unlock()
	leaf, e := ValidateServicePublicCertificate(c, cert)
	if e != nil {
		return e
	}
	peer, _ := Parse(leaf)
	if peer.Role != c.Role || peer.ID != c.ID {
		return errors.New("recovery identity mismatch")
	}
	if c.URL != "" {
		u, e := url.Parse(c.URL)
		if e != nil {
			return e
		}
		if e = leaf.VerifyHostname(u.Hostname()); e != nil {
			return e
		}
	}
	pair, e := tls.X509KeyPair([]byte(cert), key)
	if e != nil {
		return e
	}
	der, e := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if e != nil {
		return e
	}
	key = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if e = atomicPrivate(filepath.Join(c.ServiceCertificates.Directory, "current.pem"), append(key, []byte(cert)...)); e != nil {
		return e
	}
	if e = os.Remove(filepath.Join(c.ServiceCertificates.Directory, "pending.key")); e != nil && !os.IsNotExist(e) {
		return e
	}
	RefreshClients(c)
	SaveRenewalStatus(c, nil)
	return nil
}
