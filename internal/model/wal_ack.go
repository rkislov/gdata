// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

import "time"

type WALReceiver struct {
	ID                 string     `json:"id"`
	ArchiveID          string     `json:"archive_id"`
	SourceRepositoryID string     `json:"source_repository_id"`
	TargetMediaID      string     `json:"target_media_id"`
	TargetRepositoryID string     `json:"target_repository_id"`
	Generation         string     `json:"generation"`
	CreatedAt          time.Time  `json:"created_at"`
	LastContactAt      *time.Time `json:"last_contact_at,omitempty"`
	LastAcknowledgedAt *time.Time `json:"last_acknowledged_at,omitempty"`
	Released           bool       `json:"released"`
	ReleaseReason      string     `json:"release_reason,omitempty"`
}
type WALAcknowledgement struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}
type WALReceiverAck struct {
	Generation         string               `json:"generation"`
	TargetRepositoryID string               `json:"target_repository_id"`
	Files              []WALAcknowledgement `json:"files"`
}
type WALReplicationLag struct {
	State              string     `json:"state"`
	SourceProtected    bool       `json:"source_protected"`
	TargetOnline       bool       `json:"target_online"`
	ObservedAt         time.Time  `json:"observed_at"`
	PendingFiles       *int64     `json:"pending_files"`
	PendingBytes       *int64     `json:"pending_bytes"`
	OldestPendingAt    *time.Time `json:"oldest_pending_at,omitempty"`
	PendingAgeSeconds  *int64     `json:"pending_age_seconds"`
	LastContactAt      *time.Time `json:"last_contact_at,omitempty"`
	LastAcknowledgedAt *time.Time `json:"last_acknowledged_at,omitempty"`
	Error              string     `json:"error,omitempty"`
}
