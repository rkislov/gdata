// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

type KubernetesHTTPCheck struct {
	Service        string `json:"service"`
	Port           int    `json:"port"`
	Path           string `json:"path"`
	ExpectedStatus int    `json:"expected_status"`
	Contains       string `json:"contains,omitempty"`
}
type KubernetesCheckPolicy struct {
	ID              string                `json:"id"`
	JobID           string                `json:"job_id"`
	Profile         string                `json:"profile"`
	SourceNamespace string                `json:"source_namespace"`
	NamespacePrefix string                `json:"namespace_prefix"`
	IntervalSeconds int                   `json:"interval_seconds"`
	TimeoutSeconds  int                   `json:"timeout_seconds"`
	KeepOnFailure   bool                  `json:"keep_on_failure"`
	HTTP            []KubernetesHTTPCheck `json:"http,omitempty"`
	Enabled         bool                  `json:"enabled"`
	NextRun         time.Time             `json:"next_run"`
}

func (p KubernetesCheckPolicy) Validate() error {
	name := regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)
	if len(p.JobID) != 32 || len(p.Profile) > 64 || !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`).MatchString(p.Profile) || len(p.SourceNamespace) > 63 || !regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`).MatchString(p.SourceNamespace) ||
		len(p.NamespacePrefix) > 30 || !strings.HasSuffix(p.NamespacePrefix, "check-") || !name.MatchString(strings.TrimSuffix(p.NamespacePrefix, "-")) ||
		p.IntervalSeconds < 600 || p.IntervalSeconds > 31536000 || p.TimeoutSeconds < 10 || p.TimeoutSeconds > 7200 || len(p.HTTP) > 20 {
		return errors.New("invalid verification policy: interval 600..31536000s, timeout 10..7200s, prefix ending check- up to 30 characters")
	}
	for _, h := range p.HTTP {
		if !regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`).MatchString(h.Service) || len(h.Service) > 63 || h.Port < 1 || h.Port > 65535 || !strings.HasPrefix(h.Path, "/") || strings.ContainsAny(h.Path, "?#%\r\n") || strings.Contains(h.Path, "..") || len(h.Path) > 1024 || h.ExpectedStatus < 200 || h.ExpectedStatus > 299 || len(h.Contains) > 4096 {
			return errors.New("invalid HTTP service check")
		}
	}
	return nil
}

type KubernetesCheckResult struct {
	PolicyID        string                  `json:"policy_id"`
	SnapshotID      string                  `json:"snapshot_id"`
	Namespace       string                  `json:"namespace"`
	StartedAt       time.Time               `json:"started_at"`
	FinishedAt      *time.Time              `json:"finished_at,omitempty"`
	DurationSeconds float64                 `json:"duration_seconds"`
	State           string                  `json:"state"`
	ReadyPods       int                     `json:"ready_pods"`
	Checks          []KubernetesProbeResult `json:"checks"`
	Cleanup         string                  `json:"cleanup"`
	Error           string                  `json:"error,omitempty"`
}
type KubernetesProbeResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}
