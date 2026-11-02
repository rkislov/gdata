// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"errors"
	"net/http"
	"strconv"
	"time"
)

func (m *Media) catalogCopyRoutes(mux *http.ServeMux) {
	authorize := func(w http.ResponseWriter, r *http.Request) bool {
		_, e := Role(r, "manager")
		if e != nil {
			Fail(w, 403, e)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/catalog-copies", func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r) {
			return
		}
		v, e := m.CatalogCopies.List()
		respond(w, v, e)
	})
	mux.HandleFunc("PUT /v1/catalog-copies/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r) {
			return
		}
		days, e := strconv.Atoi(r.Header.Get("X-Retention-Days"))
		if e != nil {
			Fail(w, 400, errors.New("retention required"))
			return
		}
		v, e := m.CatalogCopies.Put(r.PathValue("id"), r.Header.Get("X-Archive-SHA256"), days, http.MaxBytesReader(w, r.Body, 16<<30))
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, v)
	})
	mux.HandleFunc("GET /v1/catalog-copies/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r) {
			return
		}
		f, v, e := m.CatalogCopies.Open(r.PathValue("id"))
		if e != nil {
			Fail(w, 404, e)
			return
		}
		defer f.Close()
		w.Header().Set("X-Archive-SHA256", v.SHA256)
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "catalog.srkcat", v.ReceivedAt, f)
	})
	mux.HandleFunc("POST /v1/catalog-copies/gc", func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r) {
			return
		}
		if e := m.CatalogCopies.GC(time.Now()); e != nil {
			Fail(w, 500, e)
			return
		}
		JSON(w, 200, map[string]bool{"ok": true})
	})
}
