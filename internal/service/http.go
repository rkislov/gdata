// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/telemetry"
)

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func Fail(w http.ResponseWriter, status int, e error) {
	JSON(w, status, map[string]string{"error": e.Error()})
}
func Decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if e := d.Decode(new(any)); e != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}
func Call(ctx context.Context, cl *http.Client, method, endpoint string, body any, out any, headers ...http.Header) error {
	var rd io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		rd = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, endpoint, rd)
	if e != nil {
		return e
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range headers {
		for k, values := range h {
			for _, v := range values {
				req.Header.Add(k, v)
			}
		}
	}
	resp, e := cl.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		limit := int64(32 << 20)
		if _, ok := out.(*model.Manifest); ok {
			limit = model.MaxManifestBytes
		}
		return json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(out)
	}
	return nil
}
func LocalStatus(c config.Config) map[string]any {
	return map[string]any{"id": c.ID, "role": c.Role, "installation": c.Installation, "version": model.Version, "status": "ready", "time": time.Now().UTC(), "host": telemetry.Snapshot()}
}
func Secure(c config.Config, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			cert := r.TLS.PeerCertificates[0]
			p, e := identity.Parse(cert)
			if e != nil || p.Installation != c.Installation || time.Now().After(cert.NotAfter) {
				Fail(w, 403, errors.New("invalid peer identity"))
				return
			}
			if c.Revoked != "" {
				b, e := os.ReadFile(c.Revoked)
				var revoked []string
				if e != nil || json.Unmarshal(b, &revoked) != nil {
					Fail(w, 503, errors.New("revocation list unavailable"))
					return
				}
				for _, serial := range revoked {
					if serial == cert.SerialNumber.String() {
						Fail(w, 403, errors.New("certificate revoked"))
						return
					}
				}
			}
		}
		h.ServeHTTP(w, r)
	})
}
func Serve(ctx context.Context, c config.Config, h http.Handler) error {
	t, e := identity.Config(c, true)
	if e != nil {
		return e
	}
	if c.Listen == "" {
		return errors.New("listen address required")
	}
	server := &http.Server{Addr: c.Listen, Handler: Secure(c, h), TLSConfig: t, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024}
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	errch := make(chan error, 1)
	go func() {
		slog.Info("service listening", "role", c.Role, "id", c.ID, "address", c.Listen)
		errch <- server.ListenAndServeTLS("", "")
	}()
	select {
	case e := <-errch:
		return e
	case <-ctx.Done():
		sd, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		e = server.Shutdown(sd)
		if e != nil {
			_ = server.Close()
		}
		return e
	}
}
func Role(r *http.Request, roles ...string) (identity.Peer, error) {
	p, e := identity.Request(r)
	if e != nil {
		return p, e
	}
	for _, s := range roles {
		if p.Role == s {
			return p, nil
		}
	}
	return p, errors.New("role is not authorized")
}
func Diagnostic(r *http.Request, c config.Config) error {
	p, e := identity.Request(r)
	if e != nil {
		return e
	}
	if p.Role == "admin" || p.Role == "auditor" || p.Role == "manager" || p.Role == c.Role && p.ID == c.ID {
		return nil
	}
	return errors.New("diagnostic access denied")
}
func ValidEndpoint(s string) bool {
	u, e := url.Parse(s)
	return e == nil && u.Scheme == "https" && u.User == nil && u.Host != "" && u.RawQuery == "" && u.Fragment == "" && u.Path == ""
}
func SameOrigin(r *http.Request) bool {
	o, e := url.Parse(r.Header.Get("Origin"))
	return e == nil && o.Scheme == "https" && o.Host == r.Host
}
func ClientIP(r *http.Request) string { host, _, _ := net.SplitHostPort(r.RemoteAddr); return host }

func decodeManifest(w http.ResponseWriter, r *http.Request, m *model.Manifest) error {
	limit := int64(16 << 20)
	if _, e := Role(r, "proxy"); e == nil {
		limit = model.MaxManifestBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(m); e != nil {
		return e
	}
	if e := d.Decode(new(any)); e != io.EOF {
		return errors.New("expected one manifest")
	}
	return nil
}
