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
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"gdata/srk/internal/config"
	"gdata/srk/internal/identity"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"gdata/srk/internal/virtualization"
)

type Agent struct {
	vmReports    map[string]vmReportStamp
	vmLocks      map[string]bool
	Config       config.Config
	Client       *http.Client
	mu           sync.Mutex
	active       map[string]context.CancelFunc
	progress     map[string]model.Result
	mediaClients map[string]*http.Client
}

func NewAgent(c config.Config) (*Agent, error) {
	cl, e := identity.Client(c)
	if e != nil {
		return nil, e
	}
	cl.Transport.(*http.Transport).ResponseHeaderTimeout = 0
	return &Agent{vmReports: map[string]vmReportStamp{}, vmLocks: map[string]bool{}, Config: c, Client: cl, active: map[string]context.CancelFunc{}, progress: map[string]model.Result{}, mediaClients: map[string]*http.Client{}}, nil
}
func (a *Agent) status() map[string]any {
	a.mu.Lock()
	p := map[string]model.Result{}
	for k, v := range a.progress {
		p[k] = v
	}
	a.mu.Unlock()
	s := LocalStatus(a.Config)
	s["operations"] = p
	s["allowed_roots"] = a.Config.AllowedRoots
	s["restore_roots"] = a.Config.RestoreRoots
	s["file_snapshot"] = false
	profiles := []string{}
	for name := range a.Config.PostgreSQL {
		profiles = append(profiles, name)
	}
	slices.Sort(profiles)
	s["postgresql_profiles"] = profiles
	dataProfiles := map[string][]string{"s3": {}, "mysql": {}, "kubernetes": {}, "longhorn": {}}
	for name := range a.Config.S3 {
		dataProfiles["s3"] = append(dataProfiles["s3"], name)
	}
	for name := range a.Config.MySQL {
		dataProfiles["mysql"] = append(dataProfiles["mysql"], name)
	}
	for name, p := range a.Config.Kubernetes {
		dataProfiles["kubernetes"] = append(dataProfiles["kubernetes"], name)
		if p.LonghornSnapshotClass != "" {
			dataProfiles["longhorn"] = append(dataProfiles["longhorn"], name)
		}
	}
	slices.Sort(dataProfiles["kubernetes"])
	slices.Sort(dataProfiles["longhorn"])
	slices.Sort(dataProfiles["s3"])
	slices.Sort(dataProfiles["mysql"])
	s["data_profiles"] = dataProfiles
	restoreProfiles := []string{}
	for name, p := range a.Config.Kubernetes {
		if p.AllowRestore {
			restoreProfiles = append(restoreProfiles, name)
		}
	}
	slices.Sort(restoreProfiles)
	s["kubernetes_restore_profiles"] = restoreProfiles
	checkProfiles := map[string]string{}
	for name, p := range a.Config.Kubernetes {
		if p.AllowVerification {
			checkProfiles[name] = p.RestoreNamespacePrefix + "check-"
		}
	}
	s["kubernetes_check_profiles"] = checkProfiles
	s["wal_archives"] = a.walStatus()
	if a.Config.Role == "proxy" {
		s["virtualization_adapter"] = "openstack_backup_restore"
		providers := map[string]string{}
		for name, p := range a.Config.Virtualization {
			providers[name] = p.Kind
		}
		s["virtualization_profiles"] = providers
		enabled := []string{}
		for name, p := range a.Config.Virtualization {
			if virtualization.VMProvider(p.Kind) && p.BackupEnabled {
				enabled = append(enabled, name)
			}
		}
		slices.Sort(enabled)
		s["virtualization_backup_profiles"] = enabled
	}
	return s
}
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	a.walRoutes(mux)
	a.virtualizationRoutes(mux)
	for _, p := range []string{"GET /v1/status", "GET /healthz"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			if e := Diagnostic(r, a.Config); e != nil {
				Fail(w, 403, e)
				return
			}
			JSON(w, 200, a.status())
		})
	}
	mux.HandleFunc("POST /v1/execute", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager"); e != nil {
			Fail(w, 403, e)
			return
		}
		var v model.Execute
		if e := Decode(w, r, &v); e != nil {
			Fail(w, 400, e)
			return
		}
		if a.Config.Role == "proxy" && !virtualization.VMProvider(v.Job.Type) {
			Fail(w, 501, errors.New("proxy supports execution only for OpenStack/oVirt jobs"))
			return
		}
		if a.Config.Role != "proxy" && virtualization.VMProvider(v.Job.Type) {
			Fail(w, 400, errors.New("VM backup requires proxy"))
			return
		}
		if v.Job.AgentID != a.Config.ID || v.RunID == "" || !ValidEndpoint(v.MediaURL) {
			Fail(w, 400, errors.New("invalid execution target"))
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		a.mu.Lock()
		if _, ok := a.active[v.RunID]; ok || len(a.active) >= a.Config.MaxParallel {
			a.mu.Unlock()
			Fail(w, 409, errors.New("agent busy or operation already active"))
			return
		}
		a.active[v.RunID] = cancel
		a.progress[v.RunID] = model.Result{}
		a.mu.Unlock()
		defer func() {
			a.mu.Lock()
			delete(a.active, v.RunID)
			delete(a.progress, v.RunID)
			delete(a.vmReports, v.RunID)
			a.mu.Unlock()
		}()
		var result model.Result
		var e error
		switch v.Kind {
		case "backup":
			result, e = a.Backup(ctx, v)
		case "restore":
			result, e = a.Restore(ctx, v)
		default:
			e = errors.New("unsupported operation")
		}
		if e != nil {
			Fail(w, 422, e)
			return
		}
		JSON(w, 200, result)
	})
	mux.HandleFunc("POST /v1/operations/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if _, e := Role(r, "manager", "admin"); e != nil {
			Fail(w, 403, e)
			return
		}
		a.mu.Lock()
		fn, ok := a.active[r.PathValue("id")]
		a.mu.Unlock()
		if !ok {
			Fail(w, 404, errors.New("operation not active"))
			return
		}
		fn()
		JSON(w, 200, map[string]bool{"cancel_requested": true})
	})
	return mux
}
func (a *Agent) Run(ctx context.Context) error {
	if a.Config.Role == "proxy" {
		_ = a.recoverVMJournals(ctx) // Pending journals block execution; diagnostics remain available.
	}
	go Heartbeat(ctx, a.Config, a.status)
	return Serve(ctx, a.Config, a.Handler())
}
func (a *Agent) progressUpdate(id string, v model.Result) {
	a.mu.Lock()
	a.progress[id] = v
	a.mu.Unlock()
}
func ResolveAllowed(candidate string, roots []string) (string, string, error) {
	if !filepath.IsAbs(candidate) {
		return "", "", errors.New("absolute path required")
	}
	candidate = filepath.Clean(candidate)
	for _, root := range roots {
		resolved, e := filepath.EvalSymlinks(root)
		if e != nil {
			continue
		}
		for _, prefix := range []string{filepath.Clean(root), resolved} {
			rel, e := filepath.Rel(prefix, candidate)
			if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
				return resolved, rel, nil
			}
		}
	}
	return "", "", errors.New("path outside configured allowlist")
}
func (a *Agent) media(ctx context.Context, v model.Execute, method, p string, b []byte) (*http.Response, error) {
	req, e := http.NewRequestWithContext(ctx, method, v.MediaURL+p, bytes.NewReader(b))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+v.Grant)
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	a.mu.Lock()
	cl := a.mediaClients[v.Job.MediaID]
	if cl == nil {
		cl, e = identity.Client(a.Config, identity.Peer{Role: "media", ID: v.Job.MediaID})
		if e == nil {
			cl.Transport.(*http.Transport).ResponseHeaderTimeout = 0
			a.mediaClients[v.Job.MediaID] = cl
		}
	}
	a.mu.Unlock()
	if e != nil {
		return nil, e
	}
	resp, e := cl.Do(req)
	if e != nil {
		return nil, e
	}
	if resp.StatusCode >= 300 && !(method == "HEAD" && resp.StatusCode == 404) {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("media HTTP %d: %s", resp.StatusCode, body)
	}
	return resp, nil
}
func (a *Agent) manifest(ctx context.Context, v model.Execute, id string) (model.Manifest, error) {
	var m model.Manifest
	resp, e := a.media(ctx, v, "GET", "/v1/snapshots/"+id, nil)
	if e != nil {
		return m, e
	}
	defer resp.Body.Close()
	d := json.NewDecoder(io.LimitReader(resp.Body, model.MaxManifestBytes))
	if e = d.Decode(&m); e != nil {
		return m, e
	}
	return m, repository.Validate(m)
}
func (a *Agent) Backup(ctx context.Context, v model.Execute) (model.Result, error) {
	if virtualization.NativeProvider(v.Job.Type) {
		return a.backupNative(ctx, v)
	}
	if v.Job.Type == "ovirt" {
		return a.backupOVirt(ctx, v)
	}
	if v.Job.Type == "openstack" {
		return a.backupOpenStack(ctx, v)
	}
	defer a.release(v)
	if v.Job.Type == "kubernetes" || v.Job.Type == "longhorn" {
		return a.backupKubernetes(ctx, v)
	}
	if v.Job.Type == "s3" || v.Job.Type == "mysql" {
		return a.backupData(ctx, v)
	}
	if v.Job.Type == "postgresql" {
		return a.backupPostgreSQL(ctx, v)
	}
	if v.Job.Type != "" && v.Job.Type != "files" && v.Job.Type != "disk-image" {
		return model.Result{}, errors.New("unsupported source type")
	}
	return a.backupFiles(ctx, v, v.Job.Source, a.Config.AllowedRoots, nil)
}
func (a *Agent) backupFiles(ctx context.Context, v model.Execute, source string, roots []string, pg *model.PostgreSQLBackup) (model.Result, error) {
	return a.backupFilesData(ctx, v, source, roots, pg, nil)
}
func (a *Agent) backupFilesData(ctx context.Context, v model.Execute, source string, roots []string, pg *model.PostgreSQLBackup, data *model.DataBackup) (model.Result, error) {
	return a.backupFilesMetadata(ctx, v, source, roots, pg, data, nil)
}
func (a *Agent) backupFilesMetadata(ctx context.Context, v model.Execute, source string, roots []string, pg *model.PostgreSQLBackup, data *model.DataBackup, vm *model.VMBackup) (model.Result, error) {
	res := model.Result{Consistency: "live-files", EffectiveMode: v.Job.Mode, PostgreSQL: pg}
	res.Data = data
	res.VM = vm
	if vm != nil {
		res.Consistency = vm.Consistency
		res.Phase = "vm-transfer"
	}
	if data != nil {
		res.Consistency = data.Kind + "-logical"
		res.Phase = data.Kind + "-upload"
	}
	if v.Job.Type == "disk-image" {
		if v.Job.Mode != "full" || pg != nil {
			return res, errors.New("disk-image requires full mode")
		}
		res.Consistency = "offline-disk-image"
	}
	if pg != nil {
		res.Consistency = "postgresql-physical"
		res.Phase = "postgresql-upload"
	}
	allowed, rel, e := ResolveAllowed(source, roots)
	if e != nil {
		return res, e
	}
	baseRoot, e := os.OpenRoot(allowed)
	if e != nil {
		return res, e
	}
	defer baseRoot.Close()
	root, e := baseRoot.OpenRoot(rel)
	if e != nil {
		return res, e
	}
	defer root.Close()
	if v.Job.Type == "disk-image" {
		entries, err := fs.ReadDir(root.FS(), ".")
		if err != nil {
			return res, err
		}
		if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".img") {
			return res, errors.New("disk-image source directory must contain exactly one offline RAW .img file")
		}
		st, err := root.Lstat(entries[0].Name())
		if err != nil {
			return res, err
		}
		if !st.Mode().IsRegular() || st.Size() < 512 || st.Size()%512 != 0 {
			return res, errors.New("RAW image must be a regular file aligned to 512 bytes")
		}
	}
	res.ProgressScope = "data-transfer"
	// Size is an estimate from a metadata pass; mutable sources may change afterward.
	e = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p == "." {
			return nil
		}
		info, err := root.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			res.TotalBytes += info.Size()
			res.TotalFiles++
		}
		return nil
	})
	if e != nil {
		return res, e
	}
	updateProgress(&res)
	a.reportProgress(ctx, v, res)
	old := map[string]model.File{}
	if v.Base != "" {
		m, e := a.manifest(ctx, v, v.Base)
		if e != nil {
			return res, e
		}
		for _, f := range m.Files {
			old[f.Path] = f
		}
	} else {
		res.EffectiveMode = "full"
	}
	m := model.Manifest{Version: 1, ID: v.RunID, JobID: v.Job.ID, Source: v.Job.Source, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().AddDate(0, 0, v.Job.RetentionDays), Mode: res.EffectiveMode, Base: v.Base, Consistency: res.Consistency, Files: []model.File{}}
	m.PostgreSQL = pg
	m.Data = data
	m.VM = vm
	e = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p == "." {
			return nil
		}
		info, e := root.Lstat(p)
		if e == nil && (data != nil || vm != nil) && info.IsDir() {
			return nil
		}
		if e != nil {
			return e
		}
		f := model.File{Path: p, Mode: uint32(info.Mode().Perm()), ModTime: info.ModTime().UTC(), Size: info.Size()}
		switch {
		case d.IsDir():
			f.Kind = "dir"
			f.Size = 0
		case d.Type()&os.ModeSymlink != 0:
			f.Kind = "symlink"
			f.Link, e = root.Readlink(p)
			if e != nil {
				return e
			}
			f.Size = 0
		case info.Mode().IsRegular():
			f.Kind = "file"
			f.Chunks = []string{}
			fd, e := root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
			if e != nil {
				return e
			}
			before, e := fd.Stat()
			if e != nil {
				fd.Close()
				return e
			}
			if !before.Mode().IsRegular() || !os.SameFile(before, info) {
				fd.Close()
				return errors.New("source replaced while reading")
			}
			buf := make([]byte, repository.ChunkSize)
			var read int64
			for {
				if ctx.Err() != nil {
					fd.Close()
					return ctx.Err()
				}
				n, readErr := io.ReadFull(fd, buf)
				if n > 0 {
					b := buf[:n]
					h := repository.Hash(b)
					f.Chunks = append(f.Chunks, h)
					res.Chunks++
					read += int64(n)
					res.LogicalBytes += int64(n)
					updateProgress(&res)
					head, e := a.media(ctx, v, "HEAD", "/v1/chunks/"+h, nil)
					if e != nil {
						fd.Close()
						return e
					}
					head.Body.Close()
					if head.StatusCode == 404 {
						put, e := a.media(ctx, v, "PUT", "/v1/chunks/"+h, b)
						if e != nil {
							fd.Close()
							return e
						}
						put.Body.Close()
						res.UploadedBytes += int64(n)
					}
					a.reportProgress(ctx, v, res)
				}
				if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
					break
				}
				if readErr != nil {
					fd.Close()
					return readErr
				}
			}
			after, e := fd.Stat()
			fd.Close()
			if e != nil {
				return e
			}
			if read != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
				return fmt.Errorf("file changed during backup: %s", p)
			}
			f.Size = read
			res.Files++
		default:
			return fmt.Errorf("unsupported special file: %s", p)
		}
		prior, ok := old[p]
		if res.EffectiveMode == "full" || !ok || !filesEqual(f, prior) {
			res.ChangedFiles++
		}
		m.Files = append(m.Files, f)
		return nil
	})
	if e != nil {
		return res, e
	}
	present := map[string]bool{}
	for _, f := range m.Files {
		present[f.Path] = true
	}
	for p := range old {
		if !present[p] {
			res.ChangedFiles++
		}
	}
	// Live file enumeration is explicit: no filesystem or application snapshot is claimed.
	m.Result = res
	b, e := json.Marshal(m)
	if e != nil {
		return res, e
	}
	resp, e := a.media(ctx, v, "POST", "/v1/snapshots/"+v.RunID, b)
	if e != nil {
		return res, e
	}
	var ack struct {
		Verified     bool   `json:"verified"`
		RepositoryID string `json:"repository_id"`
	}
	e = json.NewDecoder(resp.Body).Decode(&ack)
	resp.Body.Close()
	if e != nil {
		return res, e
	}
	if !ack.Verified {
		return res, errors.New("media did not confirm validation")
	}
	res.Verified = true
	res.RepositoryID = ack.RepositoryID
	completeProgress(&res)
	if pg != nil || data != nil || vm != nil {
		res.Phase = "complete"
	}
	return res, nil
}
func filesEqual(a, b model.File) bool {
	return a.Path == b.Path && a.Kind == b.Kind && a.Mode == b.Mode && a.Size == b.Size && a.ModTime.Equal(b.ModTime) && a.Link == b.Link && slices.Equal(a.Chunks, b.Chunks)
}
func (a *Agent) Restore(ctx context.Context, v model.Execute) (model.Result, error) {
	defer a.release(v)
	res := model.Result{EffectiveMode: "restore", Consistency: "live-files"}
	m, e := a.manifest(ctx, v, v.SnapshotID)
	if e != nil {
		return res, e
	}
	res.Consistency = m.Consistency
	if m.Data != nil && (m.Data.Kind == "kubernetes" || m.Data.Kind == "longhorn") && strings.HasPrefix(v.Destination, "kubernetes://") {
		return a.restoreKubernetes(ctx, v, m)
	}
	if m.Data != nil && m.Data.Kind != "kubernetes" && m.Data.Kind != "longhorn" {
		return a.restoreData(ctx, v, m)
	}
	if m.VM != nil && virtualization.NativeProvider(m.VM.Provider) {
		return a.restoreNative(ctx, v, m)
	}
	if m.VM != nil && m.VM.Provider == "ovirt" {
		return a.restoreOVirt(ctx, v, m)
	}
	if m.VM != nil {
		return a.restoreOpenStack(ctx, v, m)
	}
	if e = validatePITR(v.PITR, m.PostgreSQL); e != nil {
		return res, e
	}
	res.TotalBytes = m.Result.LogicalBytes
	res.TotalFiles = m.Result.Files
	res.ProgressScope = "data-transfer"
	updateProgress(&res)
	if m.PostgreSQL != nil {
		if _, e = a.postgreSQLRestoreProfile(m.PostgreSQL); e != nil {
			return res, e
		}
	}
	// Reject unsafe links before creating any destination files.
	for _, f := range m.Files {
		if f.Kind == "symlink" {
			target := path.Clean(path.Join(path.Dir(f.Path), f.Link))
			if path.IsAbs(f.Link) || target == ".." || strings.HasPrefix(target, "../") {
				return res, errors.New("external symlink requires an explicit future restore policy")
			}
		}
	}
	allowed, rel, e := ResolveAllowed(v.Destination, a.Config.RestoreRoots)
	if e != nil {
		return res, e
	}
	if rel == "." {
		return res, errors.New("restore destination must be a new child directory")
	}
	parent, e := os.OpenRoot(allowed)
	if e != nil {
		return res, e
	}
	defer parent.Close()
	if e = parent.MkdirAll(filepath.Dir(rel), 0700); e != nil {
		return res, e
	}
	if e = parent.Mkdir(rel, 0700); e != nil {
		return res, fmt.Errorf("destination must not exist: %w", e)
	}
	dest, e := parent.OpenRoot(rel)
	if e != nil {
		return res, e
	}
	defer dest.Close()
	for _, f := range m.Files {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if e = dest.MkdirAll(path.Dir(f.Path), 0700); e != nil {
			return res, e
		}
		switch f.Kind {
		case "dir":
			if e = dest.MkdirAll(f.Path, 0700); e != nil {
				return res, e
			}
		case "symlink":
			if e = dest.Symlink(f.Link, f.Path); e != nil {
				return res, e
			}
		case "file":
			fd, e := dest.OpenFile(f.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return res, e
			}
			var size int64
			for _, h := range f.Chunks {
				resp, e := a.media(ctx, v, "GET", "/v1/chunks/"+h, nil)
				if e != nil {
					fd.Close()
					return res, e
				}
				b, e := io.ReadAll(io.LimitReader(resp.Body, repository.ChunkSize+1))
				resp.Body.Close()
				if e != nil {
					fd.Close()
					return res, e
				}
				if len(b) > repository.ChunkSize || repository.Hash(b) != h {
					fd.Close()
					return res, errors.New("restore checksum mismatch")
				}
				if _, e = fd.Write(b); e != nil {
					fd.Close()
					return res, e
				}
				size += int64(len(b))
				res.LogicalBytes += int64(len(b))
				updateProgress(&res)
				res.Chunks++
				a.reportProgress(ctx, v, res)
			}
			if size != f.Size {
				fd.Close()
				return res, errors.New("restored size differs from manifest")
			}
			if e = fd.Chmod(os.FileMode(f.Mode) & 0777); e != nil {
				fd.Close()
				return res, e
			}
			if e = fd.Sync(); e != nil {
				fd.Close()
				return res, e
			}
			if e = fd.Close(); e != nil {
				return res, e
			}
			if e = dest.Chtimes(f.Path, f.ModTime, f.ModTime); e != nil {
				return res, e
			}
			res.Files++
		}
	}
	for i := len(m.Files) - 1; i >= 0; i-- {
		f := m.Files[i]
		if f.Kind == "dir" {
			if e = dest.Chmod(f.Path, os.FileMode(f.Mode)&0777); e != nil {
				return res, e
			}
			if e = dest.Chtimes(f.Path, f.ModTime, f.ModTime); e != nil {
				return res, e
			}
		}
	}
	if m.PostgreSQL != nil {
		res.Phase = "postgresql-validation"
		res.ProgressPercent = nil
		res.ProgressScope = "validation"
		a.reportProgress(ctx, v, res)
		if e = a.verifyPostgreSQLRestore(ctx, m.PostgreSQL, v.Destination); e != nil {
			return res, e
		}
		res.PostgreSQL = m.PostgreSQL
		if v.PITR != nil {
			if e = a.recoverPITR(ctx, v, m, &res); e != nil {
				return res, e
			}
		}
		res.Phase = "complete"
	}
	res.Verified = true
	completeProgress(&res)
	return res, nil
}
func updateProgress(res *model.Result) {
	res.ProgressBytes = res.LogicalBytes
	p := 0.0
	if res.TotalBytes > 0 {
		p = 100 * float64(res.LogicalBytes) / float64(res.TotalBytes)
	} else if res.TotalFiles > 0 {
		p = 100 * float64(res.Files) / float64(res.TotalFiles)
	}
	if p > 99 {
		p = 99
	}
	res.ProgressPercent = &p
}
func completeProgress(res *model.Result) {
	p := 100.0
	res.ProgressPercent = &p
	res.ProgressScope = "operation"
}
func (a *Agent) release(v model.Execute) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, e := a.media(ctx, v, "POST", "/v1/operations/"+v.RunID+"/release", []byte("{}"))
	if e == nil {
		resp.Body.Close()
	}
}
