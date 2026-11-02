// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalogbackup

import (
	"context"
	"encoding/json"
	"errors"
	"gdata/srk/internal/catalog"
	"gdata/srk/internal/config"
	"github.com/jackc/pgx/v5"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

type RestoreOptions struct {
	DatabaseURL, Schema, Installation, Directory string
	Drill                                        bool
}
type RestoreResult struct {
	Manifest       Manifest `json:"manifest"`
	Schema         string   `json:"schema"`
	ConfigFile     string   `json:"config_file"`
	ReviewRequired bool     `json:"review_required"`
}

func privateWrite(path string, b []byte) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	if _, e = f.Write(b); e != nil {
		return e
	}
	return f.Sync()
}
func Restore(ctx context.Context, r io.Reader, key []byte, o RestoreOptions) (out RestoreResult, err error) {
	if !regexp.MustCompile(`^[a-z][a-z0-9_]{1,62}$`).MatchString(o.Schema) || o.Schema == "public" || len(o.Schema) >= 3 && o.Schema[:3] == "pg_" || o.Installation == "" || !filepath.IsAbs(o.Directory) {
		return out, errors.New("new schema, explicit installation and absolute recovery directory required")
	}
	// The caller supplies the new database credentials out of band, never from the archive.
	uri, e := url.Parse(o.DatabaseURL)
	if e != nil || (uri.Scheme != "postgres" && uri.Scheme != "postgresql") {
		return out, errors.New("invalid target database URL")
	}
	if uri.Query().Get("sslmode") != "verify-full" && !(uri.Query().Get("sslmode") == "disable" && (uri.Hostname() == "localhost" || uri.Hostname() == "127.0.0.1" || uri.Hostname() == "::1")) {
		return out, errors.New("target database requires verify-full TLS (loopback development may use disable)")
	}
	conn, e := pgx.Connect(ctx, o.DatabaseURL)
	if e != nil {
		return out, errors.New("cannot connect to recovery database")
	}
	defer conn.Close(context.Background())
	var locked bool
	if e = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(734274623)").Scan(&locked); e != nil || !locked {
		return out, errors.New("active manager detected; stop it before restoring")
	}
	tx, e := conn.Begin(ctx)
	if e != nil {
		return out, e
	}
	defer tx.Rollback(context.Background())
	if _, e = tx.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{o.Schema}.Sanitize()); e != nil {
		return out, errors.New("recovery schema must not already exist")
	}
	if _, e = tx.Exec(ctx, "SET LOCAL search_path TO "+pgx.Identifier{o.Schema}.Sanitize()); e != nil {
		return out, e
	}
	if _, e = tx.Exec(ctx, catalog.SchemaSQL+catalog.RescueSchemaSQL); e != nil {
		return out, e
	}
	var h header
	batch := []json.RawMessage{}
	table := ""
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		b, e := json.Marshal(batch)
		if e != nil {
			return e
		}
		name := pgx.Identifier{table}.Sanitize()
		_, e = tx.Exec(ctx, "INSERT INTO "+name+" SELECT * FROM jsonb_populate_recordset(NULL::"+name+",$1::jsonb)", b)
		batch = nil
		size = 0
		return e
	}
	manifest, e := scan(ctx, r, key, func(v header) error {
		if v.Manifest.Installation != o.Installation {
			return errors.New("archive installation does not match confirmation")
		}
		h = v
		return nil
	}, func(t string, b json.RawMessage) error {
		if t != table {
			if e := flush(); e != nil {
				return e
			}
			table = t
		}
		batch = append(batch, append(json.RawMessage{}, b...))
		size += len(b)
		if len(batch) >= 250 || size >= 4*1024*1024 {
			return flush()
		}
		return nil
	})
	if e != nil {
		return out, e
	}
	if e = flush(); e != nil {
		return out, e
	}
	// Recovery never replays an old queue or revives one-time Rescue credentials.
	recovery := catalog.RecoveryState{Required: true, ArchiveID: manifest.ID, RestoredAt: time.Now().UTC()}
	paused := map[string]*[]string{"srk_jobs": &recovery.PausedJobs, "srk_kubernetes_checks": &recovery.PausedKubernetes, "srk_wal_replications": &recovery.PausedWAL}
	for table, dst := range paused {
		rows, e := tx.Query(ctx, "SELECT id FROM "+pgx.Identifier{table}.Sanitize()+" WHERE enabled ORDER BY id")
		if e != nil {
			return out, e
		}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return out, e
			}
			*dst = append(*dst, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return out, e
		}
	}
	for _, t := range []string{"srk_jobs", "srk_kubernetes_checks", "srk_wal_replications"} {
		if _, e = tx.Exec(ctx, "UPDATE "+pgx.Identifier{t}.Sanitize()+" SET enabled=false,document=jsonb_set(document,'{enabled}','false')"); e != nil {
			return out, e
		}
	}
	_, e = tx.Exec(ctx, `UPDATE srk_runs SET status='error',document=document || jsonb_build_object('status','error','error','Каталог восстановлен: прежняя операция требует сверки с исполнителем','finished_at',now()) WHERE status IN ('running','queued'); DELETE FROM srk_rescue_tokens; UPDATE srk_nodes SET document=jsonb_set(document,'{last_seen}','"1970-01-01T00:00:00Z"');`)
	if e != nil {
		return out, e
	}
	state, _ := json.Marshal(recovery)
	if _, e = tx.Exec(ctx, `INSERT INTO srk_recovery_state VALUES(1,$1) ON CONFLICT(id) DO UPDATE SET document=$1`, state); e != nil {
		return out, e
	}
	if o.Drill {
		// Execute the real importer and recovery transformations, then roll back all DDL/data.
		var unsafe int
		e = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM srk_jobs WHERE enabled)+(SELECT count(*) FROM srk_wal_replications WHERE enabled)+(SELECT count(*) FROM srk_kubernetes_checks WHERE enabled)+(SELECT count(*) FROM srk_runs WHERE status IN ('queued','running'))+(SELECT count(*) FROM srk_rescue_tokens)`).Scan(&unsafe)
		if e != nil || unsafe != 0 {
			return out, errors.New("recovery isolation checks failed")
		}
		if e = tx.Rollback(ctx); e != nil {
			return out, e
		}
		return RestoreResult{Manifest: manifest, Schema: o.Schema, ReviewRequired: true}, nil
	}
	if e = os.Mkdir(o.Directory, 0700); e != nil {
		return out, errors.New("recovery directory must not already exist")
	}
	retain := false
	defer func() {
		if !retain {
			os.RemoveAll(o.Directory)
		}
	}()
	cfg := h.Config
	paths := materialPaths(cfg)
	for name, old := range paths {
		if old == "" {
			continue
		}
		data, ok := h.Materials[name]
		if !ok || len(data) == 0 {
			return out, errors.New("missing recovery material: " + name)
		}
		if e = privateWrite(filepath.Join(o.Directory, name), data); e != nil {
			return out, e
		}
	}
	at := func(name string) string {
		if _, ok := h.Materials[name]; !ok {
			return ""
		}
		return filepath.Join(o.Directory, name)
	}
	cfg.CA = at("ca.crt")
	cfg.Cert = at("manager.crt")
	cfg.Key = at("manager.key")
	cfg.PasswordFile = at("bootstrap.password")
	cfg.CapabilityKeyFile = at("capability.key")
	cfg.Revoked = at("revoked.json")
	cfg.PersonalCAKeyFile = at("personal-ca.key")
	q := uri.Query()
	q.Set("search_path", o.Schema)
	uri.RawQuery = q.Encode()
	cfg.DatabaseURL = uri.String()
	cfg.RetentionIntervalSeconds = 0
	cfg.CatalogBackup = nil
	// New backup storage and external encryption key must be configured separately.
	b, e := json.MarshalIndent(cfg, "", "  ")
	if e != nil {
		return out, e
	}
	configFile := filepath.Join(o.Directory, "manager.json")
	if e = privateWrite(configFile, b); e != nil {
		return out, e
	}
	if e = privateWrite(filepath.Join(o.Directory, "RECOVERY-REVIEW.txt"), []byte("Stop/fence the previous manager. Review certificate revocations and users since the snapshot, storage identities and node connectivity. Schedules are paused; personal access and mutations remain blocked until administrator acknowledgement.\n")); e != nil {
		return out, e
	}
	if _, e = config.Load(configFile); e != nil {
		return out, errors.New("restored manager configuration is invalid")
	}
	if e = syncDir(o.Directory); e != nil {
		return out, e
	}
	if e = syncDir(filepath.Dir(o.Directory)); e != nil {
		return out, e
	}
	retain = true // An ambiguous commit must retain the recovery materials for diagnosis.
	if e = tx.Commit(ctx); e != nil {
		return out, errors.New("recovery commit not confirmed; retain recovery directory and check target schema")
	}
	return RestoreResult{Manifest: manifest, Schema: o.Schema, ConfigFile: configFile, ReviewRequired: true}, nil
}
