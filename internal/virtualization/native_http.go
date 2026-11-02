// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var nativeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var numericID = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})$`)
var nativeDrive = regexp.MustCompile(`^(scsi[0-9]{1,2}|sata[0-9]|ide[0-9]|virtio[0-9]{1,2}|efidisk0|tpmstate0)$`)

func NativeProvider(kind string) bool { return kind == "proxmox" || kind == "opennebula" }
func VMProvider(kind string) bool {
	return kind == "openstack" || kind == "ovirt" || NativeProvider(kind)
}
func validateNativeProfile(p Profile) error {
	if !NativeProvider(p.Kind) || !p.BackupEnabled {
		return nil
	}
	for _, dir := range []string{p.StateDir, p.StagingDir} {
		s, e := os.Lstat(dir)
		if e != nil || !filepath.IsAbs(dir) || !s.IsDir() || s.Mode().Perm()&0077 != 0 {
			return errors.New("native VM adapters require existing private state_dir and staging_dir (0700)")
		}
	}
	if p.StagingMaxBytes < 1 || p.StagingMaxBytes > model.MaxCaptureBytes || p.TimeoutSeconds < 60 || p.TimeoutSeconds > 43200 {
		return errors.New("native VM adapter requires staging_max_bytes and timeout_seconds 60–43200")
	}
	if p.Kind == "proxmox" && p.DAVMountPath != "" {
		if !filepath.IsAbs(p.DAVMountPath) || p.DAVUser == "" || !nativeName.MatchString(p.BackupStorage) {
			return errors.New("PVE WebDAV requires absolute dav_mount_path, dav_user and directory backup_storage")
		}
	}
	if p.Kind == "proxmox" && p.DAVMountPath == "" {
		if !safeCloudURL(p.PBSEndpoint) || !nativeName.MatchString(p.PBSStore) || !nativeName.MatchString(p.BackupStorage) {
			return errors.New("Proxmox HTTPS backup requires PBS endpoint, datastore and PVE backup_storage")
		}
		if p.PBSNamespace != "" {
			for _, part := range strings.Split(p.PBSNamespace, "/") {
				if !nativeName.MatchString(part) {
					return errors.New("invalid PBS namespace")
				}
			}
		}
	}
	if p.Kind == "opennebula" {
		if !numericID.MatchString(p.BackupDatastore) || len(p.OneBEXEndpoints) == 0 {
			return errors.New("OpenNebula requires interactive backup_datastore and HTTPS OneBEX endpoint mapping")
		}
		for host, endpoint := range p.OneBEXEndpoints {
			if !nativeName.MatchString(host) || !safeCloudURL(endpoint) {
				return errors.New("invalid OneBEX HTTPS mapping")
			}
		}
		u, e := url.Parse(p.RestoreExportURL)
		if e != nil || !safeCloudURL(p.RestoreExportURL) || u.Path != "" || p.RestoreExportListen == "" || !filepath.IsAbs(p.RestoreCertFile) || !filepath.IsAbs(p.RestoreKeyFile) {
			return errors.New("OpenNebula restore requires explicit HTTPS export URL/listen and TLS certificate/key")
		}
		st, e := os.Lstat(p.RestoreKeyFile)
		if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
			return errors.New("restore HTTPS private key must be private")
		}
	}
	return nil
}

type NativeAdapter interface {
	Inspect(context.Context, string, string, string) (model.VMBackup, error)
	Backup(context.Context, *Journal, string, string, string, string, func(int64, int64)) (model.VMBackup, error)
	Restore(context.Context, *Journal, model.VMBackup, model.VMRestoreRequest, func(model.VMDisk) (io.ReadCloser, error)) (model.VMRestoreResult, error)
	Cleanup(context.Context, *Journal, func(string)) error
	Close()
}

func NewNative(p Profile) (NativeAdapter, error) {
	switch p.Kind {
	case "proxmox":
		return NewProxmox(p)
	case "opennebula":
		return NewOpenNebula(p)
	}
	return nil, errors.New("unsupported native VM provider")
}

type nativeHTTP struct {
	base         *Client
	PollInterval time.Duration
}

