// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"net/http"
	"time"
)

func (m *Manager) certificateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/users/{username}/certificates", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		if _, _, e := m.DB.User(r.Context(), r.PathValue("username")); e != nil {
			Fail(w, 404, errors.New("account not found"))
			return
		}
		s, e := m.DB.CertificateState(r.Context(), r.PathValue("username"))
		s.IssuerEnabled = m.Config.PersonalCAKeyFile != ""
		respond(w, s, e)
	}))
	mux.HandleFunc("POST /v1/users/{username}/certificates", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		var v struct {
			CSR          string `json:"csr"`
			Days         int    `json:"days"`
			OverlapHours int    `json:"overlap_hours"`
			Version      int    `json:"version"`
		}
		if e := Decode(w, r, &v); e != nil {
			Fail(w, 400, e)
			return
		}
		if v.OverlapHours < 0 || v.OverlapHours > 24 || v.Version < 0 {
			Fail(w, 400, errors.New("overlap must be 0–24 hours"))
			return
		}
		if m.Config.PersonalCAKeyFile == "" {
			Fail(w, 409, errors.New("personal certificate issuer is not configured"))
			return
		}
		user := r.PathValue("username")
		u, _, e := m.DB.User(r.Context(), user)
		if e != nil || u.Disabled {
			Fail(w, 409, errors.New("active account required"))
			return
		}
		cert, leaf, key, e := identity.SignPersonalCSR(m.Config.CA, m.Config.PersonalCAKeyFile, m.Config.Installation, user, v.CSR, v.Days)
		if e != nil {
			Fail(w, 400, errors.New("invalid CSR, lifetime or issuer configuration"))
			return
		}
		sum := sha256.Sum256(leaf.Raw)
		record := model.PersonalCertificate{Certificate: cert, Username: user, Serial: leaf.SerialNumber.String(), KeyFingerprint: key, Fingerprint: hex.EncodeToString(sum[:]), IssuedAt: time.Now().UTC(), ExpiresAt: leaf.NotAfter, RetireAt: leaf.NotAfter}
		if e = m.DB.Audit(r.Context(), actor, "certificate.rotate.requested", user, "requested"); e != nil {
			Fail(w, 503, e)
			return
		}
		if e = m.DB.ChangeCertificates(r.Context(), user, v.Version, &record, time.Duration(v.OverlapHours)*time.Hour, ""); e != nil {
			Fail(w, 409, e)
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "certificate.rotated", user, record.Serial)
		JSON(w, 201, map[string]any{"certificate": cert, "record": record, "version": v.Version + 1})
	}))
	mux.HandleFunc("POST /v1/users/{username}/certificates/revoke", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		var v struct {
			Serial  string `json:"serial"`
			Version int    `json:"version"`
		}
		if e := Decode(w, r, &v); e != nil {
			Fail(w, 400, e)
			return
		}
		user := r.PathValue("username")
		if e := m.DB.Audit(r.Context(), actor, "certificate.revoke.requested", user, v.Serial); e != nil {
			Fail(w, 503, e)
			return
		}
		if e := m.DB.ChangeCertificates(r.Context(), user, v.Version, nil, 0, v.Serial); e != nil {
			Fail(w, 409, e)
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "certificate.revoked", user, v.Serial)
		JSON(w, 200, map[string]any{"version": v.Version + 1, "revoked": true})
	}))
}
