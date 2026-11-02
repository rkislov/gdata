// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

import "testing"

func TestRestoreTargetRejectsAmbiguousPhysicalSites(t *testing.T) {
	for _, kinds := range [][2]string{{"files", "postgresql"}, {"kubernetes", "longhorn"}} {
		profile := ""
		if kinds[0] == "kubernetes" {
			profile = "lab"
		}
		p := ProtectionSettings{Sites: []ProtectionSite{{ID: "a", Name: "A", DisasterDomain: "a"}, {ID: "b", Name: "B", DisasterDomain: "b"}}, RestoreTargets: []RestoreTarget{{ID: "first", SiteID: "a", NodeID: "agent", Kind: kinds[0], Profile: profile}, {ID: "second", SiteID: "b", NodeID: "agent", Kind: kinds[1], Profile: profile}}}
		if e := p.Validate(); e == nil {
			t.Fatal("same endpoint accepted on two physical sites", kinds)
		}
		p.RestoreTargets[1].SiteID = "a"
		if e := p.Validate(); e != nil {
			t.Fatal(e)
		}
	}
}
