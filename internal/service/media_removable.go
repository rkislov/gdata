// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/removable"
	"net/http"
)

func (m *Media) removableRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/removable", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager", "admin", "auditor"); e != nil {
			Fail(w, 403, e)
			return
		}
		out := []removable.Status{}
		for _, v := range m.Removable {
			out = append(out, v.Status(r.Context()))
		}
		JSON(w, 200, out)
	})
	mux.HandleFunc("POST /v1/removable/{volume}/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager"); e != nil {
			Fail(w, 403, e)
			return
		}
		v := m.Removable[r.PathValue("volume")]
		if v == nil {
			Fail(w, 404, errors.New("unknown removable volume"))
			return
		}
		var req model.TapeRequest
		if e := Decode(w, r, &req); e != nil {
			Fail(w, 400, e)
			return
		}
		id := r.PathValue("id")
		m.replicationMu.Lock()
		if m.replications == nil {
			m.replications = map[string]model.Result{}
		}
		if _, ok := m.replications[id]; ok {
			m.replicationMu.Unlock()
			Fail(w, 409, errors.New("operation active"))
			return
		}
		m.replications[id] = model.Result{Phase: "removable-start"}
		m.replicationMu.Unlock()
		defer func() { m.replicationMu.Lock(); delete(m.replications, id); m.replicationMu.Unlock() }()
		res, e := v.Execute(r.Context(), id, req, m.Repo, func(res model.Result) {
			if res.Tape != nil {
				p := *res.Tape
				p.Barcode = req.Barcode
				res.Tape = &p
			}
			m.replicationMu.Lock()
			m.replications[id] = res
			m.replicationMu.Unlock()
		})
		if e != nil {
			Fail(w, 422, e)
			return
		}
		if res.Tape == nil {
			res.Tape = &model.TapeResult{}
		}
		res.Tape.Barcode = req.Barcode
		JSON(w, 200, res)
	})
}
