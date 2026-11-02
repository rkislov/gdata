// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPostgreSQLProfileValidationAndOpaqueSources(t *testing.T) {
	for _, source := range []string{"postgresql://prod", "postgresql://cluster_1"} {
		if _, e := PostgreSQLSource(source); e != nil {
			t.Fatal(e)
		}
	}
	for _, source := range []string{"postgres://prod", "postgresql://u:p@server/db", "postgresql://prod?password=secret", "postgresql://../prod", "postgresql://prod/", "postgresql://"} {
		if _, e := PostgreSQLSource(source); e == nil {
			t.Fatalf("connection parameters accepted as alias: %s", source)
		}
	}
	c := fixture(t)
	c.Role = "agent"
	c.Development = true
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"pg_basebackup", "pg_verifybackup", "pg_waldump"} {
		if e := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0700); e != nil {
			t.Fatal(e)
		}
	}
	p := PostgreSQLProfile{Host: "127.0.0.1", Port: 5432, User: "backup", Database: "postgres", SSLMode: "disable", BinDir: dir, StagingDir: dir, TimeoutSeconds: 60}
	c.PostgreSQL = map[string]PostgreSQLProfile{"prod": p}
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*PostgreSQLProfile){func(p *PostgreSQLProfile) { p.Host = "remote.example" }, func(p *PostgreSQLProfile) { p.TimeoutSeconds = 0 }, func(p *PostgreSQLProfile) { p.StagingDir = "relative" }, func(p *PostgreSQLProfile) { p.Port = 0 }, func(p *PostgreSQLProfile) { p.SSLMode = "prefer" }} {
		bad := p
		mutate(&bad)
		c.PostgreSQL["prod"] = bad
		if c.Validate() == nil {
			t.Fatalf("unsafe profile accepted: %+v", bad)
		}
	}
	c.PostgreSQL["prod"] = p
	c.Development = false
	if c.Validate() == nil {
		t.Fatal("production accepted unencrypted backup connection")
	}
	c.Development = true
	c.Role = "manager"
	if c.validatePostgreSQL() == nil {
		t.Fatal("manager accepted source connection profile")
	}
	c.Role = "agent"
	if e := os.Chmod(dir, 0755); e != nil {
		t.Fatal(e)
	}
	if c.validatePostgreSQL() == nil {
		t.Fatal("public staging directory accepted")
	}
}
