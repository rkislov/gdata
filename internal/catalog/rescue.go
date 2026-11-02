// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/rescue"
	"time"
)

const RescueSchemaSQL = `CREATE TABLE IF NOT EXISTS srk_rescue_tokens(hash text PRIMARY KEY,kind text NOT NULL,scope_id text NOT NULL,expires_at timestamptz NOT NULL,document jsonb NOT NULL); CREATE INDEX IF NOT EXISTS srk_rescue_scope ON srk_rescue_tokens(scope_id);`

func (c *Catalog) InitRescue(ctx context.Context) error {
	_, e := c.Pool.Exec(ctx, RescueSchemaSQL)
	return e
}
func (c *Catalog) RescueIssue(ctx context.Context, hash string, s rescue.Scope) error {
	if _, e := c.Pool.Exec(ctx, `DELETE FROM srk_rescue_tokens WHERE expires_at < now()`); e != nil {
		return e
	}
	b, e := json.Marshal(s)
	if e != nil {
		return e
	}
	_, e = c.Pool.Exec(ctx, `INSERT INTO srk_rescue_tokens VALUES($1,'code',$2,$3,$4)`, hash, s.ID, time.Now().Add(10*time.Minute), b)
	return e
}
func (c *Catalog) RescueConsume(ctx context.Context, hash, sessionHash string) (rescue.Scope, error) {
	var s rescue.Scope
	tx, e := c.Pool.Begin(ctx)
	if e != nil {
		return s, e
	}
	defer tx.Rollback(ctx)
	var b []byte
	e = tx.QueryRow(ctx, `DELETE FROM srk_rescue_tokens WHERE hash=$1 AND kind='code' AND expires_at>now() RETURNING document`, hash).Scan(&b)
	if e != nil {
		return s, errors.New("код истек или уже использован")
	}
	if e = json.Unmarshal(b, &s); e != nil {
		return s, e
	}
	if !s.ExpiresAt.After(time.Now()) {
		return s, errors.New("scope expired")
	}
	_, e = tx.Exec(ctx, `INSERT INTO srk_rescue_tokens VALUES($1,'session',$2,$3,$4)`, sessionHash, s.ID, s.ExpiresAt, b)
	if e != nil {
		return s, e
	}
	return s, tx.Commit(ctx)
}
func (c *Catalog) RescueScope(ctx context.Context, hash string) (rescue.Scope, error) {
	var s rescue.Scope
	var b []byte
	e := c.Pool.QueryRow(ctx, `SELECT document FROM srk_rescue_tokens WHERE hash=$1 AND kind='session' AND expires_at>now()`, hash).Scan(&b)
	if e != nil {
		return s, errors.New("rescue session unavailable")
	}
	e = json.Unmarshal(b, &s)
	return s, e
}
func (c *Catalog) RescueRevoke(ctx context.Context, id string) error {
	_, e := c.Pool.Exec(ctx, `DELETE FROM srk_rescue_tokens WHERE scope_id=$1`, id)
	return e
}

func (c *Catalog) RescueActive(ctx context.Context, id string) error {
	var active bool
	e := c.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM srk_rescue_tokens WHERE kind='session' AND scope_id=$1 AND expires_at>now())`, id).Scan(&active)
	if e != nil {
		return e
	}
	if !active {
		return errors.New("rescue access expired or revoked")
	}
	return nil
}

func (c *Catalog) RunEvents(ctx context.Context, id string) ([]model.Audit, error) {
	return list[model.Audit](ctx, c, `SELECT document FROM srk_audit WHERE document->>'object'=$1 ORDER BY at DESC LIMIT 200`, id)
}
