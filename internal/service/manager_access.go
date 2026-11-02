// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"gdata/srk/internal/catalog"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"golang.org/x/crypto/bcrypt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

type webSession struct {
	Until    time.Time
	Username string
	Version  int
}
type principalKey struct{}

func currentUser(r *http.Request) model.User {
	u, _ := r.Context().Value(principalKey{}).(model.User)
	return u
}
func (m *Manager) principal(r *http.Request) (model.User, error) {
	if p, e := identity.Request(r); e == nil {
		if p.Role == "admin" || p.Role == "auditor" || p.Role == "manager" && p.ID == m.Config.ID {
			role := p.Role
			if role == "manager" {
				role = "auditor"
			}
			return model.User{Username: p.ID, Role: role, AllJobs: true, AllSites: true}, nil
		}
		if p.Role == "user" {
			state, e := m.DB.RecoveryState(r.Context())
			if e != nil || state.Required {
				return model.User{}, errors.New("recovery review required")
			}
			u, _, e := m.DB.User(r.Context(), p.ID)
			if e == nil && !u.Disabled && m.DB.PersonalCertificateAllowed(r.Context(), p.ID, r.TLS.PeerCertificates[0]) {
				return u, nil
			}
		}
		return model.User{}, errors.New("authentication required")
	}
	c, e := r.Cookie("srk_session")
	if e != nil {
		return model.User{}, errors.New("authentication required")
	}
	m.mu.Lock()
	s, ok := m.sessions[c.Value]
	if ok && time.Now().After(s.Until) {
		delete(m.sessions, c.Value)
		ok = false
	}
	m.mu.Unlock()
	if !ok {
		return model.User{}, errors.New("session expired")
	}
	if s.Username == "" {
		return model.User{Username: "bootstrap", Role: "admin", AllJobs: true, AllSites: true}, nil
	}
	state, e := m.DB.RecoveryState(r.Context())
	if e != nil || state.Required {
		return model.User{}, errors.New("recovery review required")
	}
	u, _, e := m.DB.User(r.Context(), s.Username)
	if e != nil || u.Disabled || u.Version != s.Version {
		return model.User{}, errors.New("session revoked")
	}
	return u, nil
}
func (m *Manager) accessRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/me", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) { JSON(w, 200, currentUser(r)) }))
	mux.HandleFunc("GET /v1/users", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		v, e := m.DB.Users(r.Context())
		respond(w, v, e)
	}))
	mux.HandleFunc("PUT /v1/users", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		var req struct {
			model.User
			Password string `json:"password"`
		}
		if e := Decode(w, r, &req); e != nil {
			Fail(w, 400, e)
			return
		}
		if e := req.User.Validate(); e != nil {
			Fail(w, 400, e)
			return
		}
		var hash []byte
		if req.Password != "" {
			if len(req.Password) < 16 || len(req.Password) > 72 {
				Fail(w, 400, errors.New("password must contain 16–72 bytes"))
				return
			}
			var e error
			hash, e = bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
			if e != nil {
				Fail(w, 400, e)
				return
			}
		}
		if e := m.DB.Audit(r.Context(), actor, "access.user.update.requested", req.Username, "requested"); e != nil {
			Fail(w, 503, errors.New("audit unavailable"))
			return
		}
		u, e := m.DB.PutUser(r.Context(), req.User, hash)
		if e != nil {
			Fail(w, 409, e)
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "access.user.updated", u.Username, "success")
		JSON(w, 200, u)
	}))
}

