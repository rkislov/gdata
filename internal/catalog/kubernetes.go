// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalog

import (
	"context"
	"encoding/json"
	"gdata/srk/internal/model"
)

func (c *Catalog) KubernetesProgress(ctx context.Context, id, agent string, res model.Result) (bool, error) {
	b, e := json.Marshal(res)
	if e != nil {
		return false, e
	}
	tag, e := c.Pool.Exec(ctx, `UPDATE srk_runs SET document=jsonb_set(document,'{result}',$3::jsonb) WHERE id=$1 AND status='running' AND document->'job'->>'agent_id'=$2 AND document->>'kind'='restore' AND document->'job'->>'type' IN ('kubernetes','longhorn')`, id, agent, b)
	return tag.RowsAffected() == 1, e
}
