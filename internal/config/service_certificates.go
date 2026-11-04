// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"errors"
	"path/filepath"
)

// ServiceCertificateConfig enables a writable, private store separate from /etc.
type ServiceCertificateConfig struct {
	Directory        string `json:"directory"`
	AutoRenew        bool   `json:"auto_renew"`
	RenewBeforeHours int    `json:"renew_before_hours,omitempty"`
	LifetimeHours    int    `json:"lifetime_hours,omitempty"`
}

func (c Config) CertificateTiming() (before, lifetime int) {
	before, lifetime = 720, 2160
	if s := c.ServiceCertificates; s != nil {
		if s.RenewBeforeHours != 0 {
			before = s.RenewBeforeHours
		}
		if s.LifetimeHours != 0 {
			lifetime = s.LifetimeHours
		}
	}
	return
}

func (c Config) validateServiceCertificates() error {
	service := c.Role == "manager" || c.Role == "media" || c.Role == "agent" || c.Role == "proxy"
	if c.ServiceCAKeyFile != "" && c.Role != "manager" {
		return errors.New("service issuer is manager-only")
	}
	if s := c.ServiceCertificates; s != nil {
		before, life := c.CertificateTiming()
		if !service || !filepath.IsAbs(s.Directory) || before < 1 || life < 2 || life > 2160 || before >= life {
			return errors.New("service certificates require a service role, absolute private directory, lifetime 2–2160 hours and renewal threshold 1..lifetime-1")
		}
		if s.AutoRenew && c.Role != "manager" && c.ManagerURL == "" {
			return errors.New("automatic renewal requires manager_url")
		}
		if s.AutoRenew && c.Role == "manager" && (c.URL == "" || c.ServiceCAKeyFile == "") {
			return errors.New("manager automatic renewal requires url and service_ca_key_file")
		}
	}
	return nil
}
