// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization_test

import (
	"bytes"
	"encoding/binary"
	"gdata/srk/internal/testcloud"
	"gdata/srk/internal/virtualization"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVMAValidationAndRestoreHeader(t *testing.T) {
	raw := testcloud.VMA("name: source\nmemory: 1024\nscsi0: local:source,size=1M\n", map[string][]byte{"scsi0": bytes.Repeat([]byte{42}, 1<<20)})
	file := filepath.Join(t.TempDir(), "vm.vma")
	check := func(data []byte) error {
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := virtualization.ValidateVMA(file)
		return err
	}
	if err := check(raw); err != nil {
		t.Fatal(err)
	}
	old := int(binary.BigEndian.Uint32(raw[56:60]))
	header, err := virtualization.RestoreVMAHeader(raw[:old], strings.Repeat("a", 32), "proxy-test")
	if err != nil {
		t.Fatal(err)
	}
	rewritten := append(header, raw[old:]...)
	if err = check(rewritten); err != nil {
		t.Fatal(err)
	}
	cfg, err := testcloud.VMAConfig(rewritten)
	if err != nil || !strings.Contains(cfg, "srk_proxy=proxy-test") || strings.Contains(cfg, "name: source") {
		t.Fatal("ownership not replaced", err, cfg)
	}
	for name, data := range map[string][]byte{"truncated": raw[:len(raw)-1], "omitted-cluster": raw[:len(raw)-66048], "duplicated-cluster": append(append([]byte(nil), raw...), raw[old:old+66048]...), "bad-header": append([]byte(nil), raw...)} {
		if name == "bad-header" {
			data[100] ^= 1
		}
		if err := check(data); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
}
