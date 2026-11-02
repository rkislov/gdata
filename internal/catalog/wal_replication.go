// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"time"
)

func WALPolicyID(p model.WALReplicationPolicy) string {
	return repository.Hash([]byte("wal-replication/v1\x00" + p.SourceRepositoryID + "\x00" + p.SourceMediaID + "\x00" + p.TargetMediaID + "\x00" + p.ArchiveID))[:32]
}
func (c *Catalog) WALPolicies(ctx context.Context) ([]model.WALReplicationPolicy, error) {
	return list[model.WALReplicationPolicy](ctx, c, "SELECT document FROM srk_wal_replications ORDER BY id")
}
func (c *Catalog) PutWALPolicy(ctx context.Context, p model.WALReplicationPolicy) error {
	if p.ID != WALPolicyID(p) || !repository.ValidWALArchiveID(p.ArchiveID) || p.SourceMediaID == p.TargetMediaID || p.IntervalSeconds < 30 || p.IntervalSeconds > 3600 {
		return errors.New("invalid continuous WAL policy")
	}
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	tx, e := c.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `INSERT INTO srk_wal_replications VALUES($1,$2,$3,$4) ON CONFLICT(id) DO UPDATE SET next_run=$2,enabled=$3,document=$4`, p.ID, p.NextRun, p.Enabled, b); e != nil {
		return e
	}
	if !p.Enabled {
		_, e = tx.Exec(ctx, `UPDATE srk_runs SET status='cancelled',document=jsonb_set(jsonb_set(jsonb_set(document,'{status}','"cancelled"'::jsonb),'{finished_at}',to_jsonb(now())),'{error}','"Политика WAL приостановлена до запуска"'::jsonb) WHERE status='queued' AND document->'wal_replication'->>'id'=$1`, p.ID)
		if e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
func (c *Catalog) WALPolicy(ctx context.Context, id string) (model.WALReplicationPolicy, error) {
	var p model.WALReplicationPolicy
	var b []byte
	e := c.Pool.QueryRow(ctx, "SELECT document FROM srk_wal_replications WHERE id=$1", id).Scan(&b)
	if e == nil {
		e = json.Unmarshal(b, &p)
	}
	return p, e
}
func (c *Catalog) ScheduleWALReplications(ctx context.Context, now time.Time) error {
	tx, e := c.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	rows, e := tx.Query(ctx, `SELECT document FROM srk_wal_replications WHERE enabled AND next_run<=$1 ORDER BY next_run LIMIT 100 FOR UPDATE`, now)
	if e != nil {
		return e
	}
	var policies []model.WALReplicationPolicy
	for rows.Next() {
		var b []byte
		var p model.WALReplicationPolicy
		if e = rows.Scan(&b); e != nil {
			rows.Close()
			return e
		}
		if e = json.Unmarshal(b, &p); e != nil {
			rows.Close()
			return e
		}
		policies = append(policies, p)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, p := range policies {
		var active bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM srk_runs WHERE status IN ('queued','running') AND document->'wal_replication'->>'id'=$1)`, p.ID).Scan(&active); e != nil {
			return e
		}
		if active {
			continue
		}
		job := model.Job{ID: p.ID, Name: "Архив WAL · " + p.ArchiveID[:min(12, len(p.ArchiveID))], MediaID: p.TargetMediaID, Type: "postgresql", Source: "wal://" + p.ArchiveID, Mode: "full"}
		r := model.Run{ID: model.ID(), JobID: job.ID, Job: job, Kind: "wal-replicate", Status: "queued", CreatedAt: now, SnapshotID: p.TargetSnapshotID, WALReplication: &p}
		b, _ := json.Marshal(r)
		if _, e = tx.Exec(ctx, `INSERT INTO srk_runs VALUES($1,$2,'queued',$3,NULL,$4)`, r.ID, r.JobID, now, b); e != nil {
			return e
		}
		a := model.Audit{ID: model.ID(), At: now, Actor: "scheduler", Action: "wal.replication.enqueue", Object: r.ID, Result: p.SourceMediaID + " → " + p.TargetMediaID}
		ab, _ := json.Marshal(a)
		if _, e = tx.Exec(ctx, `INSERT INTO srk_audit VALUES($1,$2,$3)`, a.ID, now, ab); e != nil {
			return e
		}
		p.NextRun = now.Add(time.Duration(p.IntervalSeconds) * time.Second)
		pb, _ := json.Marshal(p)
		if _, e = tx.Exec(ctx, `UPDATE srk_wal_replications SET next_run=$2,document=$3 WHERE id=$1`, p.ID, p.NextRun, pb); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
func (c *Catalog) WALReplicationProgress(ctx context.Context, id string, res model.Result) error {
	b, e := json.Marshal(res)
	if e != nil {
		return e
	}
	_, e = c.Pool.Exec(ctx, `UPDATE srk_runs SET document=jsonb_set(document,'{result}',$2::jsonb) WHERE id=$1 AND status='running' AND document->>'kind'='wal-replicate'`, id, b)
	return e
}

func (c *Catalog) LatestWALReplication(ctx context.Context, id string) (*model.Run, error) {
	rows, e := list[model.Run](ctx, c, `SELECT document FROM srk_runs WHERE document->'wal_replication'->>'id'=$1 ORDER BY created_at DESC LIMIT 1`, id)
	if e != nil || len(rows) == 0 {
		return nil, e
	}
	return &rows[0], nil
}
