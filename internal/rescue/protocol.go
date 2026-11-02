// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package rescue

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gdata/srk/internal/model"
)

type CapturePermission struct {
	RunID         string `json:"run_id"`
	JobID         string `json:"job_id"`
	Name          string `json:"name"`
	MediaID       string `json:"media_id"`
	RetentionDays int    `json:"retention_days"`
	MaxBytes      int64  `json:"max_bytes"`
}
type Scope struct {
	Capture   *CapturePermission `json:"capture,omitempty"`
	ID        string             `json:"id"`
	RunIDs    []string           `json:"run_ids"`
	ExpiresAt time.Time          `json:"expires_at"`
}
type Code struct {
	URL               string `json:"url"`
	CertificateSHA256 string `json:"certificate_sha256"`
	Secret            string `json:"secret"`
}
type Pairing struct {
	Token string `json:"token"`
	Scope Scope  `json:"scope"`
}
type Backup struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"created_at"`
	Consistency string    `json:"consistency"`
}

func Digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func Encode(c Code) string {
	b, _ := json.Marshal(c)
	return "SRK1." + base64.RawURLEncoding.EncodeToString(b)
}
func Parse(s string) (Code, error) {
	var c Code
	if len(s) > 8192 || !strings.HasPrefix(s, "SRK1.") {
		return c, errors.New("неверный код SRK1")
	}
	b, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, "SRK1."))
	if e != nil {
		return c, e
	}
	if e = json.Unmarshal(b, &c); e != nil {
		return c, e
	}
	u, e := url.Parse(c.URL)
	pin, pe := hex.DecodeString(c.CertificateSHA256)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || pe != nil || len(pin) != 32 || len(c.Secret) != 64 {
		return c, errors.New("неверные параметры подключения")
	}
	return c, nil
}

type HTTPError struct {
	StatusCode int
	Status     string
}

func (e *HTTPError) Error() string { return "сервер отклонил запрос: " + e.Status }

type Client struct {
	HTTP    *http.Client
	Code    Code
	Pairing Pairing
}

func NewClient(c Code) (*Client, error) {
	parsed, e := Parse(Encode(c))
	if e != nil {
		return nil, e
	}
	c = parsed
	u, _ := url.Parse(c.URL)
	// The administrator-authenticated offline code pins the exact server certificate.
	// Chain verification is replaced by this pin; hostname and lifetime remain checked.
	tc := &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, VerifyConnection: func(s tls.ConnectionState) error {
		if len(s.PeerCertificates) == 0 {
			return errors.New("server certificate missing")
		}
		cert := s.PeerCertificates[0]
		h := sha256.Sum256(cert.Raw)
		if hex.EncodeToString(h[:]) != c.CertificateSHA256 {
			return errors.New("отпечаток сервера не совпадает")
		}
		if time.Now().Before(cert.NotBefore) || time.Now().After(cert.NotAfter) {
			return errors.New("server certificate expired or not yet valid")
		}
		if e := cert.VerifyHostname(u.Hostname()); e != nil {
			return e
		}
		for _, usage := range cert.ExtKeyUsage {
			if usage == x509.ExtKeyUsageServerAuth || usage == x509.ExtKeyUsageAny {
				return nil
			}
		}
		return errors.New("server authentication usage required")
	}}
	return &Client{Code: c, HTTP: &http.Client{Transport: &http.Transport{TLSClientConfig: tc, ResponseHeaderTimeout: 30 * time.Second}, Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect forbidden") }}}, nil
}
func (c *Client) Request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, e := http.NewRequestWithContext(ctx, method, c.Code.URL+path, body)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	if method == "PUT" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	if c.Pairing.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Pairing.Token)
	}
	r, e := c.HTTP.Do(req)
	if e != nil {
		return nil, e
	}
	if r.StatusCode >= 300 {
		r.Body.Close()
		return nil, &HTTPError{StatusCode: r.StatusCode, Status: r.Status}
	}
	return r, nil
}
func (c *Client) JSON(ctx context.Context, method, path string, body io.Reader, out any) error {
	r, e := c.Request(ctx, method, path, body)
	if e != nil {
		return e
	}
	defer r.Body.Close()
	limit := int64(32 << 20)
	if _, ok := out.(*model.Manifest); ok {
		limit = model.MaxManifestBytes
	}
	return json.NewDecoder(io.LimitReader(r.Body, limit)).Decode(out)
}
func (c *Client) Pair(ctx context.Context) error {
	b, _ := json.Marshal(map[string]string{"code": c.Code.Secret})
	return c.JSON(ctx, "POST", "/v1/rescue/pair", strings.NewReader(string(b)), &c.Pairing)
}
func (c *Client) Backups(ctx context.Context) ([]Backup, error) {
	var b []Backup
	e := c.JSON(ctx, "GET", "/v1/rescue/backups", nil, &b)
	return b, e
}
func (c *Client) Manifest(ctx context.Context, id string) (model.Manifest, error) {
	var m model.Manifest
	e := c.JSON(ctx, "GET", "/v1/rescue/backups/"+url.PathEscape(id), nil, &m)
	return m, e
}
func (c *Client) Chunk(ctx context.Context, id, hash string) ([]byte, error) {
	r, e := c.Request(ctx, "GET", "/v1/rescue/backups/"+url.PathEscape(id)+"/chunks/"+url.PathEscape(hash), nil)
	if e != nil {
		return nil, e
	}
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
}

type CaptureAccess struct {
	Run               model.Run `json:"run"`
	MediaURL          string    `json:"media_url,omitempty"`
	CertificateSHA256 string    `json:"certificate_sha256,omitempty"`
	Token             string    `json:"token,omitempty"`
}
