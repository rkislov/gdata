// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package identity

import (
	"gdata/srk/internal/config"
	"net/http"
	"sync"
	"weak"
)

var clients = struct {
	sync.Mutex
	byDirectory map[string][]weak.Pointer[http.Transport]
}{byDirectory: map[string][]weak.Pointer[http.Transport]{}}

func registerClient(c config.Config, transport *http.Transport) {
	if c.ServiceCertificates == nil {
		return
	}
	clients.Lock()
	defer clients.Unlock()
	key := c.ServiceCertificates.Directory
	live := clients.byDirectory[key][:0]
	for _, p := range clients.byDirectory[key] {
		if p.Value() != nil {
			live = append(live, p)
		}
	}
	clients.byDirectory[key] = append(live, weak.Make(transport))
}

// Close only idle connections. Streams already carrying data remain untouched.
func RefreshClients(c config.Config) {
	if c.ServiceCertificates == nil {
		return
	}
	clients.Lock()
	defer clients.Unlock()
	key := c.ServiceCertificates.Directory
	live := clients.byDirectory[key][:0]
	for _, p := range clients.byDirectory[key] {
		if t := p.Value(); t != nil {
			t.CloseIdleConnections()
			live = append(live, p)
		}
	}
	clients.byDirectory[key] = live
}
