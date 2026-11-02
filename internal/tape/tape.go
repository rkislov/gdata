// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
// Package tape implements a persistent library emulator and fail-closed media rules.
package tape

import (
	"context"
	"errors"
	"time"
)

type Copy struct {
	SnapshotID string    `json:"snapshot_id,omitempty"`
	Bytes      int64     `json:"bytes,omitempty"`
	Verified   bool      `json:"verified"`
	ID         string    `json:"id"`
	ExpiresAt  time.Time `json:"expires_at"`
	Hold       bool      `json:"hold"`
}
type Cartridge struct {
	Barcode        string    `json:"barcode"`
	Generation     int       `json:"generation"`
	Known          bool      `json:"known"`
	WORM           bool      `json:"worm"`
	WriteProtected bool      `json:"write_protected"`
	Mounted        bool      `json:"mounted"`
	Cleaning       bool      `json:"cleaning"`
	ActiveUntil    time.Time `json:"active_until"`
	Copies         []Copy    `json:"copies"`
}
type Drive struct {
	ID         string `json:"id"`
	Generation int    `json:"generation"`
	Empty      bool   `json:"empty"`
	Online     bool   `json:"online"`
}
type Inventory struct {
	LibraryID  string      `json:"library_id"`
	ObservedAt time.Time   `json:"observed_at"`
	Cartridges []Cartridge `json:"cartridges"`
	Drives     []Drive     `json:"drives"`
}

// A transport implementation must serialize robotics and each drive independently.
// Inventory needs SMC READ ELEMENT STATUS, drive status needs SSC / sense decoding.
type Transport interface {
	Inventory(context.Context) (Inventory, error)
}

func CanRecycle(c Cartridge, at time.Time) error {
	if !c.Known || c.Barcode == "" {
		return errors.New("unknown catalog; inventory and import required")
	}
	if c.WORM || c.WriteProtected || c.Mounted || c.Cleaning || at.Before(c.ActiveUntil) {
		return errors.New("cartridge is protected, mounted or in use")
	}
	// nil means missing catalog, an explicit empty slice means a known blank tape.
	if c.Copies == nil {
		return errors.New("retention catalog unknown")
	}
	for _, copy := range c.Copies {
		if copy.ID == "" || copy.ExpiresAt.IsZero() || copy.Hold || at.Before(copy.ExpiresAt) {
			return errors.New("retained or unknown backup prevents recycling")
		}
	}
	return nil
}
func CanLoad(c Cartridge, d Drive) error {
	if !d.Online || !d.Empty || c.Mounted || c.Cleaning || c.Barcode == "" {
		return errors.New("drive or cartridge unavailable")
	}
	// Conservative LTO-9 scope: LTO-8 and LTO-9 data media. Vendor qualification remains required.
	if d.Generation != 9 || (c.Generation != 8 && c.Generation != 9) {
		return errors.New("generation combination outside supported planning scope")
	}
	return nil
}
