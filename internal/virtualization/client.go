// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Profile struct {
	DAVMountPath        string            `json:"dav_mount_path,omitempty"`
	DAVUser             string            `json:"dav_user,omitempty"`
	PBSEndpoint         string            `json:"pbs_endpoint,omitempty"`
	PBSStore            string            `json:"pbs_store,omitempty"`
	PBSNamespace        string            `json:"pbs_namespace,omitempty"`
	BackupStorage       string            `json:"backup_storage,omitempty"`
	BackupDatastore     string            `json:"backup_datastore,omitempty"`
	OneBEXEndpoints     map[string]string `json:"onebex_endpoints,omitempty"`
	RestoreExportURL    string            `json:"restore_export_url,omitempty"`
	RestoreExportListen string            `json:"restore_export_listen,omitempty"`
	RestoreCertFile     string            `json:"restore_cert_file,omitempty"`
	RestoreKeyFile      string            `json:"restore_key_file,omitempty"`
	ClientCertFile      string            `json:"client_cert_file,omitempty"`
	ClientKeyFile       string            `json:"client_key_file,omitempty"`

	StagingDir      string   `json:"staging_dir,omitempty"`
	StagingMaxBytes int64    `json:"staging_max_bytes,omitempty"`
	TransferOrigins []string `json:"transfer_origins,omitempty"`
	BackupEnabled   bool     `json:"backup_enabled,omitempty"`
	GlanceEndpoint  string   `json:"glance_endpoint,omitempty"`
	CinderEndpoint  string   `json:"cinder_endpoint,omitempty"`
	NeutronEndpoint string   `json:"neutron_endpoint,omitempty"`
	AuthURL         string   `json:"auth_url,omitempty"`
	ProjectID       string   `json:"project_id,omitempty"`
	StateDir        string   `json:"state_dir,omitempty"`
	TimeoutSeconds  int      `json:"timeout_seconds,omitempty"`
	Kind            string   `json:"kind"`
	Endpoint        string   `json:"endpoint"`
	CAFile          string   `json:"ca_file,omitempty"`
	CredentialFile  string   `json:"credential_file"`
}
type VM struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Node   string `json:"node,omitempty"`
}
type BackupPlan struct {
	Provider   string   `json:"provider"`
	VMID       string   `json:"vm_id"`
	API        []string `json:"api"`
	Required   []string `json:"required"`
	Executable bool     `json:"executable"`
	Reason     string   `json:"reason"`
}
type Provider interface {
	Inventory(context.Context) ([]VM, error)
	Plan(VM) BackupPlan
}
type Client struct {
	profile     Profile
	http        *http.Client
	auth        *OpenStack
	credentials map[string]string
}

