// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets/index.html assets/app.js assets/lifecycle.js assets/statistics.js assets/rescue.js assets/capture.js assets/wal-calendar.js assets/app.css assets/vm.js assets/replication.js assets/wal-replication.js assets/tape.js assets/removable.js assets/kubernetes.js assets/kubernetes-checks.js assets/protection.js assets/access.js assets/access-recovery.js assets/catalog-backup.js assets/service-certificates.js
var assets embed.FS

func Handler() http.Handler { sub, _ := fs.Sub(assets, "assets"); return http.FileServer(http.FS(sub)) }
