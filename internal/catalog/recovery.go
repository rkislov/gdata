// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

type RecoveryState struct {
	PausedJobs       []string   `json:"paused_jobs"`
	PausedWAL        []string   `json:"paused_wal"`
	PausedKubernetes []string   `json:"paused_kubernetes"`
	Required         bool       `json:"required"`
	ArchiveID        string     `json:"archive_id"`
	RestoredAt       time.Time  `json:"restored_at"`
	AcknowledgedAt   *time.Time `json:"acknowledged_at,omitempty"`
}

func (c *Catalog) RecoveryState(ctx context.Context) (RecoveryState, error) {
	var v RecoveryState
	var b []byte
	e := c.Pool.QueryRow(ctx, "SELECT document FROM srk_recovery_state WHERE id=1").Scan(&b)
	if errors.Is(e, pgx.ErrNoRows) {
		return v, nil
	}
	if e == nil {
		e = json.Unmarshal(b, &v)
	}
	return v, e
}
func (c *Catalog) AcknowledgeRecovery(ctx context.Context, id, actor string) error {
	tx, e := c.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	tag, e := tx.Exec(ctx, `UPDATE srk_recovery_state SET document=jsonb_set(jsonb_set(document,'{required}','false'),'{acknowledged_at}',to_jsonb(now())) WHERE id=1 AND document->>'archive_id'=$1 AND document->>'required'='true'`, id)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return errors.New("recovery state changed")
	}
	// Keep schedules paused; acknowledgment permits personal authentication and manual operations.
	b, _ := json.Marshal(map[string]any{"id": id + "-ack", "at": time.Now().UTC(), "actor": actor, "action": "catalog.recovery.acknowledged", "object": id, "result": "schedules remain paused"})
	if _, e = tx.Exec(ctx, "INSERT INTO srk_audit(id,at,document) VALUES($1,now(),$2)", id+"-ack", b); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
