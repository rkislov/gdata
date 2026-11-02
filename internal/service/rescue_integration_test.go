// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"gdata/srk/internal/rescue"
)

func TestRescuePairingScopedRestoreAndRevocation(t *testing.T) {
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	server := tlsServer(t, cfg["manager"], m.Handler())
	m.Config.URL = server.URL
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	defer media.Repo.Close()
	mediaServer := tlsServer(t, cfg["media"], media.Handler())
	ctx := context.Background()
	if e = m.DB.PutNode(ctx, model.Node{ID: cfg["media"].ID, Role: "media", URL: mediaServer.URL, LastSeen: time.Now()}); e != nil {
		t.Fatal(e)
	}
	data := bytes.Repeat([]byte{83}, 1024)
	h := repository.Hash(data)
	if e = media.Repo.Put(h, data); e != nil {
		t.Fatal(e)
	}
	id := model.ID()
	man := model.Manifest{Version: 1, ID: id, JobID: model.ID(), CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Mode: "full", Consistency: "offline-disk-image", Files: []model.File{{Path: "disk.img", Kind: "file", Size: 1024, Chunks: []string{h}}}}
	if e = media.Repo.Commit(man); e != nil {
		t.Fatal(e)
	}
	run := model.Run{ID: id, JobID: man.JobID, Job: model.Job{ID: man.JobID, Name: "rescue image", MediaID: cfg["media"].ID}, Kind: "backup", Status: "success", CreatedAt: time.Now(), Result: &model.Result{Consistency: man.Consistency}}
	if _, e = m.DB.Enqueue(ctx, run, model.ID()); e != nil {
		t.Fatal(e)
	}
	admin, e := identity.Client(cfg["admin"], identity.Peer{Role: "manager"})
	if e != nil {
		t.Fatal(e)
	}
	defer admin.CloseIdleConnections()
	var issued struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	if e = Call(ctx, admin, "POST", server.URL+"/v1/rescue/codes", map[string]any{"run_ids": []string{id}}, &issued); e != nil {
		t.Fatal(e)
	}
	code, e := rescue.Parse(issued.Code)
	if e != nil {
		t.Fatal(e)
	}
	c, e := rescue.NewClient(code)
	if e != nil {
		t.Fatal(e)
	}
	defer c.HTTP.CloseIdleConnections()
	if e = c.Pair(ctx); e != nil {
		t.Fatal(e)
	}
	replay, _ := rescue.NewClient(code)
	defer replay.HTTP.CloseIdleConnections()
	if e = replay.Pair(ctx); e == nil {
		t.Fatal("one-time code reused")
	}
	backups, e := c.Backups(ctx)
	if e != nil || len(backups) != 1 || backups[0].ID != id {
		t.Fatal("scope inventory", backups, e)
	}
	if _, e = c.Manifest(ctx, model.ID()); e == nil {
		t.Fatal("unrelated backup exposed")
	}
	got, e := c.Manifest(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	fd, e := os.Create(filepath.Join(t.TempDir(), "target.img"))
	if e != nil {
		t.Fatal(e)
	}
	defer fd.Close()
	if e = rescue.RestoreImage(ctx, got, fd, 1024, func(ctx context.Context, h string) ([]byte, error) { return c.Chunk(ctx, id, h) }, nil); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Chunk(ctx, id, repository.Hash([]byte("outside"))); e == nil {
		t.Fatal("unrelated chunk exposed")
	}
	if e = Call(ctx, admin, "DELETE", server.URL+"/v1/rescue/codes/"+issued.ID, nil, new(any)); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Backups(ctx); e == nil {
		t.Fatal("revoked session still valid")
	}
}
func TestRescueCodeAtomicConsume(t *testing.T) {
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	ctx := context.Background()
	s := rescue.Scope{ID: model.ID(), RunIDs: []string{model.ID()}, ExpiresAt: time.Now().Add(time.Hour)}
	code := model.ID() + model.ID()
	if e := m.DB.RescueIssue(ctx, rescue.Digest(code), s); e != nil {
		t.Fatal(e)
	}
	var count atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := m.DB.RescueConsume(ctx, rescue.Digest(code), rescue.Digest(model.ID())); e == nil {
				count.Add(1)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("successful consumes=%d", count.Load())
	}
}
func TestRescueCertificatePinAndReadOnlyManagerGrant(t *testing.T) {
	cfg := configs(t)
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	defer media.Repo.Close()
	server := tlsServer(t, cfg["media"], media.Handler())
	code := rescue.Code{URL: server.URL, CertificateSHA256: rescue.Digest("different certificate"), Secret: model.ID() + model.ID()}
	c, e := rescue.NewClient(code)
	if e != nil {
		t.Fatal(e)
	}
	defer c.HTTP.CloseIdleConnections()
	if e = c.Pair(context.Background()); e == nil {
		t.Fatal("wrong certificate pin accepted")
	}
	manager, e := identity.Client(cfg["manager"], identity.Peer{Role: "media"})
	if e != nil {
		t.Fatal(e)
	}
	defer manager.CloseIdleConnections()
	token := identity.Sign(identity.Grant{Subject: cfg["manager"].ID, RunID: model.ID(), Permission: "write", Expires: time.Now().Add(time.Hour).Unix()}, media.Key)
	req, _ := http.NewRequest("PUT", server.URL+"/v1/chunks/"+repository.Hash([]byte("x")), bytes.NewReader([]byte("x")))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, e := manager.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 403 {
		t.Fatalf("manager write capability accepted: %d %s", resp.StatusCode, b)
	}
	var v any
	_ = json.Unmarshal(b, &v)
}
