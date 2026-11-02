// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"gdata/srk/internal/model"
	"github.com/jackc/pgx/v5"
	"time"
)

func (c *Catalog) KubernetesChecks(ctx context.Context) ([]model.KubernetesCheckPolicy, error) {
	return list[model.KubernetesCheckPolicy](ctx, c, "SELECT document FROM srk_kubernetes_checks ORDER BY id")
}
func (c *Catalog) PutKubernetesCheck(ctx context.Context, p model.KubernetesCheckPolicy) error {
	if e := p.Validate(); e != nil {
		return e
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
	_, e = tx.Exec(ctx, `INSERT INTO srk_kubernetes_checks VALUES($1,$2,$3,$4) ON CONFLICT(id) DO UPDATE SET next_run=$2,enabled=$3,document=$4`, p.ID, p.NextRun, p.Enabled, b)
	if e != nil {
		return e
	}
	if !p.Enabled {
		_, e = tx.Exec(ctx, `UPDATE srk_runs SET status='cancelled',document=jsonb_set(jsonb_set(jsonb_set(document,'{status}','"cancelled"'::jsonb),'{finished_at}',to_jsonb(now())),'{error}','"Verification policy paused before execution"'::jsonb) WHERE status='queued' AND document->'kubernetes_check'->>'id'=$1`, p.ID)
		if e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
func (c *Catalog) LatestKubernetesCheck(ctx context.Context, id string) (*model.Run, error) {
	rows, e := list[model.Run](ctx, c, `SELECT document FROM srk_runs WHERE document->'kubernetes_check'->>'id'=$1 ORDER BY created_at DESC LIMIT 1`, id)
	if e != nil || len(rows) == 0 {
		return nil, e
	}
	return &rows[0], nil
}
func (c *Catalog) ScheduleKubernetesChecks(ctx context.Context, now time.Time) error {
	tx, e := c.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	rows, e := tx.Query(ctx, `SELECT document FROM srk_kubernetes_checks WHERE enabled AND next_run<=$1 ORDER BY next_run LIMIT 100 FOR UPDATE`, now)
	if e != nil {
		return e
	}
	var policies []model.KubernetesCheckPolicy
	for rows.Next() {
		var b []byte
		var p model.KubernetesCheckPolicy
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
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM srk_runs WHERE status IN ('queued','running') AND document->'kubernetes_check'->>'id'=$1)`, p.ID).Scan(&active); e != nil {
			return e
		}
		if active {
			continue
		}
		var jb []byte
		if e = tx.QueryRow(ctx, "SELECT document FROM srk_jobs WHERE id=$1", p.JobID).Scan(&jb); e != nil {
			return e
		}
		var job model.Job
		if e = json.Unmarshal(jb, &job); e != nil {
			return e
		}
		id := model.ID()
		run := model.Run{ID: id, JobID: job.ID, Job: job, Kind: "restore", Status: "queued", CreatedAt: now, KubernetesCheck: &p, Destination: "kubernetes://" + p.Profile + "/" + p.SourceNamespace + "/" + p.NamespacePrefix + id + "?start=true"}
		var sourceJSON []byte
		e = tx.QueryRow(ctx, `SELECT document FROM srk_runs WHERE job_id=$1 AND status='success' AND document->>'kind'='backup' AND document->'result'->>'verified'='true' AND document->>'snapshot_state'='active' AND (document->>'snapshot_expires_at')::timestamptz>$2 AND document->'result'->'data'->'kubernetes'->'namespaces' ? $3 ORDER BY created_at DESC LIMIT 1`, p.JobID, now, p.SourceNamespace).Scan(&sourceJSON)
		if errors.Is(e, pgx.ErrNoRows) {
			run.Status = "error"
			run.Error = "No available verified backup for scheduled Kubernetes check"
			run.FinishedAt = &now
			run.Result = &model.Result{KubernetesCheck: &model.KubernetesCheckResult{PolicyID: p.ID, State: "failed", Cleanup: "not-needed", Error: run.Error, StartedAt: now, FinishedAt: &now}}
		} else if e != nil {
			return e
		} else {
			var source model.Run
			if e = json.Unmarshal(sourceJSON, &source); e != nil {
				return e
			}
			// Use the source snapshot's worker/media identity, not subsequently edited job routing.
			run.Job = source.Job
			run.SnapshotID = source.SnapshotID
			if run.SnapshotID == "" {
				run.SnapshotID = source.ID
			}
		}
		b, _ := json.Marshal(run)
		if _, e = tx.Exec(ctx, `INSERT INTO srk_runs VALUES($1,$2,$3,$4,NULL,$5)`, run.ID, run.JobID, run.Status, now, b); e != nil {
			return e
		}
		a := model.Audit{ID: model.ID(), At: now, Actor: "scheduler", Action: "kubernetes.check.enqueue", Object: run.ID, Result: run.Status}
		ab, _ := json.Marshal(a)
		if _, e = tx.Exec(ctx, "INSERT INTO srk_audit VALUES($1,$2,$3)", a.ID, now, ab); e != nil {
			return e
		}
		p.NextRun = now.Add(time.Duration(p.IntervalSeconds) * time.Second)
		pb, _ := json.Marshal(p)
		if _, e = tx.Exec(ctx, "UPDATE srk_kubernetes_checks SET next_run=$2,document=$3 WHERE id=$1", p.ID, p.NextRun, pb); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
