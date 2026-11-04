// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"flag"
	"gdata/srk/internal/service"
	"net/http"
	"os"
)

func adoptServiceCertificate(ctx context.Context, cl *http.Client, base string, args []string, machine bool) error {
	if len(args) == 0 || args[0] != "adopt" {
		return usageError("service-certificates adopt --file CERT --expected-serial SERIAL --reason TEXT")
	}
	fs := flag.NewFlagSet("service-certificates adopt", flag.ContinueOnError)
	file := fs.String("file", "", "public offline certificate")
	serial := fs.String("expected-serial", "", "previous registered serial, empty for first enrollment")
	reason := fs.String("reason", "", "administrative recovery reason")
	if e := fs.Parse(args[1:]); e != nil {
		return e
	}
	if *file == "" || fs.NArg() != 0 {
		return usageError("public --file required")
	}
	b, e := os.ReadFile(*file)
	if e != nil {
		return e
	}
	var out any
	if e = service.Call(ctx, cl, "POST", base+"/v1/service-certificates/adopt", map[string]string{"certificate": string(b), "expected_serial": *serial, "reason": *reason}, &out); e != nil {
		return e
	}
	return printValue(out, machine)
}
