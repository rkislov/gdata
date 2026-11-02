// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"gdata/srk/internal/model"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCertificateCLIOutputAndRecovery(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "client.key")
	csr := filepath.Join(dir, "client.csr")
	if e := pki([]string{"csr", "--key-out", key, "--csr-out", csr}); e != nil {
		t.Fatal(e)
	}
	if e := pki([]string{"csr", "--key-out", key, "--csr-out", csr}); e == nil {
		t.Fatal("overwrote private key")
	}
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/users/alice/certificates" {
			t.Error(r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if r.Method == "POST" {
			calls++
			var v struct {
				CSR     string `json:"csr"`
				Version int    `json:"version"`
				Days    int    `json:"days"`
				Overlap int    `json:"overlap_hours"`
			}
			if e := json.NewDecoder(r.Body).Decode(&v); e != nil {
				t.Fatal(e)
			}
			if v.Version != 0 || v.Days != 30 || v.Overlap != 1 || v.CSR == "" {
				t.Error(v)
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"certificate": "public-certificate", "version": 1})
		} else {
			json.NewEncoder(w).Encode(model.CertificateState{Version: 1, Managed: true, Certificates: []model.PersonalCertificate{{Serial: "123", Certificate: "public-certificate"}}})
		}
	}))
	defer server.Close()
	out := filepath.Join(dir, "client.crt")
	args := []string{"issue", "alice", "--csr", csr, "--out", out, "--version", "0", "--overlap-hours", "1"}
	if e := certificates(context.Background(), server.Client(), server.URL, args, true); e != nil {
		t.Fatal(e)
	}
	if e := certificates(context.Background(), server.Client(), server.URL, args, true); e == nil {
		t.Fatal("overwrote certificate")
	}
	if calls != 1 {
		t.Fatal("rotated before checking output file")
	}
	recovered := filepath.Join(dir, "recovered.crt")
	if e := certificates(context.Background(), server.Client(), server.URL, []string{"get", "alice", "--serial", "123", "--out", recovered}, true); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(recovered)
	if string(b) != "public-certificate" {
		t.Fatal(string(b))
	}
}
