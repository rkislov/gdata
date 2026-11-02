// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"gdata/srk/internal/rescue"
)

type captureEnvironment struct {
	manager       *Manager
	media         *Media
	admin         *http.Client
	url, mediaURL string
}

func newCaptureEnvironment(t *testing.T) *captureEnvironment {
	t.Helper()
	cfg := configs(t)
	m := managerTest(t, cfg["manager"])
	server := tlsServer(t, cfg["manager"], m.Handler())
	m.Config.URL = server.URL
	mc := cfg["media"]
	mc.RescueCaptureEnabled = true
	mc.ManagerURL = server.URL
	media, e := NewMedia(mc)
	if e != nil {
		t.Fatal(e)
	}
	ms := tlsServer(t, mc, media.Handler())
	t.Cleanup(func() { media.Captures.Close(); media.CaptureClient.CloseIdleConnections(); _ = media.Repo.Close() })
	if e = m.DB.PutNode(context.Background(), model.Node{ID: mc.ID, Role: "media", URL: ms.URL, LastSeen: time.Now(), Status: map[string]any{"rescue_capture_enabled": true}}); e != nil {
		t.Fatal(e)
	}
	admin, e := identity.Client(cfg["admin"], identity.Peer{Role: "manager"})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(admin.CloseIdleConnections)
	startScheduler(t, m)
	return &captureEnvironment{manager: m, media: media, admin: admin, url: server.URL, mediaURL: ms.URL}
}
func (env *captureEnvironment) code(t *testing.T, limit int64) (*rescue.Client, string) {
	t.Helper()
	var issued struct{ ID, Code string }
	e := Call(context.Background(), env.admin, "POST", env.url+"/v1/rescue/codes", map[string]any{"operation": "backup", "name": "Test physical disk", "media_id": env.media.Config.ID, "retention_days": 30, "max_bytes": limit}, &issued)
	if e != nil {
		t.Fatal(e)
	}
	code, e := rescue.Parse(issued.Code)
	if e != nil {
		t.Fatal(e)
	}
	client, e := rescue.NewClient(code)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(client.HTTP.CloseIdleConnections)
	if e = client.Pair(context.Background()); e != nil {
		t.Fatal(e)
	}
	return client, issued.ID
}

// First sector contains BIOS boot instructions and a protective partition entry;
// both GPT signatures and the backup header occupy the original sector positions.
func rawBootFixture() []byte {
	b := make([]byte, repository.ChunkSize*3+512)
	boot := []byte{0xfa, 0x31, 0xc0, 0x8e, 0xd8, 0xbe, 0x16, 0x7c, 0xba, 0xe9, 0x00, 0xac, 0x84, 0xc0, 0x74, 0x03, 0xee, 0xeb, 0xf8, 0xf4, 0xeb, 0xfd}
	copy(b, boot)
	copy(b[len(boot):], []byte("SRK_06_BOOT_OK\n\x00"))
	b[450] = 0xee
	binary.LittleEndian.PutUint32(b[454:458], 1)
	binary.LittleEndian.PutUint32(b[458:462], uint32(len(b)/512-1))
	b[510] = 0x55
	b[511] = 0xaa
	copy(b[512:], []byte("EFI PART"))
	copy(b[len(b)-512:], []byte("EFI PART"))
	return b
}

type gatedDisk struct {
	reader           *bytes.Reader
	once             sync.Once
	entered, release chan struct{}
}

