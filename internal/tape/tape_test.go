// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package tape

import (
	"testing"
	"time"
)

func TestRecycleRejectsUnknownProtectedAndRetained(t *testing.T) {
	now := time.Now()
	blank := Cartridge{Barcode: "SRK001L9", Generation: 9, Known: true, Copies: []Copy{}}
	if e := CanRecycle(blank, now); e != nil {
		t.Fatal(e)
	}
	for _, change := range []func(*Cartridge){func(c *Cartridge) { c.Known = false }, func(c *Cartridge) { c.Copies = nil }, func(c *Cartridge) { c.WORM = true }, func(c *Cartridge) { c.Mounted = true }, func(c *Cartridge) { c.WriteProtected = true }, func(c *Cartridge) { c.Copies = []Copy{{ID: "copy", ExpiresAt: now.Add(time.Hour)}} }, func(c *Cartridge) { c.Copies = []Copy{{ID: "copy", ExpiresAt: now.Add(-time.Hour), Hold: true}} }} {
		c := blank
		change(&c)
		if CanRecycle(c, now) == nil {
			t.Fatal("unsafe recycle allowed", c)
		}
	}
	if CanLoad(blank, Drive{ID: "drive", Generation: 9, Online: true, Empty: true}) != nil {
		t.Fatal("supported generation rejected")
	}
	blank.Generation = 7
	if CanLoad(blank, Drive{Generation: 9, Online: true, Empty: true}) == nil {
		t.Fatal("unsupported generation accepted")
	}
}
