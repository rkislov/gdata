// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"flag"
	"gdata/srk/internal/model"
	"gdata/srk/internal/service"
	"net/http"
	"net/url"
	"os"
)

func certificates(ctx context.Context, cl *http.Client, base string, args []string, machine bool) error {
	if len(args) < 2 {
		return usageError("certificates list|issue|revoke USER")
	}
	fs := flag.NewFlagSet("certificates", flag.ContinueOnError)
	csr := fs.String("csr", "", "CSR PEM file")
	out := fs.String("out", "", "new certificate output file")
	version := fs.Int("version", -1, "current certificate generation")
	days := fs.Int("days", 30, "lifetime 1–90 days")
	overlap := fs.Int("overlap-hours", 0, "overlap 0–24 hours")
	serial := fs.String("serial", "", "serial to revoke")
	if e := fs.Parse(args[2:]); e != nil {
		return e
	}
	path := base + "/v1/users/" + url.PathEscape(args[1]) + "/certificates"
	switch args[0] {
	case "list":
		var v any
		if e := service.Call(ctx, cl, "GET", path, nil, &v); e != nil {
			return e
		}
		return printValue(v, machine)
	case "get":
		if *serial == "" || *out == "" {
			return usageError("--serial and --out required")
		}
		var state model.CertificateState
		if e := service.Call(ctx, cl, "GET", path, nil, &state); e != nil {
			return e
		}
		for _, c := range state.Certificates {
			if c.Serial == *serial {
				f, e := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if e != nil {
					return e
				}
				defer f.Close()
				_, e = f.WriteString(c.Certificate)
				return e
			}
		}
		return errors.New("certificate not found")
	case "issue":
		if *csr == "" || *out == "" || *version < 0 {
			return usageError("--csr --out --version required")
		}
		b, e := os.ReadFile(*csr)
		if e != nil {
			return e
		}
		// Reserve output before rotation so an existing file cannot be overwritten.
		f, e := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		defer f.Close()
		var result struct {
			Certificate string `json:"certificate"`
			Version     int    `json:"version"`
		}
		e = service.Call(ctx, cl, "POST", path, map[string]any{"csr": string(b), "version": *version, "days": *days, "overlap_hours": *overlap}, &result)
		if e != nil {
			os.Remove(*out)
			return e
		}
		if _, e = f.WriteString(result.Certificate); e != nil {
			return e
		}
		if e = f.Sync(); e != nil {
			return e
		}
		return printValue(map[string]any{"certificate_file": *out, "version": result.Version}, machine)
	case "revoke":
		if *version < 0 || *serial == "" {
			return usageError("--serial --version required")
		}
		var v any
		if e := service.Call(ctx, cl, "POST", path+"/revoke", map[string]any{"serial": *serial, "version": *version}, &v); e != nil {
			return e
		}
		return printValue(v, machine)
	default:
		return usageError("certificates list|issue|revoke")
	}
}
