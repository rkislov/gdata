// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"flag"
	"gdata/srk/internal/model"
	"gdata/srk/internal/tape"
	"net/url"
	"os"
)

func tapeCommand(component string, args []string) (method, path string, body any, wait bool, err error) {
	method = "GET"
	if component == "media" && len(args) == 1 && args[0] == "capabilities" {
		path = "/v1/tape/capabilities"
		return
	}
	if component != "manager" || len(args) < 2 {
		err = usageError("manager tape inventory|queue MEDIA | ACTION MEDIA BARCODE [options]; media tape capabilities")
		return
	}
	action := args[0]
	path = "/v1/media/" + url.PathEscape(args[1]) + "/tape"
	if action == "pool-set" {
		if len(args) != 3 {
			err = usageError("manager tape pool-set MEDIA FILE.json")
			return
		}
		var raw []byte
		raw, err = os.ReadFile(args[2])
		if err != nil {
			return
		}
		var p tape.Pool
		err = json.Unmarshal(raw, &p)
		if err != nil {
			return
		}
		method = "PUT"
		path += "/pools/" + url.PathEscape(p.ID)
		body = p
		return
	}
	if action == "queue" {
		path += "/queue"
		return
	}
	if action == "inventory" {
		return
	}
	if len(args) < 3 {
		err = usageError("BARCODE required")
		return
	}
	fs := flag.NewFlagSet("tape "+action, flag.ContinueOnError)
	drive := fs.String("drive", "", "configured drive ID")
	label := fs.String("label", "", "logical volume label")
	confirm := fs.String("confirm-barcode", "", "exact cartridge barcode")
	reason := fs.String("reason", "", "reason for label/erase/recycle")
	mode := fs.String("mode", "quick", "quick or long erase")
	snapshot := fs.String("run", "", "source backup run ID")
	copyID := fs.String("copy", "", "tape copy ID")
	agent := fs.String("agent", "", "restore agent or proxy ID")
	days := fs.Int("retention-days", 30, "independent retention in days")
	hold := fs.Bool("hold", false, "retain tape copy indefinitely")
	if err = fs.Parse(args[3:]); err != nil {
		return
	}
	if fs.NArg() != 0 {
		err = usageError("unexpected tape arguments")
		return
	}
	v := model.TapeRequest{Action: action, Barcode: args[2], DriveID: *drive, Label: *label, Confirm: *confirm, Reason: *reason, EraseMode: *mode, SnapshotID: *snapshot, CopyID: *copyID, AgentID: *agent, RetentionDays: *days, Hold: *hold}
	if action == "archive" {
		v.Action = "write"
		v.PoolID = args[2]
		v.Barcode = ""
	}
	if err = tape.ValidateRequest(v); err != nil {
		return
	}
	method = "POST"
	path += "/operations"
	body = v
	wait = true
	return
}