func Validate(p Profile) error {
	if p.ClientCertFile != "" || p.ClientKeyFile != "" {
		st, e := os.Lstat(p.ClientKeyFile)
		if !filepath.IsAbs(p.ClientCertFile) || !filepath.IsAbs(p.ClientKeyFile) || e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
			return errors.New("mTLS certificate/key require absolute paths and private regular key file")
		}
	}

	switch p.Kind {
	case "openstack", "ovirt", "proxmox", "opennebula":
	default:
		return errors.New("unknown virtualization provider")
	}
	u, e := url.Parse(p.Endpoint)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("virtualization endpoint must be HTTPS without credentials or query")
	}
	if e := validateNativeProfile(p); e != nil {
		return e
	}
	if e := validateOpenStackProfile(p); e != nil {
		return e
	}
	if p.CAFile != "" && !filepath.IsAbs(p.CAFile) {
		return errors.New("virtualization CA requires an absolute path")
	}
	if !filepath.IsAbs(p.CredentialFile) {
		return errors.New("virtualization credentials require an absolute private file")
	}
	info, e := os.Lstat(p.CredentialFile)
	if e != nil {
		return e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("virtualization credential file must be 0600 or 0400")
	}
	return nil
}
func New(p Profile) (*Client, error) {
	if e := Validate(p); e != nil {
		return nil, e
	}
	data, e := os.ReadFile(p.CredentialFile)
	if e != nil {
		return nil, e
	}
	credentials := map[string]string{}
	if e = json.Unmarshal(data, &credentials); e != nil {
		return nil, errors.New("invalid virtualization credential JSON")
	}
	key := "token"
	if p.Kind == "opennebula" {
		key = "session"
	}
	if p.Kind == "proxmox" {
		key = "secret"
		if credentials["token_id"] == "" || strings.ContainsAny(credentials["token_id"], "\r\n") {
			return nil, errors.New("Proxmox token_id required")
		}
	}
	if !(p.Kind == "openstack" && p.AuthURL != "" && credentials["application_credential_id"] != "" && credentials["application_credential_secret"] != "") && (credentials[key] == "" || strings.ContainsAny(credentials[key], "\r\n")) {
		return nil, errors.New("virtualization credential is missing or invalid")
	}
	roots, e := x509.SystemCertPool()
	if e != nil {
		roots = x509.NewCertPool()
	}
	if p.CAFile != "" {
		b, e := os.ReadFile(p.CAFile)
		if e != nil {
			return nil, e
		}
		if !roots.AppendCertsFromPEM(b) {
			return nil, errors.New("invalid virtualization CA")
		}
	}
	var certs []tls.Certificate
	if p.ClientCertFile != "" || p.ClientKeyFile != "" {
		pair, e := tls.LoadX509KeyPair(p.ClientCertFile, p.ClientKeyFile)
		if e != nil {
			return nil, errors.New("invalid cloud mTLS keypair")
		}
		certs = []tls.Certificate{pair}
	}
	client := &Client{profile: p, credentials: credentials, http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: certs}}, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("virtualization redirects are forbidden")
	}}}
	if p.Kind == "openstack" && p.AuthURL != "" {
		client.auth = &OpenStack{base: client, http: client.http, token: credentials["token"]}
	}
	return client, nil
}
func (c *Client) Close() { c.http.CloseIdleConnections() }
func (c *Client) request(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	endpoint := strings.TrimRight(c.profile.Endpoint, "/") + path
	req, e := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Accept", "application/json")
	switch c.profile.Kind {
	case "openstack":
		token := c.credentials["token"]
		if c.auth != nil {
			token, e = c.auth.tokenFor(ctx)
			if e != nil {
				return nil, e
			}
		}
		req.Header.Set("X-Auth-Token", token)
	case "ovirt":
		req.Header.Set("Authorization", "Bearer "+c.credentials["token"])
	case "proxmox":
		req.Header.Set("Authorization", "PVEAPIToken="+c.credentials["token_id"]+"="+c.credentials["secret"])
	case "opennebula":
		req.Header.Set("Content-Type", "text/xml")
	}
	r, e := c.http.Do(req)
	if e != nil {
		return nil, errors.New("virtualization API connection failed")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return nil, fmt.Errorf("virtualization API returned HTTP %d", r.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, 16<<20+1))
	if e != nil {
		return nil, e
	}
	if len(b) > 16<<20 {
		return nil, errors.New("virtualization response too large")
	}
	return b, nil
}
func (c *Client) Inventory(ctx context.Context) ([]VM, error) {
	out := []VM{}
	switch c.profile.Kind {
	case "openstack":
		marker := ""
		for page := 0; page < 100; page++ {
			path := "/servers/detail?limit=100"
			if marker != "" {
				path += "&marker=" + url.QueryEscape(marker)
			}
			b, e := c.request(ctx, "GET", path, nil)
			if e != nil {
				return nil, e
			}
			var v struct {
				Servers []struct{ ID, Name, Status string } `json:"servers"`
			}
			if e = json.Unmarshal(b, &v); e != nil {
				return nil, e
			}
			if v.Servers == nil {
				return nil, errors.New("Nova response has no servers collection")
			}
			for _, s := range v.Servers {
				out = append(out, VM{ID: s.ID, Name: s.Name, Status: s.Status})
			}
			if len(v.Servers) < 100 {
				return out, nil
			}
			next := v.Servers[len(v.Servers)-1].ID
			if next == marker {
				return nil, errors.New("OpenStack pagination did not advance")
			}
			marker = next
		}
		return nil, errors.New("OpenStack inventory exceeds page limit")
	case "ovirt":
		b, e := c.request(ctx, "GET", "/vms?max=10000", nil)
		if e != nil {
			return nil, e
		}
		var v struct {
			VMs []struct{ ID, Name, Status string } `json:"vm"`
		}
		if e = json.Unmarshal(b, &v); e != nil {
			return nil, e
		}
		if v.VMs == nil {
			return nil, errors.New("oVirt response has no VM collection")
		}
		if len(v.VMs) >= 10000 {
			return nil, errors.New("oVirt inventory exceeds limit")
		}
		for _, s := range v.VMs {
			out = append(out, VM{ID: s.ID, Name: s.Name, Status: s.Status})
		}
	case "proxmox":
		b, e := c.request(ctx, "GET", "/cluster/resources?type=vm", nil)
		if e != nil {
			return nil, e
		}
		var v struct {
			Data []struct {
				ID                       int `json:"vmid"`
				Name, Status, Node, Type string
			} `json:"data"`
		}
		if e = json.Unmarshal(b, &v); e != nil {
			return nil, e
		}
		if v.Data == nil {
			return nil, errors.New("Proxmox response has no data collection")
		}
		for _, s := range v.Data {
			if s.Type == "qemu" {
				out = append(out, VM{ID: strconv.Itoa(s.ID), Name: s.Name, Status: s.Status, Node: s.Node})
			}
		}
	case "opennebula":
		var escaped bytes.Buffer
		_ = xml.EscapeText(&escaped, []byte(c.credentials["session"]))
		body := []byte(`<?xml version="1.0"?><methodCall><methodName>one.vmpool.info</methodName><params><param><value><string>` + escaped.String() + `</string></value></param><param><value><int>-1</int></value></param><param><value><int>-1</int></value></param><param><value><int>-1</int></value></param><param><value><int>-1</int></value></param><param><value><string></string></value></param></params></methodCall>`)
		b, e := c.request(ctx, "POST", "", body)
		if e != nil {
			return nil, e
		}
		var envelope struct {
			Values []struct {
				Boolean string `xml:"boolean"`
				String  string `xml:"string"`
			} `xml:"params>param>value>array>data>value"`
		}
		if e = xml.Unmarshal(b, &envelope); e != nil {
			return nil, e
		}
		if len(envelope.Values) < 2 || envelope.Values[0].Boolean != "1" {
			return nil, errors.New("OpenNebula XML-RPC failed")
		}
		var actual struct {
			VMs []struct {
				ID    string `xml:"ID"`
				Name  string `xml:"NAME"`
				State string `xml:"STATE"`
			} `xml:"VM"`
		}
		if e = xml.Unmarshal([]byte(envelope.Values[1].String), &actual); e != nil {
			return nil, e
		}
		for _, s := range actual.VMs {
			out = append(out, VM{ID: s.ID, Name: s.Name, Status: s.State})
		}
	}
	if len(out) > 10000 {
		return nil, errors.New("virtualization inventory exceeds limit")
	}
	return out, nil
}
func (c *Client) Plan(vm VM) BackupPlan {
	apis := map[string][]string{"openstack": {"Nova createImage; Cinder snapshots; Glance image export"}, "ovirt": {"POST /vms/{id}/backups; image transfers; finalize backup"}, "proxmox": {"POST /nodes/{node}/vzdump; task status; archive download"}, "opennebula": {"one.vm.backup / datastore export; snapshot capability discovery"}}
	reason := "API inventory is implemented; no SRK disk backup or restore has been performed"
	if c.profile.Kind == "proxmox" {
		reason = "Proxmox inventory is available. Enable backup_enabled and configure SRK HTTPS/WebDAV transport to execute native vzdump and restore."
	}
	if c.profile.Kind == "opennebula" {
		reason = "OpenNebula inventory is available. Enable backup_enabled with OpenNebula 7.4 interactive backup, trusted HTTPS OneBEX endpoints and HTTPS restore export."
	}
	return BackupPlan{Provider: c.profile.Kind, VMID: vm.ID, API: apis[c.profile.Kind], Required: []string{"guest consistency policy", "backend capability discovery", "disk transfer to SRK", "verified restore", "cleanup and retry journal"}, Executable: false, Reason: reason}
}
