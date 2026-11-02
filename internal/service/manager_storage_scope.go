// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"gdata/srk/internal/model"
	"gdata/srk/internal/protection"
	"slices"
	"time"
)

func visibleSite(u model.User, id string) bool {
	return u.Global() || u.AllSites || slices.Contains(u.SiteIDs, id)
}

// This is a projection, never a configuration that can be written back.
func scopeProtectionSettings(u model.User, p model.ProtectionSettings, jobs []model.Job) model.ProtectionSettings {
	if u.Global() {
		return p
	}
	out := model.ProtectionSettings{Sites: []model.ProtectionSite{}, Storages: []model.ProtectionStorage{}, Policies: []model.ProtectionPolicy{}, RestoreTargets: []model.RestoreTarget{}}
	allowed := map[string]bool{}
	for _, j := range jobs {
		allowed[j.ID] = true
	}
	for _, s := range p.Storages {
		if visibleSite(u, s.SiteID) {
			out.Storages = append(out.Storages, s)
		}
	}
	for _, v := range p.Policies {
		if allowed[v.JobID] && visibleSite(u, v.SourceSiteID) {
			if !slices.ContainsFunc(out.Storages, func(s model.ProtectionStorage) bool { return s.ID == v.LonghornStorageID }) {
				v.LonghornStorageID = ""
			}
			out.Policies = append(out.Policies, v)
		}
	}
	for _, v := range p.Sites {
		if visibleSite(u, v.ID) {
			out.Sites = append(out.Sites, v)
		}
	}
	return out
}
func projectProtection(u model.User, p model.ProtectionSettings, jobs []model.Job, copies []protection.Copy, restores []protection.Restore) (model.ProtectionSettings, []model.Job, []protection.Copy, []protection.Restore) {
	if u.Global() {
		return p, jobs, copies, restores
	}
	allowed := map[string]bool{}
	for _, j := range jobs {
		allowed[j.ID] = true
	}
	stores := map[string]bool{}
	for _, s := range p.Storages {
		stores[s.ID] = true
	}
	kept := []protection.Copy{}
	used := map[string]bool{}
	points := map[string]bool{}
	for _, cp := range copies {
		if allowed[cp.JobID] && stores[cp.StorageID] {
			kept = append(kept, cp)
			used[cp.StorageID] = true
			points[cp.JobID+"/"+cp.MediaID+"/"+cp.SnapshotID] = true
		}
	}
	checks := []protection.Restore{}
	for _, r := range restores {
		if points[r.JobID+"/"+r.MediaID+"/"+r.SnapshotID] {
			checks = append(checks, r)
		}
	}
	sites := map[string]bool{}
	for _, policy := range p.Policies {
		sites[policy.SourceSiteID] = true
		if policy.LonghornStorageID != "" {
			used[policy.LonghornStorageID] = true
		}
	}
	out := p
	out.Storages = []model.ProtectionStorage{}
	out.Sites = []model.ProtectionSite{}
	for _, s := range p.Storages {
		if used[s.ID] {
			out.Storages = append(out.Storages, s)
			sites[s.SiteID] = true
		}
	}
	for _, s := range p.Sites {
		if sites[s.ID] {
			out.Sites = append(out.Sites, s)
		}
	}
	projectedJobs := []model.Job{}
	for _, j := range jobs {
		projectedJobs = append(projectedJobs, model.Job{ID: j.ID, Name: j.Name, Type: j.Type})
	}
	return out, projectedJobs, kept, checks
}
func (m *Manager) scopedRepository(ctx context.Context, u model.User, media string, v inventoryResponse) (inventoryResponse, error) {
	jobs, e := m.DB.Jobs(ctx)
	if e != nil {
		return inventoryResponse{}, e
	}
	settings, e := m.DB.ProtectionSettings(ctx)
	if e != nil {
		return inventoryResponse{}, e
	}
	p := scopeProtectionSettings(u, settings, jobs)
	mapped := slices.ContainsFunc(p.Storages, func(s model.ProtectionStorage) bool {
		return s.Kind == "repository" && s.MediaID == media && s.RepositoryID == v.Repository.ID
	})
	out := inventoryResponse{Repository: model.RepositoryInfo{}, Snapshots: []model.SnapshotInfo{}}
	if !mapped {
		return out, nil
	}
	runs, e := m.DB.ProtectionRuns(ctx)
	if e != nil {
		return out, e
	}
	cp, _, originals := protectionEvidence(jobs, runs, p, []protectionObservation{{MediaID: media, Repository: v}}, time.Now().UTC())
	p, originals, cp, _ = projectProtection(u, p, originals, cp, nil)
	owners := map[string]string{}
	names := map[string]string{}
	for _, j := range jobs {
		names[j.ID] = j.Source
	}
	for _, c := range cp {
		for _, r := range runs {
			id := r.SnapshotID
			if id == "" {
				id = r.ID
			}
			if r.Kind == "backup" && r.Status == "success" && r.Job.MediaID == media && id == c.SnapshotID && r.Result != nil && r.Result.RepositoryID == v.Repository.ID {
				owners[c.SnapshotID] = c.JobID
				break
			}
		}
	}
	for _, s := range v.Snapshots {
		job, ok := owners[s.ID]
		if !ok {
			continue
		}
		out.Snapshots = append(out.Snapshots, model.SnapshotInfo{ID: s.ID, JobID: job, Source: names[job], CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt, Mode: s.Mode, Consistency: s.Consistency, Hold: s.Hold, Verified: s.Verified, Result: model.Result{LogicalBytes: s.Result.LogicalBytes}})
	}
	if len(out.Snapshots) > 0 {
		out.Repository = model.RepositoryInfo{ID: v.Repository.ID}
	}
	return out, nil
}