// Explicit allowlist: new write routes are administrator-only until reviewed.
func permissionFor(r *http.Request) string {
	switch r.Pattern {
	case "POST /v1/jobs/{id}/run":
		return "run"
	case "POST /v1/runs/{id}/cancel":
		return "cancel"
	case "POST /v1/runs/{id}/verify":
		return "verify"
	case "POST /v1/runs/{id}/restore":
		return "restore"
	case "POST /v1/media/{media}/gc", "POST /v1/media/{media}/wal-gc":
		return "cleanup"
	case "POST /v1/media/{media}/tape/operations", "POST /v1/media/{media}/removable/{volume}/operations":
		b, e := io.ReadAll(io.LimitReader(r.Body, 65537))
		r.Body = io.NopCloser(bytes.NewReader(b))
		if e != nil || len(b) > 65536 {
			return ""
		}
		var v model.TapeRequest
		if json.Unmarshal(b, &v) != nil {
			return ""
		}
		switch v.Action {
		case "erase", "recycle":
			return "erase"
		case "restore":
			return "restore"
		case "verify":
			return "verify"
		}
	}
	return ""
}
func (m *Manager) authorize(w http.ResponseWriter, r *http.Request, u model.User, write bool) (*http.Request, bool) {
	deny := func() (*http.Request, bool) {
		_ = m.DB.Audit(r.Context(), u.Role+"/"+u.Username, "access_denied", r.URL.Path, "denied")
		Fail(w, 403, errors.New("insufficient permissions or resource scope"))
		return r, false
	}
	if write {
		if _, e := identity.Request(r); e != nil && !SameOrigin(r) {
			return deny()
		}
	}
	state, e := m.DB.RecoveryState(r.Context())
	if e != nil {
		return deny()
	}
	if state.Required && write && r.URL.Path != "/v1/logout" && !strings.HasPrefix(r.URL.Path, "/v1/catalog-backups") && r.URL.Path != "/v1/users" && !strings.HasPrefix(r.URL.Path, "/v1/users/") {
		return deny()
	}
	if strings.HasPrefix(r.URL.Path, "/v1/catalog-backups") && u.Role != "admin" {
		return deny()
	}
	if u.Role == "admin" {
		return r, true
	}
	if r.URL.Path == "/v1/me" || r.URL.Path == "/v1/logout" {
		return r, true
	}
	if strings.HasPrefix(r.URL.Path, "/v1/users") {
		return deny()
	}
	if write && !u.Can(permissionFor(r)) {
		return deny()
	}
	if write && permissionFor(r) == "restore" && strings.Contains(r.Pattern, "/operations") && !m.mediaRestoreAllowed(r, u) {
		return deny()
	}
	if r.Pattern == "GET /v1/restore-targets" {
		return r, true
	}
	if u.Global() {
		return r, true
	}
	// Restricted accounts may operate only on their selected jobs. Infrastructure
	// inventories and arbitrary restore destinations require a global account.
	switch r.Pattern {
	case "GET /v1/protection", "GET /v1/protection/settings", "GET /v1/storage-view", "GET /v1/media/{media}/snapshots", "GET /v1/status", "GET /v1/jobs", "GET /v1/runs", "GET /v1/runs/{id}", "GET /v1/runs/{id}/events", "GET /v1/reports/operations.csv", "GET /v1/reports/operations.xlsx", "POST /v1/jobs/{id}/run", "POST /v1/runs/{id}/verify", "POST /v1/runs/{id}/restore":
	case "GET /v1/nodes", "GET /v1/audit", "GET /v1/kubernetes-checks", "GET /v1/wal-replication-policies":
		JSON(w, 200, []any{})
		return r, false
	default:
		return deny()
	}
	jobs, e := m.DB.Jobs(r.Context())
	if e != nil {
		return deny()
	}
	settings, e := m.DB.ProtectionSettings(r.Context())
	if e != nil {
		return deny()
	}
	ids := []string{}
	for _, j := range jobs {
		if !u.AllJobs && !slices.Contains(u.JobIDs, j.ID) {
			continue
		}
		siteOK := u.AllSites
		for _, p := range settings.Policies {
			if p.JobID == j.ID && slices.Contains(u.SiteIDs, p.SourceSiteID) {
				siteOK = true
			}
		}
		if siteOK {
			ids = append(ids, j.ID)
		}
	}
	if id := r.PathValue("id"); id != "" {
		jobID := id
		if strings.Contains(r.Pattern, "/runs/") {
			run, e := m.DB.Run(r.Context(), id)
			if e != nil {
				return deny()
			}
			jobID = run.JobID
		}
		if !slices.Contains(ids, jobID) {
			return deny()
		}
	}
	if media := r.PathValue("media"); media != "" {
		allowed := false
		for _, s := range settings.Storages {
			if s.Kind == "repository" && s.MediaID == media && visibleSite(u, s.SiteID) {
				allowed = true
			}
		}
		if !allowed || len(ids) == 0 {
			return deny()
		}
	}
	r = r.WithContext(catalog.WithJobScope(r.Context(), ids))
	mediaIDs := []string{}
	for _, s := range settings.Storages {
		if s.MediaID != "" && visibleSite(u, s.SiteID) && !slices.Contains(mediaIDs, s.MediaID) {
			mediaIDs = append(mediaIDs, s.MediaID)
		}
	}
	r = r.WithContext(catalog.WithProtectionScope(r.Context(), mediaIDs, u.SiteIDs, u.AllSites))

	if media := r.PathValue("media"); media != "" {
		runs, err := m.DB.ProtectionRuns(r.Context())
		if err != nil {
			return deny()
		}
		owns := false
		for _, run := range runs {
			if run.Kind == "backup" && run.Status == "success" && run.Job.MediaID == media {
				owns = true
				break
			}
		}
		if !owns {
			return deny()
		}
	}

	if r.URL.Path == "/v1/status" {
		counts, e := m.DB.Counts(r.Context())
		if e != nil {
			return deny()
		}
		JSON(w, 200, map[string]any{"role": "manager", "counts": counts, "restricted": true})
		return r, false
	}
	return r, true
}
func withPrincipal(r *http.Request, u model.User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, u))
}
