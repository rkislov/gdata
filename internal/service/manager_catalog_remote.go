// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"encoding/json"
	"errors"
	"gdata/srk/internal/catalogbackup"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

type catalogRemote struct {
	client *http.Client
	base   string
}

func (v *catalogRemote) Close() { v.client.CloseIdleConnections() }
func (v *catalogRemote) Upload(ctx context.Context, id, hash string, days int, r io.Reader) (catalogbackup.Receipt, error) {
	var out catalogbackup.Receipt
	req, e := http.NewRequestWithContext(ctx, "PUT", v.base+"/v1/catalog-copies/"+url.PathEscape(id), r)
	if e != nil {
		return out, e
	}
	req.Header.Set("X-Archive-SHA256", hash)
	req.Header.Set("X-Retention-Days", strconv.Itoa(days))
	req.Header.Set("Content-Type", "application/octet-stream")
	res, e := v.client.Do(req)
	if e != nil {
		return out, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return out, errors.New("media server rejected catalog replica")
	}
	e = json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&out)
	return out, e
}
func (v *catalogRemote) Download(ctx context.Context, id string) (io.ReadCloser, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", v.base+"/v1/catalog-copies/"+url.PathEscape(id), nil)
	if e != nil {
		return nil, e
	}
	res, e := v.client.Do(req)
	if e != nil {
		return nil, e
	}
	if res.StatusCode != 200 {
		res.Body.Close()
		return nil, errors.New("remote catalog archive unavailable")
	}
	return res.Body, nil
}
func (m *Manager) catalogRemote(ctx context.Context, id string) (catalogbackup.Remote, error) {
	n, cl, e := m.mediaClient(ctx, id)
	if e != nil {
		return nil, e
	}
	return &catalogRemote{client: cl, base: n.URL}, nil
}
func (m *Manager) catalogMaintenanceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/catalog-backups/{id}/maintenance", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		if m.CatalogBackups == nil {
			Fail(w, 409, errors.New("catalog backup not configured"))
			return
		}
		var in struct {
			Kind    string `json:"kind"`
			MediaID string `json:"media_id"`
		}
		if e := Decode(w, r, &in); e != nil {
			Fail(w, 400, e)
			return
		}
		if in.MediaID != "" {
			_, cl, e := m.mediaClient(r.Context(), in.MediaID)
			if e != nil {
				Fail(w, 409, e)
				return
			}
			cl.CloseIdleConnections()
		}
		if e := m.DB.Audit(r.Context(), actor, "catalog."+in.Kind+".requested", r.PathValue("id"), "requested"); e != nil {
			Fail(w, 503, e)
			return
		}
		v, e := m.CatalogBackups.StartOperation(in.Kind, r.PathValue("id"), in.MediaID, actor)
		if e != nil {
			Fail(w, 409, e)
			return
		}
		JSON(w, 202, v)
	}))
	mux.HandleFunc("GET /v1/catalog-backups/remote/{media}/copies", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		n, cl, e := m.mediaClient(r.Context(), r.PathValue("media"))
		if e != nil {
			Fail(w, 409, e)
			return
		}
		defer cl.CloseIdleConnections()
		var out []catalogbackup.Receipt
		e = Call(r.Context(), cl, "GET", n.URL+"/v1/catalog-copies", nil, &out)
		respond(w, out, e)
	}))
	mux.HandleFunc("GET /v1/catalog-backups/remote/{media}/{id}/download", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		remote, e := m.catalogRemote(r.Context(), r.PathValue("media"))
		if e != nil {
			Fail(w, 409, e)
			return
		}
		defer remote.Close()
		if e = m.DB.Audit(r.Context(), actor, "catalog.remote.download", r.PathValue("id"), "requested"); e != nil {
			Fail(w, 503, e)
			return
		}
		reader, e := remote.Download(r.Context(), r.PathValue("id"))
		if e != nil {
			Fail(w, 404, e)
			return
		}
		defer reader.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename=\"catalog.srkcat\"")
		_, _ = io.Copy(w, reader)
	}))
}
