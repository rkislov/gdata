// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package model

import (
	"errors"
	"regexp"
	"slices"
)

// A target is an administrator-attested binding to a physical recovery site.
// VM placement is a fixed template; an operator may change only the new VM name.
type RestoreTarget struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	SiteID  string            `json:"site_id"`
	NodeID  string            `json:"node_id"`
	Kind    string            `json:"kind"`
	Profile string            `json:"profile,omitempty"`
	VM      *VMRestoreRequest `json:"vm,omitempty"`
}

func (t RestoreTarget) Validate() error {
	safe := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	if !safe.MatchString(t.ID) || !safe.MatchString(t.NodeID) || t.SiteID == "" || len(t.Name) > 200 {
		return errors.New("invalid restore target identity")
	}
	if !slices.Contains([]string{"files", "postgresql", "s3", "mysql", "kubernetes", "longhorn", "openstack", "ovirt", "proxmox", "opennebula"}, t.Kind) {
		return errors.New("unsupported restore target kind")
	}
	if (t.Kind == "files" || t.Kind == "postgresql") && t.Profile != "" {
		return errors.New("filesystem targets must not specify a profile")
	}
	vm := slices.Contains([]string{"openstack", "ovirt", "proxmox", "opennebula"}, t.Kind)
	if t.Kind != "files" && t.Kind != "postgresql" && !safe.MatchString(t.Profile) {
		return errors.New("target profile required")
	}
	if vm {
		if t.VM == nil {
			return errors.New("fixed VM placement template required")
		}
		v := *t.VM
		v.Name = "validation"
		if e := v.ValidateFor(t.Kind); e != nil {
			return e
		}
		if v.ProxyID != "" && v.ProxyID != t.NodeID || v.Profile != "" && v.Profile != t.Profile {
			return errors.New("template conflicts with target binding")
		}
	} else if t.VM != nil {
		return errors.New("VM template on non-VM target")
	}
	return nil
}
