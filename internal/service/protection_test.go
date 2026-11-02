// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"gdata/srk/internal/model"
	"gdata/srk/internal/tape"
	"testing"
	"time"
)

func TestProtectionRequiresPhysicalTapePresence(t *testing.T) {
	now := time.Now().UTC()
	id := model.ID()
	jobs := []model.Job{{ID: id, Name: "test"}}
	run := model.Run{ID: model.ID(), JobID: id, Job: model.Job{ID: id, MediaID: "media"}, Kind: "backup", Status: "success", CreatedAt: now.Add(-time.Hour), Result: &model.Result{Verified: true, RepositoryID: "repo"}}
	settings := model.ProtectionSettings{Storages: []model.ProtectionStorage{{ID: "tape", Kind: "tape", MediaID: "media", LibraryID: "LIB", Barcode: "TESTL9"}}}
	lib := tape.Library{LibraryID: "LIB", Backend: "scsi", Cartridges: []tape.Medium{{Cartridge: tape.Cartridge{Barcode: "TESTL9", Known: true, Copies: []tape.Copy{{ID: "copy", SnapshotID: run.ID, Verified: true, ExpiresAt: now.Add(time.Hour)}}}}}}
	observations := []protectionObservation{{MediaID: "media", Tape: &lib}}
	for _, present := range []bool{false, true} {
		lib.Cartridges[0].Present = present
		copies, _, _ := protectionEvidence(jobs, []model.Run{run}, settings, observations, now)
		found := false
		for _, c := range copies {
			if c.StorageID == "tape" {
				found = true
				if c.Available != present {
					t.Fatal("catalog presence substituted for physical presence", c)
				}
			}
		}
		if !found {
			t.Fatal("missing tape evidence")
		}
	}
	lib.Backend = "emulator"
	copies, _, _ := protectionEvidence(jobs, []model.Run{run}, settings, observations, now)
	for _, c := range copies {
		if c.StorageID == "tape" && c.Available {
			t.Fatal("emulator counted as hardware")
		}
	}
}
