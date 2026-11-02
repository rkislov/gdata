// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"gdata/srk/internal/virtualization"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (m *Manager) vmRoutes(mux *http.ServeMux) {
	for _, route := range []string{"GET /v1/proxies/{proxy}/virtualization", "GET /v1/proxies/{proxy}/virtualization/{profile}/vms", "GET /v1/proxies/{proxy}/virtualization/{profile}/vms/{id}/plan"} {
		mux.HandleFunc(route, m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
			n, e := m.DB.Node(r.Context(), r.PathValue("proxy"))
			if e != nil || n.Role != "proxy" || time.Since(n.LastSeen) > 30*time.Second {
				Fail(w, 409, errors.New("registered online proxy required"))
				return
			}
			path := "/v1/virtualization"
			if r.PathValue("profile") != "" {
				path += "/" + url.PathEscape(r.PathValue("profile")) + "/vms"
			}
			if r.PathValue("id") != "" {
				path += "/" + url.PathEscape(r.PathValue("id")) + "/plan"
			}
			var out any
			e = Call(r.Context(), m.Client, "GET", n.URL+path, nil, &out)
			if e != nil {
				Fail(w, 502, e)
				return
			}
			JSON(w, 200, out)
		}))
	}
	for _, method := range []string{"GET", "POST"} {
		route := method + " /v1/proxies/{proxy}/virtualization/{profile}/journal"
		if method == "POST" {
			route = method + " /v1/proxies/{proxy}/virtualization/{profile}/cleanup/{id}"
		}
		mux.HandleFunc(route, m.wrap(method == "POST", func(w http.ResponseWriter, r *http.Request, actor string) {
			node, e := m.DB.Node(r.Context(), r.PathValue("proxy"))
			if e != nil || node.Role != "proxy" || time.Since(node.LastSeen) > 30*time.Second {
				Fail(w, 409, errors.New("online proxy required"))
				return
			}
			path := "/v1/virtualization/" + url.PathEscape(r.PathValue("profile")) + "/journal"
			if r.Method == "POST" {
				path = "/v1/virtualization/" + url.PathEscape(r.PathValue("profile")) + "/cleanup/" + url.PathEscape(r.PathValue("id"))
			}
			var out any
			if e = Call(r.Context(), m.Client, r.Method, node.URL+path, nil, &out); e != nil {
				Fail(w, 409, e)
				return
			}
			if r.Method == "POST" {
				_ = m.DB.Audit(r.Context(), actor, "vm.cleanup.request", r.PathValue("id"), "success")
			}
			JSON(w, 200, out)
		}))
	}
	mux.HandleFunc("POST /v1/runs/{id}/progress", func(w http.ResponseWriter, r *http.Request) {
		peer, e := Role(r, "proxy")
		if e != nil {
			Fail(w, 403, e)
			return
		}
		run, e := m.DB.Run(r.Context(), r.PathValue("id"))
		if e != nil || !virtualization.VMProvider(run.Job.Type) || run.Job.AgentID != peer.ID {
			Fail(w, 403, errors.New("VM operation belongs to another proxy"))
			return
		}
		var res model.Result
		if e = Decode(w, r, &res); e != nil {
			Fail(w, 400, e)
			return
		}
		if len(res.Phase) > 64 || res.LogicalBytes < 0 || res.UploadedBytes < 0 || res.TotalBytes < 0 || res.ProgressPercent != nil && (*res.ProgressPercent < 0 || *res.ProgressPercent > 100) {
			Fail(w, 400, errors.New("invalid VM progress"))
			return
		}
		// A progress report cannot complete a run or claim final verification.
		res.Verified = false
		changed, e := m.DB.VMProgress(r.Context(), run.ID, peer.ID, res)
		if e != nil {
			Fail(w, 500, e)
			return
		}
		if changed {
			key := res.Phase
			if res.ProgressPercent != nil {
				key += fmt.Sprintf("/%d%%", int(*res.ProgressPercent)/5*5)
			}
			m.mu.Lock()
			previous := m.vmEvents[run.ID]
			m.vmEvents[run.ID] = key
			m.mu.Unlock()
			if key != previous {
				_ = m.DB.Audit(r.Context(), "proxy/"+peer.ID, "vm.progress", run.ID, key)
			}
		}
		JSON(w, 200, map[string]bool{"accepted": changed})
	})
	mux.HandleFunc("POST /v1/runs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		peer, e := Role(r, "proxy")
		if e != nil {
			Fail(w, 403, e)
			return
		}
		run, e := m.DB.Run(r.Context(), r.PathValue("id"))
		if e != nil || run.Job.AgentID != peer.ID || !virtualization.VMProvider(run.Job.Type) {
			Fail(w, 403, errors.New("operation belongs to another proxy"))
			return
		}
		var event struct {
			Action  string `json:"action"`
			Message string `json:"message"`
		}
		if e = Decode(w, r, &event); e != nil || !strings.HasPrefix(event.Action, "vm.") || len(event.Action) > 64 || len(event.Message) > 4096 {
			Fail(w, 400, errors.New("invalid VM event"))
			return
		}
		if e = m.DB.Audit(r.Context(), "proxy/"+peer.ID, event.Action, run.ID, event.Message); e != nil {
			Fail(w, 500, e)
			return
		}
		JSON(w, 200, map[string]bool{"recorded": true})
	})
}
