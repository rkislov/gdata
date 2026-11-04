// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
)

func renewServiceCertificate(ctx context.Context, c config.Config, force bool) error {
	if c.ServiceCertificates == nil {
		return errors.New("service certificate store is not configured")
	}
	if !c.ServiceCertificates.AutoRenew && !force {
		return nil
	}
	unlock, e := identity.RotationLock(c)
	if e != nil {
		return e
	}
	defer unlock()
	info := identity.CertificateInfo(c)
	before, lifetime := c.CertificateTiming()
	if !force && time.Until(info.ExpiresAt) > time.Duration(before)*time.Hour {
		return nil
	}
	if !time.Now().Before(info.ExpiresAt) {
		return errors.New("expired identity requires administrative recovery")
	}
	csr, key, e := identity.PendingCSR(c)
	if e != nil {
		return e
	}
	block, _ := pem.Decode([]byte(csr))
	req, e := x509.ParseCertificateRequest(block.Bytes)
	if e != nil {
		return e
	}
	cert, _, e := identity.ActivePEM(c)
	if e != nil {
		return e
	}
	block, _ = pem.Decode(cert)
	leaf, e := x509.ParseCertificate(block.Bytes)
	if e != nil {
		return e
	}
	if identity.Fingerprint(req.RawSubjectPublicKeyInfo) == identity.Fingerprint(leaf.RawSubjectPublicKeyInfo) {
		// Crash after the atomic installation but before deleting the pending key.
		if e = os.Remove(filepath.Join(c.ServiceCertificates.Directory, "pending.key")); e != nil {
			return e
		}
		csr, key, e = identity.PendingCSR(c)
		if e != nil {
			return e
		}
	}
	cl, e := identity.Client(c, identity.Peer{Role: "manager"})
	if e != nil {
		return e
	}
	defer cl.CloseIdleConnections()
	endpoint := c.ManagerURL
	if c.Role == "manager" {
		endpoint = c.URL
	}
	var out struct {
		Certificate string `json:"certificate"`
	}
	if e = Call(ctx, cl, "POST", endpoint+"/v1/service-certificates/renew", map[string]any{"csr": csr, "lifetime_hours": lifetime}, &out); e != nil {
		return e
	}
	if e = identity.InstallCertificate(c, out.Certificate, key); e != nil {
		return e
	}
	identity.RefreshClients(c)
	if e = os.Remove(filepath.Join(c.ServiceCertificates.Directory, "pending.key")); e != nil && !os.IsNotExist(e) {
		return e
	}
	identity.SaveRenewalStatus(c, nil)
	slog.Info("service certificate rotated", "role", c.Role, "id", c.ID, "serial", identity.CertificateInfo(c).Serial)
	return nil
}
func certificateLifecycle(ctx context.Context, c config.Config) {
	if c.ServiceCertificates == nil {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	// Wait for Serve to bind its listener, including the manager's own issuer.
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	retry := time.Time{}
	for {
		identity.RefreshClients(c)
		if c.ServiceCertificates.AutoRenew && time.Now().After(retry) {
			req, cancel := context.WithTimeout(ctx, 30*time.Second)
			e := renewServiceCertificate(req, c, false)
			cancel()
			if e != nil && ctx.Err() == nil {
				identity.SaveRenewalStatus(c, e)
				slog.Warn("automatic certificate renewal failed", "role", c.Role, "id", c.ID, "error", e)
				retry = time.Now().Add(5 * time.Minute)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) serviceCertificateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/service-certificates/renew", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, e := Role(r, "manager", "media", "agent", "proxy")
		if e != nil {
			Fail(w, 403, errors.New("service certificate required"))
			return
		}
		if p.Role == "manager" && p.ID != m.Config.ID {
			Fail(w, 403, errors.New("foreign manager identity"))
			return
		}
		state, e := m.DB.RecoveryState(r.Context())
		if e != nil || state.Required {
			Fail(w, 503, errors.New("certificate issuance paused during catalog recovery"))
			return
		}
		if e = m.DB.Check(r.Context()); e != nil {
			Fail(w, 503, errors.New("manager fencing lock unavailable"))
			return
		}
		if m.Config.ServiceCAKeyFile == "" {
			Fail(w, 409, errors.New("service certificate issuer is not configured"))
			return
		}
		var in struct {
			CSR           string `json:"csr"`
			LifetimeHours int    `json:"lifetime_hours"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 20*1024)
		if e = Decode(w, r, &in); e != nil {
			Fail(w, 400, errors.New("invalid renewal request"))
			return
		}
		_, maximum := m.Config.CertificateTiming()
		if in.LifetimeHours < 2 || in.LifetimeHours > maximum {
			Fail(w, 400, errors.New("lifetime exceeds issuer policy"))
			return
		}
		peer := r.TLS.PeerCertificates[0]
		req, e := identity.ValidateServiceCSR(peer, in.CSR)
		if e != nil {
			Fail(w, 400, errors.New("invalid service CSR"))
			return
		}
		issued := model.ServiceCertificate{Role: p.Role, ID: p.ID, PreviousSerial: peer.SerialNumber.String(), KeyFingerprint: identity.Fingerprint(req.RawSubjectPublicKeyInfo)}
		issued, e = m.DB.RenewServiceCertificate(r.Context(), identity.Fingerprint(peer.Raw), issued, func() (model.ServiceCertificate, error) {
			cert, leaf, err := identity.SignServiceCSR(m.Config.CA, m.Config.ServiceCAKeyFile, peer, in.CSR, in.LifetimeHours)
			if err != nil {
				return issued, err
			}
			issued.Certificate, issued.Serial, issued.Fingerprint, issued.ExpiresAt = cert, leaf.SerialNumber.String(), identity.Fingerprint(leaf.Raw), leaf.NotAfter
			return issued, nil
		})
		if e != nil {
			Fail(w, 409, errors.New("certificate generation conflict; retry with the original pending key or recover administratively"))
			return
		}
		JSON(w, 201, map[string]string{"certificate": issued.Certificate})
	})
	mux.HandleFunc("POST /v1/service-certificates/adopt", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		if currentUser(r).Role != "admin" {
			Fail(w, 403, errors.New("administrator required"))
			return
		}
		var in struct {
			Certificate    string `json:"certificate"`
			ExpectedSerial string `json:"expected_serial"`
			Reason         string `json:"reason"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 20*1024)
		if e := Decode(w, r, &in); e != nil || len(strings.TrimSpace(in.Reason)) < 8 || len(in.Reason) > 500 {
			Fail(w, 400, errors.New("public certificate, expected serial and reason (8–500 bytes) required"))
			return
		}
		leaf, e := identity.ValidateServicePublicCertificate(m.Config, in.Certificate)
		if e != nil {
			Fail(w, 400, errors.New("valid service certificate from this installation required"))
			return
		}
		peer, _ := identity.Parse(leaf)
		record := model.ServiceCertificate{Role: peer.Role, ID: peer.ID, Certificate: in.Certificate, Serial: leaf.SerialNumber.String(), Fingerprint: identity.Fingerprint(leaf.Raw), KeyFingerprint: identity.Fingerprint(leaf.RawSubjectPublicKeyInfo), ExpiresAt: leaf.NotAfter}
		if e = m.DB.AdoptServiceCertificate(r.Context(), record, in.ExpectedSerial, actor, in.Reason); e != nil {
			Fail(w, 409, e)
			return
		}
		JSON(w, 200, map[string]string{"serial": record.Serial})
	}))
	mux.HandleFunc("GET /v1/service-certificates", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		if currentUser(r).Role != "admin" {
			Fail(w, 403, errors.New("administrator required"))
			return
		}
		inventory, e := m.certificateInventory(r.Context())
		respond(w, inventory, e)
	}))
}

