// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"errors"
	"gdata/srk/internal/virtualization"
	"net/http"
)

func (a *Agent) virtualizationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/virtualization", func(w http.ResponseWriter, r *http.Request) {
		if e := Diagnostic(r, a.Config); e != nil {
			Fail(w, 403, e)
			return
		}
		if a.Config.Role != "proxy" {
			Fail(w, 404, errors.New("proxy required"))
			return
		}
		out := map[string]string{}
		for name, p := range a.Config.Virtualization {
			out[name] = p.Kind
		}
		JSON(w, 200, out)
	})
	mux.HandleFunc("GET /v1/virtualization/{profile}/journal", func(w http.ResponseWriter, r *http.Request) {
		if e := Diagnostic(r, a.Config); e != nil {
			Fail(w, 403, e)
			return
		}
		p, ok := a.Config.Virtualization[r.PathValue("profile")]
		if !ok || !p.BackupEnabled {
			Fail(w, 404, errors.New("unknown enabled profile"))
			return
		}
		pending, e := virtualization.Pending(p)
		respond(w, pending, e)
	})
	mux.HandleFunc("POST /v1/virtualization/{profile}/cleanup/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "admin", "manager"); e != nil {
			Fail(w, 403, e)
			return
		}
		p, ok := a.Config.Virtualization[r.PathValue("profile")]
		if !ok || !p.BackupEnabled {
			Fail(w, 404, errors.New("unknown enabled profile"))
			return
		}
		pending, e := virtualization.Pending(p)
		if e != nil {
			Fail(w, 500, e)
			return
		}
		for _, old := range pending {
			if old.ID != r.PathValue("id") {
				continue
			}
			j, e := virtualization.OpenJournal(p, old.ID, a.Config.ID, old.Kind)
			if e != nil {
				Fail(w, 409, e)
				return
			}
			defer j.Close()
			if virtualization.NativeProvider(p.Kind) {
				cloud, e := a.recoveryNativeCloud(p, j)
				if e != nil {
					Fail(w, 503, e)
					return
				}
				defer cloud.Close()
				if e = cloud.Cleanup(r.Context(), j, nil); e != nil {
					Fail(w, 409, e)
					return
				}
				JSON(w, 200, map[string]bool{"cleaned": true})
				return
			}
			if p.Kind == "ovirt" {
				cloud, e := virtualization.NewOVirt(p)
				if e != nil {
					Fail(w, 503, e)
					return
				}
				defer cloud.Close()
				if e = cloud.Cleanup(r.Context(), j, nil); e != nil {
					Fail(w, 409, e)
					return
				}
				JSON(w, 200, map[string]bool{"cleaned": true})
				return
			}
			cloud, e := virtualization.NewOpenStack(p)
			if e != nil {
				Fail(w, 503, e)
				return
			}
			defer cloud.Close()
			if e = cloud.Cleanup(r.Context(), j, nil); e != nil {
				Fail(w, 409, e)
				return
			}
			JSON(w, 200, map[string]bool{"cleaned": true})
			return
		}
		JSON(w, 200, map[string]bool{"cleaned": true})
	})

	for _, route := range []string{"GET /v1/virtualization/{profile}/vms", "GET /v1/virtualization/{profile}/vms/{id}/plan"} {
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			if e := Diagnostic(r, a.Config); e != nil {
				Fail(w, 403, e)
				return
			}
			p, ok := a.Config.Virtualization[r.PathValue("profile")]
			if !ok || a.Config.Role != "proxy" {
				Fail(w, 404, errors.New("unknown virtualization profile"))
				return
			}
			cl, e := virtualization.New(p)
			if e != nil {
				Fail(w, 503, errors.New("virtualization profile unavailable"))
				return
			}
			defer cl.Close()
			vms, e := cl.Inventory(r.Context())
			if e != nil {
				Fail(w, 502, e)
				return
			}
			if r.PathValue("id") == "" {
				JSON(w, 200, vms)
				return
			}
			if virtualization.NativeProvider(p.Kind) && p.BackupEnabled {
				cloud, e := virtualization.NewNative(p)
				if e != nil {
					Fail(w, 503, e)
					return
				}
				defer cloud.Close()
				meta, e := cloud.Inspect(r.Context(), r.PathValue("id"), r.PathValue("profile"), "crash-consistent")
				if e != nil {
					JSON(w, 200, map[string]any{"provider": p.Kind, "executable": false, "reason": e.Error()})
					return
				}
				JSON(w, 200, map[string]any{"provider": p.Kind, "executable": true, "backup": meta})
				return
			}

			if p.Kind == "ovirt" && p.BackupEnabled {
				cloud, e := virtualization.NewOVirt(p)
				if e != nil {
					Fail(w, 503, e)
					return
				}
				defer cloud.Close()
				meta, _, e := cloud.Preflight(r.Context(), r.PathValue("id"), r.PathValue("profile"), "crash-consistent")
				if e != nil {
					JSON(w, 200, map[string]any{"provider": "ovirt", "executable": false, "reason": e.Error()})
					return
				}
				JSON(w, 200, map[string]any{"provider": "ovirt", "executable": true, "backup": meta})
				return
			}
			if p.Kind == "openstack" && p.BackupEnabled {
				cloud, e := virtualization.NewOpenStack(p)
				if e != nil {
					Fail(w, 503, e)
					return
				}
				defer cloud.Close()
				meta, e := cloud.Preflight(r.Context(), r.PathValue("id"), r.PathValue("profile"), "crash-consistent")
				if e != nil {
					JSON(w, 200, map[string]any{"provider": "openstack", "vm_id": r.PathValue("id"), "executable": false, "reason": e.Error()})
					return
				}
				JSON(w, 200, map[string]any{"provider": "openstack", "vm_id": meta.ServerID, "executable": true, "backup": meta, "consistency": "Cold only for an already SHUTOFF source; running snapshots are crash-consistent, not application-consistent."})
				return
			}
			for _, vm := range vms {
				if vm.ID == r.PathValue("id") {
					JSON(w, 200, cl.Plan(vm))
					return
				}
			}
			Fail(w, 404, errors.New("VM not found"))
		})
	}
}
