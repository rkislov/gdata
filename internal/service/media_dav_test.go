// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"gdata/srk/internal/config"
	"gdata/srk/internal/model"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDAVIsolationQuotaSealingAndReload(t *testing.T) {
	cfg := configs(t)["media"]
	root := filepath.Join(t.TempDir(), "dav")
	os.Mkdir(root, 0700)
	credentials := filepath.Join(t.TempDir(), "users.json")
	os.WriteFile(credentials, []byte(`{"one":"long-fixture-password","two":"other-fixture-password"}`), 0600)
	cfg.WebDAV = &config.WebDAVConfig{Root: root, CredentialsFile: credentials, Listen: "127.0.0.1:9447", URL: "https://127.0.0.1:9447"}
	media, err := NewMedia(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer media.Repo.Close()
	defer media.Captures.Close()
	defer media.DAV.root.Close()
	id := strings.Repeat("a", 32)
	session := &davSession{DAVSession: model.DAVSession{ID: id, Subject: "proxy-test", User: "one", Operation: "backup", ExpiresAt: time.Now().Add(time.Hour), MaxBytes: 4}}
	media.DAV.sessions[id] = session
	os.Mkdir(filepath.Join(root, "staging", id), 0700)
	fs := &davFS{s: media.DAV, user: "one"}
	name := "staging/" + id + "/part"
	file, err := fs.OpenFile(context.Background(), name, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("four")); err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("overflow")); err == nil {
		t.Fatal("quota bypass")
	}
	file.Close()
	foreign := &davFS{s: media.DAV, user: "two"}
	if _, err = foreign.Stat(context.Background(), name); err == nil {
		t.Fatal("foreign read")
	}
	if _, err = fs.Stat(context.Background(), "staging/"+id+"/../../users.json"); err == nil {
		t.Fatal("traversal")
	}
	session.Sealed = true
	if _, err = fs.OpenFile(context.Background(), name, os.O_WRONLY, 0600); err == nil {
		t.Fatal("sealed write")
	}
	if err = media.DAV.save(session); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newDAVStore(media)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.root.Close()
	if reloaded.sessions[id] == nil || !reloaded.sessions[id].Sealed || reloaded.sessions[id].used != 4 {
		t.Fatal("lost durable session")
	}
	req := httptest.NewRequest("GET", "https://localhost/", nil)
	rec := httptest.NewRecorder()
	media.WebDAVHandler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal("anonymous access", rec.Code)
	}
	req = httptest.NewRequest("GET", "http://localhost/", nil)
	req.SetBasicAuth("one", "long-fixture-password")
	rec = httptest.NewRecorder()
	media.WebDAVHandler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal("plaintext access", rec.Code)
	}
}
