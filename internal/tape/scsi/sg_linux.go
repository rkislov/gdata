//go:build linux

// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package scsi

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

type Linux struct {
	mu      sync.Mutex
	file    *os.File
	Timeout time.Duration
}
type sgHeader struct {
	InterfaceID   int32
	Direction     int32
	CommandLength uint8
	SenseLength   uint8
	IOVecCount    uint16
	Length        uint32
	Data          uintptr
	Command       uintptr
	Sense         uintptr
	Timeout       uint32
	Flags         uint32
	PackID        int32
	User          uintptr
	Status        uint8
	MaskedStatus  uint8
	MessageStatus uint8
	SenseWritten  uint8
	HostStatus    uint16
	DriverStatus  uint16
	Residual      int32
	Duration      uint32
	Info          uint32
}

func OpenLinux(path string, timeout time.Duration) (*Linux, error) {
	if timeout < time.Second || timeout > 2*time.Hour {
		return nil, errors.New("SCSI timeout must be 1s–2h")
	}
	f, e := os.OpenFile(path, os.O_RDWR|unix.O_EXCL|unix.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	s, e := f.Stat()
	if e != nil || s.Mode()&os.ModeCharDevice == 0 {
		f.Close()
		return nil, errors.New("SCSI character device required")
	}
	if e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, e
	}
	return &Linux{file: f, Timeout: timeout}, nil
}
func (l *Linux) Close() error { l.mu.Lock(); defer l.mu.Unlock(); return l.file.Close() }
func (l *Linux) Execute(ctx context.Context, c, b []byte, d Direction) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return 0, e
	}
	if len(c) < 6 || len(c) > 16 || len(b) > 1<<20 {
		return 0, errors.New("invalid SCSI request size")
	}
	timeout := l.Timeout
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < timeout {
		timeout = time.Until(deadline)
	}
	if timeout <= 0 {
		return 0, context.DeadlineExceeded
	}
	sense := make([]byte, 96)
	h := sgHeader{InterfaceID: 'S', Direction: int32(d), CommandLength: uint8(len(c)), SenseLength: 96, Length: uint32(len(b)), Command: uintptr(unsafe.Pointer(&c[0])), Sense: uintptr(unsafe.Pointer(&sense[0])), Timeout: uint32(max(1, timeout.Milliseconds()))}
	if len(b) > 0 {
		h.Data = uintptr(unsafe.Pointer(&b[0]))
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, l.file.Fd(), 0x2285, uintptr(unsafe.Pointer(&h)))
	runtime.KeepAlive(c)
	runtime.KeepAlive(b)
	runtime.KeepAlive(sense)
	if errno != 0 {
		return 0, errno
	}
	if h.Residual < 0 || int(h.Residual) > len(b) {
		return 0, errors.New("invalid SG residual")
	}
	n := len(b) - int(h.Residual)
	if h.HostStatus != 0 || h.DriverStatus&0xf != 0 && h.DriverStatus&0xf != 8 {
		return n, errors.New("SCSI host/driver failure; position uncertain")
	}
	if h.Status != 0 || h.SenseWritten > 0 {
		return n, DecodeSense(h.Status, sense[:min(int(h.SenseWritten), len(sense))])
	}
	return n, nil
}
