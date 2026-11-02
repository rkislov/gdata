// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package testcloud

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

type OVDisk struct {
	ID          string `xml:"id,attr"`
	Alias       string `xml:"alias"`
	Description string `xml:"description"`
	Status      string `xml:"status"`
	StorageType string `xml:"storage_type"`
	Size        int64  `xml:"provisioned_size"`
	Data        []byte `xml:"-"`
}
type OVAttachment struct {
	ID        string `xml:"id,attr"`
	Bootable  bool   `xml:"bootable"`
	Active    bool   `xml:"active"`
	Interface string `xml:"interface"`
	Disk      struct {
		ID string `xml:"id,attr"`
	} `xml:"disk"`
}
type OVVM struct {
	ID          string `xml:"id,attr"`
	Name        string `xml:"name"`
	Description string `xml:"description"`
	Status      string `xml:"status"`
	Memory      int64  `xml:"memory"`
	Cluster     struct {
		ID string `xml:"id,attr"`
	} `xml:"cluster"`
	OS   string `xml:"os>type"`
	Bios string `xml:"bios>type"`
	CPU  struct {
		Architecture string `xml:"architecture"`
		Topology     struct {
			Cores   int `xml:"cores,attr"`
			Sockets int `xml:"sockets,attr"`
			Threads int `xml:"threads,attr"`
		} `xml:"topology"`
	} `xml:"cpu"`
	Disks    []OVAttachment `xml:"-"`
	Networks []string       `xml:"-"`
}
type OVTransfer struct {
	ID        string `xml:"id,attr"`
	Phase     string `xml:"phase"`
	URL       string `xml:"transfer_url"`
	Direction string `xml:"direction"`
	Disk      struct {
		ID string `xml:"id,attr"`
	} `xml:"disk"`
	Backup struct {
		ID string `xml:"id,attr"`
	} `xml:"backup"`
}
type OVBackup struct {
	ID          string `xml:"id,attr"`
	Phase       string `xml:"phase"`
	Description string `xml:"description"`
	Disks       []struct {
		ID string `xml:"id,attr"`
	} `xml:"disks>disk"`
}
type OVirtCloud struct {
	mu        sync.Mutex
	serial    int
	VMs       map[string]*OVVM
	Disks     map[string]*OVDisk
	Transfers map[string]*OVTransfer
	Backups   map[string]*OVBackup
	FailStart bool
}

