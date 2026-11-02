// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalog

import (
	"gdata/srk/internal/model"
	"testing"
)

func TestVMRestoreIdempotencyJSONRoundTrip(t *testing.T) {
	a := &model.VMRestoreRequest{Name: "restore", FlavorID: "small", Networks: []string{"network"}, SecurityGroups: []string{}}
	b := &model.VMRestoreRequest{Name: "restore", FlavorID: "small", Networks: []string{"network"}}
	if !equalVMRestore(a, b) {
		t.Fatal("empty optional list differs from omitted list")
	}
	b.Name = "different"
	if equalVMRestore(a, b) {
		t.Fatal("different VM accepted for same key")
	}
}
