// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package testcloud

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/klauspost/compress/zstd"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

type PVETask struct{ Status, Exit string }
type ProxmoxCloud struct {
	mu            sync.Mutex
	VMs           map[string]map[string]any
	States        map[string]string
	Tasks         map[string]*PVETask
	Counter       int
	DAVURL        string
	DAVClient     *http.Client
	FailRestore   bool
	CorruptBackup bool
	Archive       []byte
	Restored      []byte
}

func NewProxmox() *ProxmoxCloud {
	cfg := map[string]any{"name": "pve-source", "memory": 1024, "cores": 2, "sockets": 1, "bios": "seabios", "scsihw": "virtio-scsi-pci", "scsi0": "local-lvm:vm-100-disk-0,size=8M", "boot": "order=scsi0;net0", "net0": "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0"}
	config := "name: pve-source\nmemory: 1024\ncores: 2\nsockets: 1\nbios: seabios\nscsihw: virtio-scsi-pci\nscsi0: local-lvm:vm-100-disk-0,size=8M\nnet0: virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0\nboot: order=scsi0;net0\n"
	raw := VMA(config, map[string][]byte{"scsi0": bytes.Repeat([]byte("SRKPVE00"), 1<<20)})
	return &ProxmoxCloud{VMs: map[string]map[string]any{"100": cfg}, States: map[string]string{"100": "running"}, Tasks: map[string]*PVETask{}, Archive: raw}
}
func (c *ProxmoxCloud) task(kind, id string, work func() error) string {
	c.Counter++
	upid := fmt.Sprintf("UPID:pve:00000001:00000001:00000001:%s:%s:test@pve!srk-%d:", kind, id, c.Counter)
	task := &PVETask{Status: "running"}
	c.Tasks[upid] = task
	go func() {
		e := work()
		c.mu.Lock()
		defer c.mu.Unlock()
		task.Status = "stopped"
		task.Exit = "OK"
		if e != nil {
			task.Exit = "ERROR"
		}
	}()
	return upid
}
func (c *ProxmoxCloud) dav(method, path string, body io.Reader, headers http.Header) ([]byte, error) {
	req, e := http.NewRequest(method, c.DAVURL+path, body)
	if e != nil {
		return nil, e
	}
	req.SetBasicAuth("pve-test", "fixture-webdav-password")
	for key, values := range headers {
		req.Header[key] = values
	}
	response, e := c.DAVClient.Do(req)
	if e != nil {
		return nil, e
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("DAV %s %s returned %d", method, path, response.StatusCode)
	}
	return io.ReadAll(response.Body)
}
func (c *ProxmoxCloud) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "PVEAPIToken=test@pve!srk=fixture-pve-secret" {
			http.Error(w, "token required", 403)
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		p := strings.Split(strings.TrimPrefix(r.URL.Path, "/api2/json/"), "/")
		_ = r.ParseForm()
		reply := func(data any) { json.NewEncoder(w).Encode(map[string]any{"data": data}) }
		if p[0] == "version" {
			reply(map[string]string{"version": "8.4.1"})
			return
		}
		if strings.Join(p, "/") == "cluster/resources" {
			rows := []map[string]any{}
			for id, cfg := range c.VMs {
				rows = append(rows, map[string]any{"vmid": int(oneFixtureInt(id)), "name": cfg["name"], "node": "pve", "status": c.States[id], "type": "qemu"})
			}
			reply(rows)
			return
		}
		if strings.Join(p, "/") == "cluster/nextid" {
			id := 200
			for c.VMs[strconv.Itoa(id)] != nil {
				id++
			}
			reply(strconv.Itoa(id))
			return
		}
		if p[0] == "storage" && len(p) == 2 {
			reply(map[string]any{"type": "dir", "path": "/mnt/srk", "is_mountpoint": 1, "content": "backup"})
			return
		}
		if len(p) >= 3 && p[0] == "nodes" {
			if p[2] == "storage" {
				reply(map[string]any{"active": 1, "content": "images,backup,import"})
				return
			}
			if p[2] == "network" {
				reply([]map[string]any{{"iface": "vmbr0", "active": 1}})
				return
			}
			if p[2] == "tasks" && len(p) >= 4 {
				task := c.Tasks[p[3]]
				if task == nil {
					http.NotFound(w, r)
					return
				}
				if r.Method == "DELETE" {
					task.Status = "stopped"
					task.Exit = "ERROR"
					reply(nil)
					return
				}
				reply(map[string]string{"status": task.Status, "exitstatus": task.Exit})
				return
			}
			if p[2] == "vzdump" {
				id := r.Form.Get("vmid")
				dir := strings.TrimPrefix(r.Form.Get("dumpdir"), "/mnt/srk")
				if id != "100" || !strings.HasPrefix(dir, "/staging/") || r.Form.Get("compress") != "zstd" || r.Form.Get("remove") != "0" {
					http.Error(w, "invalid isolated backup", 400)
					return
				}
				archive := append([]byte(nil), c.Archive...)
				corrupt := c.CorruptBackup
				upid := c.task("vzdump", id, func() error {
					encoder, _ := zstd.NewWriter(nil)
					data := encoder.EncodeAll(archive, nil)
					encoder.Close()
					if corrupt {
						data = data[:len(data)/2]
					}
					file := dir + "/vzdump-qemu-100-2026_10_29-00_00_00.vma.zst"
					if _, e := c.dav("PUT", file+".tmp", bytes.NewReader(data), nil); e != nil {
						return e
					}
					_, e := c.dav("MOVE", file+".tmp", nil, http.Header{"Destination": {c.DAVURL + file}})
					return e
				})
				reply(upid)
				return
			}
			if p[2] == "qemu" {
				if len(p) == 3 && r.Method == "POST" {
					id := r.Form.Get("vmid")
					if id == "100" || c.VMs[id] != nil {
						http.Error(w, "source/existing VM protected", 409)
						return
					}
					source := strings.TrimPrefix(r.Form.Get("archive"), "/mnt/srk")
					storage := r.Form.Get("storage")
					fail := c.FailRestore
					upid := c.task("qmrestore", id, func() error {
						data, e := c.dav("GET", source, nil, nil)
						if e != nil {
							return e
						}
						config, e := VMAConfig(data)
						if e != nil {
							return e
						}
						cfg := map[string]any{}
						for _, line := range strings.Split(config, "\n") {
							key, value, ok := strings.Cut(line, ":")
							if ok {
								cfg[key] = strings.TrimSpace(value)
							}
						}
						cfg["scsi0"] = storage + ":vm-" + id + "-disk-0,size=8M"
						c.mu.Lock()
						c.VMs[id] = cfg
						c.States[id] = "stopped"
						c.Restored = data
						c.mu.Unlock()
						if fail {
							return fmt.Errorf("injected restore failure")
						}
						return nil
					})
					reply(upid)
					return
				}
				if len(p) >= 4 {
					id := p[3]
					cfg := c.VMs[id]
					if cfg == nil {
						http.NotFound(w, r)
						return
					}
					if len(p) == 4 && r.Method == "DELETE" {
						if id == "100" {
							http.Error(w, "source protected", 403)
							return
						}
						upid := c.task("qmdestroy", id, func() error { c.mu.Lock(); defer c.mu.Unlock(); delete(c.VMs, id); delete(c.States, id); return nil })
						reply(upid)
						return
					}
					if len(p) >= 5 && p[4] == "config" {
						if r.Method == "GET" {
							reply(cfg)
							return
						}
						if id == "100" {
							http.Error(w, "source immutable", 403)
							return
						}
						for _, key := range strings.Split(r.Form.Get("delete"), ",") {
							delete(cfg, key)
						}
						for key, values := range r.Form {
							if key != "delete" {
								cfg[key] = values[0]
							}
						}
						upid := c.task("qmconfig", id, func() error { return nil })
						reply(upid)
						return
					}
					if len(p) >= 6 && p[4] == "status" {
						if p[5] == "current" {
							reply(map[string]string{"status": c.States[id]})
							return
						}
						if id == "100" {
							http.Error(w, "source immutable", 403)
							return
						}
						operation := p[5]
						upid := c.task("qm"+operation, id, func() error {
							c.mu.Lock()
							defer c.mu.Unlock()
							if operation == "start" {
								c.States[id] = "running"
							} else {
								c.States[id] = "stopped"
							}
							return nil
						})
						reply(upid)
						return
					}
				}
			}
		}
		http.Error(w, "unimplemented PVE fixture API", 404)
	})
}
func oneFixtureInt(s string) int { n, _ := strconv.Atoi(s); return n }
func (c *ProxmoxCloud) Summary() (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.VMs) - 1, c.States["100"] == "running"
}
