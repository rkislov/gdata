// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

import (
	"errors"
	"regexp"
	"slices"
)

type User struct {
	AllRestoreSites bool     `json:"all_restore_sites"`
	RestoreSiteIDs  []string `json:"restore_site_ids"`
	Username        string   `json:"username"`
	Name            string   `json:"name"`
	Role            string   `json:"role"`
	Version         int      `json:"version"`
	Disabled        bool     `json:"disabled"`
	AllJobs         bool     `json:"all_jobs"`
	AllSites        bool     `json:"all_sites"`
	JobIDs          []string `json:"job_ids"`
	SiteIDs         []string `json:"site_ids"`
	Permissions     []string `json:"permissions"`
}

func (u User) Validate() error {
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`).MatchString(u.Username) || u.Username == "bootstrap" || len(u.Name) > 200 || u.Version < 0 {
		return errors.New("invalid username, name or version")
	}
	if !slices.Contains([]string{"admin", "operator", "auditor"}, u.Role) {
		return errors.New("invalid role")
	}
	if len(u.JobIDs) > 10000 || len(u.SiteIDs) > 100 || len(u.RestoreSiteIDs) > 100 || len(u.Permissions) > 8 {
		return errors.New("too many scope entries")
	}
	for _, p := range u.Permissions {
		if !slices.Contains([]string{"run", "cancel", "verify", "restore", "erase", "cleanup"}, p) {
			return errors.New("unknown permission")
		}
	}
	return nil
}
func (u User) Can(permission string) bool {
	return u.Role == "admin" || u.Role == "operator" && slices.Contains(u.Permissions, permission)
}
func (u User) Global() bool { return u.Role == "admin" || u.AllJobs && u.AllSites }
