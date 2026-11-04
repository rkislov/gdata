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

// A retry with the same pending key and predecessor retrieves the exact issued
// certificate. A retired credential cannot obtain another key or generation.
func (c *Catalog) RenewServiceCertificate(ctx context.Context, previousFingerprint string, next model.ServiceCertificate, issue func() (model.ServiceCertificate, error)) (model.ServiceCertificate, error) {
	tx, e := c.Pool.Begin(ctx)
	if e != nil {
		return next, e
	}
	defer tx.Rollback(ctx)
	_, e = tx.Exec(ctx, "INSERT INTO srk_service_certificates(role,id,document) VALUES($1,$2,'null') ON CONFLICT DO NOTHING", next.Role, next.ID)
	if e != nil {
		return next, e
	}
	var b []byte
	if e = tx.QueryRow(ctx, "SELECT document FROM srk_service_certificates WHERE role=$1 AND id=$2 FOR UPDATE", next.Role, next.ID).Scan(&b); e != nil {
		return next, e
	}
	if string(b) != "null" {
		var old model.ServiceCertificate
		if e = json.Unmarshal(b, &old); e != nil {
			return next, e
		}
		if old.PreviousFingerprint == previousFingerprint && old.KeyFingerprint == next.KeyFingerprint {
			if time.Now().After(old.ExpiresAt) {
				return next, errors.New("issued certificate expired; administrative recovery required")
			}
			return old, tx.Commit(ctx)
		}
		if old.Fingerprint != previousFingerprint {
			return next, errors.New("superseded certificate cannot renew another key")
		}
		if old.KeyFingerprint == next.KeyFingerprint {
			return next, errors.New("fresh private key required")
		}
	}
	next, e = issue()
	if e != nil {
		return next, e
	}
	next.PreviousFingerprint = previousFingerprint
	b, e = json.Marshal(next)
	if e != nil {
		return next, e
	}
	if _, e = tx.Exec(ctx, "UPDATE srk_service_certificates SET document=$3 WHERE role=$1 AND id=$2", next.Role, next.ID, b); e != nil {
		return next, e
	}
	// Issuance and audit commit together. Public certificate only, no CSR or key.
	event := model.Audit{ID: model.ID(), At: time.Now().UTC(), Actor: next.Role + "/" + next.ID, Action: "service_certificate.issued", Object: next.Role + "/" + next.ID, Result: next.Serial}
	raw, _ := json.Marshal(event)
	if _, e = tx.Exec(ctx, "INSERT INTO srk_audit VALUES($1,$2,$3)", event.ID, event.At, raw); e != nil {
		return next, e
	}
	return next, tx.Commit(ctx)
}

func (c *Catalog) NotifyCertificate(ctx context.Context, key, object, state string) (bool, error) {
	event := model.Audit{ID: key, At: time.Now().UTC(), Actor: "system/certificates", Action: "certificate.alert", Object: object, Result: state}
	b, _ := json.Marshal(event)
	tag, e := c.Pool.Exec(ctx, "INSERT INTO srk_audit VALUES($1,$2,$3) ON CONFLICT(id) DO NOTHING", event.ID, event.At, b)
	return e == nil && tag.RowsAffected() == 1, e
}

func (c *Catalog) ServiceCertificates(ctx context.Context) ([]model.ServiceCertificate, error) {
	return list[model.ServiceCertificate](ctx, c, "SELECT document FROM srk_service_certificates WHERE document <> 'null'::jsonb ORDER BY role,id")
}

func (c *Catalog) AdoptServiceCertificate(ctx context.Context, next model.ServiceCertificate, expected, actor, reason string) error {
	tx, e := c.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "INSERT INTO srk_service_certificates(role,id,document) VALUES($1,$2,'null') ON CONFLICT DO NOTHING", next.Role, next.ID); e != nil {
		return e
	}
	var b []byte
	if e = tx.QueryRow(ctx, "SELECT document FROM srk_service_certificates WHERE role=$1 AND id=$2 FOR UPDATE", next.Role, next.ID).Scan(&b); e != nil {
		return e
	}
	var old model.ServiceCertificate
	if string(b) != "null" {
		if e = json.Unmarshal(b, &old); e != nil {
			return e
		}
	}
	if old.Serial != expected {
		return errors.New("service certificate generation changed")
	}
	if old.KeyFingerprint == next.KeyFingerprint {
		return errors.New("recovery requires a fresh private key")
	}
	b, e = json.Marshal(next)
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE srk_service_certificates SET document=$3 WHERE role=$1 AND id=$2", next.Role, next.ID, b); e != nil {
		return e
	}
	event := model.Audit{ID: model.ID(), At: time.Now().UTC(), Actor: actor, Action: "service_certificate.adopted", Object: next.Role + "/" + next.ID, Result: reason}
	b, _ = json.Marshal(event)
	if _, e = tx.Exec(ctx, "INSERT INTO srk_audit VALUES($1,$2,$3)", event.ID, event.At, b); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
