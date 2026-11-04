// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"os"
	"testing"
)

func TestServiceCertificateConfigurationBounds(t *testing.T) {
	c := fixture(t)
	c.Role = "agent"
	c.ManagerURL = "https://localhost:8443"
	c.ServiceCertificates = &ServiceCertificateConfig{Directory: t.TempDir(), AutoRenew: true}
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	c.ServiceCertificates.RenewBeforeHours = 2160
	if c.Validate() == nil {
		t.Fatal("renewal loop threshold accepted")
	}
	c.ServiceCertificates.RenewBeforeHours = 720
	c.ServiceCertificates.LifetimeHours = 2161
	if c.Validate() == nil {
		t.Fatal("unbounded lifetime")
	}
	c.ServiceCertificates.LifetimeHours = 2160
	c.ServiceCertificates.Directory = "relative"
	if c.Validate() == nil {
		t.Fatal("relative credential store")
	}
	c.ServiceCertificates.Directory = t.TempDir()
	c.Role = "admin"
	if c.Validate() == nil {
		t.Fatal("service rotation for human role")
	}
	c.Role = "agent"
	c.ServiceCAKeyFile = c.Key
	if c.Validate() == nil {
		t.Fatal("service CA on agent")
	}
	c.ServiceCAKeyFile = ""
	c.ManagerURL = ""
	if c.Validate() == nil {
		t.Fatal("missing issuer URL")
	}
	c.ManagerURL = "https://localhost:8443"
	if e := os.Chmod(c.Key, 0644); e != nil {
		t.Fatal(e)
	}
	if c.Validate() == nil {
		t.Fatal("readable private key")
	}
}
