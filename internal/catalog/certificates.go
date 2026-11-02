// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalog

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"gdata/srk/internal/model"
	"github.com/jackc/pgx/v5"
	"time"
)

func (c *Catalog) CertificateState(ctx context.Context, user string) (model.CertificateState, error) {
	out := model.CertificateState{Certificates: []model.PersonalCertificate{}}
	e := c.Pool.QueryRow(ctx, "SELECT version FROM srk_user_certificate_state WHERE username=$1", user).Scan(&out.Version)
	if errors.Is(e, pgx.ErrNoRows) {
		return out, nil
	}
	if e != nil {
		return out, e
	}
	out.Managed = true
	out.Certificates, e = list[model.PersonalCertificate](ctx, c, "SELECT document FROM srk_user_certificates WHERE username=$1 ORDER BY issued_at DESC", user)
	return out, e
}
func (c *Catalog) PersonalCertificateAllowed(ctx context.Context, user string, leaf *x509.Certificate) bool {
	var managed bool
	if e := c.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM srk_user_certificate_state WHERE username=$1)", user).Scan(&managed); e != nil {
		return false
	}
	if !managed {
		return time.Now().Before(leaf.NotAfter)
	}
	var b []byte
	if e := c.Pool.QueryRow(ctx, "SELECT document FROM srk_user_certificates WHERE username=$1 AND serial=$2", user, leaf.SerialNumber.String()).Scan(&b); e != nil {
		return false
	}
	var v model.PersonalCertificate
	if json.Unmarshal(b, &v) != nil {
		return false
	}
	sum := sha256.Sum256(leaf.Raw)
	return !v.Revoked && time.Now().Before(v.RetireAt) && time.Now().Before(v.ExpiresAt) && v.Fingerprint == hex.EncodeToString(sum[:])
}

// Account row lock serializes rotations, revocations and account changes. A
// stale generation or signing failure cannot retire the current certificate.
func (c *Catalog) ChangeCertificates(ctx context.Context, user string, expected int, next *model.PersonalCertificate, overlap time.Duration, revoke string) error {
	tx, e := c.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var raw []byte
	if e = tx.QueryRow(ctx, "SELECT document FROM srk_users WHERE username=$1 FOR UPDATE", user).Scan(&raw); e != nil {
		return e
	}
	var u model.User
	if e = json.Unmarshal(raw, &u); e != nil {
		return e
	}
	if u.Disabled && next != nil {
		return errors.New("account disabled")
	}
	var version int
	e = tx.QueryRow(ctx, "SELECT version FROM srk_user_certificate_state WHERE username=$1", user).Scan(&version)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return e
	}
	if version != expected {
		return ErrUserConflict
	}
	if next != nil {
		var reused bool
		if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM srk_user_certificates WHERE key_fingerprint=$1)", next.KeyFingerprint).Scan(&reused); e != nil {
			return e
		}
		if reused {
			return errors.New("rotation requires a fresh private key")
		}
		deadline := time.Now().UTC().Add(overlap)
		_, e = tx.Exec(ctx, `UPDATE srk_user_certificates SET document=jsonb_set(document,'{retire_at}',to_jsonb($2::timestamptz)) WHERE username=$1 AND (document->>'retire_at')::timestamptz>$2`, user, deadline)
		if e != nil {
			return e
		}
		b, _ := json.Marshal(next)
		_, e = tx.Exec(ctx, "INSERT INTO srk_user_certificates(serial,username,key_fingerprint,issued_at,document) VALUES($1,$2,$3,$4,$5)", next.Serial, user, next.KeyFingerprint, next.IssuedAt, b)
	} else {
		tag, err := tx.Exec(ctx, `UPDATE srk_user_certificates SET document=jsonb_set(document,'{revoked}','true') WHERE username=$1 AND serial=$2`, user, revoke)
		e = err
		if e == nil && tag.RowsAffected() != 1 {
			return errors.New("certificate not found")
		}
	}
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO srk_user_certificate_state VALUES($1,$2) ON CONFLICT(username) DO UPDATE SET version=$2`, user, version+1)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
