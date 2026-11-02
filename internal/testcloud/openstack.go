// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
// Package testcloud is a deterministic API fixture, not a virtualization backend.
package testcloud

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const Project = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const ImageVM = "11111111-1111-4111-8111-111111111111"
const VolumeVM = "22222222-2222-4222-8222-222222222222"
const RootVolume = "33333333-3333-4333-8333-333333333333"
const DataVolume = "44444444-4444-4444-8444-444444444444"
const Network = "55555555-5555-4555-8555-555555555555"
const Token = "SRK-LOCAL-API-FIXTURE-TOKEN"

type image struct {
	Value    map[string]any
	key      string
	md5, sha string
	size     int64
}
type Cloud struct {
	mu              sync.Mutex
	Images          map[string]*image
	Volumes         map[string]map[string]any
	Snapshots       map[string]map[string]any
	Servers         map[string]map[string]any
	FailCreateVM    bool
	Encrypted       bool
	Ephemeral       bool
	ForeignSnapshot bool
	FailDelete      bool
	Requests        []string
	seq             int
	digests         map[string][2]string
}

func New() *Cloud {
	c := &Cloud{Images: map[string]*image{}, Volumes: map[string]map[string]any{}, Snapshots: map[string]map[string]any{}, Servers: map[string]map[string]any{}, digests: map[string][2]string{}}
	for _, id := range []string{RootVolume, DataVolume} {
		c.Volumes[id] = map[string]any{"id": id, "name": "source-volume", "status": "in-use", "size": 1, "metadata": map[string]string{}, "os-vol-tenant-attr:tenant_id": Project}
	}
	c.Servers[ImageVM] = c.source(ImageVM, false)
	c.Servers[VolumeVM] = c.source(VolumeVM, true)
	return c
}
func (c *Cloud) source(id string, volume bool) map[string]any {
	im := any(map[string]any{"id": "99999999-9999-4999-8999-999999999999"})
	vols := []map[string]string{}
	if volume {
		im = ""
		vols = append(vols, map[string]string{"id": RootVolume}, map[string]string{"id": DataVolume})
	}
	return map[string]any{"id": id, "name": "API fixture VM", "status": "ACTIVE", "tenant_id": Project, "image": im, "flavor": map[string]string{"id": "small"}, "metadata": map[string]string{"source": "api-fixture"}, "os-extended-volumes:volumes_attached": vols, "security_groups": []map[string]string{{"name": "default"}}, "config_drive": "False"}
}
func (c *Cloud) next() string { c.seq++; return fmt.Sprintf("77777777-7777-4777-8777-%012x", c.seq) }
func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}
func payloadKey(key string) []byte { return []byte("SRK API fixture disk: " + key + "\n") }

type pattern struct {
	key       []byte
	size, off int64
}

