// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalogbackup

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gdata/srk/internal/catalog"
	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"github.com/jackc/pgx/v5"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var tables = []string{"srk_audit", "srk_jobs", "srk_kubernetes_checks", "srk_nodes", "srk_protection_settings", "srk_recovery_state", "srk_rescue_tokens", "srk_runs", "srk_user_certificate_state", "srk_user_certificates", "srk_users", "srk_wal_replications"}

type Manifest struct {
	Format       int              `json:"format"`
	ID           string           `json:"id"`
	Installation string           `json:"installation"`
	Version      string           `json:"srk_version"`
	SchemaHash   string           `json:"schema_hash"`
	CreatedAt    time.Time        `json:"created_at"`
	Counts       map[string]int64 `json:"counts"`
}
type header struct {
	Manifest  Manifest          `json:"manifest"`
	Config    config.Config     `json:"config"`
	Materials map[string][]byte `json:"materials"`
}
type record struct {
	Type   string           `json:"type"`
	Header *header          `json:"header,omitempty"`
	Table  string           `json:"table,omitempty"`
	Row    json.RawMessage  `json:"row,omitempty"`
	Counts map[string]int64 `json:"counts,omitempty"`
}
type Progress func(table string, rows, total int64)

func schemaHash() string {
	s := sha256.Sum256([]byte(catalog.SchemaSQL + catalog.RescueSchemaSQL))
	return hex.EncodeToString(s[:])
}
func materialPaths(c config.Config) map[string]string {
	out := map[string]string{"ca.crt": c.CA, "manager.crt": c.Cert, "manager.key": c.Key, "bootstrap.password": c.PasswordFile, "capability.key": c.CapabilityKeyFile, "revoked.json": c.Revoked, "personal-ca.key": c.PersonalCAKeyFile}
	if c.CatalogBackup != nil {
		out["catalog-policy.json"] = filepath.Join(c.CatalogBackup.Directory, "policy.json")
	}
	return out
}
func collect(c config.Config) (map[string][]byte, error) {
	out := map[string][]byte{}
	for name, path := range materialPaths(c) {
		if path == "" {
			continue
		}
		f, e := os.Open(path)
		if os.IsNotExist(e) && name == "catalog-policy.json" {
			out[name] = []byte(`{"version":0,"interval_seconds":0}`)
			continue
		}
		if e != nil {
			return nil, errors.New("cannot read manager recovery material: " + name)
		}
		b, e := io.ReadAll(io.LimitReader(f, 4*1024*1024+1))
		f.Close()
		if e != nil || len(b) > 4*1024*1024 {
			return nil, errors.New("invalid recovery material size: " + name)
		}
		out[name] = b
	}
	return out, nil
}

