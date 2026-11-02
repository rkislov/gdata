// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

// TapeRequest is persisted with the operation; device paths never come from the API.
type TapeRequest struct {
	PoolID        string `json:"pool_id,omitempty"`
	VolumeID      string `json:"volume_id,omitempty"`
	Action        string `json:"action"`
	Barcode       string `json:"barcode"`
	DriveID       string `json:"drive_id,omitempty"`
	Label         string `json:"label,omitempty"`
	Confirm       string `json:"confirm,omitempty"`
	Reason        string `json:"reason,omitempty"`
	EraseMode     string `json:"erase_mode,omitempty"`
	SnapshotID    string `json:"snapshot_id,omitempty"`
	CopyID        string `json:"copy_id,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
	RetentionDays int    `json:"retention_days,omitempty"`
	Hold          bool   `json:"hold,omitempty"`
}
type TapeResult struct {
	PoolID   string        `json:"pool_id,omitempty"`
	DriveID  string        `json:"drive_id,omitempty"`
	Barcode  string        `json:"barcode"`
	CopyID   string        `json:"copy_id,omitempty"`
	Snapshot *SnapshotInfo `json:"snapshot,omitempty"`
}
