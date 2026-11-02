// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

import (
	"errors"
	"regexp"
)

type ProtectionSite struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	DisasterDomain string `json:"disaster_domain"`
}
type ProtectionStorage struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`        // repository, tape, flash, longhorn
	MediumType    string `json:"medium_type"` // disk, tape, flash, cloud
	SiteID        string `json:"site_id"`
	FailureDomain string `json:"failure_domain"`
	MediaID       string `json:"media_id,omitempty"`
	RepositoryID  string `json:"repository_id,omitempty"`
	LibraryID     string `json:"library_id,omitempty"`
	Barcode       string `json:"barcode,omitempty"`
	VolumeID      string `json:"volume_id,omitempty"`
}
type ProtectionPolicy struct {
	JobID                string `json:"job_id"`
	SourceSiteID         string `json:"source_site_id"`
	SourceMediumType     string `json:"source_medium_type"`
	SourceFailureDomain  string `json:"source_failure_domain"`
	RPOSeconds           int    `json:"rpo_seconds"`
	RestoreMaxAgeSeconds int    `json:"restore_max_age_seconds"`
	LonghornStorageID    string `json:"longhorn_storage_id,omitempty"`
}
type ProtectionSettings struct {
	RestoreTargets []RestoreTarget     `json:"restore_targets"`
	Version        int                 `json:"version"`
	Sites          []ProtectionSite    `json:"sites"`
	Storages       []ProtectionStorage `json:"storages"`
	Policies       []ProtectionPolicy  `json:"policies"`
}

func (p ProtectionSettings) Validate() error {
	id := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	medium := func(s string) bool { return s == "disk" || s == "tape" || s == "flash" || s == "cloud" }
	if len(p.Sites) > 100 || len(p.Storages) > 500 || len(p.Policies) > 10000 || p.Version < 0 {
		return errors.New("protection settings limit exceeded")
	}
	sites := map[string]bool{}
	stores := map[string]ProtectionStorage{}
	physical := map[string]bool{}
	for _, s := range p.Sites {
		if !id.MatchString(s.ID) || sites[s.ID] || len(s.Name) == 0 || len(s.Name) > 200 || !id.MatchString(s.DisasterDomain) {
			return errors.New("invalid or duplicate site")
		}
		sites[s.ID] = true
	}
	for _, s := range p.Storages {
		if !id.MatchString(s.ID) || stores[s.ID].ID != "" || len(s.Name) == 0 || len(s.Name) > 200 || !sites[s.SiteID] || !id.MatchString(s.FailureDomain) || !medium(s.MediumType) {
			return errors.New("invalid storage/site/failure domain")
		}
		key := ""
		switch s.Kind {
		case "repository":
			if !id.MatchString(s.MediaID) || !id.MatchString(s.RepositoryID) || (s.MediumType != "disk" && s.MediumType != "flash" && s.MediumType != "cloud") {
				return errors.New("repository identity and medium required")
			}
			key = "repo/" + s.RepositoryID
		case "tape":
			if !id.MatchString(s.MediaID) || !id.MatchString(s.LibraryID) || !id.MatchString(s.Barcode) || s.MediumType != "tape" {
				return errors.New("tape library and barcode required")
			}
			key = "tape/" + s.LibraryID + "/" + s.Barcode
		case "flash":
			if !id.MatchString(s.MediaID) || !id.MatchString(s.VolumeID) || s.MediumType != "flash" {
				return errors.New("removable volume identity required")
			}
			key = "flash/" + s.VolumeID
		case "longhorn":
			key = "longhorn/" + s.ID
		default:
			return errors.New("unknown storage kind")
		}
		if physical[key] {
			return errors.New("same physical storage registered more than once")
		}
		physical[key] = true
		stores[s.ID] = s
	}
	if len(p.RestoreTargets) > 500 {
		return errors.New("too many restore targets")
	}
	targetIDs := map[string]bool{}
	bindings := map[string]string{}
	for _, t := range p.RestoreTargets {
		if e := t.Validate(); e != nil {
			return e
		}
		if !sites[t.SiteID] || targetIDs[t.ID] {
			return errors.New("unknown target site or duplicate target ID")
		}
		targetIDs[t.ID] = true
		key := t.NodeID + "/" + t.Kind + "/" + t.Profile
		if t.Kind == "longhorn" {
			key = t.NodeID + "/kubernetes/" + t.Profile
		}
		// File and PostgreSQL restores both write the agent's local filesystem.
		if t.Kind == "files" || t.Kind == "postgresql" {
			key = t.NodeID + "/filesystem"
		}
		if site, ok := bindings[key]; ok && site != t.SiteID {
			return errors.New("same restore endpoint assigned to conflicting sites")
		}
		bindings[key] = t.SiteID
	}

	jobs := map[string]bool{}
	for _, v := range p.Policies {
		if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(v.JobID) || jobs[v.JobID] || !sites[v.SourceSiteID] || !medium(v.SourceMediumType) || !id.MatchString(v.SourceFailureDomain) || v.RPOSeconds < 60 || v.RPOSeconds > 31536000 || v.RestoreMaxAgeSeconds < 60 || v.RestoreMaxAgeSeconds > 31536000 {
			return errors.New("invalid protection policy")
		}
		if v.LonghornStorageID != "" && stores[v.LonghornStorageID].Kind != "longhorn" {
			return errors.New("Longhorn dependency requires a Longhorn backupstore")
		}
		jobs[v.JobID] = true
	}
	return nil
}
