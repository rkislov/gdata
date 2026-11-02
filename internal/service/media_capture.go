// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
)

func (m *Media) captureAuthority(r *http.Request) (model.CaptureTicket, error) {
	ticket, e := m.Captures.Ticket(r.PathValue("id"))
	if e != nil {
		return ticket, errors.New("capture unavailable")
	}
	if _, e = Role(r, "manager", "admin", "auditor"); e == nil {
		if r.Method != "GET" {
			return ticket, errors.New("manager capture access is limited to control and status")
		}
		return ticket, nil
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	g, e := identity.Verify(token, m.Key)
	if e != nil || g.Permission != "capture" || g.RunID != ticket.ID || g.Subject != ticket.Capture.ScopeID || g.Expires != ticket.Capture.ExpiresAt.Unix() {
		return ticket, errors.New("capture capability rejected")
	}
	if !ValidEndpoint(m.Config.ManagerURL) {
		return ticket, errors.New("capture manager endpoint unavailable")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if e = Call(ctx, m.CaptureClient, "GET", m.Config.ManagerURL+"/v1/rescue/captures/"+ticket.ID+"/authorize", nil, nil); e != nil {
		return ticket, errors.New("capture access is not active on manager")
	}
	return ticket, nil
}
func (m *Media) captureRoutes(mux *http.ServeMux) {
	if !m.Config.RescueCaptureEnabled {
		return
	}
	mux.HandleFunc("POST /v1/captures/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager"); e != nil {
			Fail(w, 403, e)
			return
		}
		var t model.CaptureTicket
		if e := Decode(w, r, &t); e != nil {
			Fail(w, 400, e)
			return
		}
		if t.ID != r.PathValue("id") || t.Job.MediaID != m.Config.ID {
			Fail(w, 400, errors.New("capture target mismatch"))
			return
		}
		if e := m.Captures.Create(t); e != nil {
			Fail(w, 409, e)
			return
		}
		JSON(w, 201, map[string]bool{"ready": true})
	})
	mux.HandleFunc("GET /v1/captures/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if _, e := m.captureAuthority(r); e != nil {
			Fail(w, 403, e)
			return
		}
		status, e := m.Captures.Status(r.PathValue("id"))
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, status)
	})
	mux.HandleFunc("POST /v1/captures/{id}/commit", func(w http.ResponseWriter, r *http.Request) {
		if _, e := m.captureAuthority(r); e != nil {
			Fail(w, 403, e)
			return
		}
		status, e := m.Captures.Commit(r.PathValue("id"))
		if e != nil {
			Fail(w, 409, e)
			return
		}
		JSON(w, 202, status)
	})
	for _, method := range []string{"POST", "PUT"} {
		mux.HandleFunc(method+" /v1/captures/{id}/blocks/{index}/{hash}", func(w http.ResponseWriter, r *http.Request) {
			if _, e := m.captureAuthority(r); e != nil {
				Fail(w, 403, e)
				return
			}
			index, e := strconv.ParseInt(r.PathValue("index"), 10, 64)
			if e != nil {
				Fail(w, 400, errors.New("invalid block index"))
				return
			}
			var data []byte
			if r.Method == "PUT" {
				data, e = io.ReadAll(http.MaxBytesReader(w, r.Body, repository.ChunkSize))
				if e != nil {
					Fail(w, 400, e)
					return
				}
			}
			present, status, e := m.Captures.Block(r.PathValue("id"), index, r.PathValue("hash"), data)
			if e != nil {
				Fail(w, 409, e)
				return
			}
			JSON(w, 200, map[string]any{"present": present, "status": status})
		})
	}
}
func peerCertificateHash(resp *http.Response) (string, error) {
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		return "", errors.New("media certificate unavailable")
	}
	sum := sha256.Sum256(resp.TLS.PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:]), nil
}
