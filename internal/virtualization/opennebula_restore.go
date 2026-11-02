// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type oneExport struct {
	server    *http.Server
	listener  net.Listener
	url       string
	mu        sync.Mutex
	streamMu  sync.Mutex
	delivered map[string]bool
	disks     map[string]model.VMDisk
	open      func(model.VMDisk) (io.ReadCloser, error)
	ctx       context.Context
	requests  sync.WaitGroup
}

func (c *OpenNebula) export(ctx context.Context, meta model.VMBackup, open func(model.VMDisk) (io.ReadCloser, error)) (*oneExport, error) {
	pair, e := tls.LoadX509KeyPair(c.base.profile.RestoreCertFile, c.base.profile.RestoreKeyFile)
	if e != nil {
		return nil, errors.New("restore HTTPS certificate/key unavailable")
	}
	listener, e := net.Listen("tcp", c.base.profile.RestoreExportListen)
	if e != nil {
		return nil, e
	}
	u, _ := url.Parse(c.base.profile.RestoreExportURL)
	if u.Port() == "0" {
		_, port, _ := net.SplitHostPort(listener.Addr().String())
		u.Host = net.JoinHostPort(u.Hostname(), port)
	}
	export := &oneExport{listener: listener, url: strings.TrimRight(u.String(), "/"), disks: map[string]model.VMDisk{}, delivered: map[string]bool{}, open: open, ctx: ctx}
	for _, disk := range meta.Disks {
		b := make([]byte, 32)
		if _, e = rand.Read(b); e != nil {
			listener.Close()
			return nil, e
		}
		export.disks[hex.EncodeToString(b)] = disk
	}
	export.server = &http.Server{ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, Handler: export}
	go func() { _ = export.server.ServeTLS(listener, "", "") }()
	return export, nil
}
func (e *oneExport) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = e.server.Shutdown(ctx)
	_ = e.server.Close()
}
func (e *oneExport) URL(disk model.VMDisk) string {
	for token, d := range e.disks {
		if d.File == disk.File {
			return e.url + "/disk/" + token
		}
	}
	return ""
}
func (e *oneExport) Delivered(file string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.delivered[file]
}
func (e *oneExport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || r.Method != "GET" && r.Method != "HEAD" || e.ctx.Err() != nil {
		http.NotFound(w, r)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/disk/")
	disk, ok := e.disks[token]
	if !ok || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(disk.Bytes, 10))
	w.Header().Set("Accept-Ranges", "none")
	if r.Method == "HEAD" {
		return
	}
	if r.Header.Get("Range") != "" {
		w.Header().Del("Content-Length")
		http.Error(w, "full sequential image download required", 416)
		return
	}
	e.streamMu.Lock()
	defer e.streamMu.Unlock()
	reader, err := e.open(disk)
	if err != nil {
		w.Header().Del("Content-Length")
		http.Error(w, "verified backup stream unavailable", 503)
		return
	}
	n, err := io.Copy(w, reader)
	closed := reader.Close()
	if err == nil && closed == nil && n == disk.Bytes {
		e.mu.Lock()
		e.delivered[disk.File] = true
		e.mu.Unlock()
	}
}
func (c *OpenNebula) Restore(ctx context.Context, j *Journal, meta model.VMBackup, r model.VMRestoreRequest, open func(model.VMDisk) (io.ReadCloser, error)) (model.VMRestoreResult, error) {
	result := model.VMRestoreResult{Name: r.Name}
	if e := r.ValidateFor("opennebula"); e != nil {
		return result, e
	}
	if !numericID.MatchString(r.FlavorID) || !numericID.MatchString(r.StorageDomainID) {
		return result, errors.New("numeric OpenNebula cluster and image datastore IDs required")
	}
	if _, e := c.rpc(ctx, "one.cluster.info", oneInt(r.FlavorID)); e != nil {
		return result, e
	}
	ds, e := c.datastore(ctx, r.StorageDomainID)
	if e != nil {
		return result, e
	}
	if ds.Template.Verify != "YES" {
		return result, errors.New("target image datastore must attest SRK_HTTPS_VERIFY=YES after hardening its downloader certificate verification")
	}
	for _, id := range r.Networks {
		if _, e = c.rpc(ctx, "one.vn.info", oneInt(id)); e != nil {
			return result, e
		}
	}
	export, e := c.export(ctx, meta, open)
	if e != nil {
		return result, e
	}
	defer export.Close()
	var images []string
	j.SourceServer = meta.ServerID
	if e = j.Save(); e != nil {
		return result, e
	}
	for i, disk := range meta.Disks {
		name := j.Name(fmt.Sprintf("image-%02d", i))
		slot, e := j.Intent("one-image", name, true)
		if e != nil {
			return result, e
		}
		kind := "DATABLOCK"
		if disk.BootIndex == 0 {
			kind = "OS"
		}
		template := `<IMAGE><NAME>` + xmlText(name) + `</NAME><TYPE>` + kind + `</TYPE><PATH>` + xmlText(export.URL(disk)) + `</PATH><DRIVER>raw</DRIVER><SRK_RUN>` + j.ID + `</SRK_RUN><SRK_PROXY>` + xmlText(j.Owner) + `</SRK_PROXY></IMAGE>`
		id, e := c.rpc(ctx, "one.image.allocate", template, oneInt(r.StorageDomainID), false)
		if e != nil {
			return result, e
		}
		if !numericID.MatchString(id) {
			return result, errors.New("invalid OpenNebula image receipt")
		}
		if e = j.RecordOpaque(slot, id); e != nil {
			return result, e
		}
		if e = c.wait(ctx, func() (bool, error) {
			image, e := c.image(ctx, id)
			if e != nil {
				return false, e
			}
			if image.Template.Run != j.ID || image.Template.Proxy != j.Owner {
				return false, errors.New("new OpenNebula image ownership differs")
			}
			if image.State == 5 {
				return false, errors.New("OpenNebula image import failed")
			}
			return image.State == 1, nil
		}); e != nil {
			return result, e
		}
		if !export.Delivered(disk.File) {
			return result, errors.New("OpenNebula marked image ready without consuming the verified HTTPS stream")
		}
		images = append(images, id)
	}
	slot, e := j.Intent("one-vm", j.Name("vm"), true)
	if e != nil {
		return result, e
	}
	template := `<VM><NAME>` + xmlText(j.Name("vm")) + `</NAME><CPU>` + strconv.Itoa(meta.Flavor.VCPUs) + `</CPU><VCPU>` + strconv.Itoa(meta.Flavor.VCPUs) + `</VCPU><MEMORY>` + strconv.Itoa(meta.Flavor.RAM) + `</MEMORY><SRK_RUN>` + j.ID + `</SRK_RUN><SRK_PROXY>` + xmlText(j.Owner) + `</SRK_PROXY><SCHED_REQUIREMENTS>CLUSTER_ID = ` + r.FlavorID + `</SCHED_REQUIREMENTS><OS><ARCH>` + xmlText(meta.Metadata["architecture"]) + `</ARCH>`
	if meta.Metadata["machine"] != "" {
		template += `<MACHINE>` + xmlText(meta.Metadata["machine"]) + `</MACHINE>`
	}
	firmware := meta.Metadata["firmware"]
	if strings.Contains(strings.ToLower(firmware), "ovmf") {
		firmware = "UEFI"
	}
	if firmware != "" {
		if firmware != "UEFI" && firmware != "UEFI_SECURE" && firmware != "BIOS" {
			return result, errors.New("unsupported saved OpenNebula firmware")
		}
		template += `<FIRMWARE>` + firmware + `</FIRMWARE>`
	}
	boot := ""
	for i, d := range meta.Disks {
		if d.BootIndex == 0 {
			boot = "disk" + strconv.Itoa(i)
		}
	}
	if boot == "" {
		return result, errors.New("OpenNebula boot disk missing")
	}
	template += `<BOOT>` + boot + `</BOOT></OS>`
	for i, id := range images {
		disk := meta.Disks[i]
		template += `<DISK><IMAGE_ID>` + id + `</IMAGE_ID><DRIVER>raw</DRIVER>`
		if prefix := disk.Properties["dev_prefix"]; prefix != "" {
			if !nativeName.MatchString(prefix) {
				return result, errors.New("invalid disk prefix")
			}
			template += `<DEV_PREFIX>` + xmlText(prefix) + `</DEV_PREFIX>`
		}
		if target := disk.Properties["target"]; target != "" {
			if !nativeName.MatchString(target) {
				return result, errors.New("invalid disk target")
			}
			template += `<TARGET>` + xmlText(target) + `</TARGET>`
		}
		template += `</DISK>`
	}
	for _, network := range r.Networks {
		template += `<NIC><NETWORK_ID>` + network + `</NETWORK_ID><MODEL>virtio</MODEL></NIC>`
	}
	template += `</VM>`
	id, e := c.rpc(ctx, "one.vm.allocate", template, true)
	if e != nil {
		return result, e
	}
	if !numericID.MatchString(id) || id == meta.ServerID {
		return result, errors.New("fresh OpenNebula VM identity required")
	}
	if e = j.RecordOpaque(slot, id); e != nil {
		return result, e
	}
	if _, e = c.rpc(ctx, "one.vm.rename", oneInt(id), r.Name); e != nil {
		return result, e
	}
	if _, e = c.rpc(ctx, "one.vm.action", "release", oneInt(id)); e != nil {
		return result, e
	}
	if e = c.wait(ctx, func() (bool, error) {
		vm, e := c.vm(ctx, id)
		if e != nil {
			return false, e
		}
		if vm.State == 7 {
			return false, errors.New("new OpenNebula VM failed")
		}
		return vm.State == 3 && vm.LCM == 3, nil
	}); e != nil {
		return result, e
	}
	vm, e := c.vm(ctx, id)
	if e != nil {
		return result, e
	}
	if vm.UserTemplate.Run != j.ID || vm.UserTemplate.Proxy != j.Owner || vm.Name != r.Name || oneInt(vm.Template.Memory) != meta.Flavor.RAM || oneInt(vm.Template.VCPU) != meta.Flavor.VCPUs || len(vm.Template.Disks) != len(images) || len(vm.Template.NICs) != len(r.Networks) {
		return result, errors.New("restored OpenNebula VM configuration differs")
	}
	for i, d := range vm.Template.Disks {
		if d.Image != images[i] {
			return result, errors.New("restored OpenNebula image attachment differs")
		}
	}
	for i, n := range vm.Template.NICs {
		if n.Network != r.Networks[i] {
			return result, errors.New("restored OpenNebula NIC differs")
		}
	}
	result.ServerID = id
	result.Status = "running"
	result.Volumes = images
	result.ControlPlaneVerified = true
	j.Restore = &result
	return result, j.Save()
}
func (c *OpenNebula) resolve(ctx context.Context, j *Journal, r Resource) (string, error) {
	method := "one.imagepool.info"
	args := []any{-2, -1, -1}
	if r.Kind == "one-vm" {
		method = "one.vmpool.info"
		args = []any{-2, -1, -1, -1, ""}
	}
	raw, e := c.rpc(ctx, method, args...)
	if e != nil {
		return "", e
	}
	var ids []string
	if r.Kind == "one-vm" {
		var pool struct {
			VMs []oneVM `xml:"VM"`
		}
		if xml.Unmarshal([]byte(raw), &pool) != nil {
			return "", errors.New("invalid OpenNebula VM pool")
		}
		for _, vm := range pool.VMs {
			if vm.Name == r.Name && vm.UserTemplate.Run == j.ID && vm.UserTemplate.Proxy == j.Owner {
				ids = append(ids, vm.ID)
			}
		}
	} else {
		var pool struct {
			Images []oneImage `xml:"IMAGE"`
		}
		if xml.Unmarshal([]byte(raw), &pool) != nil {
			return "", errors.New("invalid OpenNebula image pool")
		}
		for _, image := range pool.Images {
			if image.Name == r.Name && image.Template.Run == j.ID && image.Template.Proxy == j.Owner {
				ids = append(ids, image.ID)
			}
		}
	}
	if len(ids) != 1 {
		return "", errors.New("unresolved or ambiguous OpenNebula create intent")
	}
	return ids[0], nil
}
func (c *OpenNebula) Cleanup(ctx context.Context, j *Journal, log func(string)) error {
	for i := len(j.Resources) - 1; i >= 0; i-- {
		r := &j.Resources[i]
		if r.Deleted || j.Success && r.Keep {
			continue
		}
		if r.ID == "" {
			if r.Kind != "one-image" && r.Kind != "one-vm" {
				return errors.New("OpenNebula backup start outcome unresolved")
			}
			id, e := c.resolve(ctx, j, *r)
			if e != nil {
				return e
			}
			if e = j.RecordOpaque(i, id); e != nil {
				return e
			}
		}
		switch r.Kind {
		case "one-transfer":
			continue
		case "one-session":
			status, e := c.status(ctx, r.Name, j.SourceServer)
			if e != nil {
				return e
			}
			known := map[string]bool{}
			for _, item := range j.Resources {
				if item.Kind == "one-transfer" {
					known[item.ID] = true
				}
			}
			if len(known) == 0 {
				return errors.New("OneBEX session ownership is unresolved")
			}
			for _, transfer := range status.Transfers {
				if !known[transfer.ID] {
					return errors.New("foreign OneBEX export session; cancellation refused")
				}
			}
			endpoint := strings.TrimRight(c.base.profile.OneBEXEndpoints[r.Name], "/")
			if e = c.jsonResponse(ctx, "POST", endpoint+"/vms/"+j.SourceServer+"/cancel", map[string]string{"MESSAGE": "SRK operation cleanup"}, nil); e != nil {
				return e
			}
			for k := range j.Resources {
				if j.Resources[k].Kind == "one-transfer" {
					j.Resources[k].Deleted = true
				}
			}
		case "one-image":
			image, e := c.image(ctx, r.ID)
			if e != nil {
				return e
			}
			if image.Template.Run != j.ID || image.Template.Proxy != j.Owner {
				return errors.New("foreign OpenNebula image; deletion refused")
			}
			if _, e = c.rpc(ctx, "one.image.delete", oneInt(r.ID), false); e != nil {
				return e
			}
		case "one-vm":
			vm, e := c.vm(ctx, r.ID)
			if e != nil {
				return e
			}
			if vm.ID == j.SourceServer || vm.UserTemplate.Run != j.ID || vm.UserTemplate.Proxy != j.Owner {
				return errors.New("foreign/source OpenNebula VM; deletion refused")
			}
			if vm.State != 6 {
				if _, e = c.rpc(ctx, "one.vm.action", "terminate-hard", oneInt(r.ID)); e != nil {
					return e
				}
				if e = c.wait(ctx, func() (bool, error) { current, e := c.vm(ctx, r.ID); return current.State == 6, e }); e != nil {
					return e
				}
			}
		default:
			return errors.New("unknown OpenNebula journal resource")
		}
		r.Deleted = true
		if e := j.Save(); e != nil {
			return e
		}
		if log != nil {
			log("released " + r.Kind + " " + r.ID)
		}
	}
	for _, r := range j.Resources {
		if !r.Deleted && !(j.Success && r.Keep) {
			return errors.New("OpenNebula transfer cleanup remains pending")
		}
	}
	j.Done = true
	j.LastError = ""
	return j.Save()
}