func validateMaterials(h header) error {
	pair, e := tls.X509KeyPair(h.Materials["manager.crt"], h.Materials["manager.key"])
	if e != nil {
		return errors.New("manager key/certificate recovery material mismatch")
	}
	cert, e := x509.ParseCertificate(pair.Certificate[0])
	if e != nil {
		return errors.New("invalid manager recovery certificate")
	}
	peer, e := identity.Parse(cert)
	if e != nil || peer.Installation != h.Manifest.Installation || peer.ID != h.Config.ID || peer.Role != "manager" {
		return errors.New("recovery identity mismatch")
	}
	if len(h.Materials["capability.key"]) < 32 || len(strings.TrimSpace(string(h.Materials["bootstrap.password"]))) < 16 {
		return errors.New("invalid manager recovery credentials")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(h.Materials["ca.crt"]) {
		return errors.New("invalid recovery CA")
	}
	intermediates := x509.NewCertPool()
	for _, b := range pair.Certificate[1:] {
		c, e := x509.ParseCertificate(b)
		if e != nil {
			return errors.New("invalid recovery chain")
		}
		intermediates.AddCert(c)
	}
	if _, e = cert.Verify(x509.VerifyOptions{Roots: pool, Intermediates: intermediates, CurrentTime: h.Manifest.CreatedAt, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); e != nil {
		return errors.New("manager certificate was not valid at backup time")
	}
	if h.Config.PersonalCAKeyFile != "" {
		if _, e = tls.X509KeyPair(h.Materials["ca.crt"], h.Materials["personal-ca.key"]); e != nil {
			return errors.New("personal issuer recovery material mismatch")
		}
	}
	return nil
}

// Export holds one read-only REPEATABLE READ snapshot across all tables,
// including passwords, permissions and certificate admission state.
func Export(ctx context.Context, db *catalog.Catalog, c config.Config, key []byte, w io.Writer, id string, progress Progress) (Manifest, error) {
	h := header{Manifest: Manifest{Format: 1, ID: id, Installation: c.Installation, Version: model.Version, SchemaHash: schemaHash(), CreatedAt: time.Now().UTC(), Counts: map[string]int64{}}, Config: c}
	var e error
	h.Materials, e = collect(c)
	if e != nil {
		return h.Manifest, e
	}
	if e = validateMaterials(h); e != nil {
		return h.Manifest, e
	}
	tx, e := db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return h.Manifest, e
	}
	defer tx.Rollback(ctx)
	// Unknown catalog tables must not be silently omitted by an older binary.
	rows, e := tx.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname=current_schema() AND left(tablename,4)='srk_' ORDER BY tablename`)
	if e != nil {
		return h.Manifest, e
	}
	var actual []string
	for rows.Next() {
		var s string
		if e = rows.Scan(&s); e != nil {
			rows.Close()
			return h.Manifest, e
		}
		actual = append(actual, s)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return h.Manifest, e
	}
	if fmt.Sprint(actual) != fmt.Sprint(tables) {
		return h.Manifest, errors.New("catalog table set does not match this SRK version")
	}
	total := int64(0)
	for _, table := range tables {
		var n int64
		if e = tx.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&n); e != nil {
			return h.Manifest, e
		}
		h.Manifest.Counts[table] = n
		total += n
	}
	enc, e := encrypt(w, key)
	if e != nil {
		return h.Manifest, e
	}
	gz := gzip.NewWriter(enc)
	j := json.NewEncoder(gz)
	if e = j.Encode(record{Type: "header", Header: &h}); e != nil {
		return h.Manifest, e
	}
	done := int64(0)
	for _, table := range tables {
		if e = j.Encode(record{Type: "table", Table: table}); e != nil {
			return h.Manifest, e
		}
		rows, e := tx.Query(ctx, "SELECT to_jsonb(t) FROM "+pgx.Identifier{table}.Sanitize()+" t")
		if e != nil {
			return h.Manifest, e
		}
		for rows.Next() {
			var b []byte
			if e = rows.Scan(&b); e != nil {
				rows.Close()
				return h.Manifest, e
			}
			if len(b) > 24*1024*1024 {
				rows.Close()
				return h.Manifest, errors.New("catalog row exceeds archive format limit")
			}
			if e = j.Encode(record{Type: "row", Row: b}); e != nil {
				rows.Close()
				return h.Manifest, e
			}
			done++
			if progress != nil {
				progress(table, done, total)
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return h.Manifest, e
		}
	}
	if e = j.Encode(record{Type: "end", Counts: h.Manifest.Counts}); e != nil {
		return h.Manifest, e
	}
	if e = tx.Commit(ctx); e != nil {
		return h.Manifest, e
	}
	if e = gz.Close(); e != nil {
		return h.Manifest, e
	}
	if e = enc.Close(); e != nil {
		return h.Manifest, e
	}
	return h.Manifest, nil
}

// scan authenticates every encrypted frame and gzip trailer, including EOF.
// Callbacks may stage transactional changes but must not commit before success.
func scan(ctx context.Context, r io.Reader, key []byte, onHeader func(header) error, onRow func(string, json.RawMessage) error) (Manifest, error) {
	var manifest Manifest
	dec, e := decrypt(r, key)
	if e != nil {
		return manifest, e
	}
	gz, e := gzip.NewReader(dec)
	if e != nil {
		return manifest, errors.New("invalid encrypted catalog payload")
	}
	defer gz.Close()
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 65536), 32*1024*1024)
	counts := map[string]int64{}
	index := -1
	started, ended := false, false
	for sc.Scan() {
		if e = ctx.Err(); e != nil {
			return manifest, e
		}
		if ended {
			return manifest, errors.New("records after archive footer")
		}
		var v record
		if e = json.Unmarshal(sc.Bytes(), &v); e != nil {
			return manifest, errors.New("invalid catalog archive record")
		}
		switch v.Type {
		case "header":
			if started || v.Header == nil {
				return manifest, errors.New("duplicate/missing archive header")
			}
			started = true
			h := *v.Header
			manifest = h.Manifest
			if manifest.Version != model.Version || manifest.Format != 1 || manifest.SchemaHash != schemaHash() || manifest.Installation == "" || h.Config.Installation != manifest.Installation || h.Config.Role != "manager" || len(manifest.Counts) != len(tables) {
				return manifest, errors.New("incompatible catalog archive")
			}
			for _, t := range tables {
				n, ok := manifest.Counts[t]
				if !ok || n < 0 {
					return manifest, errors.New("invalid table inventory")
				}
				counts[t] = 0
			}
			if e = validateMaterials(h); e != nil {
				return manifest, e
			}
			if onHeader != nil {
				if e = onHeader(h); e != nil {
					return manifest, e
				}
			}
		case "table":
			if !started || index+1 >= len(tables) || v.Table != tables[index+1] {
				return manifest, errors.New("invalid table order")
			}
			index++
		case "row":
			if index < 0 || len(v.Row) == 0 {
				return manifest, errors.New("row outside table")
			}
			table := tables[index]
			counts[table]++
			if counts[table] > manifest.Counts[table] {
				return manifest, errors.New("table count mismatch")
			}
			if onRow != nil {
				if e = onRow(table, v.Row); e != nil {
					return manifest, e
				}
			}
		case "end":
			if !started || index != len(tables)-1 || !equalCounts(counts, manifest.Counts) || !equalCounts(counts, v.Counts) {
				return manifest, errors.New("incomplete catalog archive")
			}
			ended = true
		default:
			return manifest, errors.New("unknown archive record")
		}
	}
	if e = sc.Err(); e != nil {
		return manifest, errors.New("catalog archive integrity check failed")
	}
	if !ended || !dec.done {
		return manifest, errors.New("truncated catalog archive")
	}
	return manifest, nil
}
func equalCounts(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || w != v {
			return false
		}
	}
	return true
}
func Verify(ctx context.Context, r io.Reader, key []byte) (Manifest, error) {
	return scan(ctx, r, key, nil, nil)
}
func TableNames() []string { v := append([]string{}, tables...); sort.Strings(v); return v }
