// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"gdata/srk/internal/kubernetes"
	"gdata/srk/internal/removable"
	"gdata/srk/internal/tape"
	"gdata/srk/internal/virtualization"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type CatalogBackupConfig struct {
	DrillDatabaseURLFile string `json:"drill_database_url_file,omitempty"`
	Directory            string `json:"directory"`
	KeyFile              string `json:"key_file"`
}

type Config struct {
	CatalogBackup            *CatalogBackupConfig              `json:"catalog_backup,omitempty"`
	PersonalCAKeyFile        string                            `json:"personal_ca_key_file,omitempty"`
	Kubernetes               map[string]kubernetes.Profile     `json:"kubernetes,omitempty"`
	Removable                []removable.Config                `json:"removable,omitempty"`
	Tape                     *tape.Config                      `json:"tape,omitempty"`
	WebDAV                   *WebDAVConfig                     `json:"webdav,omitempty"`
	S3                       map[string]S3Profile              `json:"s3,omitempty"`
	MySQL                    map[string]MySQLProfile           `json:"mysql,omitempty"`
	RescueCaptureEnabled     bool                              `json:"rescue_capture_enabled,omitempty"`
	Virtualization           map[string]virtualization.Profile `json:"virtualization,omitempty"`
	PostgreSQL               map[string]PostgreSQLProfile      `json:"postgresql,omitempty"`
	RetentionIntervalSeconds int                               `json:"retention_interval_seconds,omitempty"`
	ReserveBytes             int64                             `json:"reserve_bytes,omitempty"`
	Development              bool                              `json:"development,omitempty"`
	Role                     string                            `json:"role"`
	ID                       string                            `json:"id"`
	Installation             string                            `json:"installation"`
	Listen                   string                            `json:"listen"`
	URL                      string                            `json:"url"`
	ManagerURL               string                            `json:"manager_url,omitempty"`
	CA                       string                            `json:"ca"`
	Cert                     string                            `json:"cert"`
	Key                      string                            `json:"key"`
	Revoked                  string                            `json:"revoked_file,omitempty"`
	DatabaseURL              string                            `json:"database_url,omitempty"`
	PasswordFile             string                            `json:"password_file,omitempty"`
	CapabilityKeyFile        string                            `json:"capability_key_file,omitempty"`
	Repository               string                            `json:"repository,omitempty"`
	AllowedRoots             []string                          `json:"allowed_roots,omitempty"`
	RestoreRoots             []string                          `json:"restore_roots,omitempty"`
	MaxParallel              int                               `json:"max_parallel"`
	PerAgent                 int                               `json:"per_agent"`
	PerMedia                 int                               `json:"per_media"`
}

