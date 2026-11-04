// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gdata/srk/internal/model"
)

const SchemaSQL = `CREATE TABLE IF NOT EXISTS srk_service_certificates(role text NOT NULL,id text NOT NULL,document jsonb NOT NULL,PRIMARY KEY(role,id));
 CREATE TABLE IF NOT EXISTS srk_user_certificate_state(username text PRIMARY KEY,version integer NOT NULL);
 CREATE TABLE IF NOT EXISTS srk_user_certificates(serial text PRIMARY KEY,username text NOT NULL,key_fingerprint text UNIQUE NOT NULL,issued_at timestamptz NOT NULL,document jsonb NOT NULL);
 CREATE INDEX IF NOT EXISTS srk_user_certificates_user ON srk_user_certificates(username);
 CREATE TABLE IF NOT EXISTS srk_users(username text PRIMARY KEY,version integer NOT NULL,document jsonb NOT NULL,password_hash bytea NOT NULL);
 CREATE TABLE IF NOT EXISTS srk_protection_settings(id integer PRIMARY KEY,version integer NOT NULL,document jsonb NOT NULL);
 CREATE TABLE IF NOT EXISTS srk_kubernetes_checks(id text PRIMARY KEY,next_run timestamptz NOT NULL,enabled boolean NOT NULL,document jsonb NOT NULL);
 CREATE TABLE IF NOT EXISTS srk_wal_replications(id text PRIMARY KEY,next_run timestamptz NOT NULL,enabled boolean NOT NULL,document jsonb NOT NULL);
 CREATE TABLE IF NOT EXISTS srk_jobs(id text PRIMARY KEY, next_run timestamptz, enabled boolean NOT NULL, document jsonb NOT NULL);
 CREATE TABLE IF NOT EXISTS srk_runs(id text PRIMARY KEY, job_id text NOT NULL, status text NOT NULL, created_at timestamptz NOT NULL, idempotency_key text UNIQUE, document jsonb NOT NULL);
 CREATE INDEX IF NOT EXISTS srk_runs_status_created ON srk_runs(status,created_at);
 CREATE INDEX IF NOT EXISTS srk_runs_job ON srk_runs(job_id,created_at DESC);
 CREATE TABLE IF NOT EXISTS srk_nodes(id text PRIMARY KEY, document jsonb NOT NULL);
 CREATE TABLE IF NOT EXISTS srk_audit(id text PRIMARY KEY, at timestamptz NOT NULL, document jsonb NOT NULL);
 CREATE INDEX IF NOT EXISTS srk_audit_at ON srk_audit(at DESC);
 CREATE INDEX IF NOT EXISTS srk_audit_object ON srk_audit((document->>'object'),at DESC);
 CREATE TABLE IF NOT EXISTS srk_recovery_state(id integer PRIMARY KEY,document jsonb NOT NULL);`

type Catalog struct {
	Pool      *pgxpool.Pool
	lock      *pgxpool.Conn
	lockMu    sync.Mutex
	closeOnce sync.Once
}

var ErrIdempotencyConflict = errors.New("idempotency key already used for a different request")

