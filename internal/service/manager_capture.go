// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/rescue"
)

func (m *Manager) captureScope(r *http.Request) (rescue.Scope, error) {
	s, e := m.rescueScope(r)
	if e != nil {
		return s, e
	}
	if s.Capture == nil {
		return s, errors.New("this code permits restore only; disk backup requires a separate write permission")
	}
	if id := r.PathValue("id"); id != "" && id != s.Capture.RunID {
		return s, errors.New("capture outside scope")
	}
	return s, nil
}
func (m *Manager) captureRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/rescue/captures", func(w http.ResponseWriter, r *http.Request) {
		s, e := m.captureScope(r)
		if e != nil {
			Fail(w, 403, e)
			return
		}
		var disk model.DiskGeometry
		if e = Decode(w, r, &disk); e != nil {
			Fail(w, 400, e)
			return
		}
		if e = disk.Validate(); e != nil || disk.Bytes > s.Capture.MaxBytes {
			Fail(w, 400, errors.New("disk exceeds authorized size or has invalid geometry"))
			return
		}
		p := s.Capture
		now := time.Now().UTC()
		job := model.Job{ID: p.JobID, Type: "disk-image", Name: p.Name, AgentID: "rescue-" + s.ID, MediaID: p.MediaID, Source: "disk://" + disk.Host + disk.Device, Mode: "full", RetentionDays: p.RetentionDays, CreatedAt: now, Version: 1}
		capture := &model.DiskCapture{ScopeID: s.ID, ExpiresAt: s.ExpiresAt, Disk: disk}
		run := model.Run{ID: p.RunID, JobID: job.ID, Job: job, Kind: "backup", Status: "queued", CreatedAt: now, Capture: capture}
		run, e = m.DB.Enqueue(r.Context(), run, "capture/"+s.ID)
		if e != nil {
			Fail(w, 409, errors.New("capture request conflicts with previously selected disk"))
			return
		}
		_ = m.DB.Audit(r.Context(), "rescue/"+s.ID, "capture.enqueue", run.ID, "success")
		JSON(w, 202, run)
	})
	mux.HandleFunc("GET /v1/rescue/captures/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		s, e := m.captureScope(r)
		if e != nil {
			Fail(w, 403, e)
			return
		}
		run, e := m.DB.Run(r.Context(), r.PathValue("id"))
		if e != nil || run.Capture == nil || run.Capture.ScopeID != s.ID {
			Fail(w, 404, errors.New("capture not found"))
			return
		}
		response := rescue.CaptureAccess{Run: run}
		if run.Status == "running" {
			n, cl, e := m.mediaClient(r.Context(), run.Job.MediaID)
			if e != nil {
				Fail(w, 503, e)
				return
			}
			defer cl.CloseIdleConnections()
			req, _ := http.NewRequestWithContext(r.Context(), "GET", n.URL+"/v1/captures/"+run.ID, nil)
			resp, e := cl.Do(req)
			if e != nil {
				Fail(w, 503, errors.New("media unavailable"))
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				pin, e := peerCertificateHash(resp)
				if e != nil {
					Fail(w, 503, e)
					return
				}
				response.MediaURL = n.URL
				response.CertificateSHA256 = pin
				response.Token = identity.Sign(identity.Grant{Subject: s.ID, RunID: run.ID, Permission: "capture", Expires: s.ExpiresAt.Unix()}, m.Key)
			}
		}
		JSON(w, 200, response)
	})
	mux.HandleFunc("GET /v1/rescue/captures/{id}/authorize", func(w http.ResponseWriter, r *http.Request) {
		peer, e := Role(r, "media")
		if e != nil {
			Fail(w, 403, e)
			return
		}
		run, e := m.DB.Run(r.Context(), r.PathValue("id"))
		if e != nil || run.Capture == nil || run.Job.MediaID != peer.ID || run.Status != "running" && run.Status != "success" {
			Fail(w, 403, errors.New("capture is not active"))
			return
		}
		if e = m.DB.Check(r.Context()); e == nil {
			e = m.DB.RescueActive(r.Context(), run.Capture.ScopeID)
		}
		if e != nil {
			Fail(w, 403, errors.New("capture permission unavailable"))
			return
		}
		if run.Status == "running" {
			m.mu.Lock()
			ctx := m.captureContexts[run.ID]
			m.mu.Unlock()
			if ctx == nil || ctx.Err() != nil {
				Fail(w, 403, errors.New("capture worker has stopped"))
				return
			}
		}
		JSON(w, 200, map[string]bool{"active": true})
	})
	mux.HandleFunc("POST /v1/rescue/captures/{id}/abort", func(w http.ResponseWriter, r *http.Request) {
		if _, e := m.captureScope(r); e != nil {
			Fail(w, 403, e)
			return
		}
		m.operationMu.Lock()
		defer m.operationMu.Unlock()
		run, e := m.DB.Run(r.Context(), r.PathValue("id"))
		if e != nil {
			Fail(w, 404, e)
			return
		}
		if run.Status == "queued" {
			now := time.Now().UTC()
			run.Status = "cancelled"
			run.FinishedAt = &now
			run.Error = "Клиент SRK Rescue прервал копирование"
			if e = m.DB.SaveRun(r.Context(), run); e != nil {
				Fail(w, 500, e)
				return
			}
		}
		if run.Status == "running" {
			m.mu.Lock()
			cancel := m.active[run.ID]
			m.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
		_ = m.DB.Audit(r.Context(), "rescue/"+run.Capture.ScopeID, "capture.abort", run.ID, "requested")
		JSON(w, 200, map[string]bool{"abort_requested": true})
	})
	mux.HandleFunc("GET /v1/runs/{id}/events", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		events, e := m.DB.RunEvents(r.Context(), r.PathValue("id"))
		respond(w, events, e)
	}))
}
func (m *Manager) executeCapture(ctx context.Context, run model.Run) (model.Result, error) {
	m.mu.Lock()
	m.captureContexts[run.ID] = ctx
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.captureContexts, run.ID); m.mu.Unlock() }()
	var last model.Result
	if e := m.DB.RescueActive(ctx, run.Capture.ScopeID); e != nil {
		return last, e
	}
	n, cl, e := m.mediaClient(ctx, run.Job.MediaID)
	if e != nil {
		return last, e
	}
	defer cl.CloseIdleConnections()
	ticket := model.CaptureTicket{ID: run.ID, Job: run.Job, Capture: *run.Capture, CreatedAt: run.CreatedAt}
	if e = Call(ctx, cl, "POST", n.URL+"/v1/captures/"+run.ID, ticket, nil); e != nil {
		return last, e
	}
	_ = m.DB.Audit(ctx, "manager/"+m.Config.ID, "capture.started", run.ID, "media ticket ready")
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastEvent := ""
	lastContact := time.Now()
	for {
		if e = ctx.Err(); e != nil {
			return last, e
		}
		if !time.Now().Before(run.Capture.ExpiresAt) {
			return last, errors.New("capture session expired")
		}
		if e = m.DB.RescueActive(ctx, run.Capture.ScopeID); e != nil {
			return last, e
		}
		poll, cancel := context.WithTimeout(ctx, 10*time.Second)
		var status model.CaptureStatus
		e = Call(poll, cl, "GET", n.URL+"/v1/captures/"+run.ID, nil, &status)
		cancel()
		if e == nil {
			if status.ID != run.ID || status.Result.Disk == nil || *status.Result.Disk != run.Capture.Disk {
				return last, errors.New("media capture identity mismatch")
			}
			lastContact = time.Now()
			last = status.Result
			run.Result = &last
			if e = m.DB.SaveRun(ctx, run); e != nil {
				return last, e
			}
			event := status.Phase
			if last.ProgressPercent != nil {
				event += "/" + strconv.Itoa(int(*last.ProgressPercent)/5*5) + "%"
			}
			if event != lastEvent {
				if e = m.DB.Audit(ctx, "media/"+run.Job.MediaID, "capture.progress", run.ID, event); e != nil {
					return last, e
				}
				lastEvent = event
			}
			if status.Phase == "complete" && last.Verified {
				return last, nil
			}
			if status.Phase == "error" {
				return last, errors.New("media capture failed: " + status.Error)
			}
			if status.Phase == "uploading" && time.Since(status.UpdatedAt) > 15*time.Minute {
				return last, errors.New("disk upload inactive for 15 minutes")
			}
		} else if time.Since(lastContact) > 30*time.Second {
			return last, fmt.Errorf("media capture status unavailable: %w", e)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-tick.C:
		}
	}
}

// Capture issuance and web diagnostics use this bounded name validation.
func captureName(s string) bool { return strings.TrimSpace(s) != "" && len(s) <= 200 }
