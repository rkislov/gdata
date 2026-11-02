// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type S3Profile struct {
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	Prefix         string `json:"prefix,omitempty"`
	CredentialFile string `json:"credential_file"`
	CAFile         string `json:"ca_file,omitempty"`
	StagingDir     string `json:"staging_dir"`
	MaxBytes       int64  `json:"max_bytes"`
	AllowRestore   bool   `json:"allow_restore,omitempty"`
}
type MySQLProfile struct {
	MaxBytes       int64  `json:"max_bytes"`
	SSLMode        string `json:"sslmode"`
	CAFile         string `json:"ca_file,omitempty"`
	Engine         string `json:"engine"`
	DefaultsFile   string `json:"defaults_file"`
	BinDir         string `json:"bin_dir"`
	Database       string `json:"database"`
	StagingDir     string `json:"staging_dir"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	AllowRestore   bool   `json:"allow_restore,omitempty"`
}

func DataSource(source, kind string) (string, error) {
	name, ok := strings.CutPrefix(source, kind+"://")
	if !ok || !profileName.MatchString(name) {
		return "", errors.New("source requires an opaque configured profile")
	}
	return name, nil
}
func privateFile(file string) bool {
	s, e := os.Lstat(file)
	return e == nil && filepath.IsAbs(file) && s.Mode().IsRegular() && s.Mode().Perm()&0077 == 0
}
func privateDir(dir string) bool {
	s, e := os.Lstat(dir)
	return e == nil && filepath.IsAbs(dir) && s.IsDir() && s.Mode().Perm()&0077 == 0
}
func (c Config) validateData() error {
	if (len(c.S3) > 0 || len(c.MySQL) > 0) && c.Role != "agent" {
		return errors.New("S3 and MySQL profiles belong on agents")
	}
	for n, p := range c.S3 {
		u, e := url.Parse(p.Endpoint)
		if !profileName.MatchString(n) || e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || p.Bucket == "" || p.Region == "" || !privateFile(p.CredentialFile) || !privateDir(p.StagingDir) || p.MaxBytes < 1 || p.MaxBytes > 4<<40 {
			return errors.New("S3 profile requires HTTPS endpoint, region, bucket, private credentials/staging and max_bytes")
		}
	}
	for n, p := range c.MySQL {
		if p.SSLMode == "disable" {
			raw, e := os.ReadFile(p.DefaultsFile)
			if e != nil {
				return e
			}
			host := ""
			socket := ""
			for _, line := range strings.Split(string(raw), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "!include") {
					return errors.New("MySQL development defaults must not include other files")
				}
				key, value, ok := strings.Cut(line, "=")
				if ok {
					switch strings.TrimSpace(key) {
					case "host":
						host = strings.TrimSpace(value)
					case "socket":
						socket = strings.TrimSpace(value)
					}
				}
			}
			if host != "localhost" && host != "127.0.0.1" && host != "::1" && !(host == "" && filepath.IsAbs(socket)) {
				return errors.New("unencrypted MySQL is restricted to explicit local development endpoints")
			}
		}
		if p.SSLMode != "verify-full" && !(c.Development && p.SSLMode == "disable") {
			return errors.New("MySQL requires verify-full TLS; disable is allowed only in explicit development configuration")
		}
		if p.SSLMode == "verify-full" && !filepath.IsAbs(p.CAFile) {
			return errors.New("MySQL TLS requires an absolute CA file")
		}
		if p.MaxBytes < 1 || p.MaxBytes > 4<<40 {
			return errors.New("MySQL requires max_bytes within 1 byte–4 TiB")
		}
		if !profileName.MatchString(n) || (p.Engine != "mysql" && p.Engine != "mariadb") || !profileName.MatchString(p.Database) || !privateFile(p.DefaultsFile) || !privateDir(p.StagingDir) || !filepath.IsAbs(p.BinDir) || p.TimeoutSeconds < 1 || p.TimeoutSeconds > 43200 {
			return errors.New("invalid MySQL/MariaDB profile")
		}
		for _, tool := range []string{p.Engine, dumpTool(p.Engine)} {
			s, e := os.Stat(filepath.Join(p.BinDir, tool))
			if e != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0111 == 0 {
				return errors.New("MySQL/MariaDB client tools required")
			}
		}
	}
	return nil
}
func dumpTool(engine string) string {
	if engine == "mariadb" {
		return "mariadb-dump"
	}
	return "mysqldump"
}
