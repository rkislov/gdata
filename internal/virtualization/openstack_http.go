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
	"strings"
	"sync"
	"time"
)

func safeCloudURL(s string) bool {
	u, e := url.Parse(s)
	return e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func validateOpenStackProfile(p Profile) error {
	if p.BackupEnabled && NativeProvider(p.Kind) {
		return nil
	}
	if p.BackupEnabled && p.Kind == "ovirt" {
		if !filepath.IsAbs(p.StateDir) || !filepath.IsAbs(p.StagingDir) || p.StagingMaxBytes < 1 || p.StagingMaxBytes > 4<<40 {
			return errors.New("oVirt requires private state/staging directories and staging_max_bytes")
		}
		for _, dir := range []string{p.StateDir, p.StagingDir} {
			if s, e := os.Lstat(dir); e != nil || !s.IsDir() || s.Mode().Perm()&0077 != 0 {
				return errors.New("oVirt requires existing private state/staging directories")
			}
		}
		for _, origin := range p.TransferOrigins {
			u, e := url.Parse(origin)
			if e != nil || !safeCloudURL(origin) || u.Path != "" {
				return errors.New("oVirt transfer origins must be explicit HTTPS origins")
			}
		}
		if p.TimeoutSeconds < 60 || p.TimeoutSeconds > 43200 {
			return errors.New("oVirt timeout requires 60–43200 seconds")
		}
		return nil
	}
	if p.BackupEnabled {
		if p.Kind != "openstack" || !model.CloudUUID(p.ProjectID) || !filepath.IsAbs(p.StateDir) || !safeCloudURL(p.GlanceEndpoint) || !safeCloudURL(p.CinderEndpoint) || !safeCloudURL(p.NeutronEndpoint) {
			return errors.New("OpenStack backup requires project UUID, HTTPS Glance/Cinder/Neutron endpoints and absolute state_dir")
		}
		if p.TimeoutSeconds != 0 && (p.TimeoutSeconds < 60 || p.TimeoutSeconds > 43200) {
			return errors.New("OpenStack timeout must be 60–43200 seconds")
		}
		if st, e := os.Stat(p.StateDir); e == nil && (!st.IsDir() || st.Mode().Perm()&0077 != 0) {
			return errors.New("OpenStack state_dir must be a private directory (0700)")
		}
	}
	if p.AuthURL != "" && (!safeCloudURL(p.AuthURL) || !model.CloudUUID(p.ProjectID)) {
		return errors.New("OpenStack auth_url must be HTTPS")
	}
	return nil
}

type APIError struct {
	Service, Method string
	Status          int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s returned HTTP %d", e.Service, e.Method, e.Status)
}
func isMissing(e error) bool { var a *APIError; return errors.As(e, &a) && a.Status == 404 }

type OpenStack struct {
	base         *Client
	http         *http.Client
	mu           sync.Mutex
	token        string
	expires      time.Time
	PollInterval time.Duration
}

func NewOpenStack(p Profile) (*OpenStack, error) {
	if !p.BackupEnabled || p.Kind != "openstack" {
		return nil, errors.New("OpenStack backup profile is not enabled")
	}
	c, e := New(p)
	if e != nil {
		return nil, e
	}
	transport := c.http.Transport.(*http.Transport)
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	c.http.Timeout = 0
	return &OpenStack{base: c, http: c.http, token: c.credentials["token"], PollInterval: time.Second}, nil
}
func (c *OpenStack) Close() { c.base.Close() }
func (c *OpenStack) tokenFor(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base.profile.AuthURL == "" {
		return c.token, nil
	}
	if c.token != "" && time.Until(c.expires) > 2*time.Minute {
		return c.token, nil
	}
	body := map[string]any{"auth": map[string]any{"identity": map[string]any{"methods": []string{"application_credential"}, "application_credential": map[string]string{"id": c.base.credentials["application_credential_id"], "secret": c.base.credentials["application_credential_secret"]}}}}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.base.profile.AuthURL, "/")+"/auth/tokens", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, e := c.http.Do(req)
	if e != nil {
		return "", errors.New("Keystone connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		return "", &APIError{"Keystone", "POST", resp.StatusCode}
	}
	var out struct {
		Token struct {
			Expires time.Time `json:"expires_at"`
			Project struct {
				ID string `json:"id"`
			} `json:"project"`
		} `json:"token"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&out); e != nil {
		return "", errors.New("invalid Keystone response")
	}
	token := resp.Header.Get("X-Subject-Token")
	if token == "" || strings.ContainsAny(token, "\r\n") || out.Token.Project.ID != c.base.profile.ProjectID || !out.Token.Expires.After(time.Now()) {
		return "", errors.New("Keystone token project or expiry mismatch")
	}
	c.token = token
	c.expires = out.Token.Expires
	return token, nil
}
func (c *OpenStack) endpoint(service string) string {
	switch service {
	case "Nova":
		return c.base.profile.Endpoint
	case "Glance":
		return c.base.profile.GlanceEndpoint
	case "Cinder":
		return c.base.profile.CinderEndpoint
	case "Neutron":
		return c.base.profile.NeutronEndpoint
	}
	return ""
}
func (c *OpenStack) do(ctx context.Context, service, method, path string, body io.Reader, length int64) (*http.Response, error) {
	token, e := c.tokenFor(ctx)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.endpoint(service), "/")+path, body)
	if e != nil {
		return nil, e
	}
	req.Header.Set("X-Auth-Token", token)
	req.Header.Set("Accept", "application/json")
	if service == "Nova" {
		req.Header.Set("OpenStack-API-Version", "compute 2.1")
	}
	if service == "Cinder" {
		req.Header.Set("OpenStack-API-Version", "volume 3.0")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if length >= 0 {
		req.ContentLength = length
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, e := c.http.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s connection failed", service)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, &APIError{service, method, resp.StatusCode}
	}
	return resp, nil
}
func (c *OpenStack) json(ctx context.Context, service, method, path string, body, out any) (http.Header, error) {
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return nil, e
		}
		reader = bytes.NewReader(b)
	}
	resp, e := c.do(ctx, service, method, path, reader, -1)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if out != nil {
		if e = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); e != nil {
			if _, ok := out.(*optionalImageResponse); !ok || e != io.EOF {
				return nil, fmt.Errorf("invalid %s response", service)
			}
		}
	}
	return resp.Header, nil
}
func (c *OpenStack) wait(ctx context.Context, check func() (bool, error)) error {
	for {
		done, e := check()
		if e != nil || done {
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.PollInterval):
		}
	}
}
func cloudID(s string) (string, error) {
	if !model.CloudUUID(s) {
		return "", errors.New("invalid OpenStack UUID")
	}
	return strings.ToLower(s), nil
}
func imageID(h http.Header, body string) (string, error) {
	if body != "" {
		return cloudID(body)
	}
	u, e := url.Parse(h.Get("Location"))
	if e != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid Nova snapshot location")
	}
	return cloudID(filepath.Base(u.Path))
}

func (c *OpenStack) patchImage(ctx context.Context, id string, b []byte) (io.ReadCloser, error) {
	token, e := c.tokenFor(ctx)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, "PATCH", strings.TrimRight(c.endpoint("Glance"), "/")+"/images/"+id, bytes.NewReader(b))
	if e != nil {
		return nil, e
	}
	req.Header.Set("X-Auth-Token", token)
	req.Header.Set("Content-Type", "application/openstack-images-v2.1-json-patch")
	resp, e := c.http.Do(req)
	if e != nil {
		return nil, errors.New("Glance patch connection failed")
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, &APIError{"Glance", "PATCH", resp.StatusCode}
	}
	return resp.Body, nil
}
