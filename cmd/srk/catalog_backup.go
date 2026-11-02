// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"gdata/srk/internal/catalogbackup"
	"gdata/srk/internal/service"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func catalogRecovery(args []string) error {
	if len(args) == 0 {
		return usageError("catalog-recovery keygen|verify|restore")
	}
	fs := flag.NewFlagSet("catalog-recovery", flag.ContinueOnError)
	file := fs.String("archive", "", "encrypted .srkcat archive")
	keyFile := fs.String("key-file", "", "separate 32-byte recovery key")
	out := fs.String("out", "", "new recovery key file")
	dsnFile := fs.String("database-url-file", "", "private target PostgreSQL URL file")
	schema := fs.String("schema", "", "new target schema")
	installation := fs.String("installation", "", "expected installation ID")
	dir := fs.String("directory", "", "new absolute recovery directory")
	timeout := fs.Duration("timeout", 2*time.Hour, "operation timeout")
	if e := fs.Parse(args[1:]); e != nil {
		return e
	}
	if *timeout <= 0 {
		return usageError("positive timeout required")
	}
	if args[0] == "keygen" {
		if *out == "" {
			return usageError("--out required")
		}
		return catalogbackup.GenerateKey(*out)
	}
	if args[0] != "verify" && args[0] != "restore" {
		return usageError("catalog-recovery keygen|verify|restore")
	}
	key, e := catalogbackup.ReadKey(*keyFile)
	if e != nil {
		return e
	}
	f, e := os.Open(*file)
	if e != nil {
		return e
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if args[0] == "verify" {
		m, e := catalogbackup.Verify(ctx, f, key)
		if e != nil {
			return e
		}
		return printValue(m, true)
	}
	st, e := os.Stat(*dsnFile)
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return errors.New("database URL file must be private (0600/0400)")
	}
	b, e := os.ReadFile(*dsnFile)
	if e != nil {
		return e
	}
	result, e := catalogbackup.Restore(ctx, f, key, catalogbackup.RestoreOptions{DatabaseURL: strings.TrimSpace(string(b)), Schema: *schema, Installation: *installation, Directory: *dir})
	if e != nil {
		return e
	}
	return printValue(result, true)
}
func catalogBackupCLI(ctx context.Context, cl *http.Client, base string, args []string, machine, wait bool) error {
	if len(args) == 0 {
		return usageError("catalog-backups list|create|verify|download|policy|acknowledge")
	}
	endpoint := base + "/v1/catalog-backups"
	switch args[0] {
	case "list":
		var out any
		if e := service.Call(ctx, cl, "GET", endpoint, nil, &out); e != nil {
			return e
		}
		return printValue(out, machine)
	case "create":
		var v catalogbackup.Entry
		if e := service.Call(ctx, cl, "POST", endpoint, nil, &v); e != nil {
			return e
		}
		for wait && v.State == "running" {
			select {
			case <-ctx.Done():
				return errors.New("wait timed out; catalog backup continues on manager")
			case <-time.After(500 * time.Millisecond):
			}
			var out struct {
				Backups []catalogbackup.Entry `json:"backups"`
			}
			if e := service.Call(ctx, cl, "GET", endpoint, nil, &out); e != nil {
				return e
			}
			found := false
			for _, entry := range out.Backups {
				if entry.ID == v.ID {
					v = entry
					found = true
					break
				}
			}
			if !found {
				return errors.New("backup status missing")
			}
		}
		if e := printValue(v, machine); e != nil {
			return e
		}
		if v.State == "error" {
			return errors.New("catalog backup failed")
		}
		return nil
	case "replicate", "drill":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		media := fs.String("media", "", "target media ID; empty for local drill")
		if len(args) < 2 {
			return usageError("catalog-backups replicate|drill ID --media MEDIA")
		}
		if e := fs.Parse(args[2:]); e != nil {
			return e
		}
		var v catalogbackup.Operation
		if e := service.Call(ctx, cl, "POST", endpoint+"/"+url.PathEscape(args[1])+"/maintenance", map[string]string{"kind": args[0], "media_id": *media}, &v); e != nil {
			return e
		}
		for wait && v.State == "running" {
			select {
			case <-ctx.Done():
				return errors.New("wait timed out; operation continues")
			case <-time.After(time.Second):
			}
			var out struct {
				Operations []catalogbackup.Operation `json:"operations"`
			}
			if e := service.Call(ctx, cl, "GET", endpoint, nil, &out); e != nil {
				return e
			}
			for _, op := range out.Operations {
				if op.ID == v.ID {
					v = op
					break
				}
			}
		}
		if e := printValue(v, machine); e != nil {
			return e
		}
		if v.State == "error" {
			return errors.New("catalog operation failed")
		}
		return nil
	case "remote-list":
		if len(args) != 2 {
			return usageError("catalog-backups remote-list MEDIA")
		}
		var out any
		if e := service.Call(ctx, cl, "GET", endpoint+"/remote/"+url.PathEscape(args[1])+"/copies", nil, &out); e != nil {
			return e
		}
		return printValue(out, machine)
	case "verify":
		if len(args) != 2 {
			return usageError("catalog-backups verify ID")
		}
		var v any
		if e := service.Call(ctx, cl, "POST", endpoint+"/"+url.PathEscape(args[1])+"/verify", nil, &v); e != nil {
			return e
		}
		return printValue(v, machine)
	case "download", "remote-download":
		if len(args) < 2 {
			return usageError("catalog-backups download ID --out FILE")
		}
		fs := flag.NewFlagSet("download", flag.ContinueOnError)
		out := fs.String("out", "", "new encrypted archive path")
		media := fs.String("media", "", "remote media ID")
		if e := fs.Parse(args[2:]); e != nil {
			return e
		}
		if *out == "" {
			return usageError("--out required")
		}
		f, e := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		ok := false
		defer func() {
			f.Close()
			if !ok {
				os.Remove(*out)
			}
		}()
		path := endpoint + "/" + url.PathEscape(args[1]) + "/download"
		if args[0] == "remote-download" {
			if *media == "" {
				return usageError("--media required")
			}
			path = endpoint + "/remote/" + url.PathEscape(*media) + "/" + url.PathEscape(args[1]) + "/download"
		}
		r, e := http.NewRequestWithContext(ctx, "GET", path, nil)
		if e != nil {
			return e
		}
		res, e := cl.Do(r)
		if e != nil {
			return e
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return fmt.Errorf("catalog archive download HTTP %d", res.StatusCode)
		}
		n, e := io.Copy(f, res.Body)
		if e != nil {
			return e
		}
		if e = f.Sync(); e != nil {
			return e
		}
		ok = true
		return printValue(map[string]any{"file": *out, "bytes": n}, machine)
	case "policy":
		fs := flag.NewFlagSet("policy", flag.ContinueOnError)
		interval := fs.Int("interval-seconds", -1, "0 disables scheduling")
		version := fs.Int("version", -1, "current policy version")
		target := fs.String("media", "", "automatic replication target")
		localDays := fs.Int("local-days", 0, "local retention; 0 unlimited")
		remoteDays := fs.Int("remote-days", 0, "independent remote retention")
		drillInterval := fs.Int("drill-interval-seconds", 0, "0 disables drills")
		if e := fs.Parse(args[1:]); e != nil {
			return e
		}
		if *interval < 0 || *version < 0 {
			return usageError("--interval-seconds and --version required")
		}
		var v any
		if e := service.Call(ctx, cl, "PUT", endpoint+"/policy", catalogbackup.Policy{Version: *version, IntervalSeconds: *interval, TargetMediaID: *target, LocalRetentionDays: *localDays, RemoteRetentionDays: *remoteDays, DrillIntervalSeconds: *drillInterval}, &v); e != nil {
			return e
		}
		return printValue(v, machine)
	case "acknowledge":
		fs := flag.NewFlagSet("acknowledge", flag.ContinueOnError)
		id := fs.String("archive-id", "", "recovered archive ID")
		confirm := fs.String("confirm-installation", "", "installation ID after reviewing recovery")
		if e := fs.Parse(args[1:]); e != nil {
			return e
		}
		if *id == "" || *confirm == "" {
			return usageError("--archive-id --confirm-installation required")
		}
		var v json.RawMessage
		if e := service.Call(ctx, cl, "POST", endpoint+"/recovery/acknowledge", map[string]string{"archive_id": *id, "confirm_installation": *confirm}, &v); e != nil {
			return e
		}
		return printValue(v, machine)
	default:
		return usageError("catalog-backups list|create|verify|download|policy|acknowledge")
	}
}
