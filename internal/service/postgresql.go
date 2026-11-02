// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gdata/srk/internal/config"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
)

func postgresEnvironment(p config.PostgreSQLProfile) []string {
	env := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "PG") {
			env = append(env, v)
		}
	}
	// A nonexistent default passfile disables implicit ~/.pgpass credential discovery.
	pass := p.Passfile
	if pass == "" {
		pass = filepath.Join(p.StagingDir, ".no-default-passfile")
	}
	return append(env, "LC_ALL=C", "PGHOST="+p.Host, "PGPORT="+strconv.Itoa(p.Port), "PGUSER="+p.User, "PGDATABASE="+p.Database, "PGSSLMODE="+p.SSLMode, "PGSSLROOTCERT="+p.SSLRootCert, "PGPASSFILE="+pass, "PGCONNECT_TIMEOUT=10", "PGAPPNAME=gdata-srk")
}
func postgresCommand(ctx context.Context, p config.PostgreSQLProfile, tool string, args ...string) error {
	return postgresCommandProgress(ctx, p, tool, nil, args...)
}
func postgresCommandProgress(ctx context.Context, p config.PostgreSQLProfile, tool string, progress func(int64, int64, float64), args ...string) error {
	cmd := exec.CommandContext(ctx, filepath.Join(p.BinDir, tool), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		e := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if e == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return e
	}
	cmd.Env = postgresEnvironment(p)
	// Diagnostics can contain server paths or connection details. Public errors only name the tool.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if progress != nil {
		cmd.Stderr = &postgresProgressWriter{report: progress}
	}
	cmd.WaitDelay = 3 * time.Second
	if e := cmd.Run(); e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s failed (%v); check PostgreSQL connectivity, permissions, version and native backup validity", tool, e)
	}
	return nil
}

var postgresProgressPattern = regexp.MustCompile(`([0-9]+)/([0-9]+) kB \(([0-9]+)%\)`)

type postgresProgressWriter struct {
	pending []byte
	report  func(int64, int64, float64)
}

func (w *postgresProgressWriter) Write(b []byte) (int, error) {
	for _, c := range b {
		if c == '\n' || c == '\r' {
			if m := postgresProgressPattern.FindSubmatch(w.pending); len(m) == 4 {
				done, _ := strconv.ParseInt(string(m[1]), 10, 64)
				total, _ := strconv.ParseInt(string(m[2]), 10, 64)
				percent, _ := strconv.ParseFloat(string(m[3]), 64)
				if total > 0 && done >= 0 && done <= total && total <= 1<<50 {
					w.report(done*1024, total*1024, min(99, percent))
				}
			}
			w.pending = w.pending[:0]
		} else if len(w.pending) < 8192 {
			w.pending = append(w.pending, c)
		}
	}
	return len(b), nil
}

var postgresVersionPattern = regexp.MustCompile(`\(PostgreSQL\) ([0-9]+)\.`)

