// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalog

import (
	"context"
	"encoding/json"
	"gdata/srk/internal/model"
)

func (c *Catalog) TapeProgress(ctx context.Context, id string, res model.Result) error {
	b, e := json.Marshal(res)
	if e != nil {
		return e
	}
	_, e = c.Pool.Exec(ctx, `UPDATE srk_runs SET document=jsonb_set(document,'{result}',$2::jsonb) WHERE id=$1 AND status='running' AND document->'tape' IS NOT NULL`, id, b)
	return e
}

// TapeQueue returns all admitted work plus a bounded history for this library.
func (c *Catalog) TapeQueue(ctx context.Context, media string) ([]model.Run, error) {
	return list[model.Run](ctx, c, `SELECT document FROM (
 SELECT document,created_at FROM srk_runs WHERE document->'tape' IS NOT NULL AND coalesce(document->'tape'->>'volume_id','')='' AND document->'job'->>'media_id'=$1 AND status IN ('queued','running')
 UNION ALL
 (SELECT document,created_at FROM srk_runs WHERE document->'tape' IS NOT NULL AND coalesce(document->'tape'->>'volume_id','')='' AND document->'job'->>'media_id'=$1 AND status NOT IN ('queued','running') ORDER BY created_at DESC LIMIT 50)
 ) AS tape_queue ORDER BY created_at ASC`, media)
}
