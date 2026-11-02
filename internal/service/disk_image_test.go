// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/rescue"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOfflineDiskImageAgentBackup(t *testing.T) {
	cfg := configs(t)
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	defer media.Repo.Close()
	server := tlsServer(t, cfg["media"], media.Handler())
	source := t.TempDir()
	_ = os.WriteFile(filepath.Join(source, "disk.img"), bytes.Repeat([]byte{45}, 1024), 0600)
	c := cfg["agent"]
	c.AllowedRoots = []string{source}
	agent, e := NewAgent(c)
	if e != nil {
		t.Fatal(e)
	}
	execution := func() model.Execute {
		v := model.Execute{RunID: model.ID(), Kind: "backup", Job: model.Job{Type: "disk-image", ID: model.ID(), AgentID: c.ID, MediaID: cfg["media"].ID, Source: source, Mode: "full", RetentionDays: 30}, MediaURL: server.URL}
		v.Grant = identity.Sign(identity.Grant{Subject: c.ID, RunID: v.RunID, Permission: "write", Expires: time.Now().Add(time.Hour).Unix()}, media.Key)
		return v
	}
	v := execution()
	res, e := agent.Backup(context.Background(), v)
	if e != nil || !res.Verified || res.Consistency != "offline-disk-image" {
		t.Fatal("image backup failed", res, e)
	}
	m, e := media.Repo.Manifest(v.RunID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = rescue.DiskImage(m); e != nil {
		t.Fatal(e)
	}
	_ = os.WriteFile(filepath.Join(source, "extra"), []byte("unexpected"), 0600)
	if _, e = agent.Backup(context.Background(), execution()); e == nil {
		t.Fatal("extra source files accepted")
	}
	_ = os.Remove(filepath.Join(source, "extra"))
	_ = os.Remove(filepath.Join(source, "disk.img"))
	_ = os.Symlink("/etc/hosts", filepath.Join(source, "disk.img"))
	if _, e = agent.Backup(context.Background(), execution()); e == nil {
		t.Fatal("image symlink accepted")
	}
}
