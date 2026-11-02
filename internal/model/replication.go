// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

import "time"

type Replication struct {
	IncludeWAL       bool   `json:"include_wal,omitempty"`
	SourceRunID      string `json:"source_run_id"`
	SourceMediaID    string `json:"source_media_id"`
	SourceSnapshotID string `json:"source_snapshot_id"`
	TargetMediaID    string `json:"target_media_id"`
	RetentionDays    int    `json:"retention_days"`
	CopyRunID        string `json:"copy_run_id"`
}
type Replica struct {
	RetentionDays      int       `json:"retention_days"`
	SourceRunID        string    `json:"source_run_id"`
	SourceMediaID      string    `json:"source_media_id"`
	SourceSnapshotID   string    `json:"source_snapshot_id"`
	SourceRepositoryID string    `json:"source_repository_id"`
	SourceManifestHash string    `json:"source_manifest_hash"`
	CapturedAt         time.Time `json:"captured_at"`
	TargetMediaID      string    `json:"target_media_id"`
	TargetRunID        string    `json:"target_run_id"`
	TargetSnapshotID   string    `json:"target_snapshot_id"`
	ExpiresAt          time.Time `json:"expires_at"`
}
type ReplicationTransfer struct {
	RunID              string      `json:"run_id"`
	Replication        Replication `json:"replication"`
	SourceURL          string      `json:"source_url"`
	SourceRepositoryID string      `json:"source_repository_id"`
	SourceGrant        string      `json:"source_grant"`
	SourceManifestHash string      `json:"source_manifest_hash"`
	LeaseExpiresAt     time.Time   `json:"lease_expires_at"`
}
