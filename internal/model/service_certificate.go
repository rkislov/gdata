// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

import "time"

type ServiceCertificate struct {
	Role                string    `json:"role"`
	ID                  string    `json:"id"`
	Serial              string    `json:"serial"`
	PreviousSerial      string    `json:"previous_serial"`
	PreviousFingerprint string    `json:"previous_fingerprint"`
	Fingerprint         string    `json:"fingerprint"`
	KeyFingerprint      string    `json:"key_fingerprint"`
	Certificate         string    `json:"certificate"`
	ExpiresAt           time.Time `json:"expires_at"`
}

type CertificateObservation struct {
	Serial     string    `json:"serial"`
	ExpiresAt  time.Time `json:"expires_at"`
	ObservedAt time.Time `json:"observed_at"`
}
