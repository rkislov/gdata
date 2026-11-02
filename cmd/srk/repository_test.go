// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"gdata/srk/internal/model"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRepositoryCLIRequiresExplicitConfirmation(t *testing.T) {
	for _, args := range [][]string{{"gc", "media-1"}, {"import", "media-1", "--snapshots", "id", "--agent", "agent"}, {"policy", "media-1", "snapshot", "--hold", "false"}} {
		if _, _, _, _, e := repositoryCommand("manager", args); e == nil {
			t.Fatalf("unconfirmed mutation allowed: %v", args)
		}
	}
}
func TestRepositoryCLIUsesReviewedPlan(t *testing.T) {
	p := model.GCPlan{RepositoryID: model.ID(), At: time.Now().UTC(), Token: "reviewed-token"}
	b, _ := json.Marshal(p)
	file := filepath.Join(t.TempDir(), "plan.json")
	if e := os.WriteFile(file, b, 0600); e != nil {
		t.Fatal(e)
	}
	method, path, body, wait, e := repositoryCommand("manager", []string{"gc", "media-1", "--plan", file, "--confirm"})
	if e != nil || method != "POST" || path != "/v1/media/media-1/gc" || !wait {
		t.Fatalf("wrong command routing: %s %s %v", method, path, e)
	}
	o := body.(model.MaintenanceOptions)
	if o.Plan.Token != p.Token || o.Plan.RepositoryID != p.RepositoryID {
		t.Fatal("reviewed plan was not preserved")
	}
}
