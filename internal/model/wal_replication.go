// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

import "time"

type WALReplicationPolicy struct {
	ID                 string    `json:"id"`
	SourceMediaID      string    `json:"source_media_id"`
	SourceRepositoryID string    `json:"source_repository_id"`
	TargetMediaID      string    `json:"target_media_id"`
	TargetRepositoryID string    `json:"target_repository_id"`
	TargetRunID        string    `json:"target_run_id"`
	TargetSnapshotID   string    `json:"target_snapshot_id"`
	ArchiveID          string    `json:"archive_id"`
	IntervalSeconds    int       `json:"interval_seconds"`
	Enabled            bool      `json:"enabled"`
	NextRun            time.Time `json:"next_run"`
	CreatedAt          time.Time `json:"created_at"`
}
type WALReplicationTransfer struct {
	AckGeneration string               `json:"ack_generation"`
	RunID         string               `json:"run_id"`
	Policy        WALReplicationPolicy `json:"policy"`
	SourceURL     string               `json:"source_url"`
	SourceGrant   string               `json:"source_grant"`
	ExpiresAt     time.Time            `json:"expires_at"`
}
type WALReplicationResult struct {
	Lag           *WALReplicationLag `json:"lag,omitempty"`
	ArchiveID     string             `json:"archive_id"`
	PolicyID      string             `json:"policy_id"`
	SourceMediaID string             `json:"source_media_id"`
	TargetMediaID string             `json:"target_media_id"`
	Segments      int                `json:"segments"`
	Copied        int                `json:"copied"`
	Reused        int                `json:"reused"`
	CheckedAt     time.Time          `json:"checked_at"`
	Calendar      *PITRCalendar      `json:"calendar,omitempty"`
}