func postgresTools(ctx context.Context, p config.PostgreSQLProfile, major int) error {
	for _, tool := range []string{"pg_basebackup", "pg_verifybackup", "pg_waldump"} {
		cmd := exec.CommandContext(ctx, filepath.Join(p.BinDir, tool), "--version")
		cmd.Env = postgresEnvironment(p)
		data, e := cmd.Output()
		if e != nil {
			return fmt.Errorf("cannot inspect %s", tool)
		}
		m := postgresVersionPattern.FindSubmatch(data)
		if len(m) != 2 {
			return errors.New("unrecognized PostgreSQL tool version")
		}
		n, _ := strconv.Atoi(string(m[1]))
		if n < 13 || n > 18 || major != 0 && n != major {
			return errors.New("PostgreSQL utilities must match the backup major version (13–18)")
		}
		if major == 0 {
			major = n
		}
	}
	return nil
}
func postgresSpace(dir string, reserve int64, extra int64) error {
	var s syscall.Statfs_t
	if e := syscall.Statfs(dir, &s); e != nil {
		return e
	}
	if uint64(s.Bavail)*uint64(s.Bsize) < uint64(reserve)+uint64(extra) {
		return errors.New("PostgreSQL staging free space reserve reached")
	}
	return nil
}
func (a *Agent) backupPostgreSQL(ctx context.Context, v model.Execute) (model.Result, error) {
	res := model.Result{Consistency: "postgresql-physical", EffectiveMode: "full"}
	name, e := config.PostgreSQLSource(v.Job.Source)
	if e != nil {
		return res, e
	}
	p, ok := a.Config.PostgreSQL[name]
	if !ok {
		return res, errors.New("PostgreSQL profile is not configured on this agent")
	}
	if v.Job.Mode != "full" || v.Base != "" {
		return res, errors.New("PostgreSQL physical backups currently require full mode")
	}
	if e = a.Config.Validate(); e != nil {
		return res, e
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	expectedSystem := ""
	if p.WALArchive != nil {
		if v.Job.MediaID != p.WALArchive.MediaID || v.MediaURL != p.WALArchive.MediaURL {
			return res, errors.New("WAL-linked base must use the same media endpoint as its archive")
		}
		control, e := readPostgresControl(ctx, p, p.WALArchive.DataDir)
		if e != nil {
			return res, e
		}
		expectedSystem = control.SystemID
		release, e := a.pinBackupWAL(ctx, v, p, name, expectedSystem)
		defer release()
		if e != nil {
			return res, e
		}
	}
	if e = postgresTools(ctx, p, 0); e != nil {
		return res, e
	}
	if e = postgresSpace(p.StagingDir, a.Config.ReserveBytes, 0); e != nil {
		return res, e
	}
	staging, e := os.MkdirTemp(p.StagingDir, "backup-*")
	if e != nil {
		return res, e
	}
	defer os.RemoveAll(staging)
	raw := filepath.Join(staging, "raw")
	data := filepath.Join(staging, "data")
	meta := &model.PostgreSQLBackup{Profile: name, StartedAt: time.Now().UTC()}
	res.Phase = "postgresql-basebackup"
	a.progressUpdate(v.RunID, res)
	// Tar output cannot write source tablespace paths. Unknown tablespaces are rejected before extraction.
	done := make(chan struct{})
	spaceCtx, stop := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		defer close(done)
		for {
			select {
			case <-spaceCtx.Done():
				return
			case <-ticker.C:
				if postgresSpace(p.StagingDir, a.Config.ReserveBytes, 0) != nil {
					stop()
					return
				}
			}
		}
	}()
	e = postgresCommandProgress(spaceCtx, p, "pg_basebackup", func(done, total int64, percent float64) {
		res.ProgressBytes = done
		res.TotalBytes = total
		res.ProgressPercent = &percent
		res.ProgressScope = "basebackup"
		a.progressUpdate(v.RunID, res)
	}, "--pgdata="+raw, "--format=tar", "--wal-method=stream", "--checkpoint=fast", "--manifest-checksums=SHA256", "--progress", "--no-password")
	stop()
	<-done
	if e != nil {
		return res, e
	}
	meta.FinishedAt = time.Now().UTC()
	entries, e := os.ReadDir(raw)
	if e != nil {
		return res, e
	}
	required := map[string]bool{"base.tar": false, "pg_wal.tar": false, "backup_manifest": false}
	for _, f := range entries {
		if _, ok := required[f.Name()]; !ok || !f.Type().IsRegular() {
			return res, errors.New("additional PostgreSQL tablespaces or unexpected backup files are not supported")
		}
		required[f.Name()] = true
	}
	for _, found := range required {
		if !found {
			return res, errors.New("PostgreSQL backup is missing base, WAL or native manifest")
		}
	}
	if e = os.Mkdir(data, 0700); e != nil {
		return res, e
	}
	res.Phase = "postgresql-unpack"
	res.ProgressScope = "unpack"
	res.TotalBytes = 0
	res.ProgressBytes = 0
	res.ProgressPercent = new(float64)
	for _, name := range []string{"base.tar", "pg_wal.tar"} {
		info, err := os.Stat(filepath.Join(raw, name))
		if err != nil {
			return res, err
		}
		res.TotalBytes += info.Size()
	}
	a.progressUpdate(v.RunID, res)
	for _, pair := range [][2]string{{"base.tar", data}, {"pg_wal.tar", filepath.Join(data, "pg_wal")}} {
		if e = os.MkdirAll(pair[1], 0700); e != nil {
			return res, e
		}
		before := res.ProgressBytes
		if e = extractPostgreSQLTarProgress(ctx, filepath.Join(raw, pair[0]), pair[1], a.Config.ReserveBytes, func(read int64) {
			res.ProgressBytes = before + read
			percent := min(99, 100*float64(res.ProgressBytes)/float64(res.TotalBytes))
			res.ProgressPercent = &percent
			a.progressUpdate(v.RunID, res)
		}); e != nil {
			return res, e
		}
	}
	b, e := os.ReadFile(filepath.Join(raw, "backup_manifest"))
	if e != nil {
		return res, e
	}
	if e = repository.Atomic(filepath.Join(data, "backup_manifest"), b, 0600); e != nil {
		return res, e
	}
	version, e := os.ReadFile(filepath.Join(data, "PG_VERSION"))
	if e != nil {
		return res, e
	}
	meta.Major, e = strconv.Atoi(strings.TrimSpace(string(version)))
	if e != nil {
		return res, errors.New("invalid PG_VERSION")
	}
	if e = postgresTools(ctx, p, meta.Major); e != nil {
		return res, e
	}
	res.Phase = "postgresql-validation"
	res.ProgressPercent = nil
	res.ProgressScope = "validation"
	a.progressUpdate(v.RunID, res)
	if e = postgresCommand(ctx, p, "pg_verifybackup", "--exit-on-error", data); e != nil {
		return res, e
	}
	var native struct {
		WALRanges []model.PostgreSQLWALRange `json:"WAL-Ranges"`
	}
	if e = json.Unmarshal(b, &native); e != nil || len(native.WALRanges) == 0 {
		return res, errors.New("native manifest has no recoverable WAL range")
	}
	meta.WALRanges = native.WALRanges
	meta.NativeVerified = true
	control, e := readPostgresControl(ctx, p, data)
	if e != nil {
		return res, e
	}
	meta.SystemID = control.SystemID
	if expectedSystem != "" && expectedSystem != control.SystemID {
		return res, errors.New("backup and local WAL source belong to different PostgreSQL clusters")
	}
	meta.SegmentSize = control.SegmentSize
	if p.WALArchive != nil {
		meta.ArchiveID = repository.WALArchiveID(a.Config.ID, name, control.SystemID)
	}
	if e = os.RemoveAll(raw); e != nil {
		return res, e
	}
	res.Phase = "postgresql-upload"
	res.PostgreSQL = meta
	a.progressUpdate(v.RunID, res)
	return a.backupFiles(ctx, v, data, []string{staging}, meta)
}

