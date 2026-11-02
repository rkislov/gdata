// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package kubernetes_test

import (
	"context"
	"gdata/srk/internal/kubernetes"
	"gdata/srk/internal/model"
	"strings"
	"testing"
	"time"
)

func TestCheckRestoreApplicationsAndCleanup(t *testing.T) {
	for _, scenario := range []string{"success", "http-fails", "pods-not-ready", "keep-failure", "delete-denied", "foreign-content", "namespace-replaced"} {
		t.Run(scenario, func(t *testing.T) {
			cloud, p, dir := restoreFixture(t)
			p.AllowVerification = true
			cloud.ProbeFail = scenario == "http-fails" || scenario == "keep-failure"
			cloud.NoReadyPods = scenario == "pods-not-ready"
			cloud.CleanupDenied = scenario == "delete-denied"
			cloud.ForeignContent = scenario == "foreign-content"
			cloud.ReplaceNamespace = scenario == "namespace-replaced"
			run := model.ID()
			policy := model.KubernetesCheckPolicy{ID: model.ID(), JobID: model.ID(), Profile: "lab", SourceNamespace: "demo", NamespacePrefix: "restore-check-", IntervalSeconds: 86400, TimeoutSeconds: 10, KeepOnFailure: scenario == "keep-failure", HTTP: []model.KubernetesHTTPCheck{{Service: "app", Port: 80, Path: "/health", ExpectedStatus: 200, Contains: "healthy"}}}
			// restoreFixture's Service has a different name and port; use no HTTP in
			// readiness/cleanup-only tests, and add an explicit selector-backed probe.
			target := kubernetes.RestoreTarget{Profile: "lab", Source: "demo", Namespace: policy.NamespacePrefix + run, Start: true}
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			_, result, err := kubernetes.CheckRestore(ctx, p, target, run, dir, policy, "snapshot", nil)
			if scenario == "success" {
				if err != nil || result.State != "passed" || result.Cleanup != "complete" || len(result.Checks) != 2 {
					t.Fatal(result, err)
				}
			} else if err == nil || result.State != "failed" {
				t.Fatal("expected failure", result, err)
			}
			if scenario == "keep-failure" {
				if result.Cleanup != "retained" {
					t.Fatal(result)
				}
			} else if scenario == "delete-denied" || scenario == "foreign-content" || scenario == "namespace-replaced" {
				if result.Cleanup != "failed" {
					t.Fatal(result)
				}
			} else {
				if result.Cleanup != "complete" || cloud.Restored("/api/v1/namespaces/"+target.Namespace) != nil {
					t.Fatal("cleanup incomplete", result)
				}
			}
			for _, action := range cloud.RestoreActions() {
				if strings.HasPrefix(action, "DELETE") && strings.Contains(action, "demo") {
					t.Fatal("source deletion", action)
				}
			}
		})
	}
}
