// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

type DataBackup struct {
	Kubernetes *KubernetesBackup `json:"kubernetes,omitempty"`
	Kind       string            `json:"kind"`
	Profile    string            `json:"profile"`
	Engine     string            `json:"engine,omitempty"`
	Database   string            `json:"database,omitempty"`
	Bucket     string            `json:"bucket,omitempty"`
	Prefix     string            `json:"prefix,omitempty"`
	Objects    []S3Object        `json:"objects,omitempty"`
}
type S3Object struct {
	File            string            `json:"file"`
	Key             string            `json:"key"`
	VersionID       string            `json:"version_id,omitempty"`
	ETag            string            `json:"etag"`
	ContentType     string            `json:"content_type,omitempty"`
	ContentEncoding string            `json:"content_encoding,omitempty"`
	CacheControl    string            `json:"cache_control,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	Tags            map[string]string `json:"tags,omitempty"`
	Bytes           int64             `json:"bytes"`
	SHA256          string            `json:"sha256"`
}

type KubernetesBackup struct {
	Namespaces      []string `json:"namespaces"`
	Objects         int      `json:"objects"`
	VolumeBackups   int      `json:"volume_backups"`
	PVCData         string   `json:"pvc_data"`
	IncludesSecrets bool     `json:"includes_secrets"`
}

type KubernetesRestore struct {
	Namespace        string                   `json:"namespace"`
	SourceNamespace  string                   `json:"source_namespace"`
	Objects          int                      `json:"objects"`
	TotalObjects     int                      `json:"total_objects"`
	Volumes          int                      `json:"volumes"`
	TotalVolumes     int                      `json:"total_volumes"`
	WorkloadsStarted bool                     `json:"workloads_started"`
	CurrentResource  string                   `json:"current_resource,omitempty"`
	State            string                   `json:"state"`
	Events           []KubernetesRestoreEvent `json:"events"`
}
type KubernetesRestoreEvent struct {
	At       string `json:"at"`
	Phase    string `json:"phase"`
	Resource string `json:"resource,omitempty"`
}