type certificateEntry struct {
	Role             string    `json:"role"`
	ID               string    `json:"id"`
	Serial           string    `json:"serial,omitempty"`
	RegisteredSerial string    `json:"registered_serial,omitempty"`
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
	ObservedAt       time.Time `json:"observed_at,omitempty"`
	State            string    `json:"state"`
	AutoRenew        bool      `json:"auto_renew"`
	RenewalError     string    `json:"renewal_error,omitempty"`
	Stale            bool      `json:"stale"`
}
type certificateInventory struct {
	Certificates  []certificateEntry `json:"certificates"`
	Alerts        []certificateEntry `json:"alerts"`
	IssuerEnabled bool               `json:"issuer_enabled"`
	CheckedAt     time.Time          `json:"checked_at"`
}

func certificateState(end time.Time, now time.Time) string {
	if end.IsZero() {
		return "unavailable"
	}
	left := end.Sub(now)
	if left <= 0 {
		return "expired"
	}
	if left <= 7*24*time.Hour {
		return "critical"
	}
	if left <= 30*24*time.Hour {
		return "expiring"
	}
	return "healthy"
}
func (m *Manager) certificateInventory(ctx context.Context) (certificateInventory, error) {
	now := time.Now().UTC()
	out := certificateInventory{Certificates: []certificateEntry{}, Alerts: []certificateEntry{}, IssuerEnabled: m.Config.ServiceCAKeyFile != "", CheckedAt: now}
	own := identity.CertificateInfo(m.Config)
	out.Certificates = append(out.Certificates, certificateEntry{Role: "manager", ID: m.Config.ID, Serial: own.Serial, ExpiresAt: own.ExpiresAt, ObservedAt: now, AutoRenew: own.AutoRenew, RenewalError: own.RenewalError})
	b, e := os.ReadFile(m.Config.CA)
	if e != nil {
		return out, e
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return out, errors.New("invalid CA")
	}
	ca, e := x509.ParseCertificate(block.Bytes)
	if e != nil {
		return out, e
	}
	out.Certificates = append(out.Certificates, certificateEntry{Role: "ca", ID: m.Config.Installation, Serial: ca.SerialNumber.String(), ExpiresAt: ca.NotAfter, ObservedAt: now})
	nodes, e := m.DB.Nodes(ctx)
	if e != nil {
		return out, e
	}
	for _, n := range nodes {
		v := certificateEntry{Role: n.Role, ID: n.ID, ObservedAt: n.LastSeen, Stale: now.Sub(n.LastSeen) > time.Minute}
		if n.Certificate != nil {
			v.Serial = n.Certificate.Serial
			v.ExpiresAt = n.Certificate.ExpiresAt
			v.ObservedAt = n.Certificate.ObservedAt
		}
		// Only health fields are self-reported; the identity/expiry above come from mTLS.
		raw, _ := json.Marshal(n.Status["certificate"])
		var status identity.CertificateStatus
		_ = json.Unmarshal(raw, &status)
		v.AutoRenew = status.AutoRenew
		v.RenewalError = status.RenewalError
		out.Certificates = append(out.Certificates, v)
	}
	registered, e := m.DB.ServiceCertificates(ctx)
	if e != nil {
		return out, e
	}
	generations := map[string]string{}
	for _, v := range registered {
		generations[v.Role+"/"+v.ID] = v.Serial
	}
	for i := range out.Certificates {
		v := &out.Certificates[i]
		v.RegisteredSerial = generations[v.Role+"/"+v.ID]
		v.State = certificateState(v.ExpiresAt, now)
		if v.State != "healthy" || v.RenewalError != "" || v.Stale {
			out.Alerts = append(out.Alerts, *v)
		}
	}
	return out, nil
}
func (m *Manager) certificateMonitor(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		req, cancel := context.WithTimeout(ctx, 10*time.Second)
		inventory, e := m.certificateInventory(req)
		if e == nil {
			for _, v := range inventory.Alerts {
				state := v.State
				if v.RenewalError != "" {
					state += "/renewal_failed"
				}
				if v.Stale {
					state += "/stale"
				}
				object := v.Role + "/" + v.ID
				key := "certificate-alert-" + identity.Fingerprint([]byte(object+"/"+v.Serial+"/"+state))
				if fresh, err := m.DB.NotifyCertificate(req, key, object, state); err == nil && fresh {
					slog.Warn("certificate requires attention", "identity", object, "state", state, "expires_at", v.ExpiresAt)
				}
			}
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// RotateServiceCertificate requests a fresh key and certificate immediately.
func RotateServiceCertificate(ctx context.Context, c config.Config) error {
	err := renewServiceCertificate(ctx, c, true)
	if err != nil && c.ServiceCertificates != nil {
		identity.SaveRenewalStatus(c, err)
	}
	return err
}
