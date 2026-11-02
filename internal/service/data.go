// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gdata/srk/internal/config"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func (a *Agent) backupData(ctx context.Context, v model.Execute) (model.Result, error) {
	kind := v.Job.Type
	name, e := config.DataSource(v.Job.Source, kind)
	if e != nil {
		return model.Result{}, e
	}
	var staging string
	if kind == "s3" {
		p, ok := a.Config.S3[name]
		if !ok {
			return model.Result{}, errors.New("S3 profile not configured")
		}
		staging = p.StagingDir
	} else {
		p, ok := a.Config.MySQL[name]
		if !ok {
			return model.Result{}, errors.New("MySQL profile not configured")
		}
		staging = p.StagingDir
	}
	dir, e := os.MkdirTemp(staging, "srk-data-")
	if e != nil {
		return model.Result{}, e
	}
	defer os.RemoveAll(dir)
	meta := &model.DataBackup{Kind: kind, Profile: name}
	if kind == "s3" {
		e = a.downloadS3(ctx, v, a.Config.S3[name], dir, meta)
	} else {
		e = a.dumpMySQL(ctx, v, a.Config.MySQL[name], dir, meta)
	}
	if e != nil {
		return model.Result{}, e
	}
	v.Base = ""
	v.Job.Mode = "full"
	return a.backupFilesData(ctx, v, dir, []string{staging}, nil, meta)
}
func (a *Agent) restoreData(ctx context.Context, v model.Execute, m model.Manifest) (model.Result, error) {
	res := model.Result{Data: m.Data, EffectiveMode: "full", Consistency: m.Consistency, Phase: "data-restore", ProgressScope: "data-transfer"}
	u, e := url.Parse(v.Destination)
	if e != nil || u.Scheme != m.Data.Kind || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return res, errors.New("destination requires KIND://TARGET_PROFILE/NEW_DATABASE_OR_PREFIX")
	}
	name := u.Host
	var staging string
	if m.Data.Kind == "s3" {
		p, ok := a.Config.S3[name]
		if !ok || !p.AllowRestore {
			return res, errors.New("target S3 profile does not allow restore")
		}
		staging = p.StagingDir
	} else {
		p, ok := a.Config.MySQL[name]
		if !ok || !p.AllowRestore || p.Engine != m.Data.Engine {
			return res, errors.New("target MySQL/MariaDB profile must allow restore and match the source engine")
		}
		staging = p.StagingDir
	}
	dir, e := os.MkdirTemp(staging, "srk-restore-")
	if e != nil {
		return res, e
	}
	defer os.RemoveAll(dir)
	for _, f := range m.Files {
		res.TotalBytes += f.Size
	}
	limit := int64(0)
	if m.Data.Kind == "s3" {
		limit = a.Config.S3[name].MaxBytes + (16 << 20)
	} else {
		limit = a.Config.MySQL[name].MaxBytes
	}
	if res.TotalBytes > limit {
		return res, errors.New("data restore staging quota exceeded")
	}
	for _, f := range m.Files {
		if f.Kind != "file" || !repository.ValidPath(f.Path) {
			return res, errors.New("invalid data backup file")
		}
		dest := filepath.Join(dir, filepath.FromSlash(f.Path))
		if e = os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
			return res, e
		}
		out, e := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return res, e
		}
		var n int64
		for _, h := range f.Chunks {
			response, err := a.media(ctx, v, "GET", "/v1/chunks/"+h, nil)
			if err != nil {
				out.Close()
				return res, err
			}
			b, err := io.ReadAll(io.LimitReader(response.Body, repository.ChunkSize+1))
			response.Body.Close()
			if err != nil || repository.Hash(b) != h {
				out.Close()
				return res, errors.New("data restore block checksum failed")
			}
			if _, err = out.Write(b); err != nil {
				out.Close()
				return res, err
			}
			n += int64(len(b))
			res.LogicalBytes += int64(len(b))
			updateProgress(&res)
			a.progressUpdate(v.RunID, res)
		}
		if e = out.Sync(); e != nil {
			out.Close()
			return res, e
		}
		if e = out.Close(); e != nil {
			return res, e
		}
		if n != f.Size {
			return res, errors.New("data restore size mismatch")
		}
		res.Files++
	}
	if m.Data.Kind == "s3" {
		e = a.restoreS3(ctx, v, a.Config.S3[name], strings.TrimPrefix(u.Path, "/"), dir, m.Data, &res)
	} else {
		e = restoreMySQL(ctx, a.Config.MySQL[name], strings.TrimPrefix(u.Path, "/"), dir, m.Data.Database)
	}
	if e != nil {
		return res, e
	}
	res.Verified = true
	res.Phase = "complete"
	completeProgress(&res)
	return res, nil
}
func sqlCommand(ctx context.Context, p config.MySQLProfile, tool string, args []string, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	argv := []string{"--defaults-file=" + p.DefaultsFile}
	if p.Engine == "mysql" {
		if p.SSLMode == "disable" {
			argv = append(argv, "--ssl-mode=DISABLED")
		} else {
			argv = append(argv, "--ssl-mode=VERIFY_IDENTITY", "--ssl-ca="+p.CAFile)
		}
	} else {
		if p.SSLMode == "disable" {
			argv = append(argv, "--skip-ssl")
		} else {
			argv = append(argv, "--ssl", "--ssl-verify-server-cert", "--ssl-ca="+p.CAFile)
		}
	}
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, filepath.Join(p.BinDir, tool), argv...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "HOME=" + p.StagingDir, "MYSQL_HISTFILE=/dev/null"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		e := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if e == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return e
	}
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 3 * time.Second
	if e := cmd.Run(); e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s failed; check native client version, TLS, privileges and database", tool)
	}
	return nil
}
func (a *Agent) dumpMySQL(ctx context.Context, v model.Execute, p config.MySQLProfile, dir string, meta *model.DataBackup) error {
	a.progressUpdate(v.RunID, model.Result{Phase: "mysql-dump", ProgressScope: "operation"})
	// A transaction snapshot does not cover MyISAM or concurrent DDL.
	var check strings.Builder
	query := "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='" + p.Database + "' AND table_type='BASE TABLE' AND engine <> 'InnoDB'"
	if e := sqlCommand(ctx, p, p.Engine, []string{"--batch", "--skip-column-names", "--execute=" + query}, nil, &check); e != nil {
		return e
	}
	if strings.TrimSpace(check.String()) != "0" {
		return errors.New("transactional backup requires all base tables to use InnoDB; concurrent DDL must be excluded")
	}
	tool := "mysqldump"
	if p.Engine == "mariadb" {
		tool = "mariadb-dump"
	}
	args := []string{"--single-transaction", "--quick", "--skip-lock-tables", "--routines", "--events", "--triggers", "--hex-blob", "--skip-comments", "--skip-dump-date", "--no-tablespaces"}
	if p.Engine == "mysql" {
		args = append(args, "--set-gtid-purged=OFF")
	}
	args = append(args, p.Database)
	out, e := os.OpenFile(filepath.Join(dir, "database.sql"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	e = sqlCommand(ctx, p, tool, args, nil, &quotaWriter{Writer: out, remaining: p.MaxBytes})
	syncErr := out.Sync()
	closeErr := out.Close()
	if e != nil {
		return e
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	meta.Engine = p.Engine
	meta.Database = p.Database
	return nil
}
func restoreMySQL(ctx context.Context, p config.MySQLProfile, database, dir, sourceDatabase string) error {
	if _, e := config.DataSource("mysql://"+database, "mysql"); e != nil || database == p.Database || database == "mysql" || database == "sys" || database == "performance_schema" || database == "information_schema" {
		return errors.New("restore requires a distinct new database name")
	}
	// Validate and rewrite the native dump before creating the new database.
	original, e := os.Open(filepath.Join(dir, "database.sql"))
	if e != nil {
		return e
	}
	rewritten, e := os.OpenFile(filepath.Join(dir, "restore.sql"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		original.Close()
		return e
	}
	e = rewriteSQLSchema(original, rewritten, sourceDatabase, database)
	original.Close()
	syncErr := rewritten.Sync()
	closeErr := rewritten.Close()
	if e != nil {
		return e
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	// No IF NOT EXISTS: existing databases, including concurrent creations, are protected.
	if e := sqlCommand(ctx, p, p.Engine, []string{"--batch", "--execute=CREATE DATABASE `" + database + "`"}, nil, io.Discard); e != nil {
		return e
	}
	input, e := os.Open(filepath.Join(dir, "restore.sql"))
	if e != nil {
		return e
	}
	defer input.Close()
	if e = sqlCommand(ctx, p, p.Engine, []string{"--binary-mode", "--database=" + database}, input, io.Discard); e != nil {
		return fmt.Errorf("restore failed; partially restored new database %s is retained for diagnosis: %w", database, e)
	}
	return nil
}
func fileSHA(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func writeDataIndex(dir string, meta *model.DataBackup) error {
	b, e := json.Marshal(meta)
	if e != nil {
		return e
	}
	return os.WriteFile(filepath.Join(dir, "source.json"), b, 0600)
}

type quotaWriter struct {
	io.Writer
	remaining int64
}

func (w *quotaWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > w.remaining {
		return 0, errors.New("data staging quota exceeded")
	}
	n, e := w.Writer.Write(b)
	w.remaining -= int64(n)
	return n, e
}
