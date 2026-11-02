// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package protection

import (
	"gdata/srk/internal/model"
	"sort"
	"time"
)

type Copy struct {
	JobID        string    `json:"job_id"`
	StorageID    string    `json:"storage_id"`
	SnapshotID   string    `json:"snapshot_id"`
	MediaID      string    `json:"media_id"`
	DataAt       time.Time `json:"data_at"`
	Available    bool      `json:"available"`
	Verified     bool      `json:"verified"`
	MetadataOnly bool      `json:"metadata_only"`
	Reason       string    `json:"reason,omitempty"`
	Namespaces   []string  `json:"namespaces,omitempty"`
	Longhorn     bool      `json:"longhorn"`
}
type Restore struct {
	JobID            string    `json:"job_id"`
	RunID            string    `json:"run_id"`
	SnapshotID       string    `json:"snapshot_id"`
	MediaID          string    `json:"media_id"`
	Namespace        string    `json:"namespace,omitempty"`
	At               time.Time `json:"at"`
	Passed           bool      `json:"passed"`
	ApplicationCheck bool      `json:"application_check"`
}
type Card struct {
	Job              model.Job               `json:"job"`
	Policy           *model.ProtectionPolicy `json:"policy,omitempty"`
	State            string                  `json:"state"`
	Reasons          []string                `json:"reasons"`
	Copies           []Copy                  `json:"copies"`
	BackupCopies     int                     `json:"backup_copies"`
	MetadataCopies   int                     `json:"metadata_copies"`
	TotalCopies      int                     `json:"total_copies"`
	MediumTypes      []string                `json:"medium_types"`
	Remote           bool                    `json:"remote"`
	RPOOK            bool                    `json:"rpo_ok"`
	LastDataAt       *time.Time              `json:"last_data_at,omitempty"`
	AgeSeconds       *int64                  `json:"age_seconds,omitempty"`
	RemoteLagSeconds *int64                  `json:"remote_lag_seconds,omitempty"`
	RestoreOK        bool                    `json:"restore_ok"`
	LastRestore      *Restore                `json:"last_restore,omitempty"`
	Dependency       string                  `json:"dependency,omitempty"`
}

