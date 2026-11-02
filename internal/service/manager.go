// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"gdata/srk/internal/catalog"
	"gdata/srk/internal/catalogbackup"
	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/report"
	"gdata/srk/internal/virtualization"
	"gdata/srk/internal/web"
)

type Manager struct {
	CatalogBackups   *catalogbackup.Store
	vmEvents         map[string]string
	captureContexts  map[string]context.Context
	Config           config.Config
	DB               *catalog.Catalog
	Client           *http.Client
	Key              []byte
	password         []byte
	mu               sync.Mutex
	sessions         map[string]webSession
	attempts         map[string][]time.Time
	active           map[string]context.CancelFunc
	operationMu      sync.Mutex
	retentionBuckets map[string]int64
}

func NewManager(ctx context.Context, c config.Config) (*Manager, error) {
	p, e := os.ReadFile(c.PasswordFile)
	if e != nil {
		return nil, e
	}
	p = []byte(strings.TrimSpace(string(p)))
	if len(p) < 16 {
		return nil, errors.New("admin password must contain at least 16 bytes")
	}
	hash, e := bcrypt.GenerateFromPassword(p, bcrypt.DefaultCost)
	if e != nil {
		return nil, e
	}
	key, e := os.ReadFile(c.CapabilityKeyFile)
	if e != nil {
		return nil, e
	}
	if len(key) < 32 {
		return nil, errors.New("capability key too short")
	}
	cl, e := identity.Client(c)
	if e != nil {
		return nil, e
	}
	cl.Transport.(*http.Transport).ResponseHeaderTimeout = 0
	db, e := catalog.Open(ctx, c.DatabaseURL)
	if e != nil {
		return nil, e
	}
	if e = db.InitRescue(ctx); e != nil {
		db.Close()
		return nil, e
	}
	if e = db.Recover(ctx); e != nil {
		db.Close()
		return nil, e
	}
	m := &Manager{vmEvents: map[string]string{}, captureContexts: map[string]context.Context{}, Config: c, DB: db, Client: cl, Key: key, password: hash, sessions: map[string]webSession{}, attempts: map[string][]time.Time{}, active: map[string]context.CancelFunc{}, retentionBuckets: map[string]int64{}}
	m.CatalogBackups, e = catalogbackup.OpenStore(ctx, db, c)
	if e != nil {
		db.Close()
		return nil, e
	}
	if m.CatalogBackups != nil {
		m.CatalogBackups.SetRemoteResolver(m.catalogRemote)
	}
	return m, nil
}
func (m *Manager) wrap(write bool, fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if len(r.Header.Get("Idempotency-Key")) > 128 {
			Fail(w, 400, errors.New("idempotency key exceeds 128 bytes"))
			return
		}
		u, e := m.principal(r)
		if e != nil {
			_ = m.DB.Audit(r.Context(), "unauthenticated", "access_denied", r.URL.Path, "denied")
			Fail(w, 401, e)
			return
		}
		r = withPrincipal(r, u)
		var allowed bool
		r, allowed = m.authorize(w, r, u, write)
		if !allowed {
			return
		}
		actor := u.Role + "/" + u.Username
		if u.Role != "admin" && r.Header.Get("Idempotency-Key") != "" {
			r.Header.Set("Idempotency-Key", fmt.Sprintf("%x", sha256.Sum256([]byte(actor+"\x00"+r.Header.Get("Idempotency-Key")))))
		}
		w.Header().Set("Cache-Control", "no-store")
		fn(w, r, actor)
	}
}
func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	m.accessRoutes(mux)
	m.catalogBackupRoutes(mux)
	m.certificateRoutes(mux)
	m.restoreTargetRoutes(mux)
	m.kubernetesRoutes(mux)
	m.protectionRoutes(mux)
	m.tapeRoutes(mux)
	m.walReplicationRoutes(mux)
	m.replicationRoutes(mux)
	m.rescueRoutes(mux)
	m.captureRoutes(mux)
	m.vmRoutes(mux)
	mux.Handle("GET /", web.Handler())
	mux.HandleFunc("POST /v1/login", func(w http.ResponseWriter, r *http.Request) {
		if !SameOrigin(r) {
			Fail(w, 403, errors.New("invalid origin"))
			return
		}
		var v struct {
			Password string `json:"password"`
			Username string `json:"username"`
		}
		if e := Decode(w, r, &v); e != nil {
			Fail(w, 400, e)
			return
		}
		ip := ClientIP(r)
		now := time.Now()
		m.mu.Lock()
		recent := []time.Time{}
		for _, t := range m.attempts[ip] {
			if now.Sub(t) < time.Minute {
				recent = append(recent, t)
			}
		}
		if len(recent) >= 5 {
			m.mu.Unlock()
			Fail(w, 429, errors.New("слишком много попыток; повторите через минуту"))
			return
		}
		m.attempts[ip] = append(recent, now)
		m.mu.Unlock()
		hash := m.password
		session := webSession{Until: now.Add(8 * time.Hour)}
		valid := true
		if v.Username != "" && v.Username != "bootstrap" {
			u, h, err := m.DB.User(r.Context(), v.Username)
			state, stateErr := m.DB.RecoveryState(r.Context())
			valid = err == nil && !u.Disabled && stateErr == nil && !state.Required
			if valid {
				hash = h
				session.Username = u.Username
				session.Version = u.Version
			}
		}
		if bcrypt.CompareHashAndPassword(hash, []byte(v.Password)) != nil || !valid {
			_ = m.DB.Audit(r.Context(), "web/"+ip, "login", "admin", "denied")
			Fail(w, 403, errors.New("неверный пароль"))
			return
		}
		token := model.ID() + model.ID()
		m.mu.Lock()
		for k, t := range m.sessions {
			if now.After(t.Until) {
				delete(m.sessions, k)
			}
		}
		m.sessions[token] = session
		m.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "srk_session", Value: token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 28800})
		loginActor := session.Username
		if loginActor == "" {
			loginActor = "bootstrap"
		}
		_ = m.DB.Audit(r.Context(), "user/"+loginActor, "login", loginActor, "success")
		JSON(w, 200, map[string]bool{"authenticated": true})
	})
	mux.HandleFunc("POST /v1/logout", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		if c, e := r.Cookie("srk_session"); e == nil {
			m.mu.Lock()
			delete(m.sessions, c.Value)
			m.mu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: "srk_session", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		JSON(w, 200, map[string]bool{"logged_out": true})
	}))
	mux.HandleFunc("GET /v1/status", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		s := LocalStatus(m.Config)
		recovery, e := m.DB.RecoveryState(r.Context())
		if e != nil {
			Fail(w, 503, errors.New("catalog unavailable"))
			return
		}
		s["recovery_required"] = recovery.Required
		s["max_parallel"] = m.Config.MaxParallel
		s["per_agent"] = m.Config.PerAgent
		s["per_media"] = m.Config.PerMedia
		s["retention_interval_seconds"] = m.Config.RetentionIntervalSeconds
		counts, e := m.DB.Counts(r.Context())
		if e != nil {
			Fail(w, 503, errors.New("catalog unavailable"))
			return
		}
		s["counts"] = counts
		if e := m.DB.Check(r.Context()); e != nil {
			Fail(w, 503, errors.New("catalog unavailable"))
			return
		}
		JSON(w, 200, s)
	}))
	mux.HandleFunc("GET /healthz", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		if e := m.DB.Check(r.Context()); e != nil {
			Fail(w, 503, errors.New("catalog unavailable"))
			return
		}
		JSON(w, 200, LocalStatus(m.Config))
	}))
	mux.HandleFunc("POST /v1/nodes/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		p, e := Role(r, "media", "agent", "proxy")
		if e != nil {
			Fail(w, 403, e)
			return
		}
		var n model.Node
		if e = Decode(w, r, &n); e != nil {
			Fail(w, 400, e)
			return
		}
		if n.ID != p.ID || n.Role != p.Role || !ValidEndpoint(n.URL) {
			Fail(w, 403, errors.New("node identity mismatch"))
			return
		}
		n.LastSeen = time.Now().UTC()
		if e = m.DB.PutNode(r.Context(), n); e != nil {
			Fail(w, 500, errors.New("catalog write failed"))
			return
		}
		JSON(w, 200, map[string]bool{"accepted": true})
	})
	mux.HandleFunc("GET /v1/nodes", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		v, e := m.DB.Nodes(r.Context())
		respond(w, v, e)
	}))
	mux.HandleFunc("GET /v1/jobs", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		v, e := m.DB.Jobs(r.Context())
		respond(w, v, e)
	}))
	mux.HandleFunc("POST /v1/jobs", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		var j model.Job
		if e := Decode(w, r, &j); e != nil {
			Fail(w, 400, e)
			return
		}
		if e := m.validateJob(r.Context(), j); e != nil {
			Fail(w, 400, e)
			return
		}
		j.ID = model.ID()
		j.Version = 1
		j.CreatedAt = time.Now().UTC()
		j.NextRun = j.CreatedAt.Add(time.Duration(j.IntervalSeconds) * time.Second)
		if j.IntervalSeconds == 0 {
			j.Enabled = false
		}
		if e := m.DB.PutJob(r.Context(), j); e != nil {
			Fail(w, 500, errors.New("cannot persist job"))
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "job.create", j.ID, "success")
		JSON(w, 201, j)
	}))
	mux.HandleFunc("POST /v1/jobs/{id}/enabled", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		m.operationMu.Lock()
		defer m.operationMu.Unlock()
		j, e := m.DB.Job(r.Context(), r.PathValue("id"))
		if e != nil {
			Fail(w, 404, errors.New("job not found"))
			return
		}
		var v struct {
			Enabled bool `json:"enabled"`
		}
		if e = Decode(w, r, &v); e != nil {
			Fail(w, 400, e)
			return
		}
		if v.Enabled && j.IntervalSeconds == 0 {
			Fail(w, 400, errors.New("manual job has no interval"))
			return
		}
		j.Enabled = v.Enabled
		j.Version++
		if j.Enabled {
			j.NextRun = time.Now().UTC().Add(time.Duration(j.IntervalSeconds) * time.Second)
		}
		e = m.DB.PutJob(r.Context(), j)
		if e != nil {
			Fail(w, 500, e)
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "job.enabled", j.ID, "success")
		JSON(w, 200, j)
	}))
	mux.HandleFunc("POST /v1/jobs/{id}/run", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		j, e := m.DB.Job(r.Context(), r.PathValue("id"))
		if e != nil {
			Fail(w, 404, errors.New("job not found"))
			return
		}
		run := model.Run{ID: model.ID(), JobID: j.ID, Job: j, Kind: "backup", Status: "queued", CreatedAt: time.Now().UTC()}
		run, e = m.DB.Enqueue(r.Context(), run, "backup/"+j.ID+"/"+idempotency(r))
		if e != nil {
			if errors.Is(e, catalog.ErrIdempotencyConflict) {
				Fail(w, 409, e)
				return
			}
			Fail(w, 500, e)
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "backup.enqueue", run.ID, "success")
		JSON(w, 202, run)
	}))
	mux.HandleFunc("GET /v1/runs", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		limit := 200
		if s := r.URL.Query().Get("limit"); s != "" {
			n, e := strconv.Atoi(s)
			if e != nil || n < 1 || n > 1000 {
				Fail(w, 400, errors.New("limit must be between 1 and 1000"))
				return
			}
			limit = n
		}
		q, e := queryRuns(r)
		if e != nil {
			Fail(w, 400, e)
			return
		}
		q.Limit = limit
		v, e := m.DB.QueryRuns(r.Context(), q)
		respond(w, v, e)
	}))
	mux.HandleFunc("GET /v1/runs/{id}", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		v, e := m.DB.Run(r.Context(), r.PathValue("id"))
		if e != nil {
			Fail(w, 404, errors.New("run not found"))
			return
		}
		JSON(w, 200, v)
	}))
	mux.HandleFunc("POST /v1/runs/{id}/cancel", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		m.operationMu.Lock()
		defer m.operationMu.Unlock()
		run, e := m.DB.Run(r.Context(), r.PathValue("id"))
		if e != nil {
			Fail(w, 404, errors.New("run not found"))
			return
		}
		if run.Status == "queued" {
			now := time.Now().UTC()
			run.Status = "cancelled"
			run.FinishedAt = &now
			if e = m.DB.SaveRun(r.Context(), run); e != nil {
				Fail(w, 500, e)
				return
			}
		} else if run.Status == "running" {
			if run.Options != nil || (run.Tape != nil && (run.Tape.Action == "erase" || run.Tape.Action == "recycle" || run.Tape.Action == "label")) {
				Fail(w, 409, errors.New("started maintenance must finish safely; only queued maintenance can be cancelled"))
				return
			}
			m.mu.Lock()
			cancel, ok := m.active[run.ID]
			m.mu.Unlock()
			if !ok {
				Fail(w, 409, errors.New("operation owner unavailable"))
				return
			}
			cancel()
		} else {
			Fail(w, 409, errors.New("operation already finished"))
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "run.cancel", run.ID, "success")
		JSON(w, 202, map[string]bool{"cancel_requested": true})
	}))
	mux.HandleFunc("POST /v1/runs/{id}/restore", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) { m.enqueueDerived(w, r, actor, "restore") }))
	mux.HandleFunc("POST /v1/runs/{id}/verify", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) { m.enqueueDerived(w, r, actor, "verify") }))
	mux.HandleFunc("GET /v1/audit", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		v, e := m.DB.Audits(r.Context())
		respond(w, v, e)
	}))
	mux.HandleFunc("GET /v1/reports/operations.csv", m.wrap(false, m.export))
	mux.HandleFunc("GET /v1/reports/operations.xlsx", m.wrap(false, m.export))
	m.maintenanceRoutes(mux)
	return mux
}
func respond(w http.ResponseWriter, v any, e error) {
	if e != nil {
		Fail(w, 500, errors.New("catalog operation failed"))
		return
	}
	JSON(w, 200, v)
}
func idempotency(r *http.Request) string {
	k := r.Header.Get("Idempotency-Key")
	if k == "" {
		return model.ID()
	}
	if len(k) > 128 {
		return model.ID()
	}
	return k
}
func (m *Manager) validateJob(ctx context.Context, j model.Job) error {
	if j.Type != "" && j.Type != "files" && j.Type != "postgresql" && j.Type != "disk-image" && !virtualization.VMProvider(j.Type) && j.Type != "s3" && j.Type != "mysql" && j.Type != "kubernetes" && j.Type != "longhorn" {
		return errors.New("unsupported source type")
	}
	if strings.TrimSpace(j.Name) == "" || len(j.Name) > 200 || j.Source == "" || j.RetentionDays < 1 || j.RetentionDays > 36500 || j.IntervalSeconds < 0 || j.IntervalSeconds > 31536000 {
		return errors.New("invalid name, source, interval or retention")
	}
	if j.Type == "disk-image" && j.Mode != "full" {
		return errors.New("disk image requires full mode")
	}
	if j.Mode != "full" && j.Mode != "incremental" && j.Mode != "differential" {
		return errors.New("unsupported backup mode")
	}
	a, e := m.DB.Node(ctx, j.AgentID)
	if e != nil || virtualization.VMProvider(j.Type) && a.Role != "proxy" || !virtualization.VMProvider(j.Type) && a.Role != "agent" {
		return errors.New("registered file agent required")
	}
	if virtualization.VMProvider(j.Type) {
		if !strings.HasPrefix(j.Source, j.Type+"://") {
			return errors.New("VM provider/source mismatch")
		}
		profile, _, err := virtualization.Source(j.Source)
		if err != nil {
			return err
		}
		if j.Mode != "full" || j.VM == nil || j.VM.Consistency != "crash-consistent" && j.VM.Consistency != "cold-only" {
			return errors.New("OpenStack requires full mode and an explicit consistency policy")
		}
		encoded, _ := json.Marshal(a.Status["virtualization_backup_profiles"])
		var names []string
		_ = json.Unmarshal(encoded, &names)
		if !slices.Contains(names, profile) {
			return errors.New("OpenStack profile is not enabled on this proxy")
		}
	}
	if j.Type == "s3" || j.Type == "mysql" || j.Type == "kubernetes" || j.Type == "longhorn" {
		name, err := config.DataSource(j.Source, j.Type)
		if err != nil {
			return err
		}
		if j.Mode != "full" {
			return errors.New("S3/MySQL sources require full mode with repository deduplication")
		}
		encoded, _ := json.Marshal(a.Status["data_profiles"])
		var profiles map[string][]string
		_ = json.Unmarshal(encoded, &profiles)
		if !slices.Contains(profiles[j.Type], name) {
			return errors.New("source profile not advertised by agent")
		}
	}
	if j.Type == "postgresql" {
		name, e := config.PostgreSQLSource(j.Source)
		if e != nil {
			return e
		}
		if j.Mode != "full" {
			return errors.New("PostgreSQL physical backup requires full mode")
		}
		encoded, _ := json.Marshal(a.Status["postgresql_profiles"])
		var profiles []string
		_ = json.Unmarshal(encoded, &profiles)
		if !slices.Contains(profiles, name) {
			return errors.New("PostgreSQL profile is not advertised by this agent")
		}
	}
	n, e := m.DB.Node(ctx, j.MediaID)
	if e != nil || n.Role != "media" {
		return errors.New("registered media server required")
	}
	return nil
}
func (m *Manager) enqueueDerived(w http.ResponseWriter, r *http.Request, actor, kind string) {
	source, e := m.DB.Run(r.Context(), r.PathValue("id"))
	if e != nil || source.Status != "success" || source.Kind != "backup" {
		Fail(w, 409, errors.New("successful backup required"))
		return
	}
	var target model.RestoreTarget
	var v struct {
		Destination     string                  `json:"destination"`
		RestoreTargetID string                  `json:"restore_target_id"`
		PITR            *model.PITRRequest      `json:"pitr,omitempty"`
		VMRestore       *model.VMRestoreRequest `json:"vm_restore,omitempty"`
	}

	if kind == "restore" {
		if e = Decode(w, r, &v); e != nil {
			Fail(w, 400, e)
			return
		}
		target, v.VMRestore, e = m.resolveRestoreTarget(r, v.RestoreTargetID, source, v.Destination, v.VMRestore)
		if e != nil {
			_ = m.DB.Audit(r.Context(), actor, "restore.target.denied", source.ID, "denied")
			Fail(w, 403, e)
			return
		}

	}
	physical := source.SnapshotID
	if physical == "" {
		physical = source.ID
	}
	n, cl, e := m.mediaClient(r.Context(), source.Job.MediaID)
	if e != nil {
		Fail(w, 409, e)
		return
	}
	defer cl.CloseIdleConnections()
	var info model.SnapshotInfo
	if e = Call(r.Context(), cl, "GET", n.URL+"/v1/repository/snapshots/"+physical, nil, &info); e != nil {
		Fail(w, 409, e)
		return
	}
	if !info.Hold && !time.Now().Before(info.ExpiresAt) {
		Fail(w, 409, errors.New("backup retention expired; renew it or import preserved data"))
		return
	}
	dest := ""
	var pitr *model.PITRRequest
	var vmRestore *model.VMRestoreRequest
	if kind == "restore" {
		if v.Destination == "" && !virtualization.VMProvider(source.Job.Type) {
			Fail(w, 400, errors.New("destination required"))
			return
		}
		dest = v.Destination
		if virtualization.VMProvider(source.Job.Type) {
			if v.VMRestore == nil {
				Fail(w, 400, errors.New("new VM restore options required"))
				return
			}
			if e = v.VMRestore.ValidateFor(source.Job.Type); e != nil {
				Fail(w, 400, e)
				return
			}
			if v.PITR != nil {
				Fail(w, 400, errors.New("PITR is not a VM restore option"))
				return
			}
			vmRestore = v.VMRestore
		}
		pitr = v.PITR
		if e = validatePITR(pitr, info.Result.PostgreSQL); e != nil {
			Fail(w, 400, e)
			return
		}
	}
	run := model.Run{ID: model.ID(), JobID: source.JobID, Job: source.Job, Kind: kind, Status: "queued", SnapshotID: physical, Destination: dest, CreatedAt: time.Now().UTC()}
	run.RestoreTargetID = target.ID
	run.RestoreSiteID = target.SiteID
	if target.NodeID != "" {
		run.Job.AgentID = target.NodeID
	}
	run.PITR = pitr
	run.VMRestore = vmRestore
	if vmRestore != nil && vmRestore.ProxyID != "" {
		node, e := m.DB.Node(r.Context(), vmRestore.ProxyID)
		if e != nil || node.Role != "proxy" {
			Fail(w, 400, errors.New("registered target proxy required"))
			return
		}
		run.Job.AgentID = vmRestore.ProxyID
	}
	run, e = m.DB.Enqueue(r.Context(), run, kind+"/"+source.ID+"/"+idempotency(r))
	if e != nil {
		if errors.Is(e, catalog.ErrIdempotencyConflict) {
			Fail(w, 409, e)
			return
		}
		Fail(w, 500, e)
		return
	}
	if target.ID != "" {
		_ = m.DB.Audit(r.Context(), actor, "restore.target.authorized", run.ID, target.SiteID+"/"+target.ID)
	}
	_ = m.DB.Audit(r.Context(), actor, kind+".enqueue", run.ID, "success")
	JSON(w, 202, run)
}
func (m *Manager) export(w http.ResponseWriter, r *http.Request, actor string) {
	q, e := queryRuns(r)
	if e != nil {
		Fail(w, 400, e)
		return
	}
	q.Offset = 0
	q.Limit = 0
	if q.From.IsZero() {
		q.From = time.Now().AddDate(0, -4, 0)
	}
	if q.To.IsZero() {
		q.To = time.Now().AddDate(0, 1, 0)
	}
	runs, e := m.DB.QueryRuns(r.Context(), q)
	if e != nil {
		Fail(w, 500, e)
		return
	}
	if q.Scope == "all" || q.Scope == "upcoming" {
		jobs, e := m.DB.Jobs(r.Context())
		if e != nil {
			Fail(w, 500, e)
			return
		}
		for _, j := range jobs {
			if !j.Enabled || j.NextRun.Before(q.From) || !j.NextRun.Before(q.To) || (q.Status != "" && q.Status != "all" && q.Status != "planned") || !strings.Contains(strings.ToLower(j.Name+" "+j.Source), strings.ToLower(q.Search)) {
				continue
			}
			runs = append(runs, model.Run{ID: "planned/" + j.ID, JobID: j.ID, Job: j, Kind: "backup", Status: "planned", CreatedAt: j.NextRun})
		}
	}
	rows := [][]string{{"id", "job", "operation", "status", "created_at", "planned_at", "finished_at", "source", "agent", "media", "logical_bytes", "uploaded_bytes", "verified", "error", "repository_id", "snapshot_state", "removed_snapshots", "removed_chunks", "removed_bytes", "imported", "skipped", "source_type", "postgresql_major", "postgresql_native_verified", "postgresql_wal_ranges", "consistency", "wal_archive_id", "pitr_target_time", "pitr_recovered", "wal_removed"}}
	for _, v := range runs {
		logical, uploaded, verified := "", "", ""
		if v.Result != nil {
			logical = strconv.FormatInt(v.Result.LogicalBytes, 10)
			uploaded = strconv.FormatInt(v.Result.UploadedBytes, 10)
			verified = strconv.FormatBool(v.Result.Verified)
		}
		finished := ""
		if v.FinishedAt != nil {
			finished = v.FinishedAt.Format(time.RFC3339)
		}
		created, planned := v.CreatedAt.Format(time.RFC3339), ""
		if v.Status == "planned" {
			planned = created
			created = ""
		}
		row := []string{v.ID, v.Job.Name, v.Kind, v.Status, created, planned, finished, v.Job.Source, v.Job.AgentID, v.Job.MediaID, logical, uploaded, verified, v.Error}
		repoID := ""
		if v.Result != nil {
			repoID = v.Result.RepositoryID
		}
		removed, blocks, freed, imported, skipped := "", "", "", "", ""
		if v.Maintenance != nil {
			a := v.Maintenance
			repoID = a.RepositoryID
			removed = strconv.Itoa(a.SnapshotsRemoved)
			blocks = strconv.Itoa(a.ChunksRemoved)
			freed = strconv.FormatInt(a.BytesRemoved, 10)
			imported = strconv.Itoa(a.Imported)
			skipped = strconv.Itoa(a.Skipped)
		}
		row = append(row, repoID, v.SnapshotState, removed, blocks, freed, imported, skipped)
		sourceType := v.Job.Type
		if sourceType == "" {
			sourceType = "files"
		}
		major, native, wal, consistency := "", "", "", ""
		if v.Result != nil {
			consistency = v.Result.Consistency
			if pg := v.Result.PostgreSQL; pg != nil {
				major = strconv.Itoa(pg.Major)
				native = strconv.FormatBool(pg.NativeVerified)
				b, _ := json.Marshal(pg.WALRanges)
				wal = string(b)
			}
		}
		row = append(row, sourceType, major, native, wal, consistency)
		archive, target, recovered := "", "", ""
		if v.Result != nil {
			if v.Result.PostgreSQL != nil {
				archive = v.Result.PostgreSQL.ArchiveID
			}
			if v.Result.PITR != nil {
				recovered = strconv.FormatBool(v.Result.PITR.Recovered)
			}
		}
		if v.PITR != nil {
			target = v.PITR.TargetTime.Format(time.RFC3339Nano)
		}
		walRemoved := ""
		if v.Maintenance != nil {
			archive = v.Maintenance.ArchiveID
			walRemoved = strconv.Itoa(v.Maintenance.WALRemoved)
		}
		row = append(row, archive, target, recovered, walRemoved)
		rows = append(rows, row)
	}
	format := "csv"
	if strings.HasSuffix(r.URL.Path, ".xlsx") {
		format = "xlsx"
		data, e := report.XLSX(rows)
		if e != nil {
			Fail(w, 422, e)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		w.Header().Set("Content-Disposition", "attachment; filename=srk-operations.xlsx")
		_, _ = w.Write(data)
	} else {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=srk-operations.csv")
		_, _ = w.Write([]byte{0xef, 0xbb, 0xbf})
		out := csv.NewWriter(w)
		for _, row := range rows {
			for i, s := range row {
				row[i] = report.CSVCell(s)
			}
			_ = out.Write(row)
		}
		out.Flush()
	}
	_ = m.DB.Audit(r.Context(), actor, "report.export", "operations."+format, "success")
}
func queryRuns(r *http.Request) (catalog.RunQuery, error) {
	q := catalog.RunQuery{Status: r.URL.Query().Get("status"), Search: r.URL.Query().Get("search"), Scope: r.URL.Query().Get("scope")}
	if q.Scope == "" {
		q.Scope = "all"
	}
	switch q.Scope {
	case "all", "now", "history", "upcoming":
	default:
		return q, errors.New("invalid activity scope")
	}
	switch q.Status {
	case "", "all", "planned", "queued", "running", "success", "error", "cancelled":
	default:
		return q, errors.New("invalid status")
	}
	if len(q.Search) > 1000 {
		return q, errors.New("search too long")
	}
	var e error
	for _, p := range []struct {
		key string
		t   *time.Time
	}{{"from", &q.From}, {"to", &q.To}} {
		if s := r.URL.Query().Get(p.key); s != "" {
			*p.t, e = time.Parse(time.RFC3339, s)
			if e != nil {
				return q, errors.New("dates must use RFC3339 with timezone")
			}
		}
	}
	if !q.From.IsZero() && !q.To.IsZero() && !q.From.Before(q.To) {
		return q, errors.New("from must precede to; range is [from,to)")
	}
	if s := r.URL.Query().Get("offset"); s != "" {
		q.Offset, e = strconv.Atoi(s)
		if e != nil || q.Offset < 0 || q.Offset > 10000000 {
			return q, errors.New("invalid offset")
		}
	}
	return q, nil
}
func (m *Manager) Run(ctx context.Context) error {
	defer func() {
		if m.CatalogBackups != nil {
			m.CatalogBackups.Close()
		}
		m.DB.Close()
	}()
	if m.CatalogBackups != nil {
		m.CatalogBackups.Run()
	}
	engineCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); m.scheduler(engineCtx) }()
	e := Serve(ctx, m.Config, m.Handler())
	cancel()
	<-done
	return e
}
func (m *Manager) scheduler(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			if e := m.DB.Check(ctx); e != nil {
				slog.Error("manager fencing lock lost; refusing dispatch")
				return
			}
			state, e := m.DB.RecoveryState(ctx)
			if e != nil || state.Required {
				continue
			}
			if now.Unix()%15 == 0 {
				m.restoreWALReceivers(ctx)
			}
			m.operationMu.Lock()
			e = m.DB.Schedule(ctx, now.UTC())
			if e == nil {
				e = m.scheduleRetention(ctx, now.UTC())
				if e == nil {
					e = m.DB.ScheduleWALReplications(ctx, now.UTC())
					if e == nil {
						e = m.DB.ScheduleKubernetesChecks(ctx, now.UTC())
					}
				}
			}
			if e == nil {
				e = m.dispatch(ctx, &wg)
			}
			m.operationMu.Unlock()
			if e != nil && ctx.Err() == nil {
				slog.Error("scheduler", "error", e)
			}
		}
	}
}
func (m *Manager) dispatch(ctx context.Context, wg *sync.WaitGroup) error {
	runs, e := m.DB.DispatchRuns(ctx)
	if e != nil {
		return e
	}
	agents := map[string]int{}
	media := map[string]int{}
	sources := map[string]int{}
	tapeDrives := map[string]int{}
	total := 0
	for _, r := range runs {
		if r.Status == "running" {
			total++
			if r.Replication == nil && r.WALReplication == nil {
				agents[r.Job.AgentID]++
			}
			if r.Replication != nil {
				media[r.Replication.SourceMediaID]++
			}
			if r.WALReplication != nil {
				media[r.WALReplication.SourceMediaID]++
			}
			media[r.Job.MediaID]++
			sources[dispatchSourceKey(r)]++
			if key := tapeDriveKey(r); key != "" {
				tapeDrives[key]++
			}
		}
	}
	for i := 0; i < len(runs); i++ {
		r := runs[i]
		if r.Status != "queued" || (tapeDriveKey(r) != "" && tapeDrives[tapeDriveKey(r)] > 0) || total >= m.Config.MaxParallel || (r.Replication == nil && r.WALReplication == nil && r.Job.AgentID != "" && agents[r.Job.AgentID] >= m.Config.PerAgent) || (r.Replication != nil && media[r.Replication.SourceMediaID] >= m.Config.PerMedia) || (r.WALReplication != nil && media[r.WALReplication.SourceMediaID] >= m.Config.PerMedia) || media[r.Job.MediaID] >= m.Config.PerMedia || sources[dispatchSourceKey(r)] > 0 {
			continue
		}
		now := time.Now().UTC()
		r.Status = "running"
		r.StartedAt = &now
		if e = m.DB.SaveRun(ctx, r); e != nil {
			return e
		}
		opctx, cancel := context.WithTimeout(ctx, 12*time.Hour)
		m.mu.Lock()
		m.active[r.ID] = cancel
		m.mu.Unlock()
		total++
		if r.Replication == nil && r.WALReplication == nil {
			agents[r.Job.AgentID]++
		}
		if r.Replication != nil {
			media[r.Replication.SourceMediaID]++
		}
		if r.WALReplication != nil {
			media[r.WALReplication.SourceMediaID]++
		}
		media[r.Job.MediaID]++
		sources[dispatchSourceKey(r)]++
		if key := tapeDriveKey(r); key != "" {
			tapeDrives[key]++
		}
		wg.Add(1)
		go func(run model.Run) {
			defer wg.Done()
			defer cancel()
			m.execute(opctx, run)
			m.mu.Lock()
			delete(m.active, run.ID)
			m.mu.Unlock()
		}(r)
	}
	return nil
}
func (m *Manager) execute(ctx context.Context, r model.Run) {
	var res model.Result
	err := func() error {
		if r.Tape != nil {
			var e error
			res, e = m.executeTape(ctx, r)
			return e
		}
		if r.WALReplication != nil {
			var e error
			res, e = m.executeWALReplication(ctx, r)
			return e
		}
		if r.Replication != nil {
			var e error
			res, e = m.executeReplication(ctx, r)
			return e
		}
		if r.Capture != nil {
			var e error
			res, e = m.executeCapture(ctx, r)
			return e
		}
		if r.Options != nil {
			return m.executeMaintenance(ctx, &r)
		}
		if r.Kind == "verify" {
			n, cl, e := m.mediaClient(ctx, r.Job.MediaID)
			if e != nil {
				return e
			}
			defer cl.CloseIdleConnections()
			var reply map[string]bool
			e = Call(ctx, cl, "POST", n.URL+"/v1/snapshots/"+r.SnapshotID+"/verify", nil, &reply)
			res.Verified = reply["verified"]
			res.EffectiveMode = "verify"
			return e
		}
		agent, e := m.DB.Node(ctx, r.Job.AgentID)
		if e != nil {
			return e
		}
		media, e := m.DB.Node(ctx, r.Job.MediaID)
		if e != nil {
			return e
		}
		if time.Since(agent.LastSeen) > 30*time.Second || time.Since(media.LastSeen) > 30*time.Second {
			return errors.New("agent or media server is offline")
		}

		base := ""
		if r.Kind == "backup" && r.Job.Mode != "full" {
			base, e = m.DB.Base(ctx, r.Job)
			if e != nil {
				return e
			}
		}
		perm := "write"
		if r.Kind == "restore" {
			perm = "read"
		}
		archiveID := ""
		if r.PITR != nil {
			n, cl, e := m.mediaClient(ctx, r.Job.MediaID)
			if e != nil {
				return e
			}
			defer cl.CloseIdleConnections()
			var info model.SnapshotInfo
			if e = Call(ctx, cl, "GET", n.URL+"/v1/repository/snapshots/"+r.SnapshotID, nil, &info); e != nil {
				return e
			}
			if e = validatePITR(r.PITR, info.Result.PostgreSQL); e != nil {
				return e
			}
			archiveID = info.Result.PostgreSQL.ArchiveID
		}
		g := identity.Sign(identity.Grant{ArchiveID: archiveID, Subject: agent.ID, RunID: r.ID, SnapshotID: r.SnapshotID, Base: base, Permission: perm, Expires: time.Now().Add(12 * time.Hour).Unix()}, m.Key)
		execution := model.Execute{RunID: r.ID, Kind: r.Kind, Job: r.Job, MediaURL: media.URL, Grant: g, Base: base, SnapshotID: r.SnapshotID, Destination: r.Destination}
		execution.PITR = r.PITR
		execution.KubernetesCheck = r.KubernetesCheck
		execution.VMRestore = r.VMRestore
		expectedRole := "agent"
		if virtualization.VMProvider(r.Job.Type) {
			expectedRole = "proxy"
		}
		client, e := identity.Client(m.Config, identity.Peer{Role: expectedRole, ID: agent.ID})
		if e != nil {
			return e
		}
		defer client.CloseIdleConnections()
		client.Transport.(*http.Transport).ResponseHeaderTimeout = 0
		return Call(ctx, client, "POST", agent.URL+"/v1/execute", execution, &res)
	}()
	if err != nil && ((r.Job.Type == "kubernetes" || r.Job.Type == "longhorn") || virtualization.VMProvider(r.Job.Type) || r.Replication != nil || r.WALReplication != nil) {
		progressCtx, progressCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer progressCancel()
		if latest, e := m.DB.Run(progressCtx, r.ID); e == nil && latest.Result != nil {
			res = *latest.Result
			res.Verified = false
		}
	}
	now := time.Now().UTC()
	r.FinishedAt = &now
	r.Result = &res
	if r.Options != nil {
		r.Result = nil
	}
	if err == nil && r.Kind == "backup" {
		expires := now.AddDate(0, 0, r.Job.RetentionDays)
		if r.Capture != nil {
			expires = r.CreatedAt.AddDate(0, 0, r.Job.RetentionDays)
		}
		r.SnapshotExpiresAt = &expires
		r.SnapshotState = "active"
	}
	r.Status = "success"
	if err != nil {
		r.Status = "error"
		r.Error = err.Error()
		if ctx.Err() != nil {
			r.Status = "cancelled"
			r.Error = "Операция отменена; частичный результат не является пригодной копией."
			if r.Options != nil {
				r.Status = "error"
				r.Error = "Результат обслуживания не подтвержден. Проверьте журнал медиасервера перед повтором."
			}
		}
	}
	save, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	saveRun := m.DB.SaveRun
	if r.Replication != nil && err == nil {
		saveRun = m.DB.CompleteReplication
	}
	if e := saveRun(save, r); e != nil {
		slog.Error("cannot persist operation outcome", "run", r.ID, "error", e)
		if r.Replication != nil && err == nil {
			r.Status = "error"
			r.Error = "Копия проверена на приёмнике, но регистрация в каталоге не подтверждена. Используйте проверку завершённой копии на приёмнике."
			retryCtx, retryCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = m.DB.SaveRun(retryCtx, r)
			retryCancel()
		}
	}
	_ = m.DB.Audit(save, "scheduler", r.Kind+".finished", r.ID, r.Status)
}

func tapeDriveKey(r model.Run) string {
	if r.Tape == nil || r.Tape.VolumeID != "" || r.Tape.DriveID == "" {
		return ""
	}
	return r.Job.MediaID + "/" + r.Tape.DriveID
}
