// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalog

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"gdata/srk/internal/model"
	"time"
)

func (c *Catalog) RegisterImport(ctx context.Context, media, agent, repoID string, points []model.SnapshotInfo) (model.MaintenanceResult, error) {
	report := model.MaintenanceResult{RepositoryID: repoID, At: time.Now().UTC()}
	tx, e := c.Pool.Begin(ctx)
	if e != nil {
		return report, e
	}
	defer tx.Rollback(ctx)
	for _, s := range points {
		if _, e := hex.DecodeString(s.ID); e != nil || len(s.ID) != 32 {
			return report, errors.New("invalid imported snapshot ID")
		}
		if !s.Verified || !s.Hold || !s.ExpiresAt.After(time.Now()) {
			return report, errors.New("import requires verified, retained and held snapshots")
		}
		var exists bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM srk_runs WHERE status IN ('running','success') AND document->>'kind'='backup' AND document->'job'->>'media_id'=$1 AND COALESCE(NULLIF(document->>'snapshot_id',''),id)=$2)`, media, s.ID).Scan(&exists); e != nil {
			return report, e
		}
		if exists {
			if _, e = tx.Exec(ctx, `UPDATE srk_runs SET document=jsonb_set(jsonb_set(jsonb_set(document,'{snapshot_expires_at}',to_jsonb($3::timestamptz)),'{snapshot_state}','"held"'::jsonb),'{result}',COALESCE(NULLIF(document->'result','null'::jsonb),'{}'::jsonb)||jsonb_build_object('repository_id',$4::text)) WHERE status IN ('running','success') AND document->>'kind'='backup' AND document->'job'->>'media_id'=$1 AND COALESCE(NULLIF(document->>'snapshot_id',''),id)=$2`, media, s.ID, s.ExpiresAt, repoID); e != nil {
				return report, e
			}
			report.Skipped++
			continue
		}
		job := model.Job{ID: model.ID(), Name: "Импорт " + s.ID[:12], AgentID: agent, MediaID: media, Source: s.Source, Mode: "full", RetentionDays: 30, Enabled: false, CreatedAt: time.Now().UTC(), Version: 1}
		if s.Consistency == "offline-disk-image" {
			job.Type = "disk-image"
		}
		if s.Result.Data != nil {
			job.Type = s.Result.Data.Kind
		}
		if s.Result.VM != nil {
			job.Type = s.Result.VM.Provider
			job.VM = &model.VMOptions{Consistency: "crash-consistent"}
		}
		if s.Result.PostgreSQL != nil {
			job.Type = "postgresql"
		}
		jb, _ := json.Marshal(job)
		if _, e = tx.Exec(ctx, `INSERT INTO srk_jobs VALUES($1,NULL,false,$2)`, job.ID, jb); e != nil {
			return report, e
		}
		finished := s.CreatedAt
		result := s.Result
		result.Verified = true
		result.RepositoryID = repoID
		run := model.Run{ID: model.ID(), JobID: job.ID, Job: job, Kind: "backup", Status: "success", SnapshotID: s.ID, SnapshotState: "held", SnapshotExpiresAt: &s.ExpiresAt, CreatedAt: s.CreatedAt, StartedAt: &finished, FinishedAt: &finished, Result: &result, Origin: &model.Origin{RepositoryID: repoID, SnapshotID: s.ID, OriginalJobID: s.JobID, ImportedAt: time.Now().UTC()}}
		b, _ := json.Marshal(run)
		if _, e = tx.Exec(ctx, `INSERT INTO srk_runs VALUES($1,$2,'success',$3,$4,$5)`, run.ID, run.JobID, run.CreatedAt, "import/"+repoID+"/"+s.ID+"/"+media, b); e != nil {
			return report, e
		}
		report.Imported++
	}
	if e = tx.Commit(ctx); e != nil {
		return report, e
	}
	return report, nil
}
func (c *Catalog) KnowsRepository(ctx context.Context, media, repoID string) (bool, error) {
	if repoID == "" {
		return false, nil
	}
	var known bool
	e := c.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM srk_runs WHERE status='success' AND document->>'kind'='backup' AND document->'job'->>'media_id'=$1 AND (document->'result'->>'repository_id'=$2 OR document->'origin'->>'repository_id'=$2))`, media, repoID).Scan(&known)
	return known, e
}
func (c *Catalog) SnapshotPolicy(ctx context.Context, media string, s model.SnapshotInfo) error {
	state := "active"
	if s.Hold {
		state = "held"
	} else if !s.ExpiresAt.After(time.Now()) {
		state = "expired"
	}
	_, e := c.Pool.Exec(ctx, `UPDATE srk_runs SET document=jsonb_set(jsonb_set(document,'{snapshot_expires_at}',to_jsonb($3::timestamptz)),'{snapshot_state}',to_jsonb($4::text)) WHERE document->>'kind'='backup' AND document->'job'->>'media_id'=$1 AND COALESCE(NULLIF(document->>'snapshot_id',''),id)=$2`, media, s.ID, s.ExpiresAt, state)
	return e
}
func (c *Catalog) MarkDeleted(ctx context.Context, media string, ids []string) error {
	_, e := c.Pool.Exec(ctx, `UPDATE srk_runs SET document=jsonb_set(document,'{snapshot_state}','"deleted"'::jsonb) WHERE document->>'kind'='backup' AND document->'job'->>'media_id'=$1 AND COALESCE(NULLIF(document->>'snapshot_id',''),id)=ANY($2::text[])`, media, ids)
	return e
}
