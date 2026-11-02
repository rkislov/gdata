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

func (a *Agent) ovirt(v model.Execute) (*virtualization.OVirt, virtualization.Profile, string, error) {
	name, _, e := virtualization.Source(v.Job.Source)
	if e != nil {
		return nil, virtualization.Profile{}, "", e
	}
	if v.Kind == "restore" && v.VMRestore != nil && v.VMRestore.Profile != "" {
		name = v.VMRestore.Profile
	}
	p, ok := a.Config.Virtualization[name]
	if !ok || a.Config.Role != "proxy" {
		return nil, p, name, errors.New("oVirt proxy profile not configured")
	}
	c, e := virtualization.NewOVirt(p)
	return c, p, name, e
}
func (a *Agent) cleanupOVirt(c *virtualization.OVirt, j *virtualization.Journal, v model.Execute, res *model.Result) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res.Phase = "vm-cleanup"
	res.ProgressPercent = nil
	res.ProgressScope = "operation"
	a.vmReport(ctx, v, *res)
	return c.Cleanup(ctx, j, func(msg string) { a.vmEvent(ctx, v.RunID, "vm.cleanup", msg) })
}
func (a *Agent) backupOVirt(ctx context.Context, v model.Execute) (res model.Result, err error) {
	defer a.release(v)
	cloud, p, profile, e := a.ovirt(v)
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
		return res, errors.New("oVirt cleanup is pending; inspect proxy journal")
	}
	j, e := virtualization.OpenJournal(p, v.RunID, a.Config.ID, "backup")
	if e != nil {
		return res, e
	}
	defer j.Close()
	if j.Done {
		return res, errors.New("oVirt operation ID already used")
	}
	defer func() {
		if e := a.cleanupOVirt(cloud, j, v, &res); e != nil {
			err = errors.Join(err, fmt.Errorf("oVirt cleanup incomplete: %w", e))
		}
		if err == nil {
			res.Phase = "complete"
			completeProgress(&res)
			a.vmReport(ctx, v, res)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	dir, e := os.MkdirTemp(p.StagingDir, "srk-ovirt-")
	if e != nil {
		return res, e
	}
	defer os.RemoveAll(dir)
	res = model.Result{Phase: "vm-snapshot", EffectiveMode: "full", ProgressScope: "operation"}
	a.vmReport(ctx, v, res)
	a.vmEvent(ctx, v.RunID, "vm.snapshot", "starting full oVirt backup with raw imageio transfers")
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
	for i, disk := range meta.Disks {
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
func (a *Agent) restoreOVirt(ctx context.Context, v model.Execute, m model.Manifest) (res model.Result, err error) {
	cloud, p, _, e := a.ovirt(v)
	if e != nil {
		return res, e
	}
	defer cloud.Close()
	if v.VMRestore == nil {
		return res, errors.New("oVirt restore options required")
	}
	j, e := virtualization.OpenJournal(p, v.RunID, a.Config.ID, "restore")
	if e != nil {
		return res, e
	}
	defer j.Close()
	if j.Done {
		return res, errors.New("oVirt restore ID already used")
	}
	defer func() {
		if e := a.cleanupOVirt(cloud, j, v, &res); e != nil {
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
			return nil, errors.New("oVirt disk missing from manifest")
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

func (a *Agent) reportProgress(ctx context.Context, v model.Execute, res model.Result) {
	if virtualization.VMProvider(v.Job.Type) {
		a.vmReport(ctx, v, res)
	} else {
		a.progressUpdate(v.RunID, res)
	}
}
