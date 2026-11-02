// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/tape"
	"gdata/srk/internal/testcloud"
	"gdata/srk/internal/virtualization"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type vmEnvironment struct {
	one     *testcloud.OpenNebulaCloud
	pve     *testcloud.ProxmoxCloud
	ovirt   *testcloud.OVirtCloud
	cloud   *testcloud.Cloud
	manager *Manager
	proxy   *Agent
	media   *Media
	admin   *http.Client
	url     string
	profile virtualization.Profile
}

func newVMEnvironment(t *testing.T) *vmEnvironment { return newVMEnvironmentKind(t, "openstack") }
func newVMEnvironmentKind(t *testing.T, kind string) *vmEnvironment {
	t.Helper()
	cloud := testcloud.New()
	pve := testcloud.NewProxmox()
	one := testcloud.NewOpenNebula()
	ovirt := testcloud.NewOVirt()
	var handler http.Handler = cloud.Handler()
	if kind == "ovirt" {
		handler = ovirt.Handler()
	}
	if kind == "proxmox" {
		handler = pve.Handler()
	}
	if kind == "opennebula" {
		handler = one.Handler()
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	ca := filepath.Join(dir, "cloud-ca.pem")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
	creds := filepath.Join(dir, "credentials.json")
	_ = os.WriteFile(creds, []byte(`{"token":"`+testcloud.Token+`"}`), 0600)
	profile := virtualization.Profile{Kind: "openstack", Endpoint: server.URL + "/nova/v2.1/" + testcloud.Project, GlanceEndpoint: server.URL + "/glance/v2", CinderEndpoint: server.URL + "/cinder/v3/" + testcloud.Project, NeutronEndpoint: server.URL + "/neutron/v2.0", ProjectID: testcloud.Project, BackupEnabled: true, StateDir: filepath.Join(dir, "journal"), CAFile: ca, CredentialFile: creds, TimeoutSeconds: 180}
	if kind == "ovirt" {
		staging := filepath.Join(dir, "ovirt-staging")
		state := filepath.Join(dir, "ovirt-journal")
		os.Mkdir(staging, 0700)
		os.Mkdir(state, 0700)
		profile = virtualization.Profile{Kind: "ovirt", Endpoint: server.URL + "/ovirt-engine/api", CAFile: ca, CredentialFile: creds, BackupEnabled: true, StateDir: state, StagingDir: staging, StagingMaxBytes: 64 << 20, TimeoutSeconds: 180, TransferOrigins: []string{server.URL}}
	}
	if kind == "proxmox" {
		os.Mkdir(filepath.Join(dir, "pve-state"), 0700)
		os.Mkdir(filepath.Join(dir, "pve-staging"), 0700)
		os.WriteFile(creds, []byte(`{"token_id":"test@pve!srk","secret":"fixture-pve-secret"}`), 0600)
		profile = virtualization.Profile{Kind: kind, Endpoint: server.URL + "/api2/json", CAFile: ca, CredentialFile: creds, BackupEnabled: true, StateDir: filepath.Join(dir, "pve-state"), StagingDir: filepath.Join(dir, "pve-staging"), StagingMaxBytes: 64 << 20, TimeoutSeconds: 180, DAVMountPath: "/mnt/srk", DAVUser: "pve-test", BackupStorage: "srk-dav"}
	}
	cfg := configs(t)
	if kind == "opennebula" {
		os.Mkdir(filepath.Join(dir, "one-state"), 0700)
		os.Mkdir(filepath.Join(dir, "one-staging"), 0700)
		os.WriteFile(creds, []byte(`{"session":"oneadmin:fixture-one-secret"}`), 0600)
		profile = virtualization.Profile{Kind: kind, Endpoint: server.URL + "/RPC2", CAFile: ca, CredentialFile: creds, BackupEnabled: true, StateDir: filepath.Join(dir, "one-state"), StagingDir: filepath.Join(dir, "one-staging"), StagingMaxBytes: 64 << 20, TimeoutSeconds: 180, BackupDatastore: "100", OneBEXEndpoints: map[string]string{"hyp1": server.URL + "/bex"}, RestoreExportURL: "https://127.0.0.1:0", RestoreExportListen: "127.0.0.1:0", RestoreCertFile: cfg["proxy"].Cert, RestoreKeyFile: cfg["proxy"].Key}
		caBytes, _ := os.ReadFile(cfg["proxy"].CA)
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(caBytes)
		one.ExportClient = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}}}
		t.Cleanup(one.ExportClient.CloseIdleConnections)
	}
	if kind == "proxmox" {
		root := filepath.Join(dir, "dav")
		os.Mkdir(root, 0700)
		credentials := filepath.Join(dir, "dav-users.json")
		os.WriteFile(credentials, []byte(`{"pve-test":"fixture-webdav-password"}`), 0600)
		mc := cfg["media"]
		mc.WebDAV = &config.WebDAVConfig{Root: root, CredentialsFile: credentials, URL: "https://127.0.0.1:9447", Listen: "127.0.0.1:9447"}
		cfg["media"] = mc
	}
	if kind == "tape" {
		mc := cfg["media"]
		mc.Tape = &tape.Config{Backend: "emulator", LibraryID: "LAB", Drives: []string{"DRIVE1", "DRIVE2"}, Cartridges: []tape.MediumSpec{{Barcode: "SRK001L9", Slot: 1, Generation: 9, CapacityBytes: 64 << 20}, {Barcode: "SRK002L9", Slot: 2, Generation: 9, CapacityBytes: 64 << 20}}}
		cfg["media"] = mc
	}
	manager := managerTest(t, cfg["manager"])
	ms := tlsServer(t, cfg["manager"], manager.Handler())
	manager.Config.URL = ms.URL
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { media.Captures.Close(); media.Repo.Close() })
	if kind == "proxmox" {
		dav := httptest.NewUnstartedServer(media.WebDAVHandler())
		cert, err := tls.LoadX509KeyPair(cfg["media"].Cert, cfg["media"].Key)
		if err != nil {
			t.Fatal(err)
		}
		dav.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
		dav.StartTLS()
		t.Cleanup(dav.Close)
		caBytes, _ := os.ReadFile(cfg["media"].CA)
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(caBytes)
		pve.DAVURL = dav.URL
		pve.DAVClient = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}}}
		t.Cleanup(pve.DAVClient.CloseIdleConnections)
		t.Cleanup(func() { media.DAV.root.Close() })
	}
	mediaHandler := media.Handler()
	if os.Getenv("SRK_VM_UI_REPLICATION_SLOW") == "1" {
		original := mediaHandler
		mediaHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/v1/chunks/") {
				timer := time.NewTimer(8 * time.Second)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-r.Context().Done():
					return
				}
			}
			original.ServeHTTP(w, r)
		})
	}
	medias := tlsServer(t, cfg["media"], mediaHandler)
	pc := cfg["proxy"]
	pc.ManagerURL = ms.URL
	pc.Virtualization = map[string]virtualization.Profile{"lab": profile}
	proxy, e := NewAgent(pc)
	if e != nil {
		t.Fatal(e)
	}
	ps := tlsServer(t, pc, proxy.Handler())
	for _, n := range []model.Node{{ID: media.Config.ID, Role: "media", URL: medias.URL, LastSeen: time.Now()}, {ID: proxy.Config.ID, Role: "proxy", URL: ps.URL, Status: proxy.status(), LastSeen: time.Now()}} {
		if e = manager.DB.PutNode(context.Background(), n); e != nil {
			t.Fatal(e)
		}
	}
	admin, e := identity.Client(cfg["admin"], identity.Peer{Role: "manager"})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(admin.CloseIdleConnections)
	startScheduler(t, manager)
	// Keep test control-plane presence current during multi-GiB streaming checks.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				for _, n := range []model.Node{{ID: media.Config.ID, Role: "media", URL: medias.URL, LastSeen: time.Now()}, {ID: proxy.Config.ID, Role: "proxy", URL: ps.URL, Status: proxy.status(), LastSeen: time.Now()}} {
					_ = manager.DB.PutNode(ctx, n)
				}
			}
		}
	}()
	return &vmEnvironment{one: one, pve: pve, ovirt: ovirt, cloud: cloud, manager: manager, proxy: proxy, media: media, admin: admin, url: ms.URL, profile: profile}
}
func (e *vmEnvironment) backup(t *testing.T, vm string) model.Run {
	t.Helper()
	var job model.Job
	j := model.Job{Type: e.profile.Kind, Name: "API fixture backup", AgentID: e.proxy.Config.ID, MediaID: e.media.Config.ID, Source: e.profile.Kind + "://lab/" + vm, Mode: "full", RetentionDays: 30, VM: &model.VMOptions{Consistency: "crash-consistent"}}
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/jobs", j, &job); err != nil {
		t.Fatal(err)
	}
	var run model.Run
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/jobs/"+job.ID+"/run", nil, &run); err != nil {
		t.Fatal(err)
	}
	return e.wait(t, run.ID)
}
func (e *vmEnvironment) wait(t *testing.T, id string) model.Run {
	t.Helper()
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		r, err := e.manager.DB.Run(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != "queued" && r.Status != "running" {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("VM operation did not finish")
	return model.Run{}
}
func TestOpenStackImageBackupDedupAndNewVMRestore(t *testing.T) {
	e := newVMEnvironment(t)
	first := e.backup(t, testcloud.ImageVM)
	if first.Status != "success" || first.Result == nil || !first.Result.Verified || first.Result.VM == nil {
		t.Fatalf("backup: %s %s", first.Status, first.Error)
	}
	images, volumes, snaps, servers := e.cloud.Summary()
	if images+volumes+snaps+servers != 0 {
		t.Fatal("temporary backup resources leaked", images, volumes, snaps, servers)
	}
	second := e.backup(t, testcloud.ImageVM)
	if second.Status != "success" || second.Result.UploadedBytes != 0 {
		t.Fatal("VM dedup failed", second.Status, second.Error)
	}
	var restore model.Run
	request := map[string]any{"vm_restore": model.VMRestoreRequest{Name: "new restored VM", Profile: "lab", FlavorID: "small", Networks: []string{testcloud.Network}}}
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+first.ID+"/restore", request, &restore); err != nil {
		t.Fatal(err)
	}
	done := e.wait(t, restore.ID)
	if done.Status != "success" || done.Result.VMRestore == nil || !done.Result.VMRestore.ControlPlaneVerified || done.Result.VMRestore.ServerID == testcloud.ImageVM {
		t.Fatal("restore", done.Status, done.Error)
	}
	manifest, err := e.media.Repo.Manifest(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	hashes := e.cloud.Hashes()
	for _, digest := range hashes {
		if digest != manifest.VM.Disks[0].SHA256 {
			t.Fatal("restored Glance image does not match independent SRK copy")
		}
	}
	events, err := e.manager.DB.RunEvents(context.Background(), first.ID)
	if err != nil || len(events) < 3 {
		t.Fatal("missing VM journal events", err)
	}
	pending, err := virtualization.Pending(e.profile)
	if err != nil || len(pending) != 0 {
		t.Fatal("cloud journal did not close", pending, err)
	}
}
func TestOpenStackVolumeBackedAllDisksIndependentRestore(t *testing.T) {
	e := newVMEnvironment(t)
	backup := e.backup(t, testcloud.VolumeVM)
	if backup.Status != "success" || backup.Result.VM == nil || len(backup.Result.VM.Disks) != 2 {
		t.Fatal("volume-backed backup", backup.Status, backup.Error)
	}
	images, volumes, snaps, servers := e.cloud.Summary()
	if images+volumes+snaps+servers != 0 {
		t.Fatal("backup depends on cloud snapshots", images, volumes, snaps, servers)
	}
	var restore model.Run
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+backup.ID+"/restore", map[string]any{"vm_restore": model.VMRestoreRequest{Name: "restored volume VM", FlavorID: "small", Networks: []string{testcloud.Network}}}, &restore); err != nil {
		t.Fatal(err)
	}
	done := e.wait(t, restore.ID)
	if done.Status != "success" || done.Result.VMRestore == nil || len(done.Result.VMRestore.Volumes) != 2 {
		t.Fatal("volume restore", done.Status, done.Error)
	}
	images, volumes, snaps, servers = e.cloud.Summary()
	if images != 2 || volumes != 2 || snaps != 0 || servers != 1 {
		t.Fatal("restored resources or rollback incorrect", images, volumes, snaps, servers)
	}
}
func TestOpenStackRejectsPartialDiskCoverageAndRollsBackFailedRestore(t *testing.T) {
	e := newVMEnvironment(t)
	e.cloud.Ephemeral = true
	failed := e.backup(t, testcloud.ImageVM)
	if failed.Status != "error" || !strings.Contains(failed.Error, "ephemeral") {
		t.Fatal("ephemeral disk silently skipped", failed.Status, failed.Error)
	}
	e.cloud.Ephemeral = false
	good := e.backup(t, testcloud.ImageVM)
	if good.Status != "success" {
		t.Fatal(good.Error)
	}
	e.cloud.FailCreateVM = true
	var restore model.Run
	if err := Call(context.Background(), e.admin, "POST", e.url+"/v1/runs/"+good.ID+"/restore", map[string]any{"vm_restore": model.VMRestoreRequest{Name: "failed restore", FlavorID: "small", Networks: []string{testcloud.Network}}}, &restore); err != nil {
		t.Fatal(err)
	}
	done := e.wait(t, restore.ID)
	if done.Status != "error" {
		t.Fatal("failed VM creation reported success", done)
	}
	images, volumes, snaps, servers := e.cloud.Summary()
	if images+volumes+snaps+servers != 0 {
		t.Fatal("failed restore left resources", images, volumes, snaps, servers)
	}
}
