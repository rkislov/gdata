// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"gdata/srk/internal/service"
)

type cliError struct {
	code int
	err  error
}

func (e *cliError) Error() string { return e.err.Error() }
func usageError(s string) error   { return &cliError{2, errors.New(s)} }
func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if e := run(os.Args[1:]); e != nil {
		code := 1
		var ce *cliError
		if errors.As(e, &ce) {
			code = ce.code
		} else if errors.Is(e, context.DeadlineExceeded) {
			code = 4
		} else if strings.Contains(e.Error(), "HTTP 403") || strings.Contains(e.Error(), "HTTP 401") {
			code = 3
		} else if strings.Contains(e.Error(), "HTTP 409") {
			code = 5
		} else {
			var network net.Error
			if errors.As(e, &network) {
				code = 4
			}
		}
		fmt.Fprintln(os.Stderr, e)
		os.Exit(code)
	}
}
func help() {
	fmt.Print(`GData SRK 0.9.2 — Copyright 2026 Кислов Роман Сергеевич — Apache 2.0

srk [--config FILE] [--endpoint URL] [--json] [--wait] [--timeout 30s] COMPONENT COMMAND

Components: manager, media, agent, proxy
  serve                   Start the configured service (foreground)
  status | health         Query the service over authenticated TLS
  check-config            Validate local configuration and certificate identity

Manager commands:
  jobs list
  jobs create --file FILE  Create a job from JSON
  jobs run ID             Enqueue backup; --wait waits for completion
  jobs enable ID | jobs disable ID
  runs list | runs get ID | runs cancel ID
  protection report | settings | put --file FILE  Protection 3-2-1
  storage-view                                 Authorized storage and copy overview
  kubernetes-checks list | put --file FILE  Scheduled Kubernetes restore checks
  runs restore ID --destination PATH_OR_URI [--target-time RFC3339] [--exclusive]
  runs restore ID --vm-request FILE  Restore into a new VM from JSON options
  runs replicate ID --media TARGET_ID --retention-days DAYS [--include-wal]
  runs reconcile-replication ID
  runs verify ID
  rescue issue RUN_ID [RUN_ID ...] | rescue revoke ACCESS_ID
  rescue issue-backup --media ID --name NAME [--retention-days 30] [--max-bytes N]
  nodes | audit
  report --out FILE [--format csv|xlsx] [--from RFC3339] [--to RFC3339]

WAL commands:
  agent wal archive PROFILE --file PATH --name WAL_NAME
  manager wal list MEDIA_ID ARCHIVE_ID
  manager wal replication list|configure RUN_ID|enable POLICY|disable POLICY|sync POLICY|release-protection POLICY --reason TEXT --confirm
  media wal list ARCHIVE_ID
  manager wal calendar MEDIA_ID ARCHIVE_ID
  manager wal gc-preview MEDIA_ID ARCHIVE_ID [--before RFC3339] [--allow-orphan]
  manager wal gc MEDIA_ID ARCHIVE_ID --plan FILE --confirm
  media wal calendar|gc-preview|gc ARCHIVE_ID [options]

Proxy commands:
  virtualization profiles | virtualization list PROFILE | virtualization plan PROFILE VM_ID
  virtualization journal PROFILE | virtualization cleanup PROFILE RUN_ID

Media commands:
  captures get RUN_ID      Read direct disk upload status
  tape capabilities        Read configured backend capabilities
  manager removable inventory MEDIA
  manager removable label|write|restore|verify|import|recycle|attach|release MEDIA VOLUME [tape-compatible options]
  manager tape pool-set MEDIA FILE.json
  manager tape archive MEDIA POOL --run RUN_ID [--retention-days N]
  manager tape inventory|queue MEDIA
  manager tape load|unload|import|verify|recover MEDIA BARCODE [--drive ID]
  manager tape label MEDIA BARCODE --label TEXT --reason TEXT --confirm-barcode BARCODE
  manager tape erase|recycle MEDIA BARCODE --reason TEXT --confirm-barcode BARCODE [--mode quick|long]
  manager tape write MEDIA BARCODE --run RUN_ID [--retention-days 30] [--hold]
  manager tape restore MEDIA BARCODE --copy COPY_ID --agent AGENT_OR_PROXY
  verify SNAPSHOT_ID       Read and verify all snapshot chunks

Repository commands (manager requires MEDIA_ID after the command):
  repository snapshots [MEDIA_ID]
  repository gc-preview [MEDIA_ID]
  repository gc [MEDIA_ID] --plan FILE --confirm
  repository import [MEDIA_ID] --snapshots ID,... --agent AGENT --retention-days 30 --confirm
  repository policy [MEDIA_ID] SNAPSHOT_ID --hold true|false --reason TEXT --confirm
  repository policy [MEDIA_ID] SNAPSHOT_ID --expires RFC3339 --confirm

Catalog recovery:
  manager catalog-backups list|create|verify ID|download ID --out FILE
  manager catalog-backups replicate|drill ID --media MEDIA
  manager catalog-backups remote-list MEDIA|remote-download ID --media MEDIA --out FILE
  manager catalog-backups policy --version N --interval-seconds SECONDS
  manager catalog-backups acknowledge --archive-id ID --confirm-installation ID
  catalog-recovery keygen --out KEY_FILE
  catalog-recovery verify --archive FILE --key-file KEY_FILE
  catalog-recovery restore --archive FILE --key-file KEY_FILE --database-url-file FILE --schema NEW_SCHEMA --installation ID --directory NEW_ABSOLUTE_DIR

Setup commands:
  dev init [--dir .local] [--database-url URL]
  pki init --dir DIR --installation ID
  pki issue --dir DIR --installation ID --role ROLE --id ID [--hosts HOST,...]
  pki csr --key-out NEW_KEY --csr-out NEW_CSR
  manager certificates list|issue|revoke USER [--csr FILE --out CERT --version N --days 30 --overlap-hours 0 --serial SERIAL]
  manager restore-targets
  runs restore ID --restore-target TARGET --destination PATH_OR_URI
  pki revoke --cert FILE --list FILE
  version | help

Global flags precede the component. JSON is written to stdout, errors to stderr.
Local development credentials and private keys are never printed by setup.
`)
}
func run(args []string) error {
	f := flag.NewFlagSet("srk", flag.ContinueOnError)
	f.SetOutput(os.Stderr)
	cfg := f.String("config", "", "configuration JSON file")
	endpoint := f.String("endpoint", "", "service HTTPS endpoint")
	jsonOut := f.Bool("json", false, "machine-readable JSON")
	wait := f.Bool("wait", false, "wait for operation to finish")
	timeout := f.Duration("timeout", 30*time.Second, "request or wait timeout")
	key := f.String("idempotency-key", "", "stable key for a retried operation")
	if e := f.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			help()
			return nil
		}
		return &cliError{2, e}
	}
	args = f.Args()
	if len(args) == 0 || args[0] == "help" {
		help()
		return nil
	}
	if args[0] == "version" {
		return printValue(map[string]string{"version": model.Version, "author": "Кислов Роман Сергеевич", "license": "Apache-2.0"}, *jsonOut)
	}
	if args[0] == "dev" {
		return dev(args[1:])
	}
	if args[0] == "catalog-recovery" {
		return catalogRecovery(args[1:])
	}
	if args[0] == "pki" {
		return pki(args[1:])
	}
	if len(args) < 2 {
		return usageError("component command required; see srk help")
	}
	component, command := args[0], args[1]
	switch component {
	case "manager", "media", "agent", "proxy":
	default:
		return usageError("unknown component")
	}
	if *cfg == "" {
		return usageError("--config is required")
	}
	c, e := config.Load(*cfg)
	if e != nil {
		return &cliError{2, e}
	}
	if command == "check-config" {
		if _, e = identity.Config(c, true); e != nil {
			return &cliError{2, e}
		}
		return printValue(map[string]any{"valid": true, "role": c.Role, "id": c.ID, "installation": c.Installation}, *jsonOut)
	}
	if command == "serve" {
		if component != c.Role {
			return usageError("component does not match configured service role")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		switch component {
		case "manager":
			m, e := service.NewManager(ctx, c)
			if e != nil {
				return e
			}
			return m.Run(ctx)
		case "media":
			m, e := service.NewMedia(c)
			if e != nil {
				return e
			}
			return m.Run(ctx)
		default:
			a, e := service.NewAgent(c)
			if e != nil {
				return e
			}
			return a.Run(ctx)
		}
	}
	if *timeout <= 0 {
		return usageError("timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	cl, e := identity.Client(c, identity.Peer{Role: component})
	if e != nil {
		return e
	}
	defer cl.CloseIdleConnections()
	base := c.URL
	if *endpoint != "" {
		base = *endpoint
	}
	if !service.ValidEndpoint(base) {
		return usageError("valid HTTPS --endpoint or configured url required")
	}
	method, path := "GET", ""
	var body any
	expectRun := false
	switch command {
	case "catalog-backups":
		if component != "manager" {
			return usageError("manager catalog-backups required")
		}
		return catalogBackupCLI(ctx, cl, base, args[2:], *jsonOut, *wait)
	case "certificates":
		if component != "manager" {
			return usageError("manager certificates required")
		}
		return certificates(ctx, cl, base, args[2:], *jsonOut)
	case "restore-targets":
		if component != "manager" {
			return usageError("manager restore-targets required")
		}
		path = "/v1/restore-targets"

	case "captures":
		if component != "media" || len(args) != 4 || args[2] != "get" {
			return usageError("media captures get RUN_ID")
		}
		path = "/v1/captures/" + url.PathEscape(args[3])
	case "removable":
		flashArgs := append([]string(nil), args[2:]...)
		if len(flashArgs) > 0 && (flashArgs[0] == "attach" || flashArgs[0] == "release") {
			flashArgs[0] = "verify"
		}
		method, path, body, expectRun, e = tapeCommand(component, flashArgs)
		if e != nil {
			return e
		}
		if method == "GET" {
			path = strings.Replace(path, "/tape", "/removable", 1)
		} else {
			req := body.(model.TapeRequest)
			req.Action = args[2]
			req.VolumeID = req.Barcode
			body = req
			path = strings.Replace(path, "/tape/operations", "/removable/"+url.PathEscape(req.VolumeID)+"/operations", 1)
		}
	case "tape":
		method, path, body, expectRun, e = tapeCommand(component, args[2:])
		if e != nil {
			return e
		}
	case "virtualization":
		if component != "proxy" || len(args) < 3 {
			return usageError("proxy virtualization profiles|list PROFILE|plan PROFILE VM_ID")
		}
		path = "/v1/virtualization"
		switch args[2] {
		case "profiles":
			if len(args) != 3 {
				return usageError("no profile expected")
			}
		case "list":
			if len(args) != 4 {
				return usageError("PROFILE required")
			}
			path += "/" + url.PathEscape(args[3]) + "/vms"
		case "journal":
			if len(args) != 4 {
				return usageError("PROFILE required")
			}
			path += "/" + url.PathEscape(args[3]) + "/journal"
		case "cleanup":
			if len(args) != 5 {
				return usageError("PROFILE RUN_ID required")
			}
			method = "POST"
			path += "/" + url.PathEscape(args[3]) + "/cleanup/" + url.PathEscape(args[4])
		case "plan":
			if len(args) != 5 {
				return usageError("PROFILE VM_ID required")
			}
			path += "/" + url.PathEscape(args[3]) + "/vms/" + url.PathEscape(args[4]) + "/plan"
		default:
			return usageError("unknown virtualization command")
		}
	case "rescue":
		if component != "manager" || len(args) < 4 {
			return usageError("manager rescue issue RUN_ID [RUN_ID ...] | revoke SCOPE_ID")
		}
		switch args[2] {
		case "issue-backup":
			fs := flag.NewFlagSet("rescue issue-backup", flag.ContinueOnError)
			media := fs.String("media", "", "media server ID")
			name := fs.String("name", "", "backup name")
			days := fs.Int("retention-days", 30, "retention in days")
			limit := fs.Int64("max-bytes", 512<<30, "maximum source disk bytes")
			if e = fs.Parse(args[3:]); e != nil {
				return e
			}
			if *media == "" || *name == "" {
				return usageError("--media and --name are required")
			}
			method = "POST"
			path = "/v1/rescue/codes"
			body = map[string]any{"operation": "backup", "media_id": *media, "name": *name, "retention_days": *days, "max_bytes": *limit}
		case "issue":
			method = "POST"
			path = "/v1/rescue/codes"
			body = map[string]any{"run_ids": args[3:]}
		case "revoke":
			if len(args) != 4 {
				return usageError("rescue revoke SCOPE_ID")
			}
			method = "DELETE"
			path = "/v1/rescue/codes/" + url.PathEscape(args[3])
		default:
			return usageError("rescue issue or revoke required")
		}
	case "me":
		path = "/v1/me"
	case "users":
		if component != "manager" || len(args) < 3 {
			return usageError("manager users list|put --file FILE")
		}
		path = "/v1/users"
		switch args[2] {
		case "list":
		case "put":
			method = "PUT"
			fs := flag.NewFlagSet("users put", flag.ContinueOnError)
			file := fs.String("file", "", "private JSON account file; version 0 for creation")
			if e = fs.Parse(args[3:]); e != nil {
				return e
			}
			raw, err := os.ReadFile(*file)
			if err != nil {
				return err
			}
			var account map[string]any
			if err = json.Unmarshal(raw, &account); err != nil {
				return err
			}
			body = account
		default:
			return usageError("manager users list|put --file FILE")
		}
	case "storage-view":
		if component != "manager" || len(args) != 2 {
			return usageError("manager storage-view")
		}
		path = "/v1/storage-view"
	case "protection":
		if component != "manager" || len(args) < 3 {
			return usageError("manager protection report|settings|put --file FILE")
		}
		path = "/v1/protection"
		switch args[2] {
		case "report":
		case "settings":
			path += "/settings"
		case "put":
			path += "/settings"
			method = "PUT"
			fs := flag.NewFlagSet("protection put", flag.ContinueOnError)
			file := fs.String("file", "", "protection settings JSON including current version")
			if e = fs.Parse(args[3:]); e != nil {
				return e
			}
			raw, err := os.ReadFile(*file)
			if err != nil {
				return err
			}
			var settings model.ProtectionSettings
			if err = json.Unmarshal(raw, &settings); err != nil {
				return err
			}
			body = settings
		default:
			return usageError("protection report|settings|put")
		}
	case "kubernetes-checks":
		if component != "manager" || len(args) < 3 {
			return usageError("manager kubernetes-checks list|put --file FILE")
		}
		path = "/v1/kubernetes-checks"
		if args[2] == "put" {
			fs := flag.NewFlagSet("kubernetes-checks put", flag.ContinueOnError)
			file := fs.String("file", "", "verification policy JSON")
			if e = fs.Parse(args[3:]); e != nil {
				return e
			}
			data, err := os.ReadFile(*file)
			if err != nil {
				return err
			}
			var policy model.KubernetesCheckPolicy
			if err = json.Unmarshal(data, &policy); err != nil {
				return err
			}
			body = policy
			method = "POST"
		} else if args[2] != "list" {
			return usageError("kubernetes-checks list|put")
		}
	case "wal":
		if component == "manager" || component == "media" {
			method, path, body, expectRun, e = walCommand(component, args[2:])
			if e != nil {
				return e
			}
			break
		}
		if component == "manager" && len(args) == 5 && args[2] == "list" {
			path = "/v1/media/" + url.PathEscape(args[3]) + "/wal/" + url.PathEscape(args[4])
			break
		}
		if component == "media" && len(args) == 4 && args[2] == "list" {
			path = "/v1/wal/" + url.PathEscape(args[3])
			break
		}
		if component != "agent" || len(args) < 4 || args[2] != "archive" {
			return usageError("agent wal archive PROFILE --file PATH --name WAL_NAME")
		}
		fs := flag.NewFlagSet("wal archive", flag.ContinueOnError)
		file := fs.String("file", "", "PostgreSQL %p")
		name := fs.String("name", "", "PostgreSQL %f")
		if e = fs.Parse(args[4:]); e != nil {
			return &cliError{2, e}
		}
		if *file == "" || !repository.WALName(*name) {
			return usageError("valid --file and --name required")
		}
		method = "POST"
		path = "/v1/postgresql/" + url.PathEscape(args[3]) + "/archive"
		body = map[string]string{"file": *file, "name": *name}
	case "repository":
		method, path, body, expectRun, e = repositoryCommand(component, args[2:])
		if e != nil {
			return e
		}
	case "status":
		path = "/v1/status"
	case "health":
		path = "/healthz"
	case "nodes", "audit":
		if component != "manager" {
			return usageError("command requires manager")
		}
		path = "/v1/" + command
	case "verify":
		if component != "media" || len(args) != 3 {
			return usageError("media verify SNAPSHOT_ID")
		}
		method = "POST"
		path = "/v1/snapshots/" + url.PathEscape(args[2]) + "/verify"
	case "report":
		if component != "manager" {
			return usageError("command requires manager")
		}
		return report(ctx, cl, base, args[2:])
	case "jobs":
		if component != "manager" || len(args) < 3 {
			return usageError("manager jobs list|create|run|enable|disable")
		}
		switch args[2] {
		case "list":
			path = "/v1/jobs"
		case "create":
			fs := flag.NewFlagSet("jobs create", flag.ContinueOnError)
			file := fs.String("file", "", "job JSON file")
			if e = fs.Parse(args[3:]); e != nil {
				return &cliError{2, e}
			}
			if *file == "" {
				return usageError("--file is required")
			}
			b, e := os.ReadFile(*file)
			if e != nil {
				return e
			}
			var j model.Job
			if e = json.Unmarshal(b, &j); e != nil {
				return &cliError{2, e}
			}
			body = j
			method = "POST"
			path = "/v1/jobs"
		case "run", "enable", "disable":
			if len(args) != 4 {
				return usageError("job ID required")
			}
			method = "POST"
			path = "/v1/jobs/" + url.PathEscape(args[3])
			if args[2] == "run" {
				path += "/run"
				expectRun = true
			} else {
				path += "/enabled"
				body = map[string]bool{"enabled": args[2] == "enable"}
			}
		default:
			return usageError("unknown jobs command")
		}
	case "runs":
		if component != "manager" || len(args) < 3 {
			return usageError("manager runs list|get|cancel|restore|verify")
		}
		switch args[2] {
		case "list":
			path = "/v1/runs"
		case "get", "cancel", "verify", "restore", "replicate", "reconcile-replication":
			if len(args) < 4 {
				return usageError("run ID required")
			}
			path = "/v1/runs/" + url.PathEscape(args[3])
			if args[2] != "get" {
				method = "POST"
				path += "/" + args[2]
				expectRun = args[2] == "restore" || args[2] == "verify"
			}
			if args[2] == "replicate" {
				fs := flag.NewFlagSet("replicate", flag.ContinueOnError)
				target := fs.String("media", "", "target media ID")
				days := fs.Int("retention-days", 30, "independent retention days")
				includeWAL := fs.Bool("include-wal", false, "continuously replicate the PostgreSQL WAL archive")
				if e = fs.Parse(args[4:]); e != nil {
					return &cliError{2, e}
				}
				if *target == "" || *days < 1 || *days > 36500 {
					return usageError("--media and retention-days 1–36500 required")
				}
				body = map[string]any{"target_media_id": *target, "retention_days": *days, "include_wal": *includeWAL}
				expectRun = true
			}
			if args[2] == "restore" {
				fs := flag.NewFlagSet("restore", flag.ContinueOnError)
				dest := fs.String("destination", "", "new restore destination")
				vmFile := fs.String("vm-request", "", "new VM restore options JSON")
				restoreTarget := fs.String("restore-target", "", "approved recovery target ID")
				target := fs.String("target-time", "", "PITR target RFC3339 with timezone")
				exclusive := fs.Bool("exclusive", false, "exclude a commit exactly at target time")
				if e = fs.Parse(args[4:]); e != nil {
					return &cliError{2, e}
				}
				if (*dest == "") == (*vmFile == "") {
					return usageError("choose --destination or --vm-request")
				}
				request := map[string]any{"destination": *dest}
				if *vmFile != "" {
					if *target != "" || *exclusive {
						return usageError("PITR options cannot be used with --vm-request")
					}
					data, err := os.ReadFile(*vmFile)
					if err != nil {
						return err
					}
					var vm model.VMRestoreRequest
					if err = json.Unmarshal(data, &vm); err != nil {
						return err
					}
					if err = vm.Validate(); err != nil && *restoreTarget == "" {
						return err
					}
					request = map[string]any{"vm_restore": vm}
				}
				if *target != "" {
					at, err := time.Parse(time.RFC3339Nano, *target)
					if err != nil {
						return usageError("--target-time requires RFC3339 with timezone")
					}
					inclusive := !*exclusive
					request["pitr"] = &model.PITRRequest{TargetTime: at, Inclusive: &inclusive}
				} else if *exclusive {
					return usageError("--exclusive requires --target-time")
				}
				request["restore_target_id"] = *restoreTarget
				body = request
			}
		default:
			return usageError("unknown runs command")
		}
	default:
		return usageError("unknown command; see srk help")
	}
	var result any
	if len(*key) > 128 {
		return usageError("idempotency key exceeds 128 bytes")
	}
	headers := http.Header{}
	if component == "media" && method == "POST" {
		headers.Set("X-Operation-ID", model.ID())
	}
	if *key != "" {
		headers.Set("Idempotency-Key", *key)
	}
	if e = service.Call(ctx, cl, method, base+path, body, &result, headers); e != nil {
		return e
	}
	if expectRun && *wait {
		b, _ := json.Marshal(result)
		var r model.Run
		if e = json.Unmarshal(b, &r); e != nil {
			return e
		}
		for r.Status == "queued" || r.Status == "running" {
			select {
			case <-ctx.Done():
				return &cliError{4, errors.New("wait timed out; server operation continues; use runs get or runs cancel")}
			case <-time.After(250 * time.Millisecond):
			}
			if e = service.Call(ctx, cl, "GET", base+"/v1/runs/"+r.ID, nil, &r); e != nil {
				return e
			}
		}
		if e = printValue(r, *jsonOut); e != nil {
			return e
		}
		if r.Status != "success" {
			return errors.New("operation finished with status " + r.Status)
		}
		return nil
	}
	return printValue(result, *jsonOut)
}
func printValue(v any, machine bool) error {
	if !machine {
		if list, ok := v.([]any); ok {
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME / ROLE\tSTATUS\tCREATED / LAST SEEN")
			for _, item := range list {
				m, ok := item.(map[string]any)
				if !ok {
					continue
				}
				name := m["name"]
				if name == nil {
					name = m["role"]
				}
				if name == nil {
					if job, ok := m["job"].(map[string]any); ok {
						name = job["name"]
					}
				}
				status := m["status"]
				if status == nil {
					status = "—"
				}
				when := m["created_at"]
				if when == nil {
					when = m["last_seen"]
				}
				fmt.Fprintf(w, "%v\t%v\t%v\t%v\n", m["id"], name, status, when)
			}
			return w.Flush()
		}
	}
	enc := json.NewEncoder(os.Stdout)
	if !machine {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(v)
}
func report(ctx context.Context, cl *http.Client, base string, args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	format := fs.String("format", "csv", "csv or xlsx")
	output := fs.String("out", "", "output file (must not exist)")
	from := fs.String("from", "", "inclusive RFC3339 start")
	to := fs.String("to", "", "exclusive RFC3339 end")
	if e := fs.Parse(args); e != nil {
		return &cliError{2, e}
	}
	if *output == "" || (*format != "csv" && *format != "xlsx") {
		return usageError("--out FILE and --format csv|xlsx required")
	}
	q := url.Values{}
	if *from != "" {
		q.Set("from", *from)
	}
	if *to != "" {
		q.Set("to", *to)
	}
	req, e := http.NewRequestWithContext(ctx, "GET", base+"/v1/reports/operations."+*format+"?"+q.Encode(), nil)
	if e != nil {
		return e
	}
	res, e := cl.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, b)
	}
	f, e := os.OpenFile(*output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = io.Copy(f, res.Body)
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		os.Remove(*output)
		return e
	}
	return closeErr
}
func pki(args []string) error {
	if len(args) == 0 {
		return usageError("pki init|issue|revoke required")
	}
	fs := flag.NewFlagSet("pki", flag.ContinueOnError)
	dir := fs.String("dir", "", "PKI directory")
	installation := fs.String("installation", "", "installation ID")
	role := fs.String("role", "", "component role")
	id := fs.String("id", "", "component ID")
	hosts := fs.String("hosts", "localhost,127.0.0.1,::1", "certificate DNS names and IPs")
	cert := fs.String("cert", "", "certificate to revoke")
	list := fs.String("list", "", "revocation list path")
	keyOut := fs.String("key-out", "", "new private key file")
	csrOut := fs.String("csr-out", "", "new CSR file")
	if e := fs.Parse(args[1:]); e != nil {
		return &cliError{2, e}
	}
	switch args[0] {
	case "csr":
		if *keyOut == "" || *csrOut == "" || *keyOut == *csrOut {
			return usageError("--key-out and --csr-out must be distinct new paths")
		}
		return identity.CreatePersonalCSR(*keyOut, *csrOut)
	case "init":
		if *dir == "" || *installation == "" {
			return usageError("--dir and --installation required")
		}
		return identity.InitCA(*dir, *installation)
	case "issue":
		if *dir == "" || *installation == "" || *role == "" || *id == "" {
			return usageError("--dir --installation --role --id required")
		}
		return identity.Issue(*dir, *installation, *role, *id, strings.Split(*hosts, ",")...)
	case "revoke":
		if *cert == "" || *list == "" {
			return usageError("--cert and --list required")
		}
		b, e := os.ReadFile(*cert)
		if e != nil {
			return e
		}
		p, _ := pem.Decode(b)
		if p == nil {
			return errors.New("invalid certificate")
		}
		c, e := x509.ParseCertificate(p.Bytes)
		if e != nil {
			return e
		}
		var serials []string
		b, e = os.ReadFile(*list)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if e == nil {
			if e = json.Unmarshal(b, &serials); e != nil {
				return e
			}
		}
		for _, s := range serials {
			if s == c.SerialNumber.String() {
				return nil
			}
		}
		serials = append(serials, c.SerialNumber.String())
		b, _ = json.Marshal(serials)
		return repository.Atomic(*list, b, 0600)
	default:
		return usageError("unknown PKI operation")
	}
}
func dev(args []string) error {
	if len(args) == 0 || args[0] != "init" {
		return usageError("dev init required")
	}
	fs := flag.NewFlagSet("dev init", flag.ContinueOnError)
	dir := fs.String("dir", ".local", "development directory")
	dsn := fs.String("database-url", "", "local PostgreSQL URL")
	if e := fs.Parse(args[1:]); e != nil {
		return &cliError{2, e}
	}
	root, e := filepath.Abs(*dir)
	if e != nil {
		return e
	}
	if _, e = os.Stat(filepath.Join(root, "manager.json")); !os.IsNotExist(e) {
		return errors.New("development installation exists; refusing to overwrite credentials")
	}
	if e = os.MkdirAll(root, 0700); e != nil {
		return e
	}
	if e = os.Chmod(root, 0700); e != nil {
		return e
	}
	pkiDir := filepath.Join(root, "pki")
	if e = identity.InitCA(pkiDir, "local"); e != nil {
		return e
	}
	revoked := filepath.Join(pkiDir, "revoked.json")
	if e = os.WriteFile(revoked, []byte("[]"), 0600); e != nil {
		return e
	}
	secret := make([]byte, 48)
	if _, e = rand.Read(secret); e != nil {
		return e
	}
	keyFile := filepath.Join(root, "capability.key")
	if e = os.WriteFile(keyFile, secret, 0600); e != nil {
		return e
	}
	if _, e = rand.Read(secret); e != nil {
		return e
	}
	passwordFile := filepath.Join(root, "admin.password")
	if e = os.WriteFile(passwordFile, []byte(hex.EncodeToString(secret[:24])+"\n"), 0600); e != nil {
		return e
	}
	if *dsn == "" {
		u, e := user.Current()
		if e != nil {
			return e
		}
		*dsn = "postgres://" + url.User(u.Username).String() + "@127.0.0.1:55432/srk?sslmode=disable"
	}
	source := filepath.Join(root, "source")
	restore := filepath.Join(root, "restores")
	for _, d := range []string{source, restore, filepath.Join(root, "repository"), filepath.Join(root, "logs")} {
		if e = os.MkdirAll(d, 0700); e != nil {
			return e
		}
	}
	if e = os.WriteFile(filepath.Join(source, "welcome.txt"), []byte("GData SRK\nТестовый файл для проверки резервного копирования.\n"), 0644); e != nil {
		return e
	}
	for i, role := range []string{"manager", "media", "agent", "proxy", "admin", "auditor"} {
		id := role + "-local"
		if e = identity.Issue(pkiDir, "local", role, id); e != nil {
			return e
		}
		port := 8443 + i
		if role == "admin" || role == "auditor" {
			port = 8443
		}
		c := config.Config{Role: role, ID: id, Installation: "local", Listen: "127.0.0.1:" + strconv.Itoa(port), URL: "https://localhost:" + strconv.Itoa(port), ManagerURL: "https://localhost:8443", CA: filepath.Join(pkiDir, "ca.crt"), Cert: filepath.Join(pkiDir, id+".crt"), Key: filepath.Join(pkiDir, id+".key"), Revoked: revoked, MaxParallel: 2, PerAgent: 1, PerMedia: 2, Development: true}
		switch role {
		case "manager":
			c.DatabaseURL = *dsn
			c.PasswordFile = passwordFile
			c.CapabilityKeyFile = keyFile
		case "media":
			c.Repository = filepath.Join(root, "repository")
			c.CapabilityKeyFile = keyFile
		case "agent":
			c.AllowedRoots = []string{source}
			c.RestoreRoots = []string{restore}
		}
		b, e := json.MarshalIndent(c, "", "  ")
		if e != nil {
			return e
		}
		if e = os.WriteFile(filepath.Join(root, role+".json"), b, 0600); e != nil {
			return e
		}
	}
	job := model.Job{Name: "Локальные документы", AgentID: "agent-local", MediaID: "media-local", Source: source, Mode: "incremental", IntervalSeconds: 86400, RetentionDays: 30, Enabled: true}
	b, _ := json.MarshalIndent(job, "", "  ")
	if e = os.WriteFile(filepath.Join(root, "example-job.json"), b, 0600); e != nil {
		return e
	}
	fmt.Printf("Created local installation: %s\nWeb: https://localhost:8443\nAdministrator password file: %s\n", root, passwordFile)
	return nil
}
