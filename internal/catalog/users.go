// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"gdata/srk/internal/model"
)

var ErrUserConflict = errors.New("account changed; reload before saving")

func (c *Catalog) Users(ctx context.Context) ([]model.User, error) {
	return list[model.User](ctx, c, "SELECT document FROM srk_users ORDER BY username")
}
func (c *Catalog) User(ctx context.Context, name string) (u model.User, hash []byte, err error) {
	var b []byte
	err = c.Pool.QueryRow(ctx, "SELECT document,password_hash FROM srk_users WHERE username=$1", name).Scan(&b, &hash)
	if err == nil {
		err = json.Unmarshal(b, &u)
	}
	return
}
func (c *Catalog) PutUser(ctx context.Context, u model.User, hash []byte) (model.User, error) {
	if e := u.Validate(); e != nil {
		return u, e
	}
	expected := u.Version
	u.Version++
	b, e := json.Marshal(u)
	if e != nil {
		return u, e
	}
	if expected == 0 && len(hash) == 0 {
		return u, errors.New("password required for new account")
	}
	var count int64
	if expected == 0 {
		tag, err := c.Pool.Exec(ctx, "INSERT INTO srk_users VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING", u.Username, u.Version, b, hash)
		e = err
		count = tag.RowsAffected()
	} else {
		tag, err := c.Pool.Exec(ctx, "UPDATE srk_users SET version=$2,document=$3,password_hash=CASE WHEN octet_length($4::bytea)>0 THEN $4 ELSE password_hash END WHERE username=$1 AND version=$5", u.Username, u.Version, b, hash, expected)
		e = err
		count = tag.RowsAffected()
	}
	if e != nil {
		return u, e
	}
	if count != 1 {
		return u, ErrUserConflict
	}
	return u, nil
}

// JobScope is set only by manager authorization, never by request parameters.
type jobScopeKey struct{}

func WithJobScope(ctx context.Context, ids []string) context.Context {
	return context.WithValue(ctx, jobScopeKey{}, ids)
}
func JobScope(ctx context.Context) ([]string, bool) {
	ids, ok := ctx.Value(jobScopeKey{}).([]string)
	return ids, ok
}
