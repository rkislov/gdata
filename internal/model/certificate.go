// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

import "time"

type PersonalCertificate struct {
	Certificate    string    `json:"certificate"`
	Serial         string    `json:"serial"`
	Username       string    `json:"username"`
	KeyFingerprint string    `json:"key_fingerprint"`
	Fingerprint    string    `json:"fingerprint"`
	IssuedAt       time.Time `json:"issued_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	RetireAt       time.Time `json:"retire_at"`
	Revoked        bool      `json:"revoked"`
}
type CertificateState struct {
	Version       int                   `json:"version"`
	Managed       bool                  `json:"managed"`
	IssuerEnabled bool                  `json:"issuer_enabled"`
	Certificates  []PersonalCertificate `json:"certificates"`
}