func (r *pattern) Read(b []byte) (int, error) {
	if r.off >= r.size {
		return 0, io.EOF
	}
	n := min(int64(len(b)), r.size-r.off)
	clear(b[:n])
	if r.off < int64(len(r.key)) {
		copy(b[:n], r.key[r.off:])
	}
	r.off += n
	return int(n), nil
}
func (c *Cloud) newImage(id, name, key string, size int64, meta map[string]string) *image {
	cacheKey := fmt.Sprintf("%s/%d", key, size)
	d, ok := c.digests[cacheKey]
	if !ok {
		m, s := md5.New(), sha256.New()
		_, _ = io.CopyBuffer(io.MultiWriter(m, s), &pattern{key: payloadKey(key), size: size}, make([]byte, 4<<20))
		d = [2]string{hex.EncodeToString(m.Sum(nil)), hex.EncodeToString(s.Sum(nil))}
		c.digests[cacheKey] = d
	}
	v := map[string]any{"id": id, "name": name, "status": "active", "owner": Project, "size": size, "disk_format": "raw", "container_format": "bare", "min_disk": 1, "checksum": d[0], "os_hash_algo": "sha256", "os_hash_value": d[1]}
	for k, val := range meta {
		v[k] = val
	}
	im := &image{Value: v, key: key, size: size, md5: d[0], sha: d[1]}
	c.Images[id] = im
	return im
}
func readBody(r *http.Request) map[string]any {
	var body map[string]any
	_ = json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&body)
	return body
}
func object(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
func text(v any) string                { s, _ := v.(string); return s }
func (c *Cloud) Handler() http.Handler { return http.HandlerFunc(c.ServeHTTP) }
func (c *Cloud) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Auth-Token") != Token {
		reply(w, 401, map[string]string{"error": "fixture token required"})
		return
	}
	path := r.URL.Path
	c.mu.Lock()
	c.Requests = append(c.Requests, r.Method+" "+path)
	// Stream data without holding the cloud metadata mutex.
	if strings.HasPrefix(path, "/glance/v2/images/") && strings.HasSuffix(path, "/file") {
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/glance/v2/images/"), "/file")
		im := c.Images[id]
		if im == nil {
			c.mu.Unlock()
			reply(w, 404, nil)
			return
		}
		if r.Method == "GET" {
			key, size := im.key, im.size
			c.mu.Unlock()
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprint(size))
			w.WriteHeader(200)
			_, _ = io.CopyBuffer(w, &pattern{key: payloadKey(key), size: size}, make([]byte, 4<<20))
			return
		}
		if r.Method == "PUT" {
			c.mu.Unlock()
			m, s := md5.New(), sha256.New()
			n, e := io.CopyBuffer(io.MultiWriter(m, s), r.Body, make([]byte, 4<<20))
			if e != nil {
				reply(w, 400, nil)
				return
			}
			c.mu.Lock()
			im.size = n
			im.md5 = hex.EncodeToString(m.Sum(nil))
			im.sha = hex.EncodeToString(s.Sum(nil))
			im.Value["status"] = "active"
			im.Value["size"] = n
			im.Value["checksum"] = im.md5
			im.Value["os_hash_algo"] = "sha256"
			im.Value["os_hash_value"] = im.sha
			c.mu.Unlock()
			reply(w, 204, nil)
			return
		}
	}
	defer c.mu.Unlock()
	if r.Method == "DELETE" && c.FailDelete {
		reply(w, 503, nil)
		return
	}
	switch {
	case path == "/nova/v2.1/"+Project+"/servers/detail":
		list := []map[string]any{}
		for _, s := range c.Servers {
			list = append(list, s)
		}
		reply(w, 200, map[string]any{"servers": list})
	case strings.HasPrefix(path, "/nova/v2.1/"+Project+"/flavors/"):
		id := strings.TrimPrefix(path, "/nova/v2.1/"+Project+"/flavors/")
		ep := 0
		if c.Ephemeral {
			ep = 1
		}
		reply(w, 200, map[string]any{"flavor": map[string]any{"id": id, "ram": 1024, "vcpus": 1, "disk": 1, "OS-FLV-EXT-DATA:ephemeral": ep, "swap": ""}})
	case path == "/neutron/v2.0/networks/"+Network:
		reply(w, 200, map[string]any{"network": map[string]any{"id": Network, "status": "ACTIVE", "project_id": Project}})
	case path == "/nova/v2.1/"+Project+"/servers" && r.Method == "POST":
		body := object(readBody(r)["server"])
		id := c.next()
		state := "ACTIVE"
		if c.FailCreateVM {
			state = "ERROR"
		}
		attached := []map[string]string{}
		if list, ok := body["block_device_mapping_v2"].([]any); ok {
			for _, m := range list {
				v := text(object(m)["uuid"])
				attached = append(attached, map[string]string{"id": v})
				if vol := c.Volumes[v]; vol != nil {
					vol["status"] = "in-use"
				}
			}
		}
		s := map[string]any{"id": id, "name": body["name"], "status": state, "tenant_id": Project, "metadata": body["metadata"], "flavor": map[string]any{"id": body["flavorRef"]}, "os-extended-volumes:volumes_attached": attached, "networks": body["networks"]}
		c.Servers[id] = s
		reply(w, 202, map[string]any{"server": map[string]any{"id": id}})
	case strings.HasPrefix(path, "/nova/v2.1/"+Project+"/servers/"):
		rest := strings.TrimPrefix(path, "/nova/v2.1/"+Project+"/servers/")
		id := strings.Split(rest, "/")[0]
		s := c.Servers[id]
		if s == nil {
			reply(w, 404, nil)
			return
		}
		if strings.HasSuffix(path, "/os-interface") {
			nets := []map[string]any{{"net_id": Network, "mac_addr": "02:00:00:00:00:01", "fixed_ips": []map[string]string{{"ip_address": "192.0.2.10"}}}}
			if values, ok := s["networks"].([]any); ok {
				nets = nil
				for _, n := range values {
					nets = append(nets, map[string]any{"net_id": object(n)["uuid"], "fixed_ips": []any{}})
				}
			}
			reply(w, 200, map[string]any{"interfaceAttachments": nets})
			return
		}
		if strings.HasSuffix(path, "/action") {
			body := object(readBody(r)["createImage"])
			meta := map[string]string{}
			for k, v := range object(body["metadata"]) {
				meta[k] = text(v)
			}
			imageID := c.next()
			name := text(body["name"])
			if id == VolumeVM {
				im := c.newImage(imageID, name, "", 0, meta)
				mapping := []map[string]any{}
				for i, vid := range []string{RootVolume, DataVolume} {
					sid := c.next()
					c.Snapshots[sid] = map[string]any{"id": sid, "name": "snapshot for " + name, "status": "available", "volume_id": vid, "size": 1, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "metadata": map[string]string{}}
					boot := -1
					if i == 0 {
						boot = 0
					}
					mapping = append(mapping, map[string]any{"snapshot_id": sid, "source_type": "snapshot", "destination_type": "volume", "boot_index": boot, "device_name": fmt.Sprintf("/dev/vd%c", 'a'+i), "volume_size": 1})
				}
				b, _ := json.Marshal(mapping)
				im.Value["block_device_mapping"] = string(b)
			} else {
				c.newImage(imageID, name, "root-disk", 8<<20, meta)
			}
			w.Header().Set("Location", "https://ignored.invalid/glance/v2/images/"+imageID)
			reply(w, 202, nil)
			return
		}
		if r.Method == "DELETE" {
			if id == ImageVM || id == VolumeVM {
				reply(w, 403, nil)
				return
			}
			if volumes, ok := s["os-extended-volumes:volumes_attached"].([]map[string]string); ok {
				for _, v := range volumes {
					if vol := c.Volumes[v["id"]]; vol != nil {
						vol["status"] = "available"
					}
				}
			}
			delete(c.Servers, id)
			reply(w, 204, nil)
			return
		}
		reply(w, 200, map[string]any{"server": s})
	case path == "/glance/v2/images":
		if r.Method == "POST" {
			b := readBody(r)
			id := text(b["id"])
			im := &image{Value: b}
			im.Value["owner"] = Project
			im.Value["status"] = "queued"
			im.Value["size"] = 0
			c.Images[id] = im
			reply(w, 201, im.Value)
			return
		}
		list := []map[string]any{}
		for _, im := range c.Images {
			if name := r.URL.Query().Get("name"); name != "" && im.Value["name"] != name {
				continue
			}
			list = append(list, im.Value)
		}
		reply(w, 200, map[string]any{"images": list})
	case strings.HasPrefix(path, "/glance/v2/images/"):
		id := strings.TrimPrefix(path, "/glance/v2/images/")
		im := c.Images[id]
		if im == nil {
			reply(w, 404, nil)
			return
		}
		if r.Method == "DELETE" {
			delete(c.Images, id)
			reply(w, 204, nil)
			return
		}
		if r.Method == "PATCH" {
			var patches []map[string]any
			_ = json.NewDecoder(r.Body).Decode(&patches)
			for _, p := range patches {
				im.Value[strings.TrimPrefix(text(p["path"]), "/")] = p["value"]
			}
		}
		reply(w, 200, im.Value)
	case path == "/cinder/v3/"+Project+"/snapshots/detail":
		list := []map[string]any{}
		for _, v := range c.Snapshots {
			if name := r.URL.Query().Get("name"); name != "" && v["name"] != name {
				continue
			}
			list = append(list, v)
		}
		reply(w, 200, map[string]any{"snapshots": list})
	case path == "/cinder/v3/"+Project+"/snapshots" && r.Method == "POST":
		b := object(readBody(r)["snapshot"])
		id := c.next()
		b["id"] = id
		b["status"] = "available"
		b["size"] = 1
		b["created_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		c.Snapshots[id] = b
		reply(w, 202, map[string]any{"snapshot": b})
	case strings.HasPrefix(path, "/cinder/v3/"+Project+"/snapshots/"):
		rest := strings.TrimPrefix(path, "/cinder/v3/"+Project+"/snapshots/")
		id := strings.Split(rest, "/")[0]
		v := c.Snapshots[id]
		if v == nil {
			reply(w, 404, nil)
			return
		}
		if strings.HasSuffix(path, "/metadata") {
			meta := object(v["metadata"])
			if old, ok := v["metadata"].(map[string]string); ok {
				for k, val := range old {
					meta[k] = val
				}
			}
			for k, val := range object(readBody(r)["metadata"]) {
				meta[k] = val
			}
			v["metadata"] = meta
			reply(w, 200, map[string]any{"metadata": meta})
			return
		}
		if r.Method == "DELETE" {
			delete(c.Snapshots, id)
			reply(w, 202, nil)
			return
		}
		reply(w, 200, map[string]any{"snapshot": v})
	case path == "/cinder/v3/"+Project+"/volumes/detail":
		list := []map[string]any{}
		for _, v := range c.Volumes {
			if name := r.URL.Query().Get("name"); name != "" && v["name"] != name {
				continue
			}
			list = append(list, v)
		}
		reply(w, 200, map[string]any{"volumes": list})
	case path == "/cinder/v3/"+Project+"/volumes" && r.Method == "POST":
		b := object(readBody(r)["volume"])
		id := c.next()
		b["id"] = id
		b["status"] = "available"
		b["size"] = 1
		b["os-vol-tenant-attr:tenant_id"] = Project
		if sid := text(b["snapshot_id"]); sid != "" {
			if snap := c.Snapshots[sid]; snap != nil {
				b["source_key"] = snap["volume_id"]
			}
		}
		c.Volumes[id] = b
		reply(w, 202, map[string]any{"volume": b})
	case strings.HasPrefix(path, "/cinder/v3/"+Project+"/volumes/"):
		rest := strings.TrimPrefix(path, "/cinder/v3/"+Project+"/volumes/")
		id := strings.Split(rest, "/")[0]
		v := c.Volumes[id]
		if v == nil {
			reply(w, 404, nil)
			return
		}
		if strings.HasSuffix(path, "/action") {
			body := object(readBody(r)["os-volume_upload_image"])
			iid := c.next()
			c.newImage(iid, text(body["image_name"]), text(v["source_key"]), 1<<30, nil)
			reply(w, 202, map[string]any{"os-volume_upload_image": map[string]any{"image_id": iid}})
			return
		}
		if r.Method == "DELETE" {
			if id == RootVolume || id == DataVolume {
				reply(w, 403, nil)
				return
			}
			delete(c.Volumes, id)
			reply(w, 202, nil)
			return
		}
		if id == RootVolume || id == DataVolume {
			v["encrypted"] = c.Encrypted
		}
		reply(w, 200, map[string]any{"volume": v})
	default:
		reply(w, 404, map[string]string{"error": "unknown fixture API"})
	}
}
func (c *Cloud) Summary() (images, volumes, snapshots, servers int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.Images), len(c.Volumes) - 2, len(c.Snapshots), len(c.Servers) - 2
}
func (c *Cloud) Hashes() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for id, im := range c.Images {
		out[id] = im.sha
	}
	return out
}
