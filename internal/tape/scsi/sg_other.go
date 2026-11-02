//go:build !linux

// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package scsi

import (
	"context"
	"errors"
	"time"
)

type Linux struct{}

func OpenLinux(string, time.Duration) (*Linux, error) { return nil, errors.New("SG_IO requires Linux") }
func (*Linux) Close() error                           { return nil }
func (*Linux) Execute(context.Context, []byte, []byte, Direction) (int, error) {
	return 0, errors.New("SG_IO requires Linux")
}
