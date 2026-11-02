// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderInventoryContracts(t *testing.T) {
	for _, kind := range []string{"openstack", "ovirt", "proxmox", "opennebula"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "openstack":
					if r.URL.Path != "/servers/detail" || r.Header.Get("X-Auth-Token") != "token" {
						t.Error("wrong Nova request")
					}
					_, _ = io.WriteString(w, `{"servers":[{"id":"1","name":"vm","status":"ACTIVE"}]}`)
				case "ovirt":
					if r.URL.Path != "/vms" || r.Header.Get("Authorization") != "Bearer token" {
						t.Error("wrong oVirt request")
					}
					_, _ = io.WriteString(w, `{"vm":[{"id":"1","name":"vm","status":"up"}]}`)
				case "proxmox":
					if r.URL.Path != "/cluster/resources" || r.Header.Get("Authorization") != "PVEAPIToken=user@pve!srk=secret" {
						t.Error("wrong Proxmox request")
					}
					_, _ = io.WriteString(w, `{"data":[{"vmid":1,"name":"vm","status":"running","node":"node","type":"qemu"},{"vmid":2,"type":"lxc"}]}`)
				case "opennebula":
					b, _ := io.ReadAll(r.Body)
					var q any
					if e := xml.Unmarshal(b, &q); e != nil {
						t.Error("invalid XML", e)
					}
					if !strings.Contains(string(b), "one.vmpool.info") {
						t.Error("wrong XML RPC method")
					}
					_, _ = io.WriteString(w, `<methodResponse><params><param><value><array><data><value><boolean>1</boolean></value><value><string>&lt;VM_POOL&gt;&lt;VM&gt;&lt;ID&gt;1&lt;/ID&gt;&lt;NAME&gt;vm&lt;/NAME&gt;&lt;STATE&gt;3&lt;/STATE&gt;&lt;/VM&gt;&lt;/VM_POOL&gt;</string></value></data></array></value></param></params></methodResponse>`)
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			ca := filepath.Join(dir, "ca.pem")
			_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
			creds := filepath.Join(dir, "credential.json")
			b, _ := json.Marshal(map[string]string{"token": "token", "token_id": "user@pve!srk", "secret": "secret", "session": "user:secret"})
			_ = os.WriteFile(creds, b, 0600)
			c, e := New(Profile{Kind: kind, Endpoint: server.URL, CAFile: ca, CredentialFile: creds})
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			v, e := c.Inventory(context.Background())
			if e != nil || len(v) != 1 || v[0].ID != "1" {
				t.Fatal("inventory", v, e)
			}
			if c.Plan(v[0]).Executable {
				t.Fatal("inventory falsely claims executable backup")
			}
			_ = os.Chmod(creds, 0644)
			if _, e = New(Profile{Kind: kind, Endpoint: server.URL, CredentialFile: creds}); e == nil {
				t.Fatal("public secret file accepted")
			}
		})
	}
}

func TestOutstandingJournalsDoNotSerializeLiveVMJobs(t *testing.T) {
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	p := Profile{StateDir: dir}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	j, e := OpenJournal(p, id, "proxy", "backup")
	if e != nil {
		t.Fatal(e)
	}
	outstanding, e := Outstanding(p)
	if e != nil || len(outstanding) != 0 {
		t.Fatal("live journal blocked another VM", outstanding, e)
	}
	j.Close()
	outstanding, e = Outstanding(p)
	if e != nil || len(outstanding) != 1 {
		t.Fatal("abandoned journal not detected", outstanding, e)
	}
}
