// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	paths := []string{}
	for _, name := range []string{"ca.crt", "node.crt", "node.key"} {
		p := filepath.Join(dir, name)
		if e := os.WriteFile(p, []byte("fixture"), 0600); e != nil {
			t.Fatal(e)
		}
		paths = append(paths, p)
	}
	return Config{Role: "admin", ID: "admin", Installation: "test", URL: "https://localhost:8443", CA: paths[0], Cert: paths[1], Key: paths[2], MaxParallel: 2, PerAgent: 1, PerMedia: 2}
}
func TestConfigurationRejectsPlaintextAndReadablePrivateKey(t *testing.T) {
	c := fixture(t)
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	c.URL = "http://localhost:8443"
	if c.Validate() == nil {
		t.Fatal("plaintext endpoint accepted")
	}
	c.URL = "https://localhost:8443"
	if e := os.Chmod(c.Key, 0644); e != nil {
		t.Fatal(e)
	}
	if c.Validate() == nil {
		t.Fatal("world-readable private key accepted")
	}
}
func TestConfigurationRejectsUnknownFields(t *testing.T) {
	c := fixture(t)
	b, _ := json.Marshal(c)
	var fields map[string]any
	_ = json.Unmarshal(b, &fields)
	fields["max_paralell"] = 4
	b, _ = json.Marshal(fields)
	p := filepath.Join(t.TempDir(), "bad.json")
	if e := os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := Load(p); e == nil {
		t.Fatal("misspelled setting accepted")
	}
}
func TestProductionDatabaseRequiresVerifiedTLS(t *testing.T) {
	c := fixture(t)
	c.Role = "manager"
	c.PasswordFile = c.Key
	c.CapabilityKeyFile = c.Key
	c.DatabaseURL = "postgres://user@127.0.0.1/srk?sslmode=disable"
	if c.Validate() == nil {
		t.Fatal("unverified production database accepted")
	}
	c.Development = true
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	c.DatabaseURL = "postgres://user@database.internal/srk?sslmode=disable"
	if c.Validate() == nil {
		t.Fatal("remote database bypass accepted")
	}
	c.Development = false
	c.DatabaseURL = "postgres://user@database.internal/srk?sslmode=verify-full"
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
}

func TestRescueCaptureRequiresExplicitMediaAndHTTPSManager(t *testing.T) {
	c := fixture(t)
	c.RescueCaptureEnabled = true
	c.ManagerURL = "https://manager.example:8443"
	if c.Validate() == nil {
		t.Fatal("rescue upload enabled on non-media role")
	}
	c.Role = "media"
	c.Repository = t.TempDir()
	c.CapabilityKeyFile = c.Key
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	c.ManagerURL = "http://manager.example:8443"
	if c.Validate() == nil {
		t.Fatal("plaintext capture control allowed")
	}
	c.ManagerURL = ""
	if c.Validate() == nil {
		t.Fatal("capture control without manager allowed")
	}
}
