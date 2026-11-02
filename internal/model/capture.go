// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

import (
	"errors"
	"strings"
	"time"
)

const MaxCaptureBytes int64 = 4 << 40
const MaxManifestBytes int64 = 96 << 20

type DiskGeometry struct {
	Host       string `json:"host"`
	Device     string `json:"device"`
	Model      string `json:"model"`
	Serial     string `json:"serial"`
	Bytes      int64  `json:"bytes"`
	SectorSize int    `json:"sector_size"`
}

func (d DiskGeometry) Validate() error {
	if d.Host == "" || len(d.Host) > 128 || !strings.HasPrefix(d.Device, "/dev/") || len(d.Device) > 256 || len(d.Model) > 256 || len(d.Serial) > 256 || strings.ContainsAny(d.Host+d.Device+d.Model+d.Serial, "\x00\r\n") || d.Bytes < 512 || d.Bytes > MaxCaptureBytes || (d.SectorSize != 512 && d.SectorSize != 4096) || d.Bytes%int64(d.SectorSize) != 0 {
		return errors.New("invalid disk identity, size or sector geometry")
	}
	return nil
}

type DiskCapture struct {
	ScopeID   string       `json:"scope_id"`
	ExpiresAt time.Time    `json:"expires_at"`
	Disk      DiskGeometry `json:"disk"`
}
type CaptureTicket struct {
	ID        string      `json:"id"`
	Job       Job         `json:"job"`
	Capture   DiskCapture `json:"capture"`
	CreatedAt time.Time   `json:"created_at"`
}
type CaptureStatus struct {
	ID        string    `json:"id"`
	Phase     string    `json:"phase"`
	NextIndex int64     `json:"next_index"`
	Result    Result    `json:"result"`
	UpdatedAt time.Time `json:"updated_at"`
	Error     string    `json:"error,omitempty"`
}
