// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var profileName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type PostgreSQLProfile struct {
	WALArchive     *WALArchiveConfig `json:"wal_archive,omitempty"`
	Host           string            `json:"host"`
	Port           int               `json:"port"`
	User           string            `json:"user"`
	Database       string            `json:"database"`
	SSLMode        string            `json:"sslmode"`
	SSLRootCert    string            `json:"sslrootcert,omitempty"`
	Passfile       string            `json:"passfile,omitempty"`
	BinDir         string            `json:"bin_dir"`
	StagingDir     string            `json:"staging_dir"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}
type WALArchiveConfig struct {
	DataDir  string `json:"data_dir"`
	MediaID  string `json:"media_id"`
	MediaURL string `json:"media_url"`
}

// PostgreSQLSource resolves an opaque configured alias, never a connection URI.
func PostgreSQLSource(source string) (string, error) {
	name, ok := strings.CutPrefix(source, "postgresql://")
	if !ok || !profileName.MatchString(name) {
		return "", errors.New("PostgreSQL source must be postgresql://PROFILE")
	}
	return name, nil
}
func (c Config) validatePostgreSQL() error {
	if len(c.PostgreSQL) > 0 && c.Role != "agent" {
		return errors.New("PostgreSQL profiles belong on agents only")
	}
	for name, p := range c.PostgreSQL {
		if p.WALArchive != nil {
			w := p.WALArchive
			if !filepath.IsAbs(w.DataDir) || w.MediaID == "" {
				return errors.New("WAL archive requires an absolute data_dir and media_id")
			}
			u, e := url.Parse(w.MediaURL)
			if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
				return errors.New("WAL archive requires a valid HTTPS media_url")
			}
			for _, tool := range []string{"pg_controldata", "postgres", "pg_ctl"} {
				if _, e := os.Stat(filepath.Join(p.BinDir, tool)); e != nil {
					return e
				}
			}
		}
		if !profileName.MatchString(name) {
			return errors.New("invalid PostgreSQL profile name")
		}
		if p.Host == "" || strings.ContainsAny(p.Host, "\x00\r\n,") || p.Port < 1 || p.Port > 65535 || p.User == "" || p.Database == "" || strings.ContainsAny(p.User+p.Database, "\x00\r\n") || p.TimeoutSeconds < 1 || p.TimeoutSeconds > 36000 {
			return fmt.Errorf("invalid PostgreSQL profile %s", name)
		}
		if p.SSLMode != "verify-full" && !(c.Development && (p.Host == "localhost" || p.Host == "127.0.0.1" || p.Host == "::1") && p.SSLMode == "disable") {
			return errors.New("PostgreSQL backup requires verify-full; disable is allowed only for explicit loopback development")
		}
		for _, file := range []string{p.BinDir, p.StagingDir, p.Passfile, p.SSLRootCert} {
			if file != "" && !filepath.IsAbs(file) {
				return errors.New("PostgreSQL paths must be absolute")
			}
		}
		if p.BinDir == "" || p.StagingDir == "" {
			return errors.New("PostgreSQL bin_dir and staging_dir are required")
		}
		if p.SSLMode == "verify-full" {
			info, e := os.Stat(p.SSLRootCert)
			if e != nil || !info.Mode().IsRegular() {
				return errors.New("PostgreSQL verify-full requires an explicit CA file in sslrootcert")
			}
		}
		info, e := os.Lstat(p.StagingDir)
		if e != nil {
			return e
		}
		if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return errors.New("PostgreSQL staging directory must be private (0700)")
		}
		if p.Passfile != "" {
			info, e = os.Lstat(p.Passfile)
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
				return errors.New("PostgreSQL passfile must have mode 0600 or 0400")
			}
		}
		for _, bin := range []string{"pg_basebackup", "pg_verifybackup", "pg_waldump"} {
			info, e = os.Stat(filepath.Join(p.BinDir, bin))
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
				return fmt.Errorf("%s is not executable", bin)
			}
		}
	}
	return nil
}
