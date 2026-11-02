// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"errors"
	"gdata/srk/internal/kubernetes"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"net/http"
	"time"
)

func (m *Manager) kubernetesRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/kubernetes-checks", m.wrap(false, func(w http.ResponseWriter, r *http.Request, actor string) {
		policies, e := m.DB.KubernetesChecks(r.Context())
		if e != nil {
			Fail(w, 500, e)
			return
		}
		out := []map[string]any{}
		for _, p := range policies {
			last, e := m.DB.LatestKubernetesCheck(r.Context(), p.ID)
			if e != nil {
				Fail(w, 500, e)
				return
			}
			out = append(out, map[string]any{"policy": p, "last_run": last})
		}
		JSON(w, 200, out)
	}))
	mux.HandleFunc("POST /v1/kubernetes-checks", m.wrap(true, func(w http.ResponseWriter, r *http.Request, actor string) {
		var p model.KubernetesCheckPolicy
		if e := Decode(w, r, &p); e != nil {
			Fail(w, 400, e)
			return
		}
		if e := p.Validate(); e != nil {
			Fail(w, 400, e)
			return
		}
		job, e := m.DB.Job(r.Context(), p.JobID)
		if e != nil || (job.Type != "kubernetes" && job.Type != "longhorn") {
			Fail(w, 400, errors.New("Kubernetes/Longhorn job required"))
			return
		}
		p.ID = repository.Hash([]byte("kubernetes-check/v1/" + p.JobID + "/" + p.Profile + "/" + p.SourceNamespace))[:32]
		p.NextRun = time.Now().UTC()
		m.operationMu.Lock()
		defer m.operationMu.Unlock()
		if e = m.DB.PutKubernetesCheck(r.Context(), p); e != nil {
			Fail(w, 500, e)
			return
		}
		_ = m.DB.Audit(r.Context(), actor, "kubernetes.check.policy", p.ID, "updated")
		JSON(w, 200, p)
	}))

	mux.HandleFunc("POST /v1/runs/{id}/kubernetes-progress", func(w http.ResponseWriter, r *http.Request) {
		peer, e := Role(r, "agent")
		if e != nil {
			Fail(w, 403, e)
			return
		}
		run, e := m.DB.Run(r.Context(), r.PathValue("id"))
		if e != nil || run.Job.AgentID != peer.ID || run.Kind != "restore" || (run.Job.Type != "kubernetes" && run.Job.Type != "longhorn") {
			Fail(w, 403, errors.New("Kubernetes restore belongs to another agent"))
			return
		}
		target, e := kubernetes.ParseRestoreTarget(run.Destination)
		if e != nil {
			Fail(w, 400, e)
			return
		}
		var res model.Result
		if e = Decode(w, r, &res); e != nil {
			Fail(w, 400, e)
			return
		}
		st := res.KubernetesRestore
		if st == nil || st.Namespace != target.Namespace || st.SourceNamespace != target.Source || len(st.Events) > 200 || len(st.CurrentResource) > 512 || len(st.State) > 64 || len(res.Phase) > 80 || st.Objects < 0 || st.TotalObjects < 0 || st.TotalObjects > 10001 || st.Volumes < 0 || res.ProgressPercent != nil && (*res.ProgressPercent < 0 || *res.ProgressPercent > 100) {
			Fail(w, 400, errors.New("invalid Kubernetes restore progress"))
			return
		}
		for _, ev := range st.Events {
			if len(ev.Resource) > 512 || len(ev.Phase) > 64 || len(ev.At) > 64 {
				Fail(w, 400, errors.New("invalid restore event"))
				return
			}
		}
		res.Verified = false
		changed, e := m.DB.KubernetesProgress(r.Context(), run.ID, peer.ID, res)
		if e != nil {
			Fail(w, 500, e)
			return
		}
		if changed && (run.Result == nil || run.Result.Phase != res.Phase || run.Result.KubernetesRestore == nil || run.Result.KubernetesRestore.CurrentResource != st.CurrentResource) {
			_ = m.DB.Audit(r.Context(), "agent/"+peer.ID, "kubernetes.restore.progress", run.ID, res.Phase+" · "+st.Namespace+" · "+st.CurrentResource)
		}
		JSON(w, 200, map[string]bool{"accepted": changed})
	})
}
