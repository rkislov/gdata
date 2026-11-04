// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

const Version = "0.10.0"

func ID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}

type Job struct {
	VM              *VMOptions `json:"vm,omitempty"`
	Type            string     `json:"type,omitempty"`
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	AgentID         string     `json:"agent_id"`
	MediaID         string     `json:"media_id"`
	Source          string     `json:"source"`
	Mode            string     `json:"mode"`
	IntervalSeconds int        `json:"interval_seconds"`
	RetentionDays   int        `json:"retention_days"`
	Enabled         bool       `json:"enabled"`
	CreatedAt       time.Time  `json:"created_at"`
	NextRun         time.Time  `json:"next_run"`
	Version         int        `json:"version"`
}
type Run struct {
	RestoreTargetID   string                 `json:"restore_target_id,omitempty"`
	RestoreSiteID     string                 `json:"restore_site_id,omitempty"`
	KubernetesCheck   *KubernetesCheckPolicy `json:"kubernetes_check,omitempty"`
	Tape              *TapeRequest           `json:"tape,omitempty"`
	WALReplication    *WALReplicationPolicy  `json:"wal_replication,omitempty"`
	Replication       *Replication           `json:"replication,omitempty"`
	VMRestore         *VMRestoreRequest      `json:"vm_restore,omitempty"`
	Capture           *DiskCapture           `json:"capture,omitempty"`
	PITR              *PITRRequest           `json:"pitr,omitempty"`
	Options           *MaintenanceOptions    `json:"options,omitempty"`
	Maintenance       *MaintenanceResult     `json:"maintenance,omitempty"`
	Origin            *Origin                `json:"origin,omitempty"`
	SnapshotExpiresAt *time.Time             `json:"snapshot_expires_at,omitempty"`
	SnapshotState     string                 `json:"snapshot_state,omitempty"`
	ID                string                 `json:"id"`
	JobID             string                 `json:"job_id"`
	Job               Job                    `json:"job"`
	Kind              string                 `json:"kind"`
	Status            string                 `json:"status"`
	CreatedAt         time.Time              `json:"created_at"`
	StartedAt         *time.Time             `json:"started_at,omitempty"`
	FinishedAt        *time.Time             `json:"finished_at,omitempty"`
	Error             string                 `json:"error,omitempty"`
	Result            *Result                `json:"result,omitempty"`
	SnapshotID        string                 `json:"snapshot_id,omitempty"`
	Destination       string                 `json:"destination,omitempty"`
}
type Result struct {
	KubernetesCheck   *KubernetesCheckResult `json:"kubernetes_check,omitempty"`
	KubernetesRestore *KubernetesRestore     `json:"kubernetes_restore,omitempty"`
	Tape              *TapeResult            `json:"tape,omitempty"`
	WALReplication    *WALReplicationResult  `json:"wal_replication,omitempty"`
	Replica           *Replica               `json:"replica,omitempty"`
	Data              *DataBackup            `json:"data,omitempty"`
	VM                *VMBackup              `json:"vm,omitempty"`
	VMRestore         *VMRestoreResult       `json:"vm_restore,omitempty"`
	Disk              *DiskGeometry          `json:"disk,omitempty"`
	PITR              *PITRResult            `json:"pitr,omitempty"`
	TotalBytes        int64                  `json:"total_bytes,omitempty"`
	ProgressBytes     int64                  `json:"progress_bytes,omitempty"`
	TotalFiles        int                    `json:"total_files,omitempty"`
	ProgressPercent   *float64               `json:"progress_percent"`
	ProgressScope     string                 `json:"progress_scope,omitempty"`
	PostgreSQL        *PostgreSQLBackup      `json:"postgresql,omitempty"`
	Phase             string                 `json:"phase,omitempty"`
	RepositoryID      string                 `json:"repository_id,omitempty"`
	Files             int                    `json:"files"`
	ChangedFiles      int                    `json:"changed_files"`
	LogicalBytes      int64                  `json:"logical_bytes"`
	UploadedBytes     int64                  `json:"uploaded_bytes"`
	Chunks            int                    `json:"chunks"`
	Verified          bool                   `json:"verified"`
	Consistency       string                 `json:"consistency"`
	EffectiveMode     string                 `json:"effective_mode"`
}
type Node struct {
	Certificate *CertificateObservation `json:"certificate,omitempty"`
	ID          string                  `json:"id"`
	Role        string                  `json:"role"`
	URL         string                  `json:"url"`
	LastSeen    time.Time               `json:"last_seen"`
	Status      map[string]any          `json:"status"`
}
type Audit struct {
	ID     string    `json:"id"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Object string    `json:"object"`
	Result string    `json:"result"`
}
type File struct {
	Path    string    `json:"path"`
	Kind    string    `json:"kind"`
	Mode    uint32    `json:"mode"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	Link    string    `json:"link,omitempty"`
	Chunks  []string  `json:"chunks,omitempty"`
}
type Manifest struct {
	Data        *DataBackup       `json:"data,omitempty"`
	VM          *VMBackup         `json:"vm,omitempty"`
	Disk        *DiskGeometry     `json:"disk,omitempty"`
	PostgreSQL  *PostgreSQLBackup `json:"postgresql,omitempty"`
	Version     int               `json:"version"`
	ID          string            `json:"id"`
	JobID       string            `json:"job_id"`
	Source      string            `json:"source"`
	CreatedAt   time.Time         `json:"created_at"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Mode        string            `json:"mode"`
	Base        string            `json:"base,omitempty"`
	Consistency string            `json:"consistency"`
	Files       []File            `json:"files"`
	Result      Result            `json:"result"`
}
type Execute struct {
	KubernetesCheck *KubernetesCheckPolicy `json:"kubernetes_check,omitempty"`
	VMRestore       *VMRestoreRequest      `json:"vm_restore,omitempty"`
	PITR            *PITRRequest           `json:"pitr,omitempty"`
	RunID           string                 `json:"run_id"`
	Kind            string                 `json:"kind"`
	Job             Job                    `json:"job"`
	MediaURL        string                 `json:"media_url"`
	Grant           string                 `json:"grant"`
	Base            string                 `json:"base,omitempty"`
	SnapshotID      string                 `json:"snapshot_id,omitempty"`
	Destination     string                 `json:"destination,omitempty"`
}
