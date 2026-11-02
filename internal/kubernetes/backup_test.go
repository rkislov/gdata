// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package kubernetes_test

import (
	"context"
	"encoding/pem"
	"gdata/srk/internal/kubernetes"
	"gdata/srk/internal/model"
	"gdata/srk/internal/testcloud"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixture(t *testing.T) (*testcloud.KubernetesCloud, kubernetes.Profile) {
	t.Helper()
	cloud := testcloud.NewKubernetes()
	server := httptest.NewTLSServer(cloud.Handler())
	t.Cleanup(server.Close)
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	ca := filepath.Join(dir, "ca.pem")
	token := filepath.Join(dir, "token")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
	os.WriteFile(token, []byte("kubernetes-fixture-token"), 0600)
	return cloud, kubernetes.Profile{Endpoint: server.URL, CAFile: ca, TokenFile: token, StagingDir: dir, Namespaces: []string{"demo"}, LonghornSnapshotClass: "lh-backup", MaxBytes: 1 << 20, TimeoutSeconds: 10}
}
func TestBackupLonghornAndPagination(t *testing.T) {
	cloud, p := fixture(t)
	dir := t.TempDir()
	run := model.ID()
	idx, e := kubernetes.Backup(context.Background(), p, run, dir, nil)
	if e != nil {
		t.Fatal(e)
	}
	if idx.Objects != 4 || len(idx.Volumes) != 1 || idx.PVCData != "longhorn-external-backupstore" || cloud.SnapshotCreates != 1 || cloud.SecretReads != 0 {
		t.Fatalf("incomplete backup: %+v", idx)
	}
	if _, e = os.Stat(filepath.Join(dir, "kubernetes-index.json")); e != nil {
		t.Fatal(e)
	}
	// Retry uses the same owned CSI object instead of creating another backup.
	_, e = kubernetes.Backup(context.Background(), p, run, t.TempDir(), nil)
	if e != nil || cloud.SnapshotCreates != 1 {
		t.Fatal("idempotency", e)
	}
}
func TestBackupFailuresDoNotPublishIndex(t *testing.T) {
	for _, mode := range []string{"class", "namespace", "pagination", "snapshot", "quota", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			cloud, p := fixture(t)
			ctx := context.Background()
			switch mode {
			case "class":
				cloud.WrongClass = true
			case "namespace":
				cloud.WrongNamespace = true
			case "pagination":
				cloud.PaginationLoop = true
			case "snapshot":
				cloud.SnapshotError = true
			case "quota":
				p.MaxBytes = 10
			case "cancel":
				cloud.NeverReady = true
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			dir := t.TempDir()
			_, e := kubernetes.Backup(ctx, p, model.ID(), dir, nil)
			if e == nil {
				t.Fatal("unsafe backup accepted")
			}
			if _, e = os.Stat(filepath.Join(dir, "kubernetes-index.json")); !os.IsNotExist(e) {
				t.Fatal("published incomplete index")
			}
		})
	}
}
func TestMetadataOnlyExplicitAndTokenSecretsExcluded(t *testing.T) {
	cloud, p := fixture(t)
	p.LonghornSnapshotClass = ""
	p.IncludeSecrets = true
	idx, e := kubernetes.Backup(context.Background(), p, model.ID(), t.TempDir(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if idx.PVCData != "not-included" || len(idx.Volumes) != 0 || cloud.SnapshotCreates != 0 || cloud.SecretReads != 1 || idx.Objects != 4 {
		t.Fatal(idx)
	}
}
