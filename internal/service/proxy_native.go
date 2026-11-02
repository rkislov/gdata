// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"gdata/srk/internal/virtualization"
	"io"
	"os"
	"path/filepath"
	"time"
)

func (a *Agent) nativeCloud(v model.Execute) (virtualization.NativeAdapter, virtualization.Profile, string, error) {
	name, _, e := virtualization.Source(v.Job.Source)
	if e != nil {
		return nil, virtualization.Profile{}, "", e
	}
	if v.Kind == "restore" && v.VMRestore != nil && v.VMRestore.Profile != "" {
		name = v.VMRestore.Profile
	}
	p, ok := a.Config.Virtualization[name]
	if !ok || a.Config.Role != "proxy" {
		return nil, p, name, errors.New("native VM proxy profile not configured")
	}
	c, e := virtualization.NewNative(p)
	if pv, ok := c.(*virtualization.Proxmox); ok {
		pv.DAV = &proxyDAV{a: a, v: v}
	}
	return c, p, name, e
}
func (a *Agent) cleanupNative(c virtualization.NativeAdapter, j *virtualization.Journal, v model.Execute, res *model.Result) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res.Phase = "vm-cleanup"
	res.ProgressPercent = nil
	res.ProgressScope = "operation"
	a.vmReport(ctx, v, *res)
	return c.Cleanup(ctx, j, func(msg string) { a.vmEvent(ctx, v.RunID, "vm.cleanup", msg) })
}
func (a *Agent) backupNative(ctx context.Context, v model.Execute) (res model.Result, err error) {
	defer a.release(v)
	cloud, p, profile, e := a.nativeCloud(v)
	if e != nil {
		return res, e
	}
	defer cloud.Close()
	_, id, e := virtualization.Source(v.Job.Source)
	if e != nil {
		return res, e
	}
	unlock, e := a.vmLock(p.Endpoint + "/" + id)
	if e != nil {
		return res, e
	}
	defer unlock()
	pending, e := virtualization.Outstanding(p)
	if e != nil {
		return res, e
	}
	if len(pending) > 0 {
		return res, errors.New("native VM cleanup is pending; inspect proxy journal")
	}
	j, e := virtualization.OpenJournal(p, v.RunID, a.Config.ID, "backup")
	if e != nil {
		return res, e
	}
	defer j.Close()
	if j.Done {
		return res, errors.New("native VM operation ID already used")
	}
	defer func() {
		if e := a.cleanupNative(cloud, j, v, &res); e != nil {
			err = errors.Join(err, fmt.Errorf("native VM cleanup incomplete: %w", e))
		}
		if err == nil {
			res.Phase = "complete"
			completeProgress(&res)
			a.vmReport(ctx, v, res)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	dir, e := os.MkdirTemp(p.StagingDir, "srk-native-")
	if e != nil {
		return res, e
	}
	defer os.RemoveAll(dir)
	res = model.Result{Phase: "vm-snapshot", EffectiveMode: "full", ProgressScope: "operation"}
	a.vmReport(ctx, v, res)
	a.vmEvent(ctx, v.RunID, "vm.snapshot", "starting full native VM backup through HTTPS")
	policy := "crash-consistent"
	if v.Job.VM != nil {
		policy = v.Job.VM.Consistency
	}
	meta, e := cloud.Backup(ctx, j, profile, id, policy, dir, func(done, total int64) {
		res.Phase = "vm-export"
		res.LogicalBytes = done
		res.TotalBytes = total
		updateProgress(&res)
		a.vmReport(ctx, v, res)
	})
	if e != nil {
		return res, e
	}
	if meta.Archive != nil {
		digest, e := fileSHA(filepath.Join(dir, meta.Archive.File))
		if e != nil {
			return res, e
		}
		meta.Archive.SHA256 = digest
	}
	for i, disk := range meta.Disks {
		if meta.Archive != nil {
			break
		}
		digest, e := fileSHA(filepath.Join(dir, disk.File))
		if e != nil {
			return res, e
		}
		meta.Disks[i].SHA256 = digest
	}
	v.Base = ""
	v.Job.Mode = "full"
	res, e = a.backupFilesMetadata(ctx, v, dir, []string{p.StagingDir}, nil, nil, &meta)
	if e != nil {
		return res, e
	}
	if e = j.MarkSuccess(); e != nil {
		return res, e
	}
	return res, nil
}
func (a *Agent) restoreNative(ctx context.Context, v model.Execute, m model.Manifest) (res model.Result, err error) {
	cloud, p, _, e := a.nativeCloud(v)
	if e != nil {
		return res, e
	}
	defer cloud.Close()
	if v.VMRestore == nil {
		return res, errors.New("native VM restore options required")
	}
	j, e := virtualization.OpenJournal(p, v.RunID, a.Config.ID, "restore")
	if e != nil {
		return res, e
	}
	defer j.Close()
	if j.Done {
		return res, errors.New("native VM restore ID already used")
	}
	defer func() {
		if e := a.cleanupNative(cloud, j, v, &res); e != nil {
			err = errors.Join(err, e)
		}
		if err == nil {
			res.Phase = "complete"
			completeProgress(&res)
			a.vmReport(ctx, v, res)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	res = model.Result{VM: m.VM, Consistency: m.Consistency, Phase: "vm-restore-transfer", ProgressScope: "data-transfer", EffectiveMode: "restore"}
	for _, disk := range m.VM.Disks {
		res.TotalBytes += disk.Bytes
	}
	a.vmReport(ctx, v, res)
	files := map[string]model.File{}
	for _, f := range m.Files {
		files[f.Path] = f
	}
	restored, e := cloud.Restore(ctx, j, *m.VM, *v.VMRestore, func(d model.VMDisk) (io.ReadCloser, error) {
		f, ok := files[d.File]
		if !ok {
			return nil, errors.New("native VM disk missing from manifest")
		}
		return newVMReader(ctx, a, v, f, d.SHA256, &res), nil
	})
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

func (a *Agent) recoveryNativeCloud(p virtualization.Profile, j *virtualization.Journal) (virtualization.NativeAdapter, error) {
	cloud, e := virtualization.NewNative(p)
	if e != nil {
		return nil, e
	}
	if pv, ok := cloud.(*virtualization.Proxmox); ok {
		for _, r := range j.Resources {
			if r.Kind == "dav-session" {
				if !ValidEndpoint(r.Name) || r.Scope == "" {
					cloud.Close()
					return nil, errors.New("invalid media endpoint in native cleanup journal")
				}
				pv.DAV = &proxyDAV{a: a, v: model.Execute{RunID: j.ID, Kind: j.Kind, MediaURL: r.Name, Job: model.Job{MediaID: r.Scope}}}
				break
			}
		}
	}
	return cloud, nil
}
