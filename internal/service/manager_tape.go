// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"gdata/srk/internal/tape"
	"net/http"
	"net/url"
	"time"
)

func (m *Manager) tapeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /v1/media/{media}/tape/pools/{pool}", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		var p tape.Pool
		if e := Decode(w, r, &p); e != nil {
			Fail(w, 400, e)
			return
		}
		if p.ID != r.PathValue("pool") {
			Fail(w, 400, errors.New("pool ID mismatch"))
			return
		}
		n, cl, e := m.mediaClient(r.Context(), r.PathValue("media"))
		if e != nil {
			Fail(w, 409, e)
			return
		}
		defer cl.CloseIdleConnections()
		if e = m.DB.Audit(r.Context(), actor, "tape.pool.update.requested", p.ID, n.ID); e != nil {
			Fail(w, 500, e)
			return
		}
		var out tape.Pool
		if e = Call(r.Context(), cl, "PUT", n.URL+"/v1/tape/pools/"+url.PathEscape(p.ID), p, &out); e != nil {
			Fail(w, 422, e)
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "tape.pool.updated", p.ID, n.ID)
		JSON(w, 200, out)
	}))

	mux.HandleFunc("GET /v1/media/{media}/tape/queue", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		runs, e := m.DB.TapeQueue(r.Context(), r.PathValue("media"))
		if e != nil {
			Fail(w, 500, e)
			return
		}
		JSON(w, 200, runs)
	}))
	mux.HandleFunc("GET /v1/media/{media}/tape", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		n, cl, e := m.mediaClient(r.Context(), r.PathValue("media"))
		if e != nil {
			Fail(w, 409, e)
			return
		}
		defer cl.CloseIdleConnections()
		var v tape.Library
		if e = Call(r.Context(), cl, "GET", n.URL+"/v1/tape", nil, &v); e != nil {
			Fail(w, 409, e)
			return
		}
		JSON(w, 200, v)
	}))
	enqueueTape := m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		var req model.TapeRequest
		if e := Decode(w, r, &req); e != nil {
			Fail(w, 400, e)
			return
		}
		if r.PathValue("volume") != "" {
			req.VolumeID = r.PathValue("volume")
			req.Barcode = req.VolumeID
		}
		checked := req
		if req.VolumeID != "" {
			if req.Confirm != "" && req.Confirm != req.VolumeID {
				Fail(w, 400, errors.New("volume confirmation mismatch"))
				return
			}
			checked.Barcode = "FLASH"
			if checked.Confirm != "" {
				checked.Confirm = "FLASH"
			}
			if checked.Action == "attach" || checked.Action == "release" {
				checked.Action = "verify"
			}
		}
		if e := tape.ValidateRequest(checked); e != nil {
			Fail(w, 400, e)
			return
		}
		n, cl, e := m.mediaClient(r.Context(), r.PathValue("media"))
		if e != nil {
			Fail(w, 409, e)
			return
		}
		if req.VolumeID == "" && req.PoolID == "" && req.DriveID == "" {
			var lib tape.Library
			e = Call(r.Context(), cl, "GET", n.URL+"/v1/tape", nil, &lib)
			if e != nil {
				cl.CloseIdleConnections()
				Fail(w, 409, e)
				return
			}
			if lib.Backend == "scsi" {
				for _, c := range lib.Cartridges {
					if c.Barcode == req.Barcode {
						req.DriveID = c.DriveID
					}
				}
				if req.Action == "recover" {
					for _, p := range lib.Positions {
						if p.Barcode == req.Barcode {
							req.DriveID = p.Drive
						}
					}
				}
				if req.DriveID == "" {
					cl.CloseIdleConnections()
					Fail(w, 409, errors.New("load cartridge into an explicit drive first"))
					return
				}
			}
		}
		cl.CloseIdleConnections()
		resource := req.Barcode
		if req.PoolID != "" {
			resource = "pool/" + req.PoolID
		}
		job := model.Job{ID: "tape/" + n.ID + "/" + resource, MediaID: n.ID, Name: "Лента · " + req.Action + " · " + resource, Source: "tape://" + n.ID + "/" + resource, Mode: "full"}
		if req.Action == "write" {
			var point model.Run
			point, e = m.DB.Run(r.Context(), req.SnapshotID)
			if e != nil || point.Status != "success" || point.Kind != "backup" || point.Job.MediaID != n.ID {
				Fail(w, 409, errors.New("successful backup on this media server required"))
				return
			}
			req.SnapshotID = point.SnapshotID
			if req.SnapshotID == "" {
				req.SnapshotID = point.ID
			}
		}
		if req.Action == "restore" {
			target, e := m.DB.Node(r.Context(), req.AgentID)
			if e != nil || (target.Role != "agent" && target.Role != "proxy") {
				Fail(w, 400, errors.New("registered restore agent or VM proxy required"))
				return
			}
		}
		run := model.Run{ID: model.ID(), JobID: job.ID, Job: job, Kind: "tape-" + req.Action, Tape: &req, Status: "queued", CreatedAt: time.Now().UTC()}
		m.operationMu.Lock()
		defer m.operationMu.Unlock()
		run, e = m.DB.Enqueue(r.Context(), run, "tape/"+n.ID+"/"+idempotency(r))
		if e != nil {
			Fail(w, 409, e)
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "tape."+req.Action+".queued", run.ID, req.Barcode+"; "+req.Reason)
		JSON(w, 202, run)
	})
	mux.HandleFunc("POST /v1/media/{media}/tape/operations", enqueueTape)
	mux.HandleFunc("POST /v1/media/{media}/removable/{volume}/operations", enqueueTape)
	mux.HandleFunc("GET /v1/media/{media}/removable", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		n, cl, e := m.mediaClient(r.Context(), r.PathValue("media"))
		if e != nil {
			Fail(w, 409, e)
			return
		}
		defer cl.CloseIdleConnections()
		var out any
		if e = Call(r.Context(), cl, "GET", n.URL+"/v1/removable", nil, &out); e != nil {
			Fail(w, 409, e)
			return
		}
		JSON(w, 200, out)
	}))
}
func (m *Manager) executeTape(ctx context.Context, r model.Run) (model.Result, error) {
	n, cl, e := m.mediaClient(ctx, r.Job.MediaID)
	if e != nil {
		return model.Result{}, e
	}
	defer cl.CloseIdleConnections()
	type outcome struct {
		res model.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		var res model.Result
		endpoint := n.URL + "/v1/tape/operations/" + r.ID
		if r.Tape.VolumeID != "" {
			endpoint = n.URL + "/v1/removable/" + url.PathEscape(r.Tape.VolumeID) + "/operations/" + r.ID
		}
		e := Call(ctx, cl, "POST", endpoint, r.Tape, &res)
		done <- outcome{res, e}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := ""
	for {
		select {
		case out := <-done:
			if out.err == nil && (!out.res.Verified || out.res.Tape == nil || (r.Tape.PoolID == "" && out.res.Tape.Barcode != r.Tape.Barcode) || (out.res.Tape != nil && r.Tape.PoolID != "" && (out.res.Tape.PoolID != r.Tape.PoolID || out.res.Tape.Barcode == ""))) {
				return out.res, errors.New("invalid tape operation receipt")
			}
			if out.err == nil && r.Tape.Action == "restore" {
				info := out.res.Tape.Snapshot
				if info == nil || info.ID != r.ID || !info.Verified {
					return out.res, errors.New("tape restore point not confirmed")
				}
				target, e := m.DB.Node(ctx, r.Tape.AgentID)
				if e != nil {
					return out.res, e
				}
				expected := "agent"
				if info.Result.VM != nil {
					expected = "proxy"
				}
				if target.Role != expected {
					return out.res, errors.New("restored backup needs a different agent role; register the held point through repository import")
				}
				if _, e = m.DB.RegisterImport(ctx, n.ID, target.ID, out.res.RepositoryID, []model.SnapshotInfo{*info}); e != nil {
					return out.res, e
				}
			}
			return out.res, out.err
		case <-ctx.Done():
			return model.Result{}, ctx.Err()
		case <-tick.C:
			poll, cancel := context.WithTimeout(ctx, 2*time.Second)
			var res model.Result
			if Call(poll, cl, "GET", n.URL+"/v1/tape/operations/"+url.PathEscape(r.ID), nil, &res) == nil {
				res.Verified = false
				_ = m.DB.TapeProgress(ctx, r.ID, res)
				key := res.Phase
				if res.ProgressPercent != nil {
					key += fmt.Sprintf("/%d%%", int(*res.ProgressPercent)/5*5)
				}
				if key != last {
					_ = m.DB.Audit(ctx, "media/"+n.ID, "tape.progress", r.ID, key)
					last = key
				}
			}
			cancel()
		}
	}
}
