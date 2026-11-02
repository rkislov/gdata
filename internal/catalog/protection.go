// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"gdata/srk/internal/model"
	"github.com/jackc/pgx/v5"
)

var ErrProtectionConflict = errors.New("protection settings changed; reload before saving")

func (c *Catalog) ProtectionSettings(ctx context.Context) (model.ProtectionSettings, error) {
	p := model.ProtectionSettings{Sites: []model.ProtectionSite{}, Storages: []model.ProtectionStorage{}, Policies: []model.ProtectionPolicy{}}
	var b []byte
	e := c.Pool.QueryRow(ctx, "SELECT document FROM srk_protection_settings WHERE id=1").Scan(&b)
	if errors.Is(e, pgx.ErrNoRows) {
		return p, nil
	}
	if e == nil {
		e = json.Unmarshal(b, &p)
	}
	return p, e
}
func (c *Catalog) PutProtectionSettings(ctx context.Context, p model.ProtectionSettings) (model.ProtectionSettings, error) {
	if e := p.Validate(); e != nil {
		return p, e
	}
	expected := p.Version
	p.Version++
	b, e := json.Marshal(p)
	if e != nil {
		return p, e
	}
	var count int64
	if expected == 0 {
		tag, e := c.Pool.Exec(ctx, "INSERT INTO srk_protection_settings VALUES(1,1,$1) ON CONFLICT DO NOTHING", b)
		if e != nil {
			return p, e
		}
		count = tag.RowsAffected()
	} else {
		tag, e := c.Pool.Exec(ctx, "UPDATE srk_protection_settings SET version=$1,document=$2 WHERE id=1 AND version=$3", p.Version, b, expected)
		if e != nil {
			return p, e
		}
		count = tag.RowsAffected()
	}
	if count != 1 {
		return p, ErrProtectionConflict
	}
	return p, nil
}

type protectionScopeKey struct{}
type protectionReadScope struct {
	Media, Sites []string
	AllSites     bool
}

func WithProtectionScope(ctx context.Context, media, sites []string, allSites bool) context.Context {
	return context.WithValue(ctx, protectionScopeKey{}, protectionReadScope{media, sites, allSites})
}
func (c *Catalog) ProtectionRuns(ctx context.Context) ([]model.Run, error) {
	where := `document->>'kind' IN ('backup','restore','tape-write','tape-restore','replicate')`
	args := []any{}
	if ids, ok := JobScope(ctx); ok {
		args = append(args, ids)
		where += ` AND job_id IN (SELECT job_id FROM owned)`
		if scope, ok := ctx.Value(protectionScopeKey{}).(protectionReadScope); ok {
			args = append(args, scope.Media)
			where += ` AND document->'job'->>'media_id'=ANY($2::text[])`
			if !scope.AllSites {
				args = append(args, scope.Sites)
				where += ` AND (document->>'kind'<>'restore' OR document->>'restore_site_id'=ANY($3::text[]))`
			}
		}
	}
	if _, ok := JobScope(ctx); ok {
		// Resolve ownership through replica/import lineage before applying visibility
		// and the history ceiling. Hidden intermediate copies are never returned.
		prefix := `WITH RECURSIVE owned(job_id,root) AS (
          SELECT id,id FROM unnest($1::text[]) AS id
          UNION
          SELECT r.job_id,o.root FROM srk_runs r JOIN owned o ON r.document->'origin'->>'original_job_id'=o.job_id
          WHERE r.document->>'kind'='backup' AND r.status='success'
        ) `
		projection := `CASE WHEN jsonb_typeof(document->'origin')='object' THEN jsonb_set(document,'{origin,original_job_id}',to_jsonb((SELECT root FROM owned WHERE owned.job_id=srk_runs.job_id ORDER BY (root=srk_runs.job_id) DESC,root LIMIT 1))) ELSE document END`
		return list[model.Run](ctx, c, prefix+"SELECT "+projection+" FROM srk_runs WHERE "+where+" ORDER BY created_at DESC LIMIT 50001", args...)
	}
	return list[model.Run](ctx, c, "SELECT document FROM srk_runs WHERE "+where+" ORDER BY created_at DESC LIMIT 50001", args...)
}
