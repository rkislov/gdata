// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package protection

import (
	"gdata/srk/internal/model"
	"testing"
	"time"
)

func fixture() (time.Time, []model.Job, model.ProtectionSettings, []Copy, []Restore) {
	now := time.Now().UTC()
	id := "11111111111111111111111111111111"
	jobs := []model.Job{{ID: id, Name: "Backup"}}
	p := model.ProtectionSettings{Sites: []model.ProtectionSite{{ID: "main", Name: "Main", DisasterDomain: "main"}, {ID: "remote", Name: "Remote", DisasterDomain: "remote"}}, Storages: []model.ProtectionStorage{{ID: "a", Name: "A", Kind: "repository", RepositoryID: "repo-a", MediaID: "media-a", SiteID: "main", FailureDomain: "array-a", MediumType: "disk"}, {ID: "b", Name: "B", Kind: "repository", RepositoryID: "repo-b", MediaID: "media-b", SiteID: "remote", FailureDomain: "array-b", MediumType: "flash"}}, Policies: []model.ProtectionPolicy{{JobID: id, SourceSiteID: "main", SourceFailureDomain: "source", SourceMediumType: "disk", RPOSeconds: 3600, RestoreMaxAgeSeconds: 86400}}}
	copies := []Copy{{JobID: id, StorageID: "a", SnapshotID: "one", MediaID: "media-a", DataAt: now.Add(-time.Minute), Available: true, Verified: true}, {JobID: id, StorageID: "b", SnapshotID: "two", MediaID: "media-b", DataAt: now.Add(-2 * time.Minute), Available: true, Verified: true}}
	restores := []Restore{{JobID: id, RunID: "restore", At: now.Add(-time.Hour), Passed: true}}
	return now, jobs, p, copies, restores
}
func TestProtectionRules(t *testing.T) {
	for _, scenario := range []string{"protected", "same-domain", "same-site-alias", "one-type", "stale", "remote-stale", "onsite-stale", "offline", "unverified", "failed-restore", "stale-restore", "duplicate-snapshots", "truncated", "source-shared"} {
		t.Run(scenario, func(t *testing.T) {
			now, jobs, p, c, r := fixture()
			switch scenario {
			case "same-domain":
				p.Storages[1].FailureDomain = p.Storages[0].FailureDomain
			case "same-site-alias":
				p.Sites[1].DisasterDomain = p.Sites[0].DisasterDomain
			case "one-type":
				p.Storages[1].MediumType = "disk"
			case "stale":
				for i := range c {
					c[i].DataAt = now.Add(-2 * time.Hour)
				}
			case "onsite-stale":
				c[0].DataAt = now.Add(-2 * time.Hour)
			case "remote-stale":
				c[1].DataAt = now.Add(-2 * time.Hour)
			case "offline":
				c[1].Available = false
			case "unverified":
				c[1].Verified = false
			case "failed-restore":
				r = append(r, Restore{JobID: jobs[0].ID, RunID: "failed", At: now})
			case "stale-restore":
				r[0].At = now.Add(-48 * time.Hour)
			case "duplicate-snapshots":
				c[1].StorageID = "a"
			case "source-shared":
				p.Storages[0].FailureDomain = "source"
			}
			card := Evaluate(now, jobs, p, c, r, scenario == "truncated")[0]
			if scenario == "protected" {
				if card.State != "protected" || card.TotalCopies != 3 || !card.Remote || !card.RestoreOK {
					t.Fatal(card)
				}
			} else if card.State == "protected" {
				t.Fatal("false protected", card)
			}
		})
	}
}
func TestLonghornCopiesRemainDependent(t *testing.T) {
	now, jobs, p, c, r := fixture()
	r = nil
	p.Storages = append(p.Storages, model.ProtectionStorage{ID: "lh", Name: "Backupstore", Kind: "longhorn", MediumType: "disk", SiteID: "remote", FailureDomain: "lh-backupstore"})
	p.Policies[0].LonghornStorageID = "lh"
	for i := range c {
		c[i].Longhorn = true
		c[i].MetadataOnly = true
		c[i].Namespaces = []string{"app"}
	}
	first := Evaluate(now, jobs, p, c, r, false)[0]
	if first.BackupCopies != 0 || first.MetadataCopies != 2 || first.RPOOK {
		t.Fatal("unconfirmed PVC data counted", first)
	}
	for _, v := range c {
		r = append(r, Restore{JobID: jobs[0].ID, SnapshotID: v.SnapshotID, MediaID: v.MediaID, Namespace: "app", At: now, Passed: true, ApplicationCheck: true})
	}
	card := Evaluate(now, jobs, p, c, r, false)[0]
	if card.BackupCopies != 1 || card.State == "protected" || !card.RestoreOK || !card.Remote {
		t.Fatal("shared backupstore counted twice", card)
	}
	c[1].Namespaces = append(c[1].Namespaces, "missing-check")
	if Evaluate(now, jobs, p, c, r, false)[0].RestoreOK {
		t.Fatal("namespace coverage ignored")
	}
}
func TestSettingsRejectAliasAndInvalidSite(t *testing.T) {
	_, _, p, _, _ := fixture()
	if e := p.Validate(); e != nil {
		t.Fatal(e)
	}
	p.Storages[1].RepositoryID = p.Storages[0].RepositoryID
	if p.Validate() == nil {
		t.Fatal("duplicate repository accepted")
	}
}
