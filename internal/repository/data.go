// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package repository

import (
	"errors"
	"gdata/srk/internal/model"
	"strings"
)

func validateDataManifest(m model.Manifest) error {
	d := m.Data
	if d == nil {
		return nil
	}
	if m.VM != nil || m.PostgreSQL != nil || m.Disk != nil || m.Mode != "full" || d.Profile == "" || d.Kind != "s3" && d.Kind != "mysql" && d.Kind != "kubernetes" && d.Kind != "longhorn" || m.Consistency != d.Kind+"-logical" {
		return errors.New("invalid data source manifest")
	}
	files := map[string]model.File{}
	for _, f := range m.Files {
		if f.Kind != "file" {
			return errors.New("data backups contain regular files only")
		}
		files[f.Path] = f
	}
	if d.Kind == "kubernetes" || d.Kind == "longhorn" {
		k := d.Kubernetes
		if k == nil || len(k.Namespaces) == 0 || k.Objects < 1 || k.Objects > 10000 || k.VolumeBackups < 0 || k.VolumeBackups > k.Objects || (k.PVCData != "not-included" && k.PVCData != "longhorn-external-backupstore") || (d.Kind == "longhorn" && k.PVCData != "longhorn-external-backupstore") || len(files) != k.Objects+1 || files["kubernetes-index.json"].Path == "" {
			return errors.New("invalid Kubernetes backup index")
		}
		return nil
	}
	if d.Kind == "mysql" {
		if d.Engine != "mysql" && d.Engine != "mariadb" || d.Database == "" || len(m.Files) != 1 || files["database.sql"].Path == "" {
			return errors.New("invalid SQL dump manifest")
		}
		return nil
	}
	if d.Bucket == "" || len(d.Objects) > 10000 || len(files) != len(d.Objects)+1 || files["source.json"].Path == "" {
		return errors.New("invalid S3 object index")
	}
	seen := map[string]bool{}
	for _, o := range d.Objects {
		f, ok := files[o.File]
		if !ok || !strings.HasPrefix(o.File, "objects/") || !ValidPath(o.File) || seen[o.Key] || o.Key == "" || !strings.HasPrefix(o.Key, d.Prefix) || f.Size != o.Bytes || o.Bytes < 0 || o.Bytes > 5<<30 || !hashPattern.MatchString(o.SHA256) {
			return errors.New("invalid S3 object descriptor")
		}
		seen[o.Key] = true
	}
	return nil
}