func newNativeHTTP(p Profile) (*nativeHTTP, error) {
	if !p.BackupEnabled {
		return nil, errors.New("backup profile is disabled")
	}
	c, e := New(p)
	if e != nil {
		return nil, e
	}
	c.http.Timeout = 0
	transport := c.http.Transport.(*http.Transport)
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	return &nativeHTTP{base: c, PollInterval: time.Second}, nil
}
func (c *nativeHTTP) Close() { c.base.Close() }
func (c *nativeHTTP) wait(ctx context.Context, check func() (bool, error)) error {
	for {
		done, e := check()
		if done || e != nil {
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.PollInterval):
		}
	}
}
func (c *nativeHTTP) request(ctx context.Context, service, method, path string, body io.Reader, contentType string, length int64) (*http.Response, error) {
	endpoint := c.base.profile.Endpoint
	auth := ""
	switch service {
	case "PVE":
		auth = "PVEAPIToken=" + c.base.credentials["token_id"] + "=" + c.base.credentials["secret"]
	case "PBS":
		endpoint = c.base.profile.PBSEndpoint
		auth = "PBSAPIToken=" + c.base.credentials["pbs_token_id"] + ":" + c.base.credentials["pbs_secret"]
	default:
		return nil, errors.New("unknown native service")
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(endpoint, "/")+path, body)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if length >= 0 {
		req.ContentLength = length
	}
	response, e := c.base.http.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s connection failed", service)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return nil, &APIError{service, method, response.StatusCode}
	}
	return response, nil
}
func (c *nativeHTTP) form(ctx context.Context, service, method, path string, values url.Values, out any) error {
	var reader io.Reader
	if values != nil {
		reader = strings.NewReader(values.Encode())
	}
	response, e := c.request(ctx, service, method, path, reader, "application/x-www-form-urlencoded", -1)
	if e != nil {
		return e
	}
	defer response.Body.Close()
	if out == nil {
		return nil
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if e = json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&envelope); e != nil || len(envelope.Data) == 0 {
		return errors.New("invalid native API envelope")
	}
	if e = json.Unmarshal(envelope.Data, out); e != nil {
		return errors.New("invalid native API data")
	}
	return nil
}
func (c *nativeHTTP) jsonResponse(ctx context.Context, method, endpoint string, body, out any) error {
	if !safeCloudURL(endpoint) {
		return errors.New("HTTPS export endpoint required")
	}
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	response, e := c.base.http.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("OneBEX HTTPS connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &APIError{"OneBEX", method, response.StatusCode}
	}
	if out != nil {
		if e = json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(out); e != nil {
			return errors.New("invalid OneBEX response")
		}
	}
	return nil
}
func (j *Journal) RecordOpaque(slot int, id string) error {
	if slot < 0 || slot >= len(j.Resources) || id == "" || len(id) > 512 || strings.ContainsAny(id, "\x00\r\n") {
		return errors.New("invalid native resource receipt")
	}
	j.Resources[slot].ID = id
	return j.Save()
}
func parseAPIID(value json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(value, &s) != nil {
		var n json.Number
		if json.Unmarshal(value, &n) != nil {
			return "", errors.New("invalid API identifier")
		}
		s = n.String()
	}
	if !numericID.MatchString(s) {
		return "", errors.New("numeric platform identifier required")
	}
	return s, nil
}
func NativeSource(source string) (string, string, error) {
	u, e := url.Parse(source)
	if e != nil || !NativeProvider(u.Scheme) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !nativeName.MatchString(u.Host) || !numericID.MatchString(strings.TrimPrefix(u.Path, "/")) {
		return "", "", errors.New("native source must be PROVIDER://PROFILE/NUMERIC_VM_ID")
	}
	return u.Host, strings.TrimPrefix(u.Path, "/"), nil
}

type DAVStore interface {
	Create(context.Context, *Journal, bool, string, int64) (model.DAVSession, error)
	Seal(context.Context, *Journal) (model.VMArchive, error)
	Open(context.Context, *Journal, string) (io.ReadCloser, error)
	Close(context.Context, *Journal) error
}
