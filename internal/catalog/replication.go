// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"gdata/srk/internal/model"
	"time"
)

func equalReplication(a, b *model.Replication) bool {
	if a == nil || b == nil {
		return a == b
	}
	x, y := *a, *b
	x.CopyRunID = ""
	y.CopyRunID = ""
	return x == y
}
func (c *Catalog) ReplicationProgress(ctx context.Context, id string, res model.Result) error {
	b, e := json.Marshal(res)
	if e != nil {
		return e
	}
	_, e = c.Pool.Exec(ctx, `UPDATE srk_runs SET document=jsonb_set(document,'{result}',$2::jsonb) WHERE id=$1 AND status='running' AND document->>'kind'='replicate'`, id, b)
	return e
}

// Register the independent recovery point and finish its transfer atomically.
func (c *Catalog) CompleteReplication(ctx context.Context, r model.Run) error {
	if r.Replication == nil || r.Result == nil || r.Result.Replica == nil || !r.Result.Verified || r.Status != "success" {
		return errors.New("verified replication outcome required")
	}
	p := r.Replication
	receipt := r.Result.Replica
	if receipt.TargetSnapshotID != r.ID || receipt.TargetRunID != p.CopyRunID || receipt.TargetMediaID != p.TargetMediaID || receipt.RetentionDays != p.RetentionDays {
		return errors.New("replica receipt identity mismatch")
	}
	job := r.Job
	job.ID = p.CopyRunID
	job.MediaID = p.TargetMediaID
	job.Name = "Реплика · " + job.Name
	job.Enabled = false
	job.IntervalSeconds = 0
	job.RetentionDays = p.RetentionDays
	job.CreatedAt = time.Now().UTC()
	job.NextRun = time.Time{}
	job.Mode = "full"
	copy := model.Run{ID: p.CopyRunID, JobID: job.ID, Job: job, Kind: "backup", Status: "success", SnapshotID: r.ID, SnapshotState: "active", SnapshotExpiresAt: &receipt.ExpiresAt, CreatedAt: r.CreatedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Result: r.Result, Origin: &model.Origin{RepositoryID: r.Result.RepositoryID, SnapshotID: r.ID, OriginalJobID: r.JobID, ImportedAt: time.Now().UTC()}}
	tx, e := c.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	jb, _ := json.Marshal(job)
	cb, _ := json.Marshal(copy)
	rb, _ := json.Marshal(r)
	if _, e = tx.Exec(ctx, `INSERT INTO srk_jobs VALUES($1,NULL,false,$2) ON CONFLICT(id) DO NOTHING`, job.ID, jb); e != nil {
		return e
	}
	created, e := tx.Exec(ctx, `INSERT INTO srk_runs VALUES($1,$2,'success',$3,$4,$5) ON CONFLICT(id) DO NOTHING`, copy.ID, copy.JobID, copy.CreatedAt, "replica-point/"+r.ID, cb)
	if e != nil {
		return e
	}
	if created.RowsAffected() == 1 {
		a := model.Audit{ID: model.ID(), At: time.Now().UTC(), Actor: "media/" + p.TargetMediaID, Action: "replica.created", Object: copy.ID, Result: "source " + p.SourceMediaID + "/" + p.SourceSnapshotID + "; operation " + r.ID}
		b, _ := json.Marshal(a)
		if _, e = tx.Exec(ctx, `INSERT INTO srk_audit VALUES($1,$2,$3)`, a.ID, a.At, b); e != nil {
			return e
		}
	}
	if p.IncludeWAL && created.RowsAffected() == 1 && r.Result.PostgreSQL != nil && r.Result.PostgreSQL.ArchiveID != "" {
		now := time.Now().UTC()
		policy := model.WALReplicationPolicy{SourceMediaID: p.SourceMediaID, SourceRepositoryID: receipt.SourceRepositoryID, TargetMediaID: p.TargetMediaID, TargetRepositoryID: r.Result.RepositoryID, TargetRunID: copy.ID, TargetSnapshotID: r.ID, ArchiveID: r.Result.PostgreSQL.ArchiveID, IntervalSeconds: 30, Enabled: true, NextRun: now, CreatedAt: now}
		policy.ID = WALPolicyID(policy)
		b, _ := json.Marshal(policy)
		if _, e = tx.Exec(ctx, `INSERT INTO srk_wal_replications VALUES($1,$2,true,$3) ON CONFLICT(id) DO UPDATE SET next_run=$2,enabled=true,document=$3`, policy.ID, now, b); e != nil {
			return e
		}
	}
	if _, e = tx.Exec(ctx, `UPDATE srk_runs SET status='success',document=$2 WHERE id=$1`, r.ID, rb); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
