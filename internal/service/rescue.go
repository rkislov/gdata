// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"gdata/srk/internal/rescue"
)

func (m *Manager) rescueRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/rescue/codes", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		var q struct {
			RunIDs        []string `json:"run_ids"`
			Operation     string   `json:"operation,omitempty"`
			Name          string   `json:"name,omitempty"`
			MediaID       string   `json:"media_id,omitempty"`
			RetentionDays int      `json:"retention_days,omitempty"`
			MaxBytes      int64    `json:"max_bytes,omitempty"`
		}
		if e := Decode(w, r, &q); e != nil || (len(q.RunIDs) == 0 && q.Operation != "backup") || len(q.RunIDs) > 100 {
			Fail(w, 400, errors.New("choose 1–100 successful backup IDs"))
			return
		}
		if q.Operation != "" && q.Operation != "restore" && q.Operation != "backup" {
			Fail(w, 400, errors.New("unknown rescue operation"))
			return
		}
		if q.Operation == "backup" {
			if len(q.RunIDs) != 0 || !captureName(q.Name) || q.RetentionDays < 1 || q.RetentionDays > 36500 || q.MaxBytes < 512 || q.MaxBytes > model.MaxCaptureBytes {
				Fail(w, 400, errors.New("backup code requires name, media, retention and maximum disk size"))
				return
			}
			n, cl, e := m.mediaClient(r.Context(), q.MediaID)
			if e != nil {
				Fail(w, 400, e)
				return
			}
			cl.CloseIdleConnections()
			if enabled, _ := n.Status["rescue_capture_enabled"].(bool); !enabled {
				Fail(w, 400, errors.New("enable rescue_capture_enabled on the selected media server first"))
				return
			}
		}
		for _, id := range q.RunIDs {
			v, e := m.DB.Run(r.Context(), id)
			if e != nil || v.Kind != "backup" || v.Status != "success" {
				Fail(w, 400, errors.New("successful backup required"))
				return
			}
		}
		cert, e := tls.LoadX509KeyPair(m.Config.Cert, m.Config.Key)
		if e != nil {
			Fail(w, 503, errors.New("server certificate unavailable"))
			return
		}
		if !ValidEndpoint(m.Config.URL) {
			Fail(w, 503, errors.New("public manager URL required"))
			return
		}
		hash := sha256.Sum256(cert.Certificate[0])
		secret := model.ID() + model.ID()
		s := rescue.Scope{ID: model.ID(), RunIDs: q.RunIDs, ExpiresAt: time.Now().UTC().Add(4 * time.Hour)}
		if q.Operation == "backup" {
			s.ExpiresAt = time.Now().UTC().Add(12 * time.Hour)
			s.Capture = &rescue.CapturePermission{RunID: model.ID(), JobID: model.ID(), Name: q.Name, MediaID: q.MediaID, RetentionDays: q.RetentionDays, MaxBytes: q.MaxBytes}
		}
		if e = m.DB.RescueIssue(r.Context(), rescue.Digest(secret), s); e != nil {
			Fail(w, 500, errors.New("cannot issue code"))
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "rescue.issue", s.ID, "success")
		JSON(w, 201, map[string]any{"id": s.ID, "code": rescue.Encode(rescue.Code{URL: m.Config.URL, CertificateSHA256: hex.EncodeToString(hash[:]), Secret: secret}), "code_expires_at": time.Now().UTC().Add(10 * time.Minute), "session_expires_at": s.ExpiresAt})
	}))
	mux.HandleFunc("DELETE /v1/rescue/codes/{id}", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		if e := m.DB.RescueRevoke(r.Context(), r.PathValue("id")); e != nil {
			Fail(w, 500, e)
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "rescue.revoke", r.PathValue("id"), "success")
		JSON(w, 200, map[string]bool{"revoked": true})
	}))
	mux.HandleFunc("POST /v1/rescue/pair", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// A high-entropy code is additionally limited per source. Entries are bounded.
		ip := "rescue/" + ClientIP(r)
		now := time.Now()
		m.mu.Lock()
		recent := []time.Time{}
		for _, t := range m.attempts[ip] {
			if now.Sub(t) < time.Minute {
				recent = append(recent, t)
			}
		}
		if len(recent) >= 5 {
			m.mu.Unlock()
			Fail(w, 429, errors.New("too many pairing attempts"))
			return
		}
		if len(m.attempts) > 10000 {
			for k, v := range m.attempts {
				if len(v) == 0 || now.Sub(v[len(v)-1]) > time.Minute {
					delete(m.attempts, k)
				}
			}
			if len(m.attempts) > 10000 {
				m.mu.Unlock()
				Fail(w, 503, errors.New("pairing busy"))
				return
			}
		}
		m.attempts[ip] = append(recent, now)
		m.mu.Unlock()
		var q struct {
			Code string `json:"code"`
		}
		if e := Decode(w, r, &q); e != nil || len(q.Code) != 64 {
			Fail(w, 403, errors.New("invalid pairing code"))
			return
		}
		token := model.ID() + model.ID()
		scope, e := m.DB.RescueConsume(r.Context(), rescue.Digest(q.Code), rescue.Digest(token))
		if e != nil {
			Fail(w, 403, errors.New("code expired, revoked or already used"))
			return
		}
		_ = m.DB.Audit(r.Context(), "rescue/"+scope.ID, "rescue.pair", scope.ID, "success")
		JSON(w, 200, rescue.Pairing{Token: token, Scope: scope})
	})
	mux.HandleFunc("POST /v1/rescue/events", func(w http.ResponseWriter, r *http.Request) {
		s, e := m.rescueScope(r)
		if e != nil {
			Fail(w, 403, e)
			return
		}
		var q struct {
			RunID  string `json:"run_id"`
			Status string `json:"status"`
		}
		if e = Decode(w, r, &q); e != nil || !slices.Contains(s.RunIDs, q.RunID) || !slices.Contains([]string{"started", "verified", "failed"}, q.Status) {
			Fail(w, 400, errors.New("invalid restore event"))
			return
		}
		if e = m.DB.Audit(r.Context(), "rescue/"+s.ID, "rescue.client_restore", q.RunID, q.Status); e != nil {
			Fail(w, 500, errors.New("audit unavailable"))
			return
		}
		JSON(w, 200, map[string]bool{"recorded": true})
	})
	mux.HandleFunc("POST /v1/rescue/disconnect", func(w http.ResponseWriter, r *http.Request) {
		s, e := m.rescueScope(r)
		if e != nil {
			Fail(w, 403, e)
			return
		}
		if e = m.DB.RescueRevoke(r.Context(), s.ID); e != nil {
			Fail(w, 500, e)
			return
		}
		_ = m.DB.Audit(r.Context(), "rescue/"+s.ID, "rescue.disconnect", s.ID, "success")
		JSON(w, 200, map[string]bool{"disconnected": true})
	})
	mux.HandleFunc("GET /v1/rescue/backups", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		s, e := m.rescueScope(r)
		if e != nil {
			Fail(w, 403, e)
			return
		}
		out := []rescue.Backup{}
		for _, id := range s.RunIDs {
			v, e := m.DB.Run(r.Context(), id)
			if e != nil {
				continue
			}
			consistency := ""
			if v.Result != nil {
				consistency = v.Result.Consistency
			}
			out = append(out, rescue.Backup{ID: id, Name: v.Job.Name, CreatedAt: v.CreatedAt, Consistency: consistency})
		}
		JSON(w, 200, out)
	})
	for _, route := range []string{"GET /v1/rescue/backups/{id}", "GET /v1/rescue/backups/{id}/chunks/{hash}"} {
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			s, e := m.rescueScope(r)
			if e != nil || !slices.Contains(s.RunIDs, r.PathValue("id")) {
				Fail(w, 403, errors.New("backup outside rescue scope"))
				return
			}
			run, e := m.DB.Run(r.Context(), r.PathValue("id"))
			if e != nil || run.Status != "success" || run.Kind != "backup" {
				Fail(w, 404, errors.New("backup unavailable"))
				return
			}
			n, cl, e := m.mediaClient(r.Context(), run.Job.MediaID)
			if e != nil {
				Fail(w, 503, e)
				return
			}
			defer cl.CloseIdleConnections()
			snapshot := run.SnapshotID
			if snapshot == "" {
				snapshot = run.ID
			}
			lease := rescue.Digest(s.ID + run.ID)[:32]
			g := identity.Sign(identity.Grant{Subject: m.Config.ID, RunID: lease, SnapshotID: snapshot, Permission: "read", Expires: s.ExpiresAt.Unix()}, m.Key)
			head := http.Header{"Authorization": []string{"Bearer " + g}}
			hash := r.PathValue("hash")
			if hash == "" {
				var man model.Manifest
				e = Call(r.Context(), cl, "GET", n.URL+"/v1/snapshots/"+url.PathEscape(snapshot), nil, &man, head)
				if e == nil {
					e = repository.Validate(man)
				}
				if e == nil {
					e = Call(r.Context(), cl, "POST", n.URL+"/v1/snapshots/"+url.PathEscape(snapshot)+"/verify", nil, nil)
				}
				if e != nil {
					Fail(w, 422, errors.New("backup integrity verification failed or retention expired"))
					return
				}
				_ = m.DB.Audit(r.Context(), "rescue/"+s.ID, "rescue.read", run.ID, "verified")
				JSON(w, 200, man)
				return
			}
			req, e := http.NewRequestWithContext(r.Context(), "GET", n.URL+"/v1/chunks/"+url.PathEscape(hash), nil)
			if e != nil {
				Fail(w, 400, e)
				return
			}
			req.Header = head
			resp, e := cl.Do(req)
			if e != nil {
				Fail(w, 502, errors.New("media unavailable"))
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				Fail(w, 422, errors.New("chunk unavailable or outside snapshot"))
				return
			}
			b, e := io.ReadAll(io.LimitReader(resp.Body, repository.ChunkSize+1))
			if e != nil || repository.Hash(b) != hash || len(b) > repository.ChunkSize {
				Fail(w, 422, errors.New("chunk checksum mismatch"))
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(b)
		})
	}
}
func (m *Manager) rescueScope(r *http.Request) (rescue.Scope, error) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(token) != 64 {
		return rescue.Scope{}, errors.New("rescue authentication required")
	}
	return m.DB.RescueScope(r.Context(), rescue.Digest(token))
}
