// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"os"
	"time"
)

func CreatePersonalCSR(keyPath, csrPath string) error {
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return e
	}
	der, e := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "SRK personal client"}}, key)
	if e != nil {
		return e
	}
	priv, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		return e
	}
	// Never overwrite an existing key or CSR.
	k, e := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	if _, e = k.Write(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv})); e != nil {
		k.Close()
		return e
	}
	if e = k.Close(); e != nil {
		return e
	}
	f, e := os.OpenFile(csrPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	ce := f.Close()
	if e != nil {
		return e
	}
	return ce
}
func SignPersonalCSR(caPath, keyPath, installation, username, csr string, days int) (string, *x509.Certificate, string, error) {
	fail := func(e error) (string, *x509.Certificate, string, error) { return "", nil, "", e }
	if days < 1 || days > 90 || len(csr) > 16384 {
		return fail(errors.New("certificate lifetime must be 1–90 days; CSR at most 16 KiB"))
	}
	block, rest := pem.Decode([]byte(csr))
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(rest) != 0 {
		return fail(errors.New("one PEM CSR required"))
	}
	req, e := x509.ParseCertificateRequest(block.Bytes)
	if e != nil {
		return fail(e)
	}
	if e = req.CheckSignature(); e != nil {
		return fail(e)
	}
	switch k := req.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return fail(errors.New("unsupported EC curve"))
		}
	case *rsa.PublicKey:
		if k.N.BitLen() < 3072 {
			return fail(errors.New("RSA key must be at least 3072 bits"))
		}
	case ed25519.PublicKey:
	default:
		return fail(errors.New("unsupported public key"))
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
	switch kp.Type {
	case "EC PRIVATE KEY":
		parsed, e = x509.ParseECPrivateKey(kp.Bytes)
	case "RSA PRIVATE KEY":
		parsed, e = x509.ParsePKCS1PrivateKey(kp.Bytes)
	default:
		parsed, e = x509.ParsePKCS8PrivateKey(kp.Bytes)
	}
	if e != nil {
		return fail(e)
	}
	key, ok := parsed.(crypto.Signer)
	if !ok {
		return fail(errors.New("issuer key is not a signer"))
	}
	a, _ := x509.MarshalPKIXPublicKey(ca.PublicKey)
	b, _ := x509.MarshalPKIXPublicKey(key.Public())
	if string(a) != string(b) || !ca.IsCA {
		return fail(errors.New("issuer key/certificate mismatch"))
	}
	now := time.Now().UTC()
	end := now.Add(time.Duration(days) * 24 * time.Hour)
	if now.Before(ca.NotBefore) || !end.Before(ca.NotAfter) {
		return fail(errors.New("requested lifetime outside issuer validity"))
	}
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 160))
	if e != nil {
		return fail(e)
	}
	uri := &url.URL{Scheme: "spiffe", Host: "srk", Path: "/" + installation + "/user/" + username}
	leaf := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "user/" + username}, URIs: []*url.URL{uri}, NotBefore: now.Add(-time.Minute), NotAfter: end, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, e := x509.CreateCertificate(rand.Reader, leaf, ca, req.PublicKey, key)
	if e != nil {
		return fail(e)
	}
	cert, e := x509.ParseCertificate(der)
	if e != nil {
		return fail(e)
	}
	sum := sha256.Sum256(req.RawSubjectPublicKeyInfo)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), cert, hex.EncodeToString(sum[:]), nil
}
