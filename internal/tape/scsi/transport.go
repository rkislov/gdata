// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package scsi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

type Direction int32

const (
	None       Direction = -1
	ToDevice   Direction = -2
	FromDevice Direction = -3
)

type Executor interface {
	Execute(context.Context, []byte, []byte, Direction) (int, error)
}
type SenseError struct {
	Status             byte
	Key, ASC, ASCQ     byte
	Filemark, EOM, ILI bool
}

func (e *SenseError) Error() string {
	return fmt.Sprintf("SCSI status=%02x sense=%02x/%02x/%02x filemark=%t EOM=%t ILI=%t", e.Status, e.Key, e.ASC, e.ASCQ, e.Filemark, e.EOM, e.ILI)
}
func DecodeSense(status byte, b []byte) error {
	e := &SenseError{Status: status}
	if len(b) >= 14 && (b[0]&0x7f == 0x70 || b[0]&0x7f == 0x71) {
		e.Key = b[2] & 15
		e.ASC = b[12]
		e.ASCQ = b[13]
		e.Filemark = b[2]&128 != 0
		e.EOM = b[2]&64 != 0
		e.ILI = b[2]&32 != 0
	}
	if len(b) >= 4 && (b[0]&0x7f == 0x72 || b[0]&0x7f == 0x73) {
		e.Key = b[1] & 15
		e.ASC = b[2]
		e.ASCQ = b[3]
	}
	return e
}

type Device struct {
	mu sync.Mutex
	IO Executor
}

func (d *Device) Ready(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, e := d.IO.Execute(ctx, make([]byte, 6), nil, None)
	return e
}
func (d *Device) Inquiry(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := make([]byte, 96)
	n, e := d.IO.Execute(ctx, []byte{0x12, 0, 0, 0, 96, 0}, b, FromDevice)
	if e != nil {
		return "", e
	}
	if n < 36 {
		return "", io.ErrUnexpectedEOF
	}
	return string(b[8:36]), nil
}
func (d *Device) Move(ctx context.Context, robot, source, destination uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if source == destination {
		return errors.New("identical source and destination")
	}
	c := make([]byte, 12)
	c[0] = 0xa5
	binary.BigEndian.PutUint16(c[2:4], robot)
	binary.BigEndian.PutUint16(c[4:6], source)
	binary.BigEndian.PutUint16(c[6:8], destination)
	_, e := d.IO.Execute(ctx, c, nil, None)
	return e
}
func (d *Device) Elements(ctx context.Context, start, count uint16) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := make([]byte, 1<<20)
	c := make([]byte, 12)
	c[0] = 0xb8
	c[1] = 0x10
	binary.BigEndian.PutUint16(c[2:4], start)
	binary.BigEndian.PutUint16(c[4:6], count)
	c[7] = 0x10
	n, e := d.IO.Execute(ctx, c, b, FromDevice)
	if e != nil {
		return nil, e
	}
	if n < 8 {
		return nil, io.ErrUnexpectedEOF
	}
	length := int(b[5])<<16 | int(b[6])<<8 | int(b[7])
	if length > n-8 {
		return nil, errors.New("truncated element inventory")
	}
	return b[:8+length], nil
}
func (d *Device) Rewind(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, e := d.IO.Execute(ctx, []byte{1, 0, 0, 0, 0, 0}, nil, None)
	return e
}
func (d *Device) SpaceFiles(ctx context.Context, count int32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if count < -8388608 || count > 8388607 {
		return errors.New("file count outside SPACE(6) range")
	}
	c := []byte{0x11, 1, byte(count >> 16), byte(count >> 8), byte(count), 0}
	_, e := d.IO.Execute(ctx, c, nil, None)
	return e
}

// WriteFile serializes the entire tape file, including a synchronous filemark.
// No failed write is retried: its physical position may be uncertain.
func (d *Device) WriteFile(ctx context.Context, r io.Reader) error {
	return d.WriteFileProgress(ctx, r, nil)
}
func (d *Device) WriteFileProgress(ctx context.Context, r io.Reader, progress func(int64)) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := make([]byte, 256<<10)
	var total int64
	for {
		n, e := io.ReadFull(r, b)
		if e != nil && e != io.EOF && e != io.ErrUnexpectedEOF {
			return e
		}
		if n > 0 {
			c := []byte{0x0a, 0, byte(n >> 16), byte(n >> 8), byte(n), 0}
			got, err := d.IO.Execute(ctx, c, b[:n], ToDevice)
			if err != nil {
				return err
			}
			if got != n {
				return io.ErrShortWrite
			}
			total += int64(n)
			if progress != nil {
				progress(total)
			}
		}
		if e != nil {
			break
		}
	}
	_, e := d.IO.Execute(ctx, []byte{0x10, 0, 0, 0, 1, 0}, nil, None)
	return e
}

