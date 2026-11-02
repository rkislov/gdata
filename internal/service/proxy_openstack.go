// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"
	"time"

	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"gdata/srk/internal/virtualization"
)

func (a *Agent) openStack(v model.Execute) (*virtualization.OpenStack, virtualization.Profile, string, error) {
	profile, _, e := virtualization.Source(v.Job.Source)
	if v.Kind == "restore" && v.VMRestore != nil && v.VMRestore.Profile != "" {
		profile = v.VMRestore.Profile
		e = nil
	}
	if e != nil {
		return nil, virtualization.Profile{}, "", e
	}
	p, ok := a.Config.Virtualization[profile]
	if !ok || a.Config.Role != "proxy" {
		return nil, p, profile, errors.New("OpenStack proxy profile not found")
	}
	cl, e := virtualization.NewOpenStack(p)
	return cl, p, profile, e
}
func (a *Agent) vmReport(ctx context.Context, v model.Execute, res model.Result) {
	if res.VM != nil {
		b, _ := json.Marshal(res.VM)
		var copy model.VMBackup
		_ = json.Unmarshal(b, &copy)
		res.VM = &copy
	}
	a.progressUpdate(v.RunID, res)
	if a.Config.ManagerURL == "" {
		return
	}
	a.mu.Lock()
	old := a.vmReports[v.RunID]
	now := time.Now()
	send := old.phase != res.Phase || now.Sub(old.at) > time.Second
	if send {
		a.vmReports[v.RunID] = vmReportStamp{at: now, phase: res.Phase}
	}
	a.mu.Unlock()
	if send {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = Call(c, a.Client, "POST", a.Config.ManagerURL+"/v1/runs/"+v.RunID+"/progress", res, nil)
	}
}
func (a *Agent) vmEvent(ctx context.Context, id, action, message string) {
	if a.Config.ManagerURL == "" {
		return
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = Call(c, a.Client, "POST", a.Config.ManagerURL+"/v1/runs/"+id+"/events", map[string]string{"action": action, "message": message}, nil)
}

type vmReportStamp struct {
	at    time.Time
	phase string
}

func (a *Agent) vmLock(key string) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.vmLocks[key] {
		return nil, errors.New("VM already has an active proxy operation")
	}
	a.vmLocks[key] = true
	return func() { a.mu.Lock(); delete(a.vmLocks, key); a.mu.Unlock() }, nil
}
func (a *Agent) cleanupVM(c *virtualization.OpenStack, j *virtualization.Journal, v model.Execute, res *model.Result) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res.Phase = "vm-cleanup"
	res.ProgressScope = "operation"
	res.ProgressPercent = nil
	a.vmReport(ctx, v, *res)
	e := c.Cleanup(ctx, j, func(message string) { a.vmEvent(ctx, v.RunID, "vm.cleanup", message) })
	if e != nil {
		a.vmEvent(ctx, v.RunID, "vm.cleanup_error", e.Error())
	}
	return e
}
func (a *Agent) backupOpenStack(ctx context.Context, v model.Execute) (res model.Result, err error) {
	defer a.release(v)
	c, p, profile, e := a.openStack(v)
	if e != nil {
		return res, e
	}
	defer c.Close()
	_, vmID, e := virtualization.Source(v.Job.Source)
	if e != nil {
		return res, e
	}
	unlock, e := a.vmLock(p.Endpoint + "/" + vmID)
	if e != nil {
		return res, e
	}
	defer unlock()
	pending, e := virtualization.Outstanding(p)
	if e != nil {
		return res, e
	}
	for _, j := range pending {
		if j.ID != v.RunID {
			return res, errors.New("proxy has pending cloud cleanup; inspect virtualization journal first")
		}
	}
	j, e := virtualization.OpenJournal(p, v.RunID, a.Config.ID, "backup")
	if e != nil {
		return res, e
	}
	defer j.Close()
	if j.Done {
		return res, errors.New("VM operation ID was already used")
	}
	defer func() {
		if e := a.cleanupVM(c, j, v, &res); e != nil {
			err = errors.Join(err, fmt.Errorf("temporary cloud cleanup incomplete: %w", e))
		}
		if err == nil {
			res.Phase = "complete"
			completeProgress(&res)
			a.vmReport(ctx, v, res)
		}
	}()
	timeout := p.TimeoutSeconds
	if timeout == 0 {
		timeout = 7200
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	res = model.Result{Phase: "vm-snapshot", ProgressScope: "operation", EffectiveMode: "full"}
	a.vmReport(ctx, v, res)
	policy := "crash-consistent"
	if v.Job.VM != nil {
		policy = v.Job.VM.Consistency
	}
	backup, exports, e := c.Prepare(ctx, j, profile, vmID, policy, func(msg string) { a.vmEvent(ctx, v.RunID, "vm.snapshot", msg) })
	if e != nil {
		return res, e
	}
	res.VM = &backup
	res.Consistency = backup.Consistency
	res.TotalFiles = len(exports)
	for _, x := range exports {
		if x.Disk.Bytes <= 0 || x.Disk.Bytes > model.MaxCaptureBytes-res.TotalBytes {
			return res, errors.New("VM export exceeds the supported 4 TiB payload bound")
		}
		res.TotalBytes += x.Disk.Bytes
	}
	manifest := model.Manifest{Version: 1, ID: v.RunID, JobID: v.Job.ID, Source: v.Job.Source, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().AddDate(0, 0, v.Job.RetentionDays), Mode: "full", Consistency: res.Consistency, VM: &backup, Files: []model.File{}}
	res.Phase = "vm-transfer"
	res.ProgressScope = "data-transfer"
	for i, x := range exports {
		stream, e := c.Download(ctx, x.Image)
		if e != nil {
			return res, e
		}
		file, digest, e := a.uploadVMStream(ctx, v, x, stream, &res)
		closeErr := stream.Close()
		if e != nil {
			return res, e
		}
		if closeErr != nil {
			return res, closeErr
		}
		backup.Disks[i].SHA256 = digest
		manifest.Files = append(manifest.Files, file)
		res.Files++
		res.ChangedFiles++
	}
	res.Phase = "vm-validation"
	res.ProgressScope = "validation"
	res.ProgressPercent = nil
	res.VM = &backup
	a.vmReport(ctx, v, res)
	manifest.Result = res
	data, e := json.Marshal(manifest)
	if e != nil {
		return res, e
	}
	resp, e := a.media(ctx, v, "POST", "/v1/snapshots/"+v.RunID, data)
	if e != nil {
		return res, e
	}
	var ack struct {
		Verified     bool   `json:"verified"`
		RepositoryID string `json:"repository_id"`
	}
	e = json.NewDecoder(resp.Body).Decode(&ack)
	resp.Body.Close()
	if e != nil || !ack.Verified {
		return res, errors.New("media did not confirm the VM snapshot integrity")
	}
	res.Verified = true
	res.RepositoryID = ack.RepositoryID
	if e = j.MarkSuccess(); e != nil {
		return res, e
	}
	return res, nil
}
func (a *Agent) uploadVMStream(ctx context.Context, v model.Execute, x virtualization.Export, reader io.Reader, res *model.Result) (model.File, string, error) {
	f := model.File{Path: x.Disk.File, Kind: "file", Mode: 0600, Size: x.Disk.Bytes, ModTime: time.Now().UTC(), Chunks: []string{}}
	digest := sha256.New()
	var verify hash.Hash
	expected := ""
	if x.Image.HashAlgo == "sha512" && x.Image.HashValue != "" {
		verify = sha512.New()
		expected = x.Image.HashValue
	} else if x.Image.HashAlgo == "sha256" && x.Image.HashValue != "" {
		verify = sha256.New()
		expected = x.Image.HashValue
	} else if x.Image.Checksum != "" {
		verify = md5.New()
		expected = x.Image.Checksum
	} else {
		return f, "", errors.New("Glance image has no supported integrity checksum")
	}
	input := io.TeeReader(reader, io.MultiWriter(digest, verify))
	buf := make([]byte, repository.ChunkSize)
	for read := int64(0); read < f.Size; {
		if e := ctx.Err(); e != nil {
			return f, "", e
		}
		size := min(int64(len(buf)), f.Size-read)
		n, e := io.ReadFull(input, buf[:size])
		if e != nil {
			return f, "", fmt.Errorf("truncated VM image: %w", e)
		}
		if read == 0 && x.Disk.DiskFormat == "qcow2" {
			if e = standaloneQCOW2(buf[:n]); e != nil {
				return f, "", e
			}
		}
		b := buf[:n]
		h := repository.Hash(b)
		f.Chunks = append(f.Chunks, h)
		head, e := a.media(ctx, v, "HEAD", "/v1/chunks/"+h, nil)
		if e != nil {
			return f, "", e
		}
		head.Body.Close()
		if head.StatusCode == 404 {
			put, e := a.media(ctx, v, "PUT", "/v1/chunks/"+h, b)
			if e != nil {
				return f, "", e
			}
			put.Body.Close()
			res.UploadedBytes += int64(n)
		}
		read += int64(n)
		res.LogicalBytes += int64(n)
		res.Chunks++
		updateProgress(res)
		a.vmReport(ctx, v, *res)
	}
	var extra [1]byte
	if n, e := input.Read(extra[:]); n != 0 || e != io.EOF {
		return f, "", errors.New("VM image exceeds declared size or stream failed")
	}
	if !strings.EqualFold(hex.EncodeToString(verify.Sum(nil)), expected) {
		return f, "", errors.New("Glance image checksum mismatch")
	}
	return f, hex.EncodeToString(digest.Sum(nil)), nil
}
func standaloneQCOW2(b []byte) error {
	if len(b) < 72 || !bytes.Equal(b[:4], []byte{'Q', 'F', 'I', 0xfb}) {
		return errors.New("invalid qcow2 header")
	}
	version := binary.BigEndian.Uint32(b[4:8])
	if version != 2 && version != 3 {
		return errors.New("unsupported qcow2 version")
	}
	if binary.BigEndian.Uint64(b[8:16]) != 0 || binary.BigEndian.Uint32(b[16:20]) != 0 {
		return errors.New("qcow2 snapshot depends on an external backing file")
	}
	if version == 3 && (len(b) < 104 || binary.BigEndian.Uint64(b[72:80])&7 != 0) {
		return errors.New("qcow2 image has dirty, corrupt or external-data features")
	}
	return nil
}
func (a *Agent) restoreOpenStack(ctx context.Context, v model.Execute, manifest model.Manifest) (res model.Result, err error) {
	c, p, _, e := a.openStack(v)
	if e != nil {
		return res, e
	}
	defer c.Close()
	if v.VMRestore == nil {
		return res, errors.New("new VM restore options are required")
	}
	if e = v.VMRestore.Validate(); e != nil {
		return res, e
	}
	j, e := virtualization.OpenJournal(p, v.RunID, a.Config.ID, "restore")
	if e != nil {
		return res, e
	}
	defer j.Close()
	if j.Done {
		return res, errors.New("restore operation ID was already used")
	}
	defer func() {
		if e := a.cleanupVM(c, j, v, &res); e != nil {
			err = errors.Join(err, fmt.Errorf("restore cleanup incomplete: %w", e))
		}
		if err == nil {
			res.Phase = "complete"
			completeProgress(&res)
			a.vmReport(ctx, v, res)
		}
	}()
	timeout := p.TimeoutSeconds
	if timeout == 0 {
		timeout = 7200
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	res = model.Result{VM: manifest.VM, Phase: "vm-restore-validation", ProgressScope: "validation", EffectiveMode: "restore", Consistency: manifest.Consistency, TotalFiles: len(manifest.Files)}
	for _, f := range manifest.Files {
		res.TotalBytes += f.Size
	}
	a.vmReport(ctx, v, res)
	verify, e := a.media(ctx, v, "POST", "/v1/snapshots/"+v.SnapshotID+"/verify", nil)
	if e != nil {
		return res, e
	}
	verify.Body.Close()
	files := map[string]model.File{}
	for _, f := range manifest.Files {
		files[f.Path] = f
	}
	res.Phase = "vm-restore-transfer"
	res.ProgressScope = "data-transfer"
	restored, e := c.Restore(ctx, j, *manifest.VM, *v.VMRestore, func(d model.VMDisk) (io.ReadCloser, error) {
		f, ok := files[d.File]
		if !ok {
			return nil, errors.New("disk is absent from verified manifest")
		}
		return newVMReader(ctx, a, v, f, d.SHA256, &res), nil
	}, func(msg string) { a.vmEvent(ctx, v.RunID, "vm.restore", msg) })
	if e != nil {
		return res, e
	}
	res.VMRestore = &restored
	res.Verified = true
	if e = j.MarkSuccess(); e != nil {
		return res, e
	}
	return res, nil
}

// Verified blocks are streamed into Glance without creating a local image file.
type vmReader struct {
	mu       sync.Mutex
	closed   bool
	ctx      context.Context
	a        *Agent
	v        model.Execute
	file     model.File
	expected string
	res      *model.Result
	index    int
	buffer   []byte
	read     int64
	digest   hash.Hash
	done     bool
	err      error
}

func newVMReader(ctx context.Context, a *Agent, v model.Execute, f model.File, expected string, res *model.Result) *vmReader {
	return &vmReader{ctx: ctx, a: a, v: v, file: f, expected: expected, res: res, digest: sha256.New()}
}
func (r *vmReader) Read(dst []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	if r.err != nil {
		return 0, r.err
	}
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	if len(r.buffer) == 0 {
		if r.index == len(r.file.Chunks) {
			if r.read != r.file.Size || hex.EncodeToString(r.digest.Sum(nil)) != r.expected {
				r.err = errors.New("restored disk digest mismatch")
				return 0, r.err
			}
			r.done = true
			return 0, io.EOF
		}
		hash := r.file.Chunks[r.index]
		resp, e := r.a.media(r.ctx, r.v, "GET", "/v1/chunks/"+hash, nil)
		if e != nil {
			return 0, e
		}
		b, e := io.ReadAll(io.LimitReader(resp.Body, repository.ChunkSize+1))
		resp.Body.Close()
		if e != nil {
			return 0, e
		}
		size := min(int64(repository.ChunkSize), r.file.Size-r.read)
		if int64(len(b)) != size || repository.Hash(b) != hash {
			return 0, errors.New("restored VM block is corrupt")
		}
		r.buffer = b
		r.index++
	}
	n := copy(dst, r.buffer)
	r.buffer = r.buffer[n:]
	r.read += int64(n)
	_, _ = r.digest.Write(dst[:n])
	r.res.LogicalBytes += int64(n)
	updateProgress(r.res)
	r.a.vmReport(r.ctx, r.v, *r.res)
	return n, nil
}
func (r *vmReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.err != nil {
		return r.err
	}
	if r.read != r.file.Size || len(r.buffer) != 0 || hex.EncodeToString(r.digest.Sum(nil)) != r.expected {
		return errors.New("Glance did not consume the complete verified disk")
	}
	return nil
}

func (a *Agent) recoverVMJournals(ctx context.Context) error {
	for _, p := range a.Config.Virtualization {
		if !p.BackupEnabled || !virtualization.VMProvider(p.Kind) {
			continue
		}
		pending, e := virtualization.Outstanding(p)
		if e != nil {
			return e
		}
		if len(pending) == 0 {
			continue
		}
		if virtualization.NativeProvider(p.Kind) {
			for _, old := range pending {
				j, e := virtualization.OpenJournal(p, old.ID, a.Config.ID, old.Kind)
				if e != nil {
					return e
				}
				cloud, e := a.recoveryNativeCloud(p, j)
				if e != nil {
					j.Close()
					return e
				}
				cleanup, cancel := context.WithTimeout(ctx, 5*time.Minute)
				e = cloud.Cleanup(cleanup, j, nil)
				cancel()
				j.Close()
				cloud.Close()
				if e != nil {
					return e
				}
			}
			continue
		}

		if p.Kind == "ovirt" {
			cloud, e := virtualization.NewOVirt(p)
			if e != nil {
				return e
			}
			for _, old := range pending {
				j, e := virtualization.OpenJournal(p, old.ID, a.Config.ID, old.Kind)
				if e != nil {
					cloud.Close()
					return e
				}
				cleanup, cancel := context.WithTimeout(ctx, 5*time.Minute)
				e = cloud.Cleanup(cleanup, j, nil)
				cancel()
				j.Close()
				if e != nil {
					cloud.Close()
					return e
				}
			}
			cloud.Close()
			continue
		}
		cloud, e := virtualization.NewOpenStack(p)
		if e != nil {
			return e
		}
		for _, old := range pending {
			j, e := virtualization.OpenJournal(p, old.ID, a.Config.ID, old.Kind)
			if e != nil {
				cloud.Close()
				return e
			}
			cleanup, cancel := context.WithTimeout(ctx, 5*time.Minute)
			e = cloud.Cleanup(cleanup, j, nil)
			cancel()
			j.Close()
			if e != nil {
				cloud.Close()
				return fmt.Errorf("pending VM operation %s requires cleanup: %w", old.ID, e)
			}
		}
		cloud.Close()
	}
	return nil
}
