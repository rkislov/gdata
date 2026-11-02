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
	"strconv"
	"strings"
	"sync"
)

type ONEImage struct {
	ID, Name, DS, Run, Proxy string
	State                    int
	Data                     []byte
}
type ONEVM struct {
	ID, Name, Run, Proxy, Template string
	State, LCM                     int
}
type OpenNebulaCloud struct {
	mu           sync.Mutex
	Images       map[string]*ONEImage
	VMs          map[string]*ONEVM
	Data         []byte
	ExportClient *http.Client
	BackupID     string
	Active       bool
	Counter      int
	FailStart    bool
	BadRange     bool
}

func NewOpenNebula() *OpenNebulaCloud {
	return &OpenNebulaCloud{Images: map[string]*ONEImage{}, VMs: map[string]*ONEVM{}, Data: bytes.Repeat([]byte("SRKONE00"), 1<<20), Counter: 500}
}
func oneXML(s string) string { var b strings.Builder; xml.EscapeText(&b, []byte(s)); return b.String() }
func oneTag(s, tag string) string {
	start := "<" + tag + ">"
	end := "</" + tag + ">"
	_, after, ok := strings.Cut(s, start)
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(after, end)
	var x struct {
		Text string `xml:",chardata"`
	}
	if xml.Unmarshal([]byte("<x>"+v+"</x>"), &x) == nil {
		return x.Text
	}
	return v
}
func (c *OpenNebulaCloud) source() string {
	backup := ""
	if c.BackupID != "" {
		backup = "<BACKUP_IDS><ID>" + c.BackupID + "</ID></BACKUP_IDS>"
	}
	lcm := 3
	if c.Active {
		lcm = 69
	}
	return fmt.Sprintf(`<VM><ID>100</ID><NAME>one-source</NAME><STATE>3</STATE><LCM_STATE>%d</LCM_STATE><TEMPLATE><CPU>2</CPU><VCPU>2</VCPU><MEMORY>1024</MEMORY><OS><ARCH>x86_64</ARCH><BOOT>disk0</BOOT></OS><DISK><DISK_ID>0</DISK_ID><IMAGE_ID>42</IMAGE_ID><SIZE>8</SIZE><TARGET>vda</TARGET><DEV_PREFIX>vd</DEV_PREFIX></DISK><NIC><NETWORK_ID>1</NETWORK_ID><MODEL>virtio</MODEL></NIC></TEMPLATE><BACKUPS><BACKUP_CONFIG><MODE>FULL</MODE><INTERACTIVE>YES</INTERACTIVE><KEEP_LAST>0</KEEP_LAST><FS_FREEZE>NONE</FS_FREEZE></BACKUP_CONFIG>%s</BACKUPS><HISTORY_RECORDS><HISTORY><HOSTNAME>hyp1</HOSTNAME><CID>0</CID></HISTORY></HISTORY_RECORDS></VM>`, lcm, backup)
}
func (c *OpenNebulaCloud) imageXML(v *ONEImage) string {
	return fmt.Sprintf(`<IMAGE><ID>%s</ID><NAME>%s</NAME><STATE>%d</STATE><DATASTORE_ID>%s</DATASTORE_ID><TEMPLATE><SRK_RUN>%s</SRK_RUN><SRK_PROXY>%s</SRK_PROXY></TEMPLATE></IMAGE>`, v.ID, oneXML(v.Name), v.State, v.DS, v.Run, v.Proxy)
}
func (c *OpenNebulaCloud) vmXML(v *ONEVM) string {
	return fmt.Sprintf(`<VM><ID>%s</ID><NAME>%s</NAME><STATE>%d</STATE><LCM_STATE>%d</LCM_STATE><TEMPLATE>%s</TEMPLATE><USER_TEMPLATE><SRK_RUN>%s</SRK_RUN><SRK_PROXY>%s</SRK_PROXY></USER_TEMPLATE></VM>`, v.ID, oneXML(v.Name), v.State, v.LCM, v.Template, v.Run, v.Proxy)
}
func (c *OpenNebulaCloud) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/bex/") {
			c.bex(w, r)
			return
		}
		var req struct {
			Method string `xml:"methodName"`
			Params []struct {
				String string `xml:"value>string"`
				Int    string `xml:"value>int"`
				Bool   string `xml:"value>boolean"`
			} `xml:"params>param"`
		}
		if xml.NewDecoder(r.Body).Decode(&req) != nil || len(req.Params) == 0 || req.Params[0].String != "oneadmin:fixture-one-secret" {
			http.Error(w, "session required", 403)
			return
		}
		args := []string{}
		for _, p := range req.Params[1:] {
			v := p.String
			if p.Int != "" {
				v = p.Int
			}
			if p.Bool != "" {
				v = p.Bool
			}
			args = append(args, v)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		reply := func(v string, ok bool) {
			flag := "1"
			if !ok {
				flag = "0"
			}
			fmt.Fprintf(w, `<methodResponse><params><param><value><array><data><value><boolean>%s</boolean></value><value><string>%s</string></value><value><int>0</int></value></data></array></value></param></params></methodResponse>`, flag, oneXML(v))
		}
		arg := func(i int) string {
			if i < len(args) {
				return args[i]
			}
			return ""
		}
		switch req.Method {
		case "one.vm.info":
			if arg(0) == "100" {
				reply(c.source(), true)
			} else if v := c.VMs[arg(0)]; v != nil {
				reply(c.vmXML(v), true)
			} else {
				reply("missing VM", false)
			}
		case "one.vmpool.info":
			raw := "<VM_POOL>" + c.source()
			for _, v := range c.VMs {
				raw += c.vmXML(v)
			}
			reply(raw+"</VM_POOL>", true)
		case "one.datastore.info":
			driver := "fs"
			verify := "YES"
			if arg(0) == "100" {
				driver = "interactive"
			}
			reply(`<DATASTORE><ID>`+arg(0)+`</ID><TEMPLATE><DS_MAD>`+driver+`</DS_MAD><SRK_HTTPS_VERIFY>`+verify+`</SRK_HTTPS_VERIFY></TEMPLATE></DATASTORE>`, true)
		case "one.cluster.info", "one.vn.info":
			reply("<RESOURCE><ID>"+arg(0)+"</ID></RESOURCE>", true)
		case "one.vm.backup":
			if arg(0) != "100" || arg(1) != "100" || arg(2) != "1" || c.Active {
				reply("invalid backup", false)
				return
			}
			c.Counter++
			id := strconv.Itoa(c.Counter)
			c.BackupID = id
			c.Images[id] = &ONEImage{ID: id, Name: "backup-metadata", DS: "100", State: 1}
			c.Active = true
			reply("", true)
		case "one.image.info":
			if v := c.Images[arg(0)]; v != nil {
				reply(c.imageXML(v), true)
			} else {
				reply("missing image", false)
			}
		case "one.imagepool.info":
			raw := "<IMAGE_POOL>"
			for _, v := range c.Images {
				raw += c.imageXML(v)
			}
			reply(raw+"</IMAGE_POOL>", true)
		case "one.image.update":
			if v := c.Images[arg(0)]; v != nil {
				v.Run = oneTag(arg(1), "SRK_RUN")
				v.Proxy = oneTag(arg(1), "SRK_PROXY")
				reply("", true)
			} else {
				reply("missing", false)
			}
		case "one.image.delete":
			v := c.Images[arg(0)]
			if v == nil || v.Run == "" {
				reply("source protected", false)
				return
			}
			delete(c.Images, arg(0))
			if c.BackupID == arg(0) {
				c.BackupID = ""
			}
			reply("", true)
		case "one.image.allocate":
			template := arg(0)
			location := oneTag(template, "PATH")
			if !strings.HasPrefix(location, "https://") {
				reply("HTTPS required", false)
				return
			}
			c.Counter++
			id := strconv.Itoa(c.Counter)
			v := &ONEImage{ID: id, Name: oneTag(template, "NAME"), DS: arg(1), Run: oneTag(template, "SRK_RUN"), Proxy: oneTag(template, "SRK_PROXY"), State: 2}
			c.Images[id] = v
			go func() {
				res, err := c.ExportClient.Get(location)
				var data []byte
				if err == nil {
					data, err = io.ReadAll(res.Body)
					res.Body.Close()
					if res.StatusCode != 200 {
						err = fmt.Errorf("download status %d", res.StatusCode)
					}
				}
				c.mu.Lock()
				defer c.mu.Unlock()
				if err != nil {
					v.State = 5
				} else {
					v.Data = data
					v.State = 1
				}
			}()
			reply(id, true)
		case "one.vm.allocate":
			template := strings.TrimSuffix(strings.TrimPrefix(arg(0), "<VM>"), "</VM>")
			if arg(1) != "1" {
				reply("must allocate held", false)
				return
			}
			c.Counter++
			id := strconv.Itoa(c.Counter)
			c.VMs[id] = &ONEVM{ID: id, Name: oneTag(template, "NAME"), Run: oneTag(template, "SRK_RUN"), Proxy: oneTag(template, "SRK_PROXY"), State: 2, Template: template}
			reply(id, true)
		case "one.vm.rename":
			if v := c.VMs[arg(0)]; v != nil {
				v.Name = arg(1)
				reply("", true)
			} else {
				reply("source protected", false)
			}
		case "one.vm.action":
			v := c.VMs[arg(1)]
			if v == nil {
				reply("source protected", false)
				return
			}
			switch arg(0) {
			case "release":
				if c.FailStart {
					v.State = 7
				} else {
					v.State = 3
					v.LCM = 3
				}
			case "terminate-hard":
				v.State = 6
				v.LCM = 0
			default:
				reply("unsupported action", false)
				return
			}
			reply("", true)
		default:
			reply("unsupported fixture method "+req.Method, false)
		}
	})
}
func (c *OpenNebulaCloud) bex(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/bex")
	transfer := "one-100-0-fixture"
	switch {
	case p == "/status":
		status := "idle"
		trs := []map[string]any{}
		if c.Active {
			status = "executing"
			trs = append(trs, map[string]any{"TRANSFER_ID": transfer, "DISK_ID": 0, "STATUS": "ready", "RC": true})
		}
		json.NewEncoder(w).Encode(map[string]any{"VM_ID": 100, "STATUS": status, "TRANSFERS": trs})
	case p == "/transfers/"+transfer+"/info":
		json.NewEncoder(w).Encode(map[string]any{"TRANSFER_ID": transfer, "SIZE": 8, "FORMAT": "qcow2"})
	case p == "/images/"+transfer:
		var start, end int
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if start < 0 || end < start || end >= len(c.Data) {
			http.Error(w, "range", 416)
			return
		}
		value := fmt.Sprintf("bytes %d-%d/*", start, end)
		if c.BadRange {
			value = "bytes 0-0/*"
		}
		w.Header().Set("Content-Range", value)
		w.WriteHeader(206)
		w.Write(c.Data[start : end+1])
	case p == "/transfer/"+transfer+"/finalize":
		json.NewEncoder(w).Encode(map[string]bool{"RC": true})
	case p == "/vms/100/finish" || p == "/vms/100/cancel":
		c.Active = false
		json.NewEncoder(w).Encode(map[string]bool{"RC": true})
	default:
		http.NotFound(w, r)
	}
}
func (c *OpenNebulaCloud) Summary() (vms, images int, source bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range c.VMs {
		if v.State != 6 {
			vms++
		}
	}
	return vms, len(c.Images), !c.Active
}
func (c *OpenNebulaCloud) RestoredData() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := [][]byte{}
	for _, v := range c.Images {
		if v.DS != "100" {
			out = append(out, append([]byte(nil), v.Data...))
		}
	}
	return out
}