func (g *gatedDisk) ReadAt(b []byte, off int64) (int, error) {
	if off >= repository.ChunkSize {
		g.once.Do(func() { close(g.entered); <-g.release })
	}
	return g.reader.ReadAt(b, off)
}
func TestRescueDirectDiskCaptureProgressDedupAndRestore(t *testing.T) {
	env := newCaptureEnvironment(t)
	data := rawBootFixture()
	client, _ := env.code(t, int64(len(data)))
	disk := model.DiskGeometry{Host: "physical-test", Device: "/dev/vda", Model: "QEMU test disk", Serial: "SRK-TEST", Bytes: int64(len(data)), SectorSize: 512}
	source := &gatedDisk{reader: bytes.NewReader(data), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	var run model.Run
	go func() { var e error; run, e = client.Capture(context.Background(), source, disk, nil); done <- e }()
	select {
	case <-source.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("direct disk transfer did not start")
	}
	id := client.Pairing.Scope.Capture.RunID
	eventually(t, func() bool {
		r, e := env.manager.DB.Run(context.Background(), id)
		return e == nil && r.Status == "running" && r.Result != nil && r.Result.ProgressBytes == repository.ChunkSize && r.Result.ProgressPercent != nil && *r.Result.ProgressPercent > 0 && *r.Result.ProgressPercent < 100 && !r.Result.Verified
	})
	events, e := env.manager.DB.RunEvents(context.Background(), id)
	if e != nil || len(events) < 2 {
		t.Fatal("no persistent activity log", events, e)
	}
	close(source.release)
	select {
	case e = <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("capture did not finish")
	}
	if run.Status != "success" || run.Result == nil || !run.Result.Verified || run.Result.Disk == nil {
		t.Fatal("manager did not confirm capture", run)
	}
	manifest, e := env.media.Repo.Manifest(id)
	if e != nil || manifest.Disk == nil || *manifest.Disk != disk {
		t.Fatal("disk geometry missing", manifest, e)
	}
	var raw bytes.Buffer
	for _, h := range manifest.Files[0].Chunks {
		b, e := env.media.Repo.Get(h)
		if e != nil {
			t.Fatal(e)
		}
		raw.Write(b)
	}
	if !bytes.Equal(raw.Bytes(), data) {
		t.Fatal("partition table, bootloader or disk sectors changed")
	}
	if backups, e := client.Backups(context.Background()); e != nil || len(backups) != 0 {
		t.Fatal("write code gained restore rights", e)
	}
	second, _ := env.code(t, int64(len(data)))
	again, e := second.Capture(context.Background(), bytes.NewReader(data), disk, nil)
	if e != nil {
		t.Fatal(e)
	}
	if again.Result.UploadedBytes != 0 || again.Result.LogicalBytes != int64(len(data)) {
		t.Fatal("second disk backup was not deduplicated", again.Result)
	}
	var issued struct{ Code string }
	if e = Call(context.Background(), env.admin, "POST", env.url+"/v1/rescue/codes", map[string]any{"run_ids": []string{id}}, &issued); e != nil {
		t.Fatal(e)
	}
	parsed, _ := rescue.Parse(issued.Code)
	restore, _ := rescue.NewClient(parsed)
	defer restore.HTTP.CloseIdleConnections()
	if e = restore.Pair(context.Background()); e != nil {
		t.Fatal(e)
	}
	man, e := restore.Manifest(context.Background(), id)
	if e != nil {
		t.Fatal(e)
	}
	target, e := os.Create(filepath.Join(t.TempDir(), "restored.raw"))
	if e != nil {
		t.Fatal(e)
	}
	defer target.Close()
	if e = rescue.RestoreImage(context.Background(), man, target, disk.Bytes, func(ctx context.Context, h string) ([]byte, error) { return restore.Chunk(ctx, id, h) }, nil); e != nil {
		t.Fatal(e)
	}
	actual := make([]byte, len(data))
	if _, e = target.ReadAt(actual, 0); e != nil || !bytes.Equal(actual, data) {
		t.Fatal("full raw restore differs", e)
	}
	if _, e = restore.Capture(context.Background(), bytes.NewReader(data), disk, nil); e == nil {
		t.Fatal("restore code gained capture rights")
	}
}
func TestRescueCaptureRevocationAndUnauthorizedMediaEndpoints(t *testing.T) {
	env := newCaptureEnvironment(t)
	data := rawBootFixture()
	client, scope := env.code(t, int64(len(data)))
	disk := model.DiskGeometry{Host: "physical-test", Device: "/dev/vda", Bytes: int64(len(data)), SectorSize: 512}
	if _, e := client.Capture(context.Background(), bytes.NewReader(data), model.DiskGeometry{Host: disk.Host, Device: disk.Device, Bytes: disk.Bytes + 512, SectorSize: 512}, nil); e == nil {
		t.Fatal("administrator's disk limit ignored")
	}
	source := &gatedDisk{reader: bytes.NewReader(data), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, e := client.Capture(context.Background(), source, disk, nil); done <- e }()
	select {
	case <-source.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("capture did not start")
	}
	id := client.Pairing.Scope.Capture.RunID
	access, e := client.CaptureStatus(context.Background(), id)
	if e != nil {
		t.Fatal(e)
	}
	anonymous, e := rescue.NewClient(rescue.Code{URL: access.MediaURL, CertificateSHA256: access.CertificateSHA256, Secret: client.Code.Secret})
	if e != nil {
		t.Fatal(e)
	}
	defer anonymous.HTTP.CloseIdleConnections()
	anonymous.Pairing.Token = access.Token
	if _, e = anonymous.Request(context.Background(), "GET", "/v1/status", nil); e == nil {
		t.Fatal("capture bearer accessed component diagnostics")
	}
	if _, e = anonymous.Request(context.Background(), "POST", "/v1/snapshots/"+id, bytes.NewReader([]byte("{}"))); e == nil {
		t.Fatal("capture bearer used generic snapshot writes")
	}
	if e = Call(context.Background(), env.admin, "DELETE", env.url+"/v1/rescue/codes/"+scope, nil, new(any)); e != nil {
		t.Fatal(e)
	}
	if _, e = anonymous.Request(context.Background(), "POST", "/v1/captures/"+id+"/blocks/1/"+repository.Hash(data[repository.ChunkSize:repository.ChunkSize*2]), nil); e == nil {
		t.Fatal("revoked capture capability still writes")
	}
	close(source.release)
	select {
	case e = <-done:
		if e == nil {
			t.Fatal("revoked capture reported success")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("revoked client stuck")
	}
	eventually(t, func() bool {
		r, e := env.manager.DB.Run(context.Background(), id)
		return e == nil && r.Status == "error"
	})
	if _, e = env.media.Repo.Manifest(id); !os.IsNotExist(e) {
		t.Fatal("partial capture became a snapshot", e)
	}
}
