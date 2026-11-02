// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"errors"
	"gdata/srk/internal/model"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
)

func restoreSiteAllowed(u model.User, site string) bool {
	return u.Role == "admin" || u.AllRestoreSites || slices.Contains(u.RestoreSiteIDs, site)
}
func (m *Manager) restoreTargetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/restore-targets", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		p, e := m.DB.ProtectionSettings(r.Context())
		if e != nil {
			Fail(w, 503, errors.New("catalog unavailable"))
			return
		}
		out := []model.RestoreTarget{}
		u := currentUser(r)
		for _, t := range p.RestoreTargets {
			if u.Can("restore") && restoreSiteAllowed(u, t.SiteID) {
				out = append(out, t)
			}
		}
		JSON(w, 200, out)
	}))
}
func (m *Manager) resolveRestoreTarget(r *http.Request, id string, source model.Run, dest string, vm *model.VMRestoreRequest) (model.RestoreTarget, *model.VMRestoreRequest, error) {
	deny := errors.New("restore target is missing, unauthorized or incompatible")
	u := currentUser(r)
	if id == "" {
		if u.Role == "admin" {
			return model.RestoreTarget{}, vm, nil
		}
		return model.RestoreTarget{}, nil, deny
	}
	p, e := m.DB.ProtectionSettings(r.Context())
	if e != nil {
		return model.RestoreTarget{}, nil, deny
	}
	for _, t := range p.RestoreTargets {
		if t.ID != id {
			continue
		}
		kind := source.Job.Type
		if kind == "" {
			kind = "files"
		}
		if t.Kind != kind || !restoreSiteAllowed(u, t.SiteID) {
			return t, nil, deny
		}
		node, e := m.DB.Node(r.Context(), t.NodeID)
		role := "agent"
		if t.VM != nil {
			role = "proxy"
		}
		if e != nil || node.Role != role {
			return t, nil, deny
		}
		if t.VM != nil {
			if vm == nil || vm.Name == "" {
				return t, nil, errors.New("new VM name required")
			}
			// Every placement option comes from the approved template, never from caller input.
			v := *t.VM
			v.Name = vm.Name
			v.ProxyID = t.NodeID
			v.Profile = t.Profile
			v.Provider = t.Kind
			return t, &v, nil
		}
		if kind == "files" || kind == "postgresql" {
			if !filepath.IsAbs(dest) || filepath.Clean(dest) != dest {
				return t, nil, errors.New("canonical absolute restore path required")
			}
		} else {
			v, e := url.Parse(dest)
			scheme := kind
			if kind == "longhorn" {
				scheme = "kubernetes"
			}
			if e != nil || v.Scheme != scheme || v.Host != t.Profile || v.User != nil || v.Fragment != "" {
				return t, nil, deny
			}
		}
		return t, vm, nil
	}
	return model.RestoreTarget{}, nil, deny
}

// Tape restores target a media repository, not an agent/profile. Every storage
// registered on that media must be in an authorized site; unknown placement fails closed.
func (m *Manager) mediaRestoreAllowed(r *http.Request, u model.User) bool {
	p, e := m.DB.ProtectionSettings(r.Context())
	if e != nil {
		return false
	}
	found := false
	for _, s := range p.Storages {
		if s.MediaID == r.PathValue("media") && s.Kind == "repository" {
			found = true
			if !restoreSiteAllowed(u, s.SiteID) {
				return false
			}
		}
	}
	return found
}