func Load(path string) (Config, error) {
	var c Config
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	if !json.Valid(b) {
		return c, errors.New("invalid configuration JSON")
	}
	if c.MaxParallel == 0 {
		c.MaxParallel = 4
	}
	if c.PerAgent == 0 {
		c.PerAgent = 1
	}
	if c.PerMedia == 0 {
		c.PerMedia = 4
	}
	if c.ReserveBytes == 0 {
		c.ReserveBytes = 1 << 30
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if c.CatalogBackup != nil {
		if c.Role != "manager" || !filepath.IsAbs(c.CatalogBackup.Directory) || !filepath.IsAbs(c.CatalogBackup.KeyFile) {
			return errors.New("catalog backup requires manager role and absolute directory/key paths")
		}
		if c.CatalogBackup.DrillDatabaseURLFile != "" && !filepath.IsAbs(c.CatalogBackup.DrillDatabaseURLFile) {
			return errors.New("drill database URL file must be absolute")
		}
		rel, e := filepath.Rel(c.CatalogBackup.Directory, c.CatalogBackup.KeyFile)
		if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("catalog encryption key must be outside backup directory")
		}
	}

	if c.PersonalCAKeyFile != "" && c.Role != "manager" {
		return errors.New("personal certificate issuer is manager-only")
	}
	for name, p := range c.Kubernetes {
		if c.Role != "agent" || !profileName.MatchString(name) {
			return errors.New("Kubernetes profiles require an agent and a valid profile name")
		}
		if e := p.Validate(); e != nil {
			return e
		}
	}

	seenVolumes := map[string]bool{}
	for _, v := range c.Removable {
		if c.Role != "media" {
			return errors.New("removable volumes belong on media")
		}
		if e := v.Validate(); e != nil {
			return e
		}
		if seenVolumes[v.ID] || seenVolumes[v.Mount] || seenVolumes[v.UUID] {
			return errors.New("duplicate removable volume")
		}
		seenVolumes[v.ID] = true
		seenVolumes[v.Mount] = true
		seenVolumes[v.UUID] = true
	}

	if c.Tape != nil {
		if c.Role != "media" || (c.Tape.Backend == "emulator" && !c.Development) {
			return errors.New("tape emulator requires explicit development media configuration")
		}
		if e := c.Tape.Validate(); e != nil {
			return e
		}
	}

	if c.RescueCaptureEnabled {
		u, e := url.Parse(c.ManagerURL)
		if c.Role != "media" || e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("rescue capture requires media role and an HTTPS manager_url")
		}
	}

	if len(c.Virtualization) > 0 && c.Role != "proxy" {
		return errors.New("virtualization profiles belong on proxy")
	}
	for name, p := range c.Virtualization {
		if name == "" || len(name) > 64 {
			return errors.New("invalid virtualization profile name")
		}
		if e := virtualization.Validate(p); e != nil {
			return e
		}
	}

	if e := c.validateWebDAV(); e != nil {
		return e
	}
	if e := c.validateData(); e != nil {
		return e
	}
	if e := c.validatePostgreSQL(); e != nil {
		return e
	}
	if c.ID == "" || c.Installation == "" {
		return errors.New("id and installation are required")
	}
	switch c.Role {
	case "manager", "media", "agent", "proxy", "admin", "auditor", "user", "wal":
	default:
		return errors.New("unknown component role")
	}
	if c.CA == "" || c.Cert == "" || c.Key == "" {
		return errors.New("CA, certificate and key are required")
	}
	for _, p := range []string{c.CA, c.Cert, c.Key} {
		if _, e := os.Stat(p); e != nil {
			return e
		}
	}
	for _, p := range []string{c.Key, c.PasswordFile, c.CapabilityKeyFile, c.PersonalCAKeyFile} {
		if p == "" {
			continue
		}
		info, e := os.Stat(p)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("private credential file %s must have mode 0600 or 0400", p)
		}
	}
	if c.MaxParallel < 1 || c.PerAgent < 1 || c.PerMedia < 1 {
		return errors.New("parallel limits must be positive")
	}
	if c.ReserveBytes < 0 {
		return errors.New("reserve_bytes must not be negative")
	}
	if c.RetentionIntervalSeconds != 0 && (c.RetentionIntervalSeconds < 60 || c.RetentionIntervalSeconds > 86400*30) {
		return errors.New("retention interval must be 0 (disabled) or 60–2592000 seconds")
	}
	for _, s := range []string{c.URL, c.ManagerURL} {
		if s != "" {
			u, e := url.Parse(s)
			if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				return fmt.Errorf("invalid HTTPS endpoint %q", s)
			}
		}
	}
	for _, p := range append(append([]string{}, c.AllowedRoots...), c.RestoreRoots...) {
		if !filepath.IsAbs(p) {
			return errors.New("source and restore roots must be absolute")
		}
	}
	if c.Role == "manager" && (c.DatabaseURL == "" || c.PasswordFile == "" || c.CapabilityKeyFile == "") {
		return errors.New("manager requires database_url, password_file and capability_key_file")
	}
	if c.Role == "manager" {
		u, e := url.Parse(c.DatabaseURL)
		if e != nil || u.Scheme != "postgres" && u.Scheme != "postgresql" {
			return errors.New("database_url must be a PostgreSQL URL")
		}
		if u.Query().Get("sslmode") != "verify-full" {
			if !c.Development || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") {
				return errors.New("PostgreSQL requires sslmode=verify-full outside explicit loopback development mode")
			}
		}
	}
	if c.Role == "media" && (c.Repository == "" || c.CapabilityKeyFile == "") {
		return errors.New("media requires repository and capability key")
	}
	return nil
}
