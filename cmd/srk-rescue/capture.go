// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"gdata/srk/internal/model"
	"gdata/srk/internal/rescue"
)

func expectedSector(m model.Manifest) int {
	if m.Disk != nil {
		return m.Disk.SectorSize
	}
	return 512
}
func backupDisk(ctx context.Context, c *rescue.Client) error {
	permission := c.Pairing.Scope.Capture
	if permission == nil {
		return errors.New("restore code is read-only; ask the administrator for a disk backup code")
	}
	available, e := disks(false)
	if e != nil {
		return e
	}
	candidates := []disk{}
	for _, d := range available {
		if d.Size <= permission.MaxBytes && (d.SectorSize == 512 || d.SectorSize == 4096) {
			candidates = append(candidates, d)
		}
	}
	if len(candidates) == 0 {
		return errors.New("no eligible unmounted disk within the authorized size limit")
	}
	fmt.Printf("Backup: %s\nMedia: %s. Retention: %d days. Maximum disk: %d bytes.\n", permission.Name, permission.MediaID, permission.RetentionDays, permission.MaxBytes)
	fmt.Println("Copy the ENTIRE disk including partition table and boot areas. The source must be offline and unmounted; shared storage must not be modified by another host. No source sectors will be written.")
	for i, d := range candidates {
		fmt.Printf("%d  %s  %s  serial=%s  bytes=%d  sector=%d\n", i+1, d.Path, d.Model, d.Serial, d.Size, d.SectorSize)
	}
	pick := choice(len(candidates))
	if pick < 0 {
		return nil
	}
	target := candidates[pick]
	confirmation := strings.TrimSpace("BACKUP " + target.Path + " " + strings.TrimSpace(target.Serial))
	if ask("Type exactly ["+confirmation+"]: ") != confirmation {
		return errors.New("confirmation does not match; nothing copied")
	}
	current, e := disks(false)
	if e != nil {
		return e
	}
	found := false
	for _, d := range current {
		if d.Path == target.Path && d.Size == target.Size && d.Serial == target.Serial && d.Model == target.Model && d.SectorSize == target.SectorSize {
			found = true
		}
	}
	if !found {
		return errors.New("source disk identity or state changed")
	}
	before, e := os.Lstat(target.Path)
	if e != nil {
		return e
	}
	if before.Mode()&os.ModeDevice == 0 || before.Mode()&os.ModeCharDevice != 0 {
		return errors.New("block device required")
	}
	source, e := os.OpenFile(target.Path, os.O_RDONLY|syscall.O_EXCL|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return e
	}
	defer source.Close()
	after, e := source.Stat()
	if e != nil || !os.SameFile(before, after) {
		return errors.New("source disk identity changed while opening")
	}
	host, e := os.Hostname()
	if e != nil {
		return e
	}
	geometry := model.DiskGeometry{Host: host, Device: target.Path, Model: strings.TrimSpace(target.Model), Serial: strings.TrimSpace(target.Serial), Bytes: target.Size, SectorSize: target.SectorSize}
	result, e := c.Capture(ctx, source, geometry, func(r model.Result) {
		if r.Phase == "disk-verification" {
			fmt.Print("\rMedia server is verifying the complete image...                                      ")
			return
		}
		percent := 0.0
		if r.ProgressPercent != nil {
			percent = *r.ProgressPercent
		}
		saved := r.LogicalBytes - r.UploadedBytes
		fmt.Printf("\rCopied %.1f%%  %d / %d bytes; uploaded %d; reused %d", percent, r.ProgressBytes, r.TotalBytes, r.UploadedBytes, saved)
	})
	fmt.Println()
	if e != nil {
		return e
	}
	fmt.Println("Backup verified. Run ID:", result.ID, ". The administrator can issue a restore code from the web interface.")
	return nil
}
