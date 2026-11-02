// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"flag"
	"gdata/srk/internal/model"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func repositoryCommand(component string, args []string) (method, path string, body any, wait bool, err error) {
	if component != "manager" && component != "media" {
		err = usageError("repository commands require manager or media")
		return
	}
	if len(args) == 0 {
		err = usageError("repository snapshots|gc-preview|gc|import|policy required")
		return
	}
	kind := args[0]
	args = args[1:]
	prefix := "/v1/repository"
	if component == "manager" {
		if len(args) == 0 {
			err = usageError("media server ID required")
			return
		}
		prefix = "/v1/media/" + url.PathEscape(args[0])
		args = args[1:]
	}
	method = "GET"
	if kind == "snapshots" {
		path = prefix + "/snapshots"
		return
	}
	if kind == "audit" && component == "media" {
		path = prefix + "/audit"
		return
	}
	if kind == "gc-preview" {
		path = prefix + "/gc"
		return
	}
	o := model.MaintenanceOptions{}
	if kind == "policy" {
		if len(args) == 0 {
			err = usageError("snapshot ID required")
			return
		}
		o.SnapshotID = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("repository "+kind, flag.ContinueOnError)
	confirmed := fs.Bool("confirm", false, "confirm applying the reviewed operation")
	file := fs.String("plan", "", "cleanup preview JSON file")
	ids := fs.String("snapshots", "", "comma-separated snapshot IDs")
	agent := fs.String("agent", "", "restore agent ID")
	days := fs.Int("retention-days", 30, "new minimum retention period for import")
	hold := fs.String("hold", "", "true or false")
	reason := fs.String("reason", "", "hold reason")
	expires := fs.String("expires", "", "new RFC3339 retention deadline")
	if err = fs.Parse(args); err != nil {
		err = &cliError{2, err}
		return
	}
	if !*confirmed {
		err = usageError("--confirm is required; inspect repository snapshots or gc-preview first")
		return
	}
	method = "POST"
	path = prefix + "/" + kind
	switch kind {
	case "gc":
		if *file == "" {
			err = usageError("--plan FILE required")
			return
		}
		b, e := os.ReadFile(*file)
		if e != nil {
			err = e
			return
		}
		var plan model.GCPlan
		if err = json.Unmarshal(b, &plan); err != nil {
			return
		}
		o.Plan = &plan
	case "import":
		if *ids == "" {
			err = usageError("--snapshots is required")
			return
		}
		o.SnapshotIDs = strings.Split(*ids, ",")
		for i := range o.SnapshotIDs {
			o.SnapshotIDs[i] = strings.TrimSpace(o.SnapshotIDs[i])
		}
		o.AgentID = *agent
		o.RetentionDays = *days
		if component == "manager" && *agent == "" {
			err = usageError("--agent is required for catalog registration")
			return
		}
	case "policy":
		p := model.SnapshotPolicy{Reason: *reason}
		if *hold != "" {
			v, e := strconv.ParseBool(*hold)
			if e != nil {
				err = usageError("--hold must be true or false")
				return
			}
			p.Hold = &v
		}
		if *expires != "" {
			v, e := time.Parse(time.RFC3339, *expires)
			if e != nil {
				err = usageError("--expires requires RFC3339 with timezone")
				return
			}
			p.ExpiresAt = &v
		}
		if p.Hold == nil && p.ExpiresAt == nil {
			err = usageError("set --hold and/or --expires")
			return
		}
		o.Policy = &p
	default:
		err = usageError("unknown repository command")
		return
	}
	if component == "manager" {
		body = o
		wait = true
		return
	}
	switch kind {
	case "gc":
		body = struct {
			OperationID string        `json:"operation_id"`
			Plan        *model.GCPlan `json:"plan"`
		}{model.ID(), o.Plan}
	case "import":
		body = struct {
			SnapshotIDs   []string `json:"snapshot_ids"`
			RetentionDays int      `json:"retention_days"`
		}{o.SnapshotIDs, o.RetentionDays}
	case "policy":
		path = prefix + "/snapshots/" + url.PathEscape(o.SnapshotID) + "/policy"
		body = o.Policy
	}
	return
}