// ReadFile stops only at a filemark, never treating EOM or an I/O failure as EOF.
func (d *Device) ReadFile(ctx context.Context, w io.Writer, maxBytes int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if maxBytes <= 0 {
		return errors.New("bounded restore size required")
	}
	b := make([]byte, 256<<10)
	var total int64
	for {
		c := []byte{8, 2, 4, 0, 0, 0}
		n, e := d.IO.Execute(ctx, c, b, FromDevice)
		if n < 0 || n > len(b) {
			return errors.New("invalid SCSI transfer length")
		}
		var sense *SenseError
		mark := errors.As(e, &sense) && sense.Filemark && !sense.EOM && sense.Key == 0
		if e != nil && !mark {
			if n > 0 {
				return fmt.Errorf("SCSI error with partial data: %v", e)
			}
			return e
		}
		total += int64(n)
		if total > maxBytes {
			return errors.New("tape file exceeds restore bound")
		}
		if n > 0 {
			written, err := w.Write(b[:n])
			if err != nil {
				return err
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if mark {
			return nil
		}
		if n == 0 {
			return errors.New("zero-length read without filemark")
		}
	}
}

// CheckIdentity binds a configured SCSI path to its peripheral type and VPD serial.
func (d *Device) CheckIdentity(ctx context.Context, kind byte, serial string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := make([]byte, 255)
	n, e := d.IO.Execute(ctx, []byte{0x12, 0, 0, 0, 96, 0}, b[:96], FromDevice)
	if e != nil {
		return e
	}
	if n < 36 || b[0]&31 != kind {
		return errors.New("unexpected SCSI peripheral type")
	}
	n, e = d.IO.Execute(ctx, []byte{0x12, 1, 0x80, 0, 255, 0}, b, FromDevice)
	if e != nil {
		return e
	}
	if n < 4 || b[1] != 0x80 || int(binary.BigEndian.Uint16(b[2:4])) > n-4 {
		return errors.New("invalid VPD serial response")
	}
	got := strings.TrimSpace(string(b[4 : 4+int(binary.BigEndian.Uint16(b[2:4]))]))
	if got == "" || got != serial {
		return errors.New("SCSI serial mismatch")
	}
	return nil
}
func (d *Device) CheckVariableMode(ctx context.Context, writing bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := make([]byte, 255)
	n, e := d.IO.Execute(ctx, []byte{0x1a, 0, 0, 0, 255, 0}, b, FromDevice)
	if e != nil {
		return e
	}
	if n < 12 || b[3] < 8 || b[9] != 0 || b[10] != 0 || b[11] != 0 {
		return errors.New("drive must use variable block mode")
	}
	if writing && b[2]&0x80 != 0 {
		return errors.New("drive reports write protected medium")
	}
	return nil
}

type Element struct {
	Address   uint16
	Type      byte
	Full      bool
	Exception bool
	Barcode   string
}

func ParseElements(b []byte) ([]Element, error) {
	if len(b) < 8 {
		return nil, io.ErrUnexpectedEOF
	}
	out := []Element{}
	for p := 8; p < len(b); {
		if len(b)-p < 8 {
			return nil, io.ErrUnexpectedEOF
		}
		kind := b[p]
		tags := b[p+1]&0x80 != 0
		size := int(binary.BigEndian.Uint16(b[p+2 : p+4]))
		length := int(b[p+5])<<16 | int(b[p+6])<<8 | int(b[p+7])
		p += 8
		if size < 12 || length > len(b)-p || length%size != 0 {
			return nil, errors.New("invalid element descriptor page")
		}
		for end := p + length; p < end; p += size {
			v := Element{Address: binary.BigEndian.Uint16(b[p : p+2]), Type: kind, Full: b[p+2]&1 != 0, Exception: b[p+2]&4 != 0}
			if tags {
				if size < 48 {
					return nil, errors.New("short volume tag")
				}
				v.Barcode = strings.TrimSpace(string(b[p+12 : p+44]))
			}
			out = append(out, v)
		}
	}
	return out, nil
}

// CheckRewritable accepts only the explicitly supported LTO-8/9 data media.
// Unknown, cleaning and WORM types fail closed, independently of configuration.
func (d *Device) CheckRewritable(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := make([]byte, 255)
	n, e := d.IO.Execute(ctx, []byte{0x1a, 0, 0, 0, 255, 0}, b, FromDevice)
	if e != nil {
		return e
	}
	if n < 12 || b[2]&0x80 != 0 || (b[1] != 0x88 && b[1] != 0x98) {
		return errors.New("medium is protected, WORM or not a supported rewritable LTO type")
	}
	return nil
}

// Erase is synchronous. A failed command is never automatically retried.
func (d *Device) Erase(ctx context.Context, long bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	flags := byte(0)
	if long {
		flags = 1
	}
	_, e := d.IO.Execute(ctx, []byte{0x19, flags, 0, 0, 0, 0}, nil, None)
	return e
}