func Open(ctx context.Context, dsn string) (*Catalog, error) {
	p, e := pgxpool.New(ctx, dsn)
	if e != nil {
		return nil, e
	}
	c := &Catalog{Pool: p}
	if e = p.Ping(ctx); e != nil {
		p.Close()
		return nil, e
	}
	// Keep the session-level scheduler lock on its own checked-out connection.
	conn, e := p.Acquire(ctx)
	if e != nil {
		p.Close()
		return nil, e
	}
	var ok bool
	if e = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(734274623)").Scan(&ok); e != nil || !ok {
		conn.Release()
		p.Close()
		return nil, errors.New("catalog already has an active manager")
	}
	c.lock = conn
	_, e = p.Exec(ctx, SchemaSQL)
	if e != nil {
		c.Close()
		return nil, e
	}
	return c, nil
}
func (c *Catalog) Close() {
	c.closeOnce.Do(func() {
		c.lockMu.Lock()
		if c.lock != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = c.lock.Exec(ctx, "SELECT pg_advisory_unlock(734274623)")
			c.lock.Release()
			c.lock = nil
		}
		c.lockMu.Unlock()
		c.Pool.Close()
	})
}
func (c *Catalog) Check(ctx context.Context) error {
	c.lockMu.Lock()
	defer c.lockMu.Unlock()
	if c.lock == nil {
		return errors.New("catalog closed")
	}
	return c.lock.QueryRow(ctx, "SELECT 1").Scan(new(int))
}
func (c *Catalog) PutJob(ctx context.Context, j model.Job) error {
	b, _ := json.Marshal(j)
	_, e := c.Pool.Exec(ctx, `INSERT INTO srk_jobs VALUES($1,$2,$3,$4) ON CONFLICT(id) DO UPDATE SET next_run=$2,enabled=$3,document=$4`, j.ID, j.NextRun, j.Enabled, b)
	return e
}
func (c *Catalog) Job(ctx context.Context, id string) (model.Job, error) {
	var j model.Job
	var b []byte
	e := c.Pool.QueryRow(ctx, "SELECT document FROM srk_jobs WHERE id=$1", id).Scan(&b)
	if e != nil {
		return j, e
	}
	e = json.Unmarshal(b, &j)
	return j, e
}
func list[T any](ctx context.Context, c *Catalog, q string, args ...any) ([]T, error) {
	out := []T{}
	rows, e := c.Pool.Query(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		var x T
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &x); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (c *Catalog) Jobs(ctx context.Context) ([]model.Job, error) {
	if ids, ok := JobScope(ctx); ok {
		return list[model.Job](ctx, c, "SELECT document FROM srk_jobs WHERE id=ANY($1) ORDER BY document->>'name'", ids)
	}
	return list[model.Job](ctx, c, "SELECT document FROM srk_jobs ORDER BY document->>'name'")
}
func (c *Catalog) Runs(ctx context.Context, limit int) ([]model.Run, error) {
	return list[model.Run](ctx, c, "SELECT document FROM srk_runs ORDER BY created_at DESC LIMIT $1", limit)
}
func (c *Catalog) DispatchRuns(ctx context.Context) ([]model.Run, error) {
	return list[model.Run](ctx, c, `SELECT document FROM srk_runs WHERE status='running' UNION ALL (SELECT document FROM srk_runs WHERE status='queued' ORDER BY created_at ASC,id ASC LIMIT 1000)`)
}
func (c *Catalog) RunsBetween(ctx context.Context, from, to time.Time) ([]model.Run, error) {
	return list[model.Run](ctx, c, "SELECT document FROM srk_runs WHERE created_at >= $1 AND created_at < $2 ORDER BY created_at DESC", from, to)
}

type RunQuery struct {
	From, To              time.Time
	Status, Search, Scope string
	Limit, Offset         int
}

func (c *Catalog) Counts(ctx context.Context) (map[string]int64, error) {
	var running, queued, success, failed int64
	query := `SELECT count(*) FILTER (WHERE status='running'),count(*) FILTER (WHERE status='queued'),count(*) FILTER (WHERE status='success' AND document->>'kind'='backup'),count(*) FILTER (WHERE status='error') FROM srk_runs`
	var args []any
	if ids, ok := JobScope(ctx); ok {
		query += " WHERE job_id=ANY($1)"
		args = append(args, ids)
	}
	e := c.Pool.QueryRow(ctx, query, args...).Scan(&running, &queued, &success, &failed)
	return map[string]int64{"running": running, "queued": queued, "successful_backups": success, "errors": failed}, e
}
func (c *Catalog) QueryRuns(ctx context.Context, q RunQuery) ([]model.Run, error) {
	clauses := []string{"TRUE"}
	args := []any{}
	add := func(expr string, v any) {
		args = append(args, v)
		clauses = append(clauses, fmt.Sprintf(expr, len(args)))
	}
	if ids, ok := JobScope(ctx); ok {
		add("job_id=ANY($%d)", ids)
	}
	if !q.From.IsZero() {
		add("created_at >= $%d", q.From)
	}
	if !q.To.IsZero() {
		add("created_at < $%d", q.To)
	}
	if q.Status != "" && q.Status != "all" {
		add("status=$%d", q.Status)
	}
	if q.Search != "" {
		add("strpos(lower(coalesce(document->'job'->>'name','') || ' ' || id || ' ' || coalesce(document->'job'->>'source','')),lower($%d))>0", q.Search)
	}
	if q.Scope == "now" {
		clauses = append(clauses, "status IN ('running','queued')")
	}
	if q.Scope == "history" {
		clauses = append(clauses, "status NOT IN ('running','queued')")
	}
	if q.Scope == "upcoming" {
		clauses = append(clauses, "FALSE")
	}
	sql := "SELECT document FROM srk_runs WHERE " + strings.Join(clauses, " AND ") + " ORDER BY created_at DESC,id DESC"
	if q.Limit > 0 {
		args = append(args, q.Limit)
		sql += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	if q.Offset > 0 {
		args = append(args, q.Offset)
		sql += fmt.Sprintf(" OFFSET $%d", len(args))
	}
	return list[model.Run](ctx, c, sql, args...)
}
func (c *Catalog) Run(ctx context.Context, id string) (model.Run, error) {
	var r model.Run
	var b []byte
	e := c.Pool.QueryRow(ctx, "SELECT document FROM srk_runs WHERE id=$1", id).Scan(&b)
	if e != nil {
		return r, e
	}
	e = json.Unmarshal(b, &r)
	return r, e
}
func (c *Catalog) Enqueue(ctx context.Context, r model.Run, key string) (model.Run, error) {
	requested := r
	b, _ := json.Marshal(r)
	var stored []byte
	var k any
	if key != "" {
		k = key
	}
	e := c.Pool.QueryRow(ctx, `INSERT INTO srk_runs VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(idempotency_key) DO UPDATE SET idempotency_key=EXCLUDED.idempotency_key RETURNING document`, r.ID, r.JobID, r.Status, r.CreatedAt, k, b).Scan(&stored)
	if e != nil {
		return r, e
	}
	var persisted model.Run
	e = json.Unmarshal(stored, &persisted)
	r = persisted
	if e == nil && (r.Kind != requested.Kind || r.JobID != requested.JobID || r.SnapshotID != requested.SnapshotID || r.Destination != requested.Destination || !reflect.DeepEqual(r.Options, requested.Options) || !reflect.DeepEqual(r.PITR, requested.PITR) || !reflect.DeepEqual(r.Capture, requested.Capture) || !equalVMRestore(r.VMRestore, requested.VMRestore) || !equalReplication(r.Replication, requested.Replication) || !reflect.DeepEqual(r.WALReplication, requested.WALReplication) || !reflect.DeepEqual(r.Tape, requested.Tape)) {
		return r, ErrIdempotencyConflict
	}
	return r, e
}
func (c *Catalog) SaveRun(ctx context.Context, r model.Run) error {
	b, _ := json.Marshal(r)
	_, e := c.Pool.Exec(ctx, "UPDATE srk_runs SET status=$2,document=$3 WHERE id=$1", r.ID, r.Status, b)
	return e
}
func (c *Catalog) Base(ctx context.Context, j model.Job) (string, error) {
	var id string
	q := `SELECT COALESCE(NULLIF(document->>'snapshot_id',''),id) FROM srk_runs WHERE job_id=$1 AND status='success' AND document->>'kind'='backup' AND COALESCE(document->>'snapshot_state','')<>'deleted' AND COALESCE((document->>'snapshot_expires_at')::timestamptz,created_at + (document->'job'->>'retention_days')::int * interval '1 day') > now()`
	if j.Mode == "differential" {
		q += ` AND document->'result'->>'effective_mode'='full'`
	}
	q += " ORDER BY created_at DESC LIMIT 1"
	e := c.Pool.QueryRow(ctx, q, j.ID).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", nil
	}
	return id, e
}
func (c *Catalog) Node(ctx context.Context, id string) (model.Node, error) {
	var n model.Node
	var b []byte
	e := c.Pool.QueryRow(ctx, "SELECT document FROM srk_nodes WHERE id=$1", id).Scan(&b)
	if e != nil {
		return n, e
	}
	e = json.Unmarshal(b, &n)
	return n, e
}
func (c *Catalog) Nodes(ctx context.Context) ([]model.Node, error) {
	return list[model.Node](ctx, c, "SELECT document FROM srk_nodes ORDER BY id")
}
func (c *Catalog) PutNode(ctx context.Context, n model.Node) error {
	b, _ := json.Marshal(n)
	_, e := c.Pool.Exec(ctx, "INSERT INTO srk_nodes VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET document=$2", n.ID, b)
	return e
}
func (c *Catalog) Audit(ctx context.Context, actor, action, object, result string) error {
	a := model.Audit{ID: model.ID(), At: time.Now().UTC(), Actor: actor, Action: action, Object: object, Result: result}
	b, _ := json.Marshal(a)
	_, e := c.Pool.Exec(ctx, "INSERT INTO srk_audit VALUES($1,$2,$3)", a.ID, a.At, b)
	return e
}
func (c *Catalog) Audits(ctx context.Context) ([]model.Audit, error) {
	return list[model.Audit](ctx, c, "SELECT document FROM srk_audit ORDER BY at DESC LIMIT 500")
}
func (c *Catalog) Recover(ctx context.Context) error {
	runs, e := list[model.Run](ctx, c, "SELECT document FROM srk_runs WHERE status='running'")
	if e != nil {
		return e
	}
	for _, r := range runs {
		now := time.Now().UTC()
		r.Status = "error"
		r.Error = "Управление перезапущено; результат операции не подтвержден. Проверьте исполнителя перед повтором."
		r.FinishedAt = &now
		if e = c.SaveRun(ctx, r); e != nil {
			return e
		}
	}
	return nil
}
func (c *Catalog) Schedule(ctx context.Context, now time.Time) error {
	tx, e := c.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	rows, e := tx.Query(ctx, "SELECT document FROM srk_jobs WHERE enabled AND next_run <= $1 ORDER BY next_run FOR UPDATE SKIP LOCKED", now)
	if e != nil {
		return e
	}
	jobs := []model.Job{}
	for rows.Next() {
		var b []byte
		var j model.Job
		if e = rows.Scan(&b); e != nil {
			rows.Close()
			return e
		}
		if e = json.Unmarshal(b, &j); e != nil {
			rows.Close()
			return e
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return e
	}
	for _, j := range jobs {
		if j.IntervalSeconds <= 0 {
			continue
		}
		run := model.Run{ID: model.ID(), JobID: j.ID, Job: j, Kind: "backup", Status: "queued", CreatedAt: now}
		b, _ := json.Marshal(run)
		if _, e = tx.Exec(ctx, "INSERT INTO srk_runs SELECT $1,$2,$3,$4,NULL,$5 WHERE NOT EXISTS(SELECT 1 FROM srk_runs WHERE job_id=$2 AND status='queued')", run.ID, run.JobID, run.Status, run.CreatedAt, b); e != nil {
			return e
		}
		j.NextRun = now.Add(time.Duration(j.IntervalSeconds) * time.Second)
		jb, _ := json.Marshal(j)
		if _, e = tx.Exec(ctx, "UPDATE srk_jobs SET next_run=$2,document=$3 WHERE id=$1", j.ID, j.NextRun, jb); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}

func (c *Catalog) VMProgress(ctx context.Context, id, proxy string, res model.Result) (bool, error) {
	b, e := json.Marshal(res)
	if e != nil {
		return false, e
	}
	tag, e := c.Pool.Exec(ctx, `UPDATE srk_runs SET document=jsonb_set(document,'{result}',$3::jsonb) WHERE id=$1 AND status='running' AND document->'job'->>'agent_id'=$2 AND document->'job'->>'type' IN ('openstack','ovirt','proxmox','opennebula')`, id, proxy, b)
	return tag.RowsAffected() == 1, e
}

// JSON omitempty normalizes empty optional slices across catalog round trips.
func equalVMRestore(a, b *model.VMRestoreRequest) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
