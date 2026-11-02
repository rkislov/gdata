// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"encoding/json"
	"gdata/srk/internal/model"
	"gdata/srk/internal/testcloud"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in browser lab. Only the isolated test catalog and deterministic cloud
// fixture are exposed on loopback; production handlers keep their mTLS policy.
func TestVMUILab(t *testing.T) {
	dir := os.Getenv("SRK_VM_UI_LAB")
	if dir == "" {
		t.Skip("opt-in isolated browser lab")
	}
	kind := os.Getenv("SRK_VM_UI_PROVIDER")
	if kind == "" {
		kind = "openstack"
	}
	e := newVMEnvironmentKind(t, kind)
	if os.Getenv("SRK_VM_UI_TAPE") == "1" {
		backup := e.backup(t, testcloud.ImageVM)
		if backup.Status != "success" {
			t.Fatal(backup.Error)
		}
	}
	if os.Getenv("SRK_VM_UI_REPLICATION") == "1" {
		addReplicaMedia(t, e)
		backup := e.backup(t, testcloud.ImageVM)
		if backup.Status != "success" {
			t.Fatal(backup.Error)
		}
	}
	var walBase model.Run
	if os.Getenv("SRK_VM_UI_WAL_REPLICATION") == "1" {
		addReplicaMedia(t, e)
		walBase = seedWALReplicationBase(t, e)
		putWALReplicationSegment(t, e.media, walBase.Result.PostgreSQL, 2)
		putWALReplicationSegment(t, e.media, walBase.Result.PostgreSQL, 4)
	}
	repaired := false
	target, _ := url.Parse(e.url)
	gateway := httputil.NewSingleHostReverseProxy(target)
	gateway.Transport = e.admin.Transport
	gateway.Director = func(r *http.Request) {
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host
		r.Host = target.Host
		r.Header.Del("Origin")
		r.Header.Del("Cookie")
	}
	server := httptest.NewServer(gateway)
	defer server.Close()
	info, _ := json.Marshal(map[string]string{"url": server.URL, "kind": "isolated API fixture; no real hypervisor"})
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ready.json"), info, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("isolated UI lab ready", server.URL)
	deadline := time.Now().Add(20 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
			return
		}
		if walBase.ID != "" && !repaired {
			if _, err := os.Stat(filepath.Join(dir, "repair-gap")); err == nil {
				putWALReplicationSegment(t, e.media, walBase.Result.PostgreSQL, 3)
				repaired = true
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatal("UI lab timeout")
}