// Extraction is confined by os.Root and rejects links, duplicate paths and special files.
func extractPostgreSQLTar(ctx context.Context, archive, dest string, reserve int64) error {
	return extractPostgreSQLTarProgress(ctx, archive, dest, reserve, nil)
}
func extractPostgreSQLTarProgress(ctx context.Context, archive, dest string, reserve int64, progress func(int64)) error {
	fd, e := os.Open(archive)
	if e != nil {
		return e
	}
	defer fd.Close()
	root, e := os.OpenRoot(dest)
	if e != nil {
		return e
	}
	defer root.Close()
	tr := tar.NewReader(&countingReader{r: fd, report: progress})
	seen := map[string]bool{}
	for {
		if e = ctx.Err(); e != nil {
			return e
		}
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(h.Name, "/")
		name = strings.TrimPrefix(name, "./")
		if name == "." && h.Typeflag == tar.TypeDir {
			continue
		}
		if !repository.ValidPath(name) || seen[name] || h.Size < 0 {
			return fmt.Errorf("unsafe or duplicate PostgreSQL archive entry %q (type %d)", h.Name, h.Typeflag)
		}
		seen[name] = true
		switch h.Typeflag {
		case tar.TypeDir:
			if e = root.MkdirAll(name, 0700); e != nil {
				return e
			}
		case tar.TypeReg, tar.TypeRegA:
			if e = postgresSpace(dest, reserve, h.Size); e != nil {
				return e
			}
			if e = root.MkdirAll(path.Dir(name), 0700); e != nil {
				return e
			}
			out, e := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if e != nil {
				return e
			}
			_, e = io.Copy(out, &contextReader{ctx: ctx, r: tr})
			if e == nil {
				e = out.Sync()
			}
			closeErr := out.Close()
			if e != nil {
				return e
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return errors.New("PostgreSQL archive links and special files are unsupported")
		}
	}
}

type countingReader struct {
	r      io.Reader
	read   int64
	report func(int64)
}

func (r *countingReader) Read(b []byte) (int, error) {
	n, e := r.r.Read(b)
	r.read += int64(n)
	if r.report != nil {
		r.report(r.read)
	}
	return n, e
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(b)
}
func (a *Agent) postgreSQLRestoreProfile(meta *model.PostgreSQLBackup) (config.PostgreSQLProfile, error) {
	p, ok := a.Config.PostgreSQL[meta.Profile]
	if !ok {
		return p, errors.New("restore requires the PostgreSQL profile and matching native tools on the target agent")
	}
	if !meta.NativeVerified || meta.Major < 13 || meta.Major > 18 {
		return p, errors.New("invalid PostgreSQL backup metadata")
	}
	return p, nil
}
func (a *Agent) verifyPostgreSQLRestore(ctx context.Context, meta *model.PostgreSQLBackup, dest string) error {
	p, e := a.postgreSQLRestoreProfile(meta)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	if e = postgresTools(ctx, p, meta.Major); e != nil {
		return e
	}
	return postgresCommand(ctx, p, "pg_verifybackup", "--exit-on-error", dest)
}
