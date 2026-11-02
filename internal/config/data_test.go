// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSQLDevelopmentTLSCannotUseRemoteDefaults(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	defaults := filepath.Join(dir, "client.cnf")
	os.WriteFile(defaults, []byte("[client]\nhost=remote.example\nuser=backup\n"), 0600)
	for _, name := range []string{"mysql", "mysqldump"} {
		os.WriteFile(filepath.Join(dir, name), []byte("native tool placeholder"), 0700)
	}
	p := MySQLProfile{Engine: "mysql", Database: "source", DefaultsFile: defaults, BinDir: dir, StagingDir: dir, SSLMode: "disable", TimeoutSeconds: 60, MaxBytes: 1 << 20}
	cfg := Config{Role: "agent", Development: true, MySQL: map[string]MySQLProfile{"source": p}}
	if cfg.validateData() == nil {
		t.Fatal("remote unencrypted SQL allowed")
	}
	os.WriteFile(defaults, []byte("[client]\nhost=127.0.0.1\nuser=backup\n"), 0600)
	if e := cfg.validateData(); e != nil {
		t.Fatal("local development rejected", e)
	}
	cfg.Development = false
	if cfg.validateData() == nil {
		t.Fatal("unencrypted production profile allowed")
	}
}
func TestDataAliasesDoNotAcceptConnectionURIs(t *testing.T) {
	for _, s := range []string{"mysql://user:password@db/name", "s3://profile/../bucket", "mysql://profile?token=secret"} {
		kind := "mysql"
		if s[:2] == "s3" {
			kind = "s3"
		}
		if _, e := DataSource(s, kind); e == nil {
			t.Fatal("URI credentials/path accepted", s)
		}
	}
}
