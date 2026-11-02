// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"errors"
	"gdata/srk/internal/config"
	"gdata/srk/internal/kubernetes"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func (a *Agent) backupKubernetes(ctx context.Context, v model.Execute) (model.Result, error) {
	name, e := config.DataSource(v.Job.Source, v.Job.Type)
	if e != nil {
		return model.Result{}, e
	}
	p, ok := a.Config.Kubernetes[name]
	if !ok || (v.Job.Type == "longhorn" && p.LonghornSnapshotClass == "") {
		return model.Result{}, errors.New("Kubernetes/Longhorn profile is not configured")
	}
	dir, e := os.MkdirTemp(p.StagingDir, "srk-kubernetes-")
	if e != nil {
		return model.Result{}, e
	}
	defer os.RemoveAll(dir)
	idx, e := kubernetes.Backup(ctx, p, v.RunID, dir, func(phase string, n int) {
		a.progressUpdate(v.RunID, model.Result{Phase: phase, Files: n, ProgressScope: phase})
	})
	if e != nil {
		return model.Result{}, e
	}
	meta := &model.DataBackup{Kind: v.Job.Type, Profile: name, Kubernetes: &model.KubernetesBackup{Namespaces: idx.Namespaces, Objects: idx.Objects, VolumeBackups: len(idx.Volumes), PVCData: idx.PVCData, IncludesSecrets: idx.IncludesSecrets}}
	v.Base = ""
	v.Job.Mode = "full"
	return a.backupFilesData(ctx, v, dir, []string{p.StagingDir}, nil, meta)
}

