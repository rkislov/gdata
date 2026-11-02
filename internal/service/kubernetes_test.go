// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"gdata/srk/internal/config"
	"gdata/srk/internal/kubernetes"
	"gdata/srk/internal/testcloud"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestKubernetesLonghornBackupDedupAndExportHTTPS(t *testing.T) {
	cloud := testcloud.NewKubernetes()
	srv := httptest.NewTLSServer(cloud.Handler())
	defer srv.Close()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	ca := filepath.Join(dir, "ca.pem")
	token := filepath.Join(dir, "token")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600)
	os.WriteFile(token, []byte("kubernetes-fixture-token"), 0600)
	p := kubernetes.Profile{Endpoint: srv.URL, CAFile: ca, TokenFile: token, StagingDir: dir, Namespaces: []string{"demo"}, LonghornSnapshotClass: "lh-backup", MaxBytes: 1 << 20, TimeoutSeconds: 10, AllowRestore: true, RestoreNamespacePrefix: "restore-", RestoreStorageClass: "longhorn"}
	restoreRoot := t.TempDir()
	a, execute := dataWorker(t, func(c *config.Config) {
		c.Kubernetes = map[string]kubernetes.Profile{"lab": p}
		c.RestoreRoots = []string{restoreRoot}
	})
	first := execute("backup", "longhorn://lab", "")
	res, e := a.Backup(context.Background(), first)
	if e != nil || !res.Verified || res.Data == nil || res.Data.Kubernetes.VolumeBackups != 1 {
		t.Fatal("Longhorn backup", res, e)
	}
	second := execute("backup", "longhorn://lab", "")
	again, e := a.Backup(context.Background(), second)
	if e != nil || again.UploadedBytes >= again.LogicalBytes {
		t.Fatal("object deduplication", again, e)
	}
	restore := execute("restore", "longhorn://lab", first.RunID)
	restore.Destination = filepath.Join(restoreRoot, "export")
	out, e := a.Restore(context.Background(), restore)
	if e != nil || !out.Verified {
		t.Fatal("verified export", out, e)
	}
	b, e := os.ReadFile(filepath.Join(restore.Destination, "kubernetes-index.json"))
	if e != nil {
		t.Fatal(e)
	}
	var idx kubernetes.Index
	if json.Unmarshal(b, &idx) != nil || len(idx.Volumes) != 1 || idx.PVCData != "longhorn-external-backupstore" {
		t.Fatal("missing volume dependencies")
	}
	clusterRestore := execute("restore", "longhorn://lab", first.RunID)
	clusterRestore.Destination = "kubernetes://lab/demo/restore-demo"
	clusterResult, err := a.Restore(context.Background(), clusterRestore)
	if err != nil || !clusterResult.Verified || clusterResult.KubernetesRestore == nil || clusterResult.KubernetesRestore.State != "prepared" || clusterResult.KubernetesRestore.Volumes != 1 {
		t.Fatal("media to Kubernetes restore", clusterResult, err)
	}
	if len(clusterResult.KubernetesRestore.Events) == 0 {
		t.Fatal("missing restore journal")
	}
	if _, e = a.Restore(context.Background(), restore); e == nil {
		t.Fatal("existing export overwritten")
	}
}
