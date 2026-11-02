// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

import "time"

type PostgreSQLBackup struct {
	SystemID       string               `json:"system_id,omitempty"`
	SegmentSize    int64                `json:"segment_size,omitempty"`
	ArchiveID      string               `json:"archive_id,omitempty"`
	Profile        string               `json:"profile"`
	Major          int                  `json:"major"`
	StartedAt      time.Time            `json:"started_at"`
	FinishedAt     time.Time            `json:"finished_at"`
	WALRanges      []PostgreSQLWALRange `json:"wal_ranges"`
	NativeVerified bool                 `json:"native_verified"`
}
type PITRRequest struct {
	TargetTime time.Time `json:"target_time"`
	Inclusive  *bool     `json:"inclusive,omitempty"`
}

func (p PITRRequest) IncludeTarget() bool { return p.Inclusive == nil || *p.Inclusive }

type PITRResult struct {
	TargetTime time.Time `json:"target_time"`
	Inclusive  bool      `json:"inclusive"`
	ArchiveID  string    `json:"archive_id"`
	Timeline   int       `json:"timeline"`
	Segments   int       `json:"segments"`
	Recovered  bool      `json:"recovered"`
}
type WALRecord struct {
	FirstCommitAt      *time.Time `json:"first_commit_at,omitempty"`
	LastCommitAt       *time.Time `json:"last_commit_at,omitempty"`
	TimeBoundsVerified bool       `json:"time_bounds_verified,omitempty"`
	ArchiveID          string     `json:"archive_id"`
	AgentID            string     `json:"agent_id"`
	Profile            string     `json:"profile"`
	SystemID           string     `json:"system_id"`
	Major              int        `json:"major"`
	SegmentSize        int64      `json:"segment_size"`
	Name               string     `json:"name"`
	Bytes              int64      `json:"bytes"`
	SHA256             string     `json:"sha256"`
	ArchivedAt         time.Time  `json:"archived_at"`
}
type WALPin struct {
	ID        string    `json:"id"`
	ArchiveID string    `json:"archive_id"`
	AgentID   string    `json:"agent_id"`
	ExpiresAt time.Time `json:"expires_at"`
}
type WALGCPlan struct {
	ReplicationProtectedFiles int       `json:"replication_protected_files,omitempty"`
	ReplicationProtectedBytes int64     `json:"replication_protected_bytes,omitempty"`
	RepositoryID              string    `json:"repository_id"`
	ArchiveID                 string    `json:"archive_id"`
	At                        time.Time `json:"at"`
	Before                    time.Time `json:"before"`
	AllowOrphan               bool      `json:"allow_orphan"`
	Names                     []string  `json:"names"`
	Bytes                     int64     `json:"bytes"`
	Dependencies              []string  `json:"dependencies"`
	ActivePins                int       `json:"active_pins"`
	Blocked                   []string  `json:"blocked"`
	Fingerprint               string    `json:"fingerprint"`
	Token                     string    `json:"token"`
}
type WALGap struct {
	Timeline int    `json:"timeline"`
	From     string `json:"from"`
	To       string `json:"to"`
	Count    uint64 `json:"count"`
}
type PITRWindow struct {
	ContiguousEnd string     `json:"contiguous_end"`
	BaseID        string     `json:"base_id"`
	CreatedAt     time.Time  `json:"created_at"`
	From          time.Time  `json:"from"`
	ToExclusive   *time.Time `json:"to_exclusive,omitempty"`
	ExpiresAt     time.Time  `json:"expires_at"`
	Hold          bool       `json:"hold"`
	Status        string     `json:"status"`
	Gaps          []WALGap   `json:"gaps"`
	Notes         []string   `json:"notes"`
}
type PITRCalendar struct {
	ArchiveID          string       `json:"archive_id"`
	At                 time.Time    `json:"at"`
	Windows            []PITRWindow `json:"windows"`
	Records            int          `json:"records"`
	TimestampedRecords int          `json:"timestamped_records"`
}
type PostgreSQLWALRange struct {
	Timeline int    `json:"Timeline"`
	StartLSN string `json:"Start-LSN"`
	EndLSN   string `json:"End-LSN"`
}
