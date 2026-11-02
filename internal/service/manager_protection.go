// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"errors"
	"gdata/srk/internal/catalog"
	"gdata/srk/internal/model"
	"gdata/srk/internal/protection"
	"gdata/srk/internal/removable"
	"gdata/srk/internal/tape"
	"net/http"
	"sort"
	"sync"
	"time"
)

type protectionObservation struct {
	MediaID    string             `json:"media_id"`
	Repository inventoryResponse  `json:"repository"`
	Tape       *tape.Library      `json:"tape,omitempty"`
	Removable  []removable.Status `json:"removable,omitempty"`
	Errors     []string           `json:"errors"`
}

func (m *Manager) protectionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/protection/settings", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		p, e := m.DB.ProtectionSettings(r.Context())
		if e != nil {
			Fail(w, 500, e)
			return
		}
		if !currentUser(r).Global() {
			jobs, err := m.DB.Jobs(r.Context())
			if err != nil {
				Fail(w, 503, errors.New("scoped settings unavailable"))
				return
			}
			runs, err := m.DB.ProtectionRuns(r.Context())
			if err != nil {
				Fail(w, 503, errors.New("scoped settings unavailable"))
				return
			}
			p = scopeProtectionSettings(currentUser(r), p, jobs)
			copies, checks, originals := protectionEvidence(jobs, runs, p, nil, time.Now().UTC())
			p, _, _, _ = projectProtection(currentUser(r), p, originals, copies, checks)
		}
		JSON(w, 200, p)
	}))
	mux.HandleFunc("PUT /v1/protection/settings", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		var p model.ProtectionSettings
		if e := Decode(w, r, &p); e != nil {
			Fail(w, 400, e)
			return
		}
		if e := p.Validate(); e != nil {
			Fail(w, 400, e)
			return
		}
		for _, target := range p.RestoreTargets {
			n, e := m.DB.Node(r.Context(), target.NodeID)
			role := "agent"
			if target.VM != nil {
				role = "proxy"
			}
			if e != nil || n.Role != role {
				Fail(w, 400, errors.New("registered restore target node required"))
				return
			}
		}
		for _, policy := range p.Policies {
			if _, e := m.DB.Job(r.Context(), policy.JobID); e != nil {
				Fail(w, 400, errors.New("unknown protection job"))
				return
			}
		}
		for _, s := range p.Storages {
			if s.Kind != "longhorn" {
				n, e := m.DB.Node(r.Context(), s.MediaID)
				if e != nil || n.Role != "media" {
					Fail(w, 400, errors.New("registered media server required"))
					return
				}
			}
		}
		saved, e := m.DB.PutProtectionSettings(r.Context(), p)
		if errors.Is(e, catalog.ErrProtectionConflict) {
			Fail(w, 409, e)
			return
		}
		if e != nil {
			Fail(w, 500, e)
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "protection.settings", "settings", "sites, storage identities and job requirements updated")
		JSON(w, 200, saved)
	}))
	report := m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		p, e := m.DB.ProtectionSettings(ctx)
		if e != nil {
			Fail(w, 500, e)
			return
		}
		jobs, e := m.DB.Jobs(ctx)
		if e != nil {
			Fail(w, 500, e)
			return
		}
		runs, e := m.DB.ProtectionRuns(ctx)
		if e != nil {
			Fail(w, 500, e)
			return
		}
		nodes, e := m.DB.Nodes(ctx)
		if e != nil {
			Fail(w, 500, e)
			return
		}
		u := currentUser(r)
		if !u.Global() {
			p = scopeProtectionSettings(u, p, jobs)
			knownMedia := map[string]bool{}
			for _, run := range runs {
				if run.Kind == "backup" && run.Status == "success" {
					knownMedia[run.Job.MediaID] = true
				}
			}
			selected := []model.Node{}
			for _, n := range nodes {
				for _, store := range p.Storages {
					if store.MediaID == n.ID && knownMedia[n.ID] {
						selected = append(selected, n)
						break
					}
				}
			}
			nodes = selected
			filtered := []model.Run{}
			for _, run := range runs {
				if run.Kind == "restore" && run.RestoreSiteID != "" && !visibleSite(u, run.RestoreSiteID) {
					continue
				}
				filtered = append(filtered, run)
			}
			runs = filtered
		}
		observations := m.protectionInventories(ctx, nodes, p)
		copies, restores, originalJobs := protectionEvidence(jobs, runs, p, observations, time.Now().UTC())
		p, originalJobs, copies, restores = projectProtection(u, p, originalJobs, copies, restores)
		now := time.Now().UTC()
		discovery := []map[string]any{}
		for _, o := range observations {
			if !u.Global() {
				points := map[string]bool{}
				for _, cp := range copies {
					if cp.MediaID == o.MediaID {
						points[cp.SnapshotID] = true
					}
				}
				if len(points) > 0 {
					discovery = append(discovery, map[string]any{"media_id": o.MediaID, "snapshot_count": len(points), "errors": []string{}})
				}
				continue
			}
			entry := map[string]any{"media_id": o.MediaID, "repository_id": o.Repository.Repository.ID, "snapshot_count": len(o.Repository.Snapshots), "errors": o.Errors}
			if o.Tape != nil {
				entry["library_id"] = o.Tape.LibraryID
				entry["tape_backend"] = o.Tape.Backend
			}
			volumes := []map[string]string{}
			for _, v := range o.Removable {
				volumes = append(volumes, map[string]string{"id": v.ID, "uuid": v.UUID, "state": v.State})
			}
			entry["volumes"] = volumes
			discovery = append(discovery, entry)
		}
		JSON(w, 200, map[string]any{"at": now, "scope_limited": !u.Global(), "settings": p, "cards": protection.Evaluate(now, originalJobs, p, copies, restores, len(runs) > 50000), "observations": discovery, "history_truncated": len(runs) > 50000})
	})
	mux.HandleFunc("GET /v1/protection", report)
	mux.HandleFunc("GET /v1/storage-view", report)
}
func (m *Manager) protectionInventories(ctx context.Context, nodes []model.Node, p model.ProtectionSettings) []protectionObservation {
	out := []protectionObservation{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, node := range nodes {
		if node.Role != "media" {
			continue
		}
		wg.Add(1)
		go func(n model.Node) {
			defer wg.Done()
			o := protectionObservation{MediaID: n.ID, Errors: []string{}, Removable: []removable.Status{}}
			defer func() { mu.Lock(); out = append(out, o); mu.Unlock() }()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				o.Errors = append(o.Errors, "Инвентаризация не завершена")
				return
			}
			poll, stop := context.WithTimeout(ctx, 5*time.Second)
			defer stop()
			n, cl, e := m.mediaClient(poll, n.ID)
			if e != nil {
				o.Errors = append(o.Errors, "Медиасервер недоступен")
				return
			}
			defer cl.CloseIdleConnections()
			if e = Call(poll, cl, "GET", n.URL+"/v1/repository/snapshots?verify=false", nil, &o.Repository); e != nil {
				o.Repository = inventoryResponse{}
				o.Errors = append(o.Errors, "Не подтверждён каталог дискового репозитория")
			}
			needTape, needFlash := false, false
			for _, s := range p.Storages {
				if s.MediaID == n.ID {
					needTape = needTape || s.Kind == "tape"
					needFlash = needFlash || s.Kind == "flash"
				}
			}
			if needTape {
				var lib tape.Library
				if e = Call(poll, cl, "GET", n.URL+"/v1/tape", nil, &lib); e != nil {
					o.Errors = append(o.Errors, "Не подтверждён каталог лент")
				} else {
					o.Tape = &lib
				}
			}
			if needFlash {
				if e = Call(poll, cl, "GET", n.URL+"/v1/removable", nil, &o.Removable); e != nil {
					o.Removable = nil
					o.Errors = append(o.Errors, "Не подтверждён каталог съёмных накопителей")
				}
			}
		}(node)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].MediaID < out[j].MediaID })
	return out
}
func protectionEvidence(jobs []model.Job, runs []model.Run, settings model.ProtectionSettings, observations []protectionObservation, now time.Time) ([]protection.Copy, []protection.Restore, []model.Job) {
	byRun := map[string]model.Run{}
	byPoint := map[string]model.Run{}
	jobSet := map[string]bool{}
	for _, j := range jobs {
		jobSet[j.ID] = true
	}
	for _, r := range runs {
		byRun[r.ID] = r
		if r.Kind == "backup" && r.Status == "success" {
			id := r.SnapshotID
			if id == "" {
				id = r.ID
			}
			key := r.Job.MediaID + "/" + id
			if _, ok := byPoint[key]; !ok {
				byPoint[key] = r
			}
		}
	}
	var root func(model.Run, map[string]bool) string
	root = func(r model.Run, seen map[string]bool) string {
		if seen[r.ID] {
			return ""
		}
		seen[r.ID] = true
		if r.Result != nil && r.Result.Replica != nil {
			if source, ok := byRun[r.Result.Replica.SourceRunID]; ok {
				return root(source, seen)
			}
		}
		if r.Origin != nil && r.Origin.OriginalJobID != "" {
			if jobSet[r.Origin.OriginalJobID] {
				return r.Origin.OriginalJobID
			}
			return ""
		}
		if jobSet[r.JobID] {
			return r.JobID
		}
		return ""
	}
	synthetic := map[string]bool{}
	for _, r := range runs {
		if r.Kind == "backup" && r.Origin != nil && root(r, map[string]bool{}) != r.JobID {
			synthetic[r.JobID] = true
		}
	}
	originals := []model.Job{}
	for _, j := range jobs {
		if !synthetic[j.ID] {
			originals = append(originals, j)
		}
	}
	copies := []protection.Copy{}
	restores := []protection.Restore{}
	storageFor := func(kind, media, identity, library string) string {
		for _, s := range settings.Storages {
			if s.Kind != kind || s.MediaID != media {
				continue
			}
			if kind == "repository" && s.RepositoryID == identity || kind == "tape" && s.Barcode == identity && s.LibraryID == library || kind == "flash" && s.VolumeID == identity {
				return s.ID
			}
		}
		return ""
	}
	metadata := func(cp *protection.Copy, r model.Run) {
		if r.Result != nil && r.Result.Data != nil && r.Result.Data.Kubernetes != nil {
			k := r.Result.Data.Kubernetes
			cp.Namespaces = k.Namespaces
			cp.Longhorn = k.PVCData == "longhorn-external-backupstore" && k.VolumeBackups > 0
			cp.MetadataOnly = true
		}
	}
	dataAt := func(r model.Run) time.Time {
		if r.Result != nil && r.Result.Replica != nil {
			return r.Result.Replica.CapturedAt
		}
		if r.StartedAt != nil {
			return *r.StartedAt
		}
		return r.CreatedAt
	}
	observed := map[string]bool{}
	for _, o := range observations {
		for _, s := range o.Repository.Snapshots {
			r, ok := byPoint[o.MediaID+"/"+s.ID]
			if !ok {
				continue
			}
			cp := protection.Copy{JobID: root(r, map[string]bool{}), StorageID: storageFor("repository", o.MediaID, o.Repository.Repository.ID, ""), SnapshotID: s.ID, MediaID: o.MediaID, DataAt: s.CreatedAt, Available: s.Hold || s.ExpiresAt.After(now), Verified: r.Result != nil && r.Result.Verified && r.Result.RepositoryID == o.Repository.Repository.ID}
			if r.Result != nil && r.Result.Replica != nil {
				cp.DataAt = r.Result.Replica.CapturedAt
			}
			if !cp.Available {
				cp.Reason = "Срок хранения истёк"
			} else if !cp.Verified {
				cp.Reason = "Нет подтверждённого результата проверки этой копии"
			}
			metadata(&cp, r)
			copies = append(copies, cp)
			observed[o.MediaID+"/"+s.ID] = true
		}
		addTape := func(lib tape.Library, kind, identity string, online bool) {
			for _, medium := range lib.Cartridges {
				ident := identity
				if kind == "tape" {
					ident = medium.Barcode
				}
				for _, c := range medium.Copies {
					r, ok := byPoint[o.MediaID+"/"+c.SnapshotID]
					if !ok {
						continue
					}
					cp := protection.Copy{JobID: root(r, map[string]bool{}), StorageID: storageFor(kind, o.MediaID, ident, lib.LibraryID), SnapshotID: c.SnapshotID, MediaID: o.MediaID, DataAt: dataAt(r), Verified: c.Verified, Available: online && (kind != "tape" || medium.Present) && medium.Known && !medium.Dirty && !medium.Erasing && (c.Hold || c.ExpiresAt.After(now))}
					if !cp.Available {
						cp.Reason = "Носитель недоступен, не подтверждён или срок копии истёк"
					}
					metadata(&cp, r)
					copies = append(copies, cp)
				}
			}
		}
		if o.Tape != nil {
			addTape(*o.Tape, "tape", "", o.Tape.Backend != "emulator")
		}
		for _, v := range o.Removable {
			if v.Catalog != nil {
				addTape(*v.Catalog, "flash", v.UUID, v.State == "connected")
			}
		}
	}
	// Historical receipt is displayed but cannot substitute current inventory.
	for key, r := range byPoint {
		if observed[key] || r.SnapshotState == "deleted" {
			continue
		}
		id := r.SnapshotID
		if id == "" {
			id = r.ID
		}
		repo := ""
		if r.Result != nil {
			repo = r.Result.RepositoryID
		}
		cp := protection.Copy{JobID: root(r, map[string]bool{}), StorageID: storageFor("repository", r.Job.MediaID, repo, ""), SnapshotID: id, MediaID: r.Job.MediaID, DataAt: dataAt(r), Reason: "Наличие копии не подтверждено текущей инвентаризацией"}
		metadata(&cp, r)
		copies = append(copies, cp)
	}
	for _, r := range runs {
		if r.Kind != "restore" || r.Status == "queued" || r.Status == "running" || r.Status == "cancelled" && r.StartedAt == nil {
			continue
		}
		source, ok := byPoint[r.Job.MediaID+"/"+r.SnapshotID]
		job := r.JobID
		if ok {
			job = root(source, map[string]bool{})
		}
		at := r.CreatedAt
		if r.FinishedAt != nil {
			at = *r.FinishedAt
		}
		v := protection.Restore{JobID: job, RunID: r.ID, SnapshotID: r.SnapshotID, MediaID: r.Job.MediaID, At: at, Passed: r.Status == "success" && r.Result != nil && r.Result.Verified}
		if r.KubernetesCheck != nil {
			v.Namespace = r.KubernetesCheck.SourceNamespace
			v.ApplicationCheck = true
			v.Passed = v.Passed && r.Result.KubernetesCheck != nil && r.Result.KubernetesCheck.State == "passed" && r.Result.KubernetesCheck.Cleanup == "complete"
		} else if r.Result != nil && r.Result.KubernetesRestore != nil {
			v.Namespace = r.Result.KubernetesRestore.SourceNamespace
			v.Passed = false
		}
		restores = append(restores, v)
	}
	return copies, restores, originals
}
