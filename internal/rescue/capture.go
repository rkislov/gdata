// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package rescue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
)

func (c *Client) CaptureStatus(ctx context.Context, id string) (CaptureAccess, error) {
	var out CaptureAccess
	e := c.JSON(ctx, "GET", "/v1/rescue/captures/"+id, nil, &out)
	return out, e
}
func retryCapture(ctx context.Context, fn func() error) error {
	var e error
	for attempt := 0; attempt < 4; attempt++ {
		if e = fn(); e == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var response *HTTPError
		if errors.As(e, &response) && response.StatusCode < 500 && response.StatusCode != 429 {
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
		}
	}
	return e
}

// Capture reads each sector through a bounded buffer. It never stages a disk image
// on the rescue system. Source exclusion and geometry checks belong to the caller.
func (c *Client) Capture(ctx context.Context, source io.ReaderAt, disk model.DiskGeometry, progress func(model.Result)) (run model.Run, err error) {
	if c.Pairing.Scope.Capture == nil {
		return run, errors.New("a separate backup code is required")
	}
	if e := disk.Validate(); e != nil {
		return run, e
	}
	if disk.Bytes > c.Pairing.Scope.Capture.MaxBytes {
		return run, errors.New("disk exceeds the administrator's size limit")
	}
	ctx, cancel := context.WithDeadline(ctx, c.Pairing.Scope.ExpiresAt)
	defer cancel()
	id := c.Pairing.Scope.Capture.RunID
	started := false
	defer func() {
		if err != nil && started {
			abort, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = c.JSON(abort, "POST", "/v1/rescue/captures/"+id+"/abort", nil, new(any))
		}
	}()
	b, _ := json.Marshal(disk)
	err = retryCapture(ctx, func() error { return c.JSON(ctx, "POST", "/v1/rescue/captures", bytes.NewReader(b), &run) })
	if err != nil {
		return run, err
	}
	started = true
	var access CaptureAccess
	for {
		err = retryCapture(ctx, func() error { var e error; access, e = c.CaptureStatus(ctx, id); return e })
		if err != nil {
			return run, err
		}
		run = access.Run
		if run.Status == "error" || run.Status == "cancelled" {
			return run, fmt.Errorf("capture stopped: %s", run.Error)
		}
		if run.Status == "success" {
			if run.Result == nil || !run.Result.Verified {
				return run, errors.New("manager did not confirm disk integrity")
			}
			return run, nil
		}
		if access.Token != "" {
			break
		}
		select {
		case <-ctx.Done():
			return run, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if access.Run.Capture == nil || access.Run.Capture.Disk != disk {
		return run, errors.New("capture source identity mismatch")
	}
	media, err := NewClient(Code{URL: access.MediaURL, CertificateSHA256: access.CertificateSHA256, Secret: strings.Repeat("0", 64)})
	if err != nil {
		return run, err
	}
	defer media.HTTP.CloseIdleConnections()
	media.Pairing.Token = access.Token
	buffer := make([]byte, repository.ChunkSize)
	for offset, index := int64(0), int64(0); offset < disk.Bytes; index++ {
		if e := ctx.Err(); e != nil {
			return run, e
		}
		size := min(int64(len(buffer)), disk.Bytes-offset)
		block := buffer[:size]
		if _, e := source.ReadAt(block, offset); e != nil {
			return run, fmt.Errorf("cannot read disk at byte %d: %w", offset, e)
		}
		hash := repository.Hash(block)
		path := "/v1/captures/" + id + "/blocks/" + strconv.FormatInt(index, 10) + "/" + hash
		var ack struct {
			Present bool                `json:"present"`
			Status  model.CaptureStatus `json:"status"`
		}
		if e := retryCapture(ctx, func() error { return media.JSON(ctx, "POST", path, nil, &ack) }); e != nil {
			return run, e
		}
		if !ack.Present {
			if e := retryCapture(ctx, func() error { return media.JSON(ctx, "PUT", path, bytes.NewReader(block), &ack) }); e != nil {
				return run, e
			}
		}
		if !ack.Present || ack.Status.ID != id || ack.Status.NextIndex <= index {
			return run, errors.New("media did not acknowledge the disk block")
		}
		if progress != nil {
			progress(ack.Status.Result)
		}
		offset += size
	}
	var status model.CaptureStatus
	if e := retryCapture(ctx, func() error { return media.JSON(ctx, "POST", "/v1/captures/"+id+"/commit", nil, &status) }); e != nil {
		return run, e
	}
	for {
		access, e := c.CaptureStatus(ctx, id)
		if e != nil {
			return run, e
		}
		run = access.Run
		if run.Result != nil && progress != nil {
			progress(*run.Result)
		}
		switch run.Status {
		case "success":
			if run.Result != nil && run.Result.Verified {
				return run, nil
			}
			return run, errors.New("capture completed without native media verification")
		case "error", "cancelled":
			return run, fmt.Errorf("capture stopped: %s", run.Error)
		}
		select {
		case <-ctx.Done():
			return run, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
