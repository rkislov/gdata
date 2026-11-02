// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

import "time"

type RepositoryInfo struct {
	ID        string    `json:"id"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}
type SnapshotInfo struct {
	ID           string    `json:"id"`
	JobID        string    `json:"job_id"`
	Source       string    `json:"source"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Mode         string    `json:"mode"`
	Consistency  string    `json:"consistency"`
	Hold         bool      `json:"hold"`
	HoldReason   string    `json:"hold_reason,omitempty"`
	ManifestHash string    `json:"manifest_hash"`
	Result       Result    `json:"result"`
	Verified     bool      `json:"verified"`
}
type SnapshotPolicy struct {
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Hold      *bool      `json:"hold,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}
type Lease struct {
	ID         string    `json:"id"`
	Subject    string    `json:"subject"`
	Kind       string    `json:"kind"`
	SnapshotID string    `json:"snapshot_id,omitempty"`
	Base       string    `json:"base,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
	Closed     bool      `json:"closed"`
}
type GCPlan struct {
	RepositoryID  string    `json:"repository_id"`
	At            time.Time `json:"at"`
	Token         string    `json:"token"`
	Snapshots     []string  `json:"snapshots"`
	Chunks        []string  `json:"chunks"`
	ExpiredLeases []string  `json:"expired_leases"`
	Bytes         int64     `json:"bytes"`
	ActiveReaders int       `json:"active_readers"`
	ActiveWriters int       `json:"active_writers"`
	Fingerprint   string    `json:"fingerprint"`
}
type MaintenanceOptions struct {
	WALPlan       *WALGCPlan      `json:"wal_plan,omitempty"`
	MediaID       string          `json:"media_id"`
	AgentID       string          `json:"agent_id,omitempty"`
	SnapshotIDs   []string        `json:"snapshot_ids,omitempty"`
	RetentionDays int             `json:"retention_days,omitempty"`
	Plan          *GCPlan         `json:"plan,omitempty"`
	Automatic     bool            `json:"automatic,omitempty"`
	SnapshotID    string          `json:"snapshot_id,omitempty"`
	Policy        *SnapshotPolicy `json:"policy,omitempty"`
}
type MaintenanceResult struct {
	WALRemoved       int       `json:"wal_removed,omitempty"`
	ArchiveID        string    `json:"archive_id,omitempty"`
	RemovedSnapshots []string  `json:"removed_snapshots,omitempty"`
	OperationID      string    `json:"operation_id"`
	RepositoryID     string    `json:"repository_id"`
	At               time.Time `json:"at"`
	SnapshotsRemoved int       `json:"snapshots_removed"`
	ChunksRemoved    int       `json:"chunks_removed"`
	BytesRemoved     int64     `json:"bytes_removed"`
	DeferredWriters  int       `json:"deferred_writers"`
	Imported         int       `json:"imported"`
	Skipped          int       `json:"skipped"`
}
type Origin struct {
	RepositoryID  string    `json:"repository_id"`
	SnapshotID    string    `json:"snapshot_id"`
	OriginalJobID string    `json:"original_job_id"`
	ImportedAt    time.Time `json:"imported_at"`
}
type RepositoryAudit struct {
	ID         string     `json:"id"`
	Actor      string     `json:"actor"`
	Action     string     `json:"action"`
	Object     string     `json:"object"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Phase      string     `json:"phase"`
	Error      string     `json:"error,omitempty"`
}
