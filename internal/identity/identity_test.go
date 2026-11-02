// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package identity

import (
	"strings"
	"testing"
	"time"
)

func TestCapabilityRejectsTamperingExpiryAndOtherKey(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	g := Grant{Subject: "agent", RunID: "run", Permission: "write", Expires: time.Now().Add(time.Hour).Unix()}
	token := Sign(g, key)
	v, e := Verify(token, key)
	if e != nil || v.Subject != "agent" {
		t.Fatalf("valid capability: %v %v", v, e)
	}
	if _, e = Verify(token, []byte(strings.Repeat("x", 32))); e == nil {
		t.Fatal("wrong key accepted")
	}
	if _, e = Verify(token+"x", key); e == nil {
		t.Fatal("tampering accepted")
	}
	g.Expires = time.Now().Add(-time.Second).Unix()
	if _, e = Verify(Sign(g, key), key); e == nil {
		t.Fatal("expired grant accepted")
	}
}
func TestCAAndKeyCannotBeOverwritten(t *testing.T) {
	dir := t.TempDir()
	if e := InitCA(dir, "test"); e != nil {
		t.Fatal(e)
	}
	if InitCA(dir, "test") == nil {
		t.Fatal("CA overwrite permitted")
	}
	if e := Issue(dir, "test", "agent", "agent-1"); e != nil {
		t.Fatal(e)
	}
	if Issue(dir, "test", "agent", "agent-1") == nil {
		t.Fatal("key overwrite permitted")
	}
}
