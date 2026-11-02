// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"github.com/minio/minio-go/v7"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dockerTest(t *testing.T, args ...string) string {
	t.Helper()
	b, e := exec.Command("docker", args...).CombinedOutput()
	if e != nil {
		t.Fatalf("docker %v: %s %v", args, b, e)
	}
	return strings.TrimSpace(string(b))
}
func dataWorker(t *testing.T, configure func(*config.Config)) (*Agent, func(string, string, string) model.Execute) {
	t.Helper()
	cfg := configs(t)
	media, e := NewMedia(cfg["media"])
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { media.Repo.Close(); media.Captures.Close() })
	server := tlsServer(t, cfg["media"], media.Handler())
	ac := cfg["agent"]
	configure(&ac)
	a, e := NewAgent(ac)
	if e != nil {
		t.Fatal(e)
	}
	factory := func(kind, source, snapshot string) model.Execute {
		v := model.Execute{RunID: model.ID(), Kind: kind, Job: model.Job{ID: model.ID(), Type: strings.Split(source, ":")[0], Name: "native data integration", AgentID: ac.ID, MediaID: cfg["media"].ID, Source: source, Mode: "full", RetentionDays: 30}, MediaURL: server.URL, SnapshotID: snapshot}
		permission := "write"
		if kind == "restore" {
			permission = "read"
		}
		v.Grant = identity.Sign(identity.Grant{Subject: ac.ID, RunID: v.RunID, SnapshotID: snapshot, Permission: permission, Expires: time.Now().Add(time.Hour).Unix()}, bytes.Repeat([]byte("k"), 32))
		return v
	}
	return a, factory
}
func TestNativeMinIOS3RoundTripMetadataDedupAndNoOverwrite(t *testing.T) {
	if os.Getenv("SRK_TEST_MINIO_IMAGE") == "" {
		t.Skip("set SRK_TEST_MINIO_IMAGE to a local MinIO image")
	}
	dir := t.TempDir()
	certDir := filepath.Join(dir, "certs")
	os.Mkdir(certDir, 0700)
	certServer := httptest.NewTLSServer(nil)
	cert := certServer.TLS.Certificates[0]
	certServer.Close()
	key, _ := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	os.WriteFile(filepath.Join(certDir, "public.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600)
	os.WriteFile(filepath.Join(certDir, "private.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600)
	name := "srk-minio-test-" + model.ID()[:12]
	dockerTest(t, "run", "-d", "--rm", "--name", name, "-e", "MINIO_ROOT_USER=srk-test", "-e", "MINIO_ROOT_PASSWORD=srk-fixture-password-only", "-v", certDir+":/root/.minio/certs:ro", "-p", "127.0.0.1::9000", os.Getenv("SRK_TEST_MINIO_IMAGE"), "server", "/data")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	port := strings.TrimPrefix(dockerTest(t, "port", name, "9000/tcp"), "127.0.0.1:")
	creds := filepath.Join(dir, "creds.json")
	os.WriteFile(creds, []byte(`{"access_key":"srk-test","secret_key":"srk-fixture-password-only"}`), 0600)
	profile := config.S3Profile{Endpoint: "https://127.0.0.1:" + port, Region: "us-east-1", Bucket: "source-bucket", CredentialFile: creds, CAFile: filepath.Join(certDir, "public.crt"), StagingDir: dir, MaxBytes: 64 << 20, AllowRestore: true}
	c, e := s3Client(profile)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	eventually(t, func() bool { return c.MakeBucket(ctx, profile.Bucket, minio.MakeBucketOptions{}) == nil })
	c.MakeBucket(ctx, "restore-bucket", minio.MakeBucketOptions{})
	payload := bytes.Repeat([]byte("SRK-S3-data"), 500000)
	for _, key := range []string{"папка/object +%.bin", "nested/opaque-object-key"} {
		_, e = c.PutObject(ctx, profile.Bucket, key, bytes.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{ContentType: "application/custom", UserMetadata: map[string]string{"project": "srk"}, UserTags: map[string]string{"test": "backup"}})
		if e != nil {
			t.Fatal(e)
		}
	}
	_, e = c.PutObject(ctx, profile.Bucket, "empty", bytes.NewReader(nil), 0, minio.PutObjectOptions{})
	if e != nil {
		t.Fatal(e)
	}
	target := profile
	target.Bucket = "restore-bucket"
	a, execute := dataWorker(t, func(c *config.Config) { c.S3 = map[string]config.S3Profile{"source": profile, "target": target} })
	first := execute("backup", "s3://source", "")
	r, e := a.Backup(ctx, first)
	if e != nil || !r.Verified || r.Data == nil || len(r.Data.Objects) != 3 {
		t.Fatal("S3 backup", r, e)
	}
	second := execute("backup", "s3://source", "")
	r, e = a.Backup(ctx, second)
	if e != nil || r.UploadedBytes != 0 {
		t.Fatal("S3 dedup", r.UploadedBytes, e)
	}
	restore := execute("restore", "s3://source", first.RunID)
	restore.Destination = "s3://target/recovered/"
	r, e = a.Restore(ctx, restore)
	if e != nil || !r.Verified {
		t.Fatal("S3 restore", e)
	}
	obj, e := c.GetObject(ctx, target.Bucket, "recovered/папка/object +%.bin", minio.GetObjectOptions{})
	if e != nil {
		t.Fatal(e)
	}
	got, e := io.ReadAll(obj)
	obj.Close()
	if e != nil || !bytes.Equal(got, payload) {
		t.Fatal("S3 restored bytes differ", e)
	}
	stat, e := c.StatObject(ctx, target.Bucket, "recovered/папка/object +%.bin", minio.StatObjectOptions{})
	if e != nil || stat.ContentType != "application/custom" || stat.UserMetadata["Project"] != "srk" && stat.UserMetadata["project"] != "srk" {
		t.Fatal("metadata", stat, e)
	}
	if _, e = a.Restore(ctx, restore); e == nil {
		t.Fatal("S3 overwrite allowed")
	}
}
func TestNativeMariaDBDumpRestoreAndExistingDatabaseProtection(t *testing.T) {
	nativeSQLRoundTrip(t, "mariadb", os.Getenv("SRK_TEST_MARIADB_IMAGE"))
}
func TestNativeMySQLDumpRestoreAndExistingDatabaseProtection(t *testing.T) {
	nativeSQLRoundTrip(t, "mysql", os.Getenv("SRK_TEST_MYSQL_IMAGE"))
}
func nativeSQLRoundTrip(t *testing.T, engine, image string) {
	if image == "" {
		t.Skip("set SRK_TEST_MARIADB_IMAGE or SRK_TEST_MYSQL_IMAGE")
	}
	dir := t.TempDir()
	name := "srk-mariadb-test-" + model.ID()[:12]
	variable := "MARIADB_ALLOW_EMPTY_ROOT_PASSWORD=1"
	admin := "mariadb-admin"
	dump := "mariadb-dump"
	if engine == "mysql" {
		variable = "MYSQL_ALLOW_EMPTY_PASSWORD=1"
		admin = "mysqladmin"
		dump = "mysqldump"
	}
	dockerTest(t, "run", "-d", "--rm", "--name", name, "-e", variable, "-v", dir+":"+dir, image, "--skip-networking")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	deadline := time.Now().Add(90 * time.Second)
	for {
		logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
		initialized := strings.Contains(string(logs), "init process done. Ready for start up.")
		if initialized && exec.Command("docker", "exec", name, admin, "--socket=/run/mysqld/mysqld.sock", "ping").Run() == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("MariaDB not ready", dockerTest(t, "logs", name))
		}
		time.Sleep(time.Second)
	}
	defaults := filepath.Join(dir, "client.cnf")
	os.WriteFile(defaults, []byte("[client]\nuser=root\nsocket=/run/mysqld/mysqld.sock\n"), 0600)
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0700)
	docker, _ := exec.LookPath("docker")
	for _, tool := range []string{engine, dump} {
		script := "#!/bin/sh\nexec " + docker + " exec -i " + name + " /usr/bin/" + tool + " \"$@\"\n"
		os.WriteFile(filepath.Join(bin, tool), []byte(script), 0700)
	}
	profile := config.MySQLProfile{MaxBytes: 64 << 20, Engine: engine, DefaultsFile: defaults, BinDir: bin, Database: "source_db", StagingDir: dir, TimeoutSeconds: 90, SSLMode: "disable", AllowRestore: true}
	ctx := context.Background()
	seed := "CREATE DATABASE source_db; CREATE TABLE source_db.t(id INT PRIMARY KEY, value VARBINARY(100)) ENGINE=InnoDB; INSERT INTO source_db.t VALUES(1,0x00ff1122); CREATE TRIGGER source_db.tr BEFORE UPDATE ON source_db.t FOR EACH ROW SET NEW.id=OLD.id;"
	if e := sqlCommand(ctx, profile, engine, []string{"--execute=" + seed}, nil, io.Discard); e != nil {
		t.Fatal(e)
	}
	a, execute := dataWorker(t, func(c *config.Config) { c.MySQL = map[string]config.MySQLProfile{"source": profile, "target": profile} })
	first := execute("backup", "mysql://source", "")
	res, e := a.Backup(ctx, first)
	if e != nil || !res.Verified {
		t.Fatal("MariaDB dump", e)
	}
	second := execute("backup", "mysql://source", "")
	res, e = a.Backup(ctx, second)
	if e != nil || res.UploadedBytes != 0 {
		t.Fatal("SQL dedup", res.UploadedBytes, e)
	}
	restore := execute("restore", "mysql://source", first.RunID)
	restore.Destination = "mysql://target/restored_db"
	res, e = a.Restore(ctx, restore)
	if e != nil || !res.Verified {
		diag := t.TempDir()
		_ = a.dumpMySQL(ctx, first, profile, diag, &model.DataBackup{})
		input, _ := os.Open(filepath.Join(diag, "database.sql"))
		cmd := exec.Command(filepath.Join(bin, "mariadb"), "--defaults-file="+defaults, "--skip-ssl", "--binary-mode", "--database=restored_db")
		cmd.Stdin = input
		out, err := cmd.CombinedOutput()
		input.Close()
		t.Fatalf("MariaDB restore %v; diagnostic=%s %v", e, out, err)
	}
	var got strings.Builder
	if e = sqlCommand(ctx, profile, engine, []string{"--batch", "--skip-column-names", "--execute=SELECT HEX(value) FROM restored_db.t"}, nil, &got); e != nil || strings.TrimSpace(got.String()) != "00FF1122" {
		t.Fatal("restored SQL data differs", got.String(), e)
	}
	if _, e = a.Restore(ctx, restore); e == nil {
		t.Fatal("existing database overwritten")
	}
	if e = sqlCommand(ctx, profile, engine, []string{"--execute=CREATE TABLE source_db.nontransactional(id INT) ENGINE=MyISAM"}, nil, io.Discard); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Backup(ctx, execute("backup", "mysql://source", "")); e == nil {
		t.Fatal("MyISAM falsely claimed transaction-consistent")
	}
}
