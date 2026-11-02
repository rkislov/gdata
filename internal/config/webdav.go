// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"errors"
	"net/url"
	"path/filepath"
)

type WebDAVConfig struct {
	Listen          string `json:"listen"`
	URL             string `json:"url"`
	Root            string `json:"root"`
	CredentialsFile string `json:"credentials_file"`
}

func (c Config) validateWebDAV() error {
	p := c.WebDAV
	if p == nil {
		return nil
	}
	u, e := url.Parse(p.URL)
	if c.Role != "media" || e != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || p.Listen == "" || !privateDir(p.Root) || !privateFile(p.CredentialsFile) {
		return errors.New("WebDAV requires media role, HTTPS origin/listen, private root and credentials file")
	}
	if filepath.Clean(p.Root) == filepath.Clean(c.Repository) {
		return errors.New("WebDAV staging must be separate from the repository")
	}
	return nil
}