func (a *Agent) kubernetesReport(ctx context.Context, v model.Execute, res model.Result) {
	if res.KubernetesRestore != nil {
		copy := *res.KubernetesRestore
		copy.Events = append([]model.KubernetesRestoreEvent{}, copy.Events...)
		res.KubernetesRestore = &copy
	}
	a.progressUpdate(v.RunID, res)
	if a.Config.ManagerURL != "" {
		reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = Call(reportCtx, a.Client, "POST", a.Config.ManagerURL+"/v1/runs/"+v.RunID+"/kubernetes-progress", res, nil)
	}
}
func (a *Agent) restoreKubernetes(ctx context.Context, v model.Execute, m model.Manifest) (res model.Result, err error) {
	target, e := kubernetes.ParseRestoreTarget(v.Destination)
	if e != nil {
		return res, e
	}
	p, ok := a.Config.Kubernetes[target.Profile]
	if !ok || !p.AllowRestore {
		return res, errors.New("Kubernetes target profile does not allow restore")
	}
	if m.Data == nil || m.Data.Kubernetes == nil || !slices.Contains(m.Data.Kubernetes.Namespaces, target.Source) {
		return res, errors.New("source namespace is absent from backup")
	}
	if !strings.HasPrefix(target.Namespace, p.RestoreNamespacePrefix) {
		return res, errors.New("target namespace is outside profile prefix")
	}
	res = model.Result{Data: m.Data, EffectiveMode: "restore", Consistency: m.Consistency, Phase: "kubernetes-restore-download", ProgressScope: "data-transfer", KubernetesRestore: &model.KubernetesRestore{Namespace: target.Namespace, SourceNamespace: target.Source, WorkloadsStarted: target.Start, State: "download"}}
	if v.KubernetesCheck != nil {
		res.KubernetesCheck = &model.KubernetesCheckResult{PolicyID: v.KubernetesCheck.ID, SnapshotID: v.SnapshotID, Namespace: target.Namespace, State: "restoring", Cleanup: "pending", StartedAt: time.Now().UTC()}
	}
	defer func() {
		if err != nil {
			if res.KubernetesCheck != nil {
				res.KubernetesCheck.State = "failed"
				res.KubernetesCheck.Error = err.Error()
				if res.KubernetesCheck.FinishedAt == nil {
					now := time.Now().UTC()
					res.KubernetesCheck.FinishedAt = &now
					res.KubernetesCheck.DurationSeconds = now.Sub(res.KubernetesCheck.StartedAt).Seconds()
					if res.KubernetesCheck.Cleanup == "pending" {
						res.KubernetesCheck.Cleanup = "not-needed"
					}
				}
			}
			res.Verified = false
			res.Phase = "kubernetes-restore-failed"
			a.kubernetesReport(ctx, v, res)
		}
	}()
	dir, e := os.MkdirTemp(p.StagingDir, "srk-kubernetes-restore-")
	if e != nil {
		return res, e
	}
	defer os.RemoveAll(dir)
	for _, f := range m.Files {
		if f.Size < 0 || f.Size > p.MaxBytes-res.TotalBytes {
			return res, errors.New("restore staging quota exceeded")
		}
		res.TotalBytes += f.Size
	}
	a.kubernetesReport(ctx, v, res)
	for _, f := range m.Files {
		if f.Kind != "file" || !repository.ValidPath(f.Path) || strings.Contains(f.Path, "/") {
			return res, errors.New("invalid Kubernetes archive file")
		}
		out, e := os.OpenFile(filepath.Join(dir, f.Path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return res, e
		}
		var n int64
		for _, hash := range f.Chunks {
			response, e := a.media(ctx, v, "GET", "/v1/chunks/"+hash, nil)
			if e != nil {
				out.Close()
				return res, e
			}
			b, e := io.ReadAll(io.LimitReader(response.Body, repository.ChunkSize+1))
			response.Body.Close()
			if e != nil || int64(len(b)) > f.Size-n || repository.Hash(b) != hash {
				out.Close()
				return res, errors.New("Kubernetes restore block checksum or length failed")
			}
			if _, e = out.Write(b); e != nil {
				out.Close()
				return res, e
			}
			n += int64(len(b))
			res.LogicalBytes += int64(len(b))
		}
		if e = out.Sync(); e != nil {
			out.Close()
			return res, e
		}
		if e = out.Close(); e != nil {
			return res, e
		}
		if n != f.Size {
			return res, errors.New("Kubernetes restore file size mismatch")
		}
		res.Files++
		updateProgress(&res)
		a.kubernetesReport(ctx, v, res)
	}
	res.ProgressPercent = nil
	res.ProgressScope = "kubernetes-restore"
	update := func(st model.KubernetesRestore) {
		res.KubernetesRestore = &st
		res.Phase = "kubernetes-restore-" + st.State
		if res.KubernetesCheck != nil {
			res.Phase = "kubernetes-check-" + res.KubernetesCheck.State
			if res.KubernetesCheck.Cleanup == "running" {
				res.Phase = "kubernetes-check-cleanup"
			}
		}
		if st.TotalObjects > 0 {
			percent := float64(st.Objects) * 100 / float64(st.TotalObjects)
			if percent > 99 {
				percent = 99
			}
			res.ProgressPercent = &percent
		}
		a.kubernetesReport(ctx, v, res)
	}
	var state model.KubernetesRestore
	if v.KubernetesCheck != nil {
		var check model.KubernetesCheckResult
		state, check, e = kubernetes.CheckRestore(ctx, p, target, v.RunID, dir, *v.KubernetesCheck, v.SnapshotID, func(st model.KubernetesRestore, cr *model.KubernetesCheckResult) {
			res.KubernetesCheck = cr
			if st.Namespace == "" {
				st = *res.KubernetesRestore
			}
			update(st)
		})
		res.KubernetesCheck = &check
	} else {
		state, e = kubernetes.Restore(ctx, p, target, v.RunID, dir, update)
	}
	res.KubernetesRestore = &state
	if e != nil {
		return res, e
	}
	res.Verified = true
	res.Phase = "complete"
	completeProgress(&res)
	a.kubernetesReport(ctx, v, res)
	return res, nil
}