func NewOVirt() *OVirtCloud {
	c := &OVirtCloud{VMs: map[string]*OVVM{}, Disks: map[string]*OVDisk{}, Transfers: map[string]*OVTransfer{}, Backups: map[string]*OVBackup{}}
	vm := &OVVM{ID: ImageVM, Name: "oVirt API fixture", Status: "up", Memory: 1 << 30, OS: "rhel_8x64", Bios: "q35_ovmf", Networks: []string{Network}}
	vm.Cluster.ID = Project
	vm.CPU.Architecture = "x86_64"
	vm.CPU.Topology.Cores = 2
	vm.CPU.Topology.Sockets = 1
	vm.CPU.Topology.Threads = 1
	attachment := OVAttachment{ID: DataVolume, Bootable: true, Active: true, Interface: "virtio_scsi"}
	attachment.Disk.ID = RootVolume
	vm.Disks = []OVAttachment{attachment}
	c.VMs[ImageVM] = vm
	c.Disks[RootVolume] = &OVDisk{ID: RootVolume, Alias: "source-disk", Status: "ok", StorageType: "image", Size: 8 << 20, Data: bytes.Repeat([]byte("SRKOVIRT"), 1<<20)}
	return c
}
func (c *OVirtCloud) next() string {
	c.serial++
	return fmt.Sprintf("88888888-8888-4888-8888-%012d", c.serial)
}
func ovXML(w http.ResponseWriter, root string, value any) {
	if e := xml.NewEncoder(w).EncodeElement(value, xml.StartElement{Name: xml.Name{Local: root}}); e != nil {
		http.Error(w, "fixture encoding error", 500)
	}
}
func (c *OVirtCloud) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/ovirt-engine/api"), "/")
		parts := strings.Split(path, "/")
		method := r.Method
		if parts[0] == "images" && len(parts) == 2 {
			if r.Header.Get("Authorization") != "" {
				http.Error(w, "engine credential leaked to imageio", 403)
				return
			}
			transfer := c.Transfers[parts[1]]
			if transfer == nil {
				http.NotFound(w, r)
				return
			}
			disk := c.Disks[transfer.Disk.ID]
			if disk == nil {
				http.NotFound(w, r)
				return
			}
			if method == "GET" {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", fmt.Sprint(len(disk.Data)))
				w.Write(disk.Data)
				return
			}
			if method == "PUT" && transfer.Direction == "upload" {
				data, e := io.ReadAll(io.LimitReader(r.Body, disk.Size+1))
				if e != nil || int64(len(data)) != disk.Size {
					http.Error(w, "bad upload length", 400)
					return
				}
				disk.Data = data
				return
			}
		}
		if r.Header.Get("Authorization") != "Bearer "+Token {
			http.Error(w, "fixture token required", 403)
			return
		}
		if path == "vms" {
			if method == "GET" {
				if strings.Contains(r.Header.Get("Accept"), "json") {
					list := []map[string]string{}
					for _, vm := range c.VMs {
						list = append(list, map[string]string{"id": vm.ID, "name": vm.Name, "status": vm.Status})
					}
					json.NewEncoder(w).Encode(map[string]any{"vm": list})
					return
				}
				list := struct {
					Items []OVVM `xml:"vm"`
				}{}
				for _, vm := range c.VMs {
					list.Items = append(list.Items, *vm)
				}
				ovXML(w, "vms", list)
				return
			}
			if method == "POST" {
				var vm OVVM
				if xml.NewDecoder(r.Body).Decode(&vm) != nil {
					http.Error(w, "bad VM", 400)
					return
				}
				vm.ID = c.next()
				vm.Status = "down"
				c.VMs[vm.ID] = &vm
				w.WriteHeader(201)
				ovXML(w, "vm", vm)
				return
			}
		}
		if parts[0] == "vms" && len(parts) >= 2 {
			vm := c.VMs[parts[1]]
			if vm == nil {
				http.NotFound(w, r)
				return
			}
			if len(parts) == 2 {
				switch method {
				case "GET":
					ovXML(w, "vm", vm)
					return
				case "PUT":
					var update struct {
						Name string `xml:"name"`
					}
					xml.NewDecoder(r.Body).Decode(&update)
					vm.Name = update.Name
					ovXML(w, "vm", vm)
					return
				case "DELETE":
					if vm.ID == ImageVM {
						http.Error(w, "source protected", 403)
						return
					}
					delete(c.VMs, vm.ID)
					return
				}
			}
			if parts[2] == "hostdevices" {
				io.WriteString(w, "<host_devices/>")
				return
			}
			if parts[2] == "diskattachments" {
				if method == "GET" {
					ovXML(w, "disk_attachments", struct {
						Items []OVAttachment `xml:"disk_attachment"`
					}{vm.Disks})
					return
				}
				var disk OVAttachment
				xml.NewDecoder(r.Body).Decode(&disk)
				disk.ID = c.next()
				vm.Disks = append(vm.Disks, disk)
				ovXML(w, "disk_attachment", disk)
				return
			}
			if parts[2] == "nics" {
				if method == "GET" {
					type nic struct {
						Profile struct {
							ID string `xml:"id,attr"`
						} `xml:"vnic_profile"`
					}
					out := struct {
						Items []nic `xml:"nic"`
					}{}
					for _, id := range vm.Networks {
						n := nic{}
						n.Profile.ID = id
						out.Items = append(out.Items, n)
					}
					ovXML(w, "nics", out)
					return
				}
				var nic struct {
					Profile struct {
						ID string `xml:"id,attr"`
					} `xml:"vnic_profile"`
				}
				xml.NewDecoder(r.Body).Decode(&nic)
				vm.Networks = append(vm.Networks, nic.Profile.ID)
				io.WriteString(w, "<nic/>")
				return
			}
			if parts[2] == "start" {
				if c.FailStart {
					vm.Status = "unknown"
				} else {
					vm.Status = "up"
				}
				return
			}
			if parts[2] == "stop" {
				if vm.ID == ImageVM {
					http.Error(w, "source protected", 403)
					return
				}
				vm.Status = "down"
				return
			}
			if parts[2] == "backups" {
				if len(parts) == 3 && method == "POST" {
					var backup OVBackup
					xml.NewDecoder(r.Body).Decode(&backup)
					backup.Phase = "ready"
					c.Backups[backup.ID] = &backup
					w.WriteHeader(201)
					ovXML(w, "backup", backup)
					return
				}
				if len(parts) >= 4 {
					backup := c.Backups[parts[3]]
					if backup == nil {
						http.NotFound(w, r)
						return
					}
					if len(parts) == 4 {
						ovXML(w, "backup", backup)
						return
					}
					if parts[4] == "finalize" {
						backup.Phase = "succeeded"
						return
					}
					if parts[4] == "disks" {
						ovXML(w, "disks", struct {
							Items []struct {
								ID string `xml:"id,attr"`
							} `xml:"disk"`
						}{backup.Disks})
						return
					}
				}
			}
		}
		if path == "disks" {
			if method == "POST" {
				var disk OVDisk
				xml.NewDecoder(r.Body).Decode(&disk)
				disk.ID = c.next()
				disk.Status = "ok"
				disk.StorageType = "image"
				c.Disks[disk.ID] = &disk
				w.WriteHeader(201)
				ovXML(w, "disk", disk)
				return
			}
			list := struct {
				Items []OVDisk `xml:"disk"`
			}{}
			for _, disk := range c.Disks {
				list.Items = append(list.Items, *disk)
			}
			ovXML(w, "disks", list)
			return
		}
		if parts[0] == "disks" && len(parts) == 2 {
			disk := c.Disks[parts[1]]
			if disk == nil {
				http.NotFound(w, r)
				return
			}
			if method == "DELETE" {
				if disk.ID == RootVolume {
					http.Error(w, "source protected", 403)
					return
				}
				delete(c.Disks, disk.ID)
				return
			}
			ovXML(w, "disk", disk)
			return
		}
		if path == "imagetransfers" {
			if method == "POST" {
				var transfer OVTransfer
				xml.NewDecoder(r.Body).Decode(&transfer)
				transfer.ID = c.next()
				transfer.Phase = "transferring"
				transfer.URL = "https://" + r.Host + "/images/" + transfer.ID
				c.Transfers[transfer.ID] = &transfer
				w.WriteHeader(201)
				ovXML(w, "image_transfer", transfer)
				return
			}
			list := struct {
				Items []OVTransfer `xml:"image_transfer"`
			}{}
			for _, transfer := range c.Transfers {
				list.Items = append(list.Items, *transfer)
			}
			ovXML(w, "image_transfers", list)
			return
		}
		if parts[0] == "imagetransfers" && len(parts) >= 2 {
			transfer := c.Transfers[parts[1]]
			if transfer == nil {
				http.NotFound(w, r)
				return
			}
			if len(parts) == 3 {
				delete(c.Transfers, transfer.ID)
				return
			}
			ovXML(w, "image_transfer", transfer)
			return
		}
		if len(parts) == 2 && (parts[0] == "clusters" || parts[0] == "storagedomains" || parts[0] == "vnicprofiles") {
			io.WriteString(w, `<resource id="`+parts[1]+`"/>`)
			return
		}
		http.Error(w, "unimplemented oVirt fixture path", 404)
	})
}
func (c *OVirtCloud) Summary() (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	active := 0
	for _, b := range c.Backups {
		if b.Phase != "succeeded" {
			active++
		}
	}
	return len(c.VMs) - 1, len(c.Disks) - 1, len(c.Transfers) + active
}
