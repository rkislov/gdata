// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"flag"
	"gdata/srk/internal/model"
	"net/url"
	"os"
	"strings"
	"time"
)

func walCommand(component string, args []string) (method, path string, body any, wait bool, err error) {
	method = "GET"
	if len(args) < 1 {
		err = usageError("wal list|calendar|gc-preview|gc required")
		return
	}
	if args[0] == "replication" && component == "manager" {
		return walReplicationCommand(args[1:])
	}
	kind := args[0]
	args = args[1:]
	prefix := "/v1/wal/"
	if component == "manager" {
		if len(args) < 2 {
			err = usageError("MEDIA_ID ARCHIVE_ID required")
			return
		}
		prefix = "/v1/media/" + url.PathEscape(args[0]) + "/wal/"
		args = args[1:]
	} else if component != "media" {
		err = usageError("WAL inspection requires manager or media")
		return
	}
	if len(args) < 1 {
		err = usageError("ARCHIVE_ID required")
		return
	}
	archive := args[0]
	args = args[1:]
	path = prefix + url.PathEscape(archive)
	if kind == "list" {
		return
	}
	if kind == "calendar" {
		path += "/calendar"
		return
	}
	fs := flag.NewFlagSet("wal "+kind, flag.ContinueOnError)
	before := fs.String("before", time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339), "only data archived before this date")
	orphan := fs.Bool("allow-orphan", false, "explicitly review an archive without physical bases")
	file := fs.String("plan", "", "reviewed WAL plan JSON")
	confirm := fs.Bool("confirm", false, "confirm reviewed cleanup")
	if err = fs.Parse(args); err != nil {
		return
	}
	if kind == "gc-preview" {
		path += "/gc?before=" + url.QueryEscape(*before)
		if *orphan {
			path += "&allow_orphan=true"
		}
		return
	}
	if kind != "gc" || !*confirm || *file == "" {
		err = usageError("wal gc requires --plan FILE --confirm")
		return
	}
	var p model.WALGCPlan
	b, e := os.ReadFile(*file)
	if e != nil {
		err = e
		return
	}
	if err = json.Unmarshal(b, &p); err != nil {
		return
	}
	if p.ArchiveID != archive {
		err = usageError("WAL plan belongs to a different archive")
		return
	}
	method = "POST"
	if component == "manager" {
		path = "/v1/media/" + url.PathEscape(prefixMedia(prefix)) + "/wal-gc"
		body = model.MaintenanceOptions{WALPlan: &p}
		wait = true
	} else {
		path += "/gc"
		body = map[string]any{"operation_id": model.ID(), "plan": p}
	}
	return
}
func prefixMedia(s string) string {
	parts := strings.Split(s, "/")
	if len(parts) > 3 {
		v, _ := url.PathUnescape(parts[3])
		return v
	}
	return ""
}

func walReplicationCommand(args []string) (method, path string, body any, wait bool, err error) {
	method = "GET"
	if len(args) < 1 {
		err = usageError("wal replication list|configure RUN_ID|enable POLICY|disable POLICY|sync POLICY|release-protection POLICY --reason TEXT --confirm")
		return
	}
	action := args[0]
	if action == "list" {
		path = "/v1/wal-replication-policies"
		return
	}
	if len(args) < 2 {
		err = usageError("run or policy ID required")
		return
	}
	method = "POST"
	id := url.PathEscape(args[1])
	if action == "configure" {
		fs := flag.NewFlagSet("WAL replication", flag.ContinueOnError)
		interval := fs.Int("interval-seconds", 30, "polling interval 30–3600s")
		if err = fs.Parse(args[2:]); err != nil {
			return
		}
		if *interval < 30 || *interval > 3600 {
			err = usageError("interval must be 30–3600 seconds")
			return
		}
		path = "/v1/runs/" + id + "/wal-replication"
		body = map[string]any{"interval_seconds": *interval, "enabled": true}
		return
	}
	path = "/v1/wal-replication-policies/" + id
	switch action {
	case "release-protection":
		fs := flag.NewFlagSet("release WAL protection", flag.ContinueOnError)
		reason := fs.String("reason", "", "reason for abandoning pending WAL")
		confirm := fs.Bool("confirm", false, "allow GC of unacknowledged WAL")
		if err = fs.Parse(args[2:]); err != nil {
			return
		}
		if !*confirm || strings.TrimSpace(*reason) == "" {
			err = usageError("--reason and --confirm required")
			return
		}
		path += "/release-protection"
		body = map[string]any{"reason": *reason, "confirm": true}
	case "enable", "disable":
		path += "/enabled"
		body = map[string]bool{"enabled": action == "enable"}
	case "sync":
		path += "/run"
	default:
		err = usageError("unknown WAL replication command")
	}
	return
}