func Evaluate(now time.Time, jobs []model.Job, settings model.ProtectionSettings, copies []Copy, restores []Restore, incomplete bool) []Card {
	policies := map[string]model.ProtectionPolicy{}
	sites := map[string]model.ProtectionSite{}
	stores := map[string]model.ProtectionStorage{}
	for _, p := range settings.Policies {
		policies[p.JobID] = p
	}
	for _, s := range settings.Sites {
		sites[s.ID] = s
	}
	for _, s := range settings.Storages {
		stores[s.ID] = s
	}
	copiesByJob := map[string][]Copy{}
	restoresByJob := map[string][]Restore{}
	for _, cp := range copies {
		copiesByJob[cp.JobID] = append(copiesByJob[cp.JobID], cp)
	}
	for _, r := range restores {
		restoresByJob[r.JobID] = append(restoresByJob[r.JobID], r)
	}
	out := []Card{}
	for _, job := range jobs {
		copies := copiesByJob[job.ID]
		restores := restoresByJob[job.ID]
		card := Card{Job: job, State: "attention", Reasons: []string{}, Copies: []Copy{}, MediumTypes: []string{}}
		p, configured := policies[job.ID]
		if configured {
			card.Policy = &p
		} else {
			card.Reasons = append(card.Reasons, "Не настроены площадка источника, RPO и период проверки восстановления")
		}
		types := map[string]bool{}
		domains := map[string]bool{}
		freshDomains := map[string]bool{}
		physical := map[string]Copy{}
		if configured {
			types[p.SourceMediumType] = true
		}
		// One latest usable recovery point per physical storage. Repeated snapshots
		// do not multiply independent copies.
		for _, cp := range copies {
			if cp.JobID != job.ID {
				continue
			}
			key := cp.StorageID
			if key == "" {
				key = "unmapped/" + cp.MediaID
			}
			old, found := physical[key]
			eligible := cp.Available && cp.Verified
			oldEligible := old.Available && old.Verified
			if !found || eligible && !oldEligible || eligible == oldEligible && cp.DataAt.After(old.DataAt) {
				physical[key] = cp
			}
		}
		var latest, remote time.Time
		usableCopies := 0
		usedLonghorn := false
		hasKube := false
		namespaces := map[string]bool{}
		for _, cp := range physical {
			s, mapped := stores[cp.StorageID]
			if cp.Longhorn || cp.MetadataOnly {
				hasKube = true
				for _, ns := range cp.Namespaces {
					namespaces[ns] = true
				}
			}
			if !cp.Available || !cp.Verified || cp.DataAt.IsZero() || cp.DataAt.After(now) {
				card.Copies = append(card.Copies, cp)
				continue
			}
			if !cp.Longhorn && !cp.MetadataOnly {
				usableCopies++
			}
			if !cp.Longhorn && !cp.MetadataOnly && cp.DataAt.After(latest) {
				latest = cp.DataAt
			}
			if !mapped {
				cp.Reason = "Хранилище не привязано к площадке"
				card.Copies = append(card.Copies, cp)
				continue
			}
			if cp.Longhorn || cp.MetadataOnly {
				card.MetadataCopies++
				cp.MetadataOnly = true
				if !cp.Longhorn {
					card.Dependency = "Данные PVC не подтверждены: архив содержит API-объекты"
					card.Copies = append(card.Copies, cp)
					continue
				}
				dep := stores[p.LonghornStorageID]
				card.Dependency = "Копии метаданных используют один внешний backupstore Longhorn; независимость данных PVC не размножается"
				complete := dep.Kind == "longhorn" && len(cp.Namespaces) > 0
				for _, ns := range cp.Namespaces {
					confirmed := false
					for _, r := range restores {
						if r.JobID == job.ID && r.Passed && r.ApplicationCheck && r.SnapshotID == cp.SnapshotID && r.MediaID == cp.MediaID && r.Namespace == ns && !r.At.After(now) && now.Sub(r.At) <= time.Duration(p.RestoreMaxAgeSeconds)*time.Second {
							confirmed = true
						}
					}
					if !confirmed {
						complete = false
					}
				}
				if !complete {
					cp.Reason = "Доступность зависимых PVC этой копии не подтверждена свежими проверками всех namespaces"
					card.Copies = append(card.Copies, cp)
					continue
				}
				if cp.DataAt.After(latest) {
					latest = cp.DataAt
				}
				usableCopies++
				// An external backupstore contributes at most one complete bundle, even if
				// its API metadata has been replicated to several repositories.
				isRemote := configured && sites[s.SiteID].DisasterDomain != sites[p.SourceSiteID].DisasterDomain && sites[dep.SiteID].DisasterDomain != sites[p.SourceSiteID].DisasterDomain
				independent := dep.FailureDomain != p.SourceFailureDomain && s.FailureDomain != p.SourceFailureDomain
				if independent && isRemote && cp.DataAt.After(remote) {
					remote = cp.DataAt
				}
				if independent && !usedLonghorn {
					domains[dep.FailureDomain] = true
					if configured && now.Sub(cp.DataAt) <= time.Duration(p.RPOSeconds)*time.Second {
						freshDomains[dep.FailureDomain] = true
					}
					types[dep.MediumType] = true
					usedLonghorn = true
				}
				cp.Reason = "Метаданные + подтверждённая зависимость Longhorn; общий backupstore считается один раз"
				card.Copies = append(card.Copies, cp)
				continue
			}
			if s.FailureDomain == p.SourceFailureDomain {
				cp.Reason = "Общий домен отказа с оригиналом"
				card.Copies = append(card.Copies, cp)
				continue
			}
			domains[s.FailureDomain] = true
			if configured && now.Sub(cp.DataAt) <= time.Duration(p.RPOSeconds)*time.Second {
				freshDomains[s.FailureDomain] = true
			}
			types[s.MediumType] = true
			if configured && sites[s.SiteID].DisasterDomain != sites[p.SourceSiteID].DisasterDomain && cp.DataAt.After(remote) {
				remote = cp.DataAt
			}
			card.Copies = append(card.Copies, cp)
		}
		card.BackupCopies = len(domains)
		card.TotalCopies = card.BackupCopies
		if configured {
			card.TotalCopies++
		}
		for typ := range types {
			card.MediumTypes = append(card.MediumTypes, typ)
		}
		sort.Strings(card.MediumTypes)
		sort.Slice(card.Copies, func(i, j int) bool { return card.Copies[i].StorageID < card.Copies[j].StorageID })
		if !latest.IsZero() {
			card.LastDataAt = &latest
			age := int64(now.Sub(latest).Seconds())
			card.AgeSeconds = &age
			card.RPOOK = configured && age <= int64(p.RPOSeconds)
		}
		if !remote.IsZero() {
			card.Remote = true
			lag := int64(latest.Sub(remote).Seconds())
			if lag < 0 {
				lag = 0
			}
			card.RemoteLagSeconds = &lag
		}
		var latestAttempt *Restore
		for i := range restores {
			r := restores[i]
			if r.JobID == job.ID && (!hasKube || r.ApplicationCheck) && !r.At.After(now) && (latestAttempt == nil || r.At.After(latestAttempt.At)) {
				copy := r
				latestAttempt = &copy
			}
		}
		card.LastRestore = latestAttempt
		card.RestoreOK = configured && latestAttempt != nil && latestAttempt.Passed && now.Sub(latestAttempt.At) <= time.Duration(p.RestoreMaxAgeSeconds)*time.Second
		if hasKube {
			card.RestoreOK = configured && len(namespaces) > 0
			for ns := range namespaces {
				var attempt *Restore
				for _, r := range restores {
					if r.JobID == job.ID && r.Namespace == ns && r.ApplicationCheck && !r.At.After(now) && (attempt == nil || r.At.After(attempt.At)) {
						v := r
						attempt = &v
					}
				}
				if attempt == nil || !attempt.Passed || now.Sub(attempt.At) > time.Duration(p.RestoreMaxAgeSeconds)*time.Second {
					card.RestoreOK = false
				}
			}
		}
		if card.BackupCopies < 2 {
			card.Reasons = append(card.Reasons, "Меньше двух независимых резервных копий")
		}
		if card.BackupCopies >= 2 && len(freshDomains) < 2 {
			card.Reasons = append(card.Reasons, "Не все обязательные независимые копии укладываются в RPO")
		}
		if len(types) < 2 {
			card.Reasons = append(card.Reasons, "Не подтверждены два типа носителей с учётом оригинала")
		}
		if !card.Remote {
			card.Reasons = append(card.Reasons, "Нет подтверждённой копии за пределами зоны аварии источника")
		}
		if !card.RPOOK {
			card.Reasons = append(card.Reasons, "RPO по возрасту доступной точки восстановления нарушен или неизвестен")
		}
		if !card.RestoreOK {
			card.Reasons = append(card.Reasons, "Нет свежей успешной проверки восстановления либо последняя проверка неуспешна")
		}
		if !remote.IsZero() && now.Sub(remote) > time.Duration(p.RPOSeconds)*time.Second {
			card.Reasons = append(card.Reasons, "Удалённая копия отстаёт от требуемого RPO")
		}
		if incomplete {
			card.Reasons = append(card.Reasons, "История превышает лимит расчёта; полнота данных не подтверждена")
		}
		if len(card.Reasons) == 0 {
			card.State = "protected"
		} else if usableCopies == 0 {
			card.State = "no-confirmed-copy"
		}
		out = append(out, card)
	}
	return out
}
