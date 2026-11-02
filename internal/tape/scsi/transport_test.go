// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package scsi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type bench struct {
	calls   [][]byte
	records [][]byte
	read    int
	fail    byte
}

func (b *bench) Execute(ctx context.Context, c, p []byte, d Direction) (int, error) {
	if e := ctx.Err(); e != nil {
		return 0, e
	}
	b.calls = append(b.calls, append([]byte(nil), c...))
	if b.fail == c[0] {
		return 0, &SenseError{Status: 2, Key: 3, EOM: true}
	}
	switch c[0] {
	case 0x0a:
		b.records = append(b.records, append([]byte(nil), p...))
		return len(p), nil
	case 8:
		if b.read == len(b.records) {
			return 0, &SenseError{Status: 2, Filemark: true}
		}
		n := copy(p, b.records[b.read])
		b.read++
		return n, nil
	case 1:
		b.read = 0
	case 0xb8:
		p[5] = 0
		p[6] = 0
		p[7] = 0
		return 8, nil
	}
	return 0, nil
}
func TestCommandBenchRoundTrip(t *testing.T) {
	b := &bench{fail: 255}
	d := &Device{IO: b}
	ctx := context.Background()
	if e := d.Move(ctx, 1, 0x100, 0x200); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(b.calls[0], []byte{0xa5, 0, 0, 1, 1, 0, 2, 0, 0, 0, 0, 0}) {
		t.Fatal("wrong MOVE MEDIUM", b.calls[0])
	}
	source := bytes.Repeat([]byte("SRK"), 200000)
	if e := d.WriteFile(ctx, bytes.NewReader(source)); e != nil {
		t.Fatal(e)
	}
	last := b.calls[len(b.calls)-1]
	if !bytes.Equal(last, []byte{0x10, 0, 0, 0, 1, 0}) {
		t.Fatal("missing synchronous filemark")
	}
	if e := d.Rewind(ctx); e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	if e := d.ReadFile(ctx, &out, int64(len(source))); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(out.Bytes(), source) {
		t.Fatal("roundtrip mismatch")
	}
}
func TestFailureNotRetriedOrPublishedAsEOF(t *testing.T) {
	b := &bench{fail: 0x0a}
	d := &Device{IO: b}
	if e := d.WriteFile(context.Background(), bytes.NewReader([]byte("data"))); e == nil {
		t.Fatal("write error ignored")
	}
	if len(b.calls) != 1 {
		t.Fatal("failed write retried or filemark written")
	}
	b.fail = 8
	if e := d.ReadFile(context.Background(), io.Discard, 100); e == nil || errors.Is(e, io.EOF) {
		t.Fatal("EOM accepted as EOF")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := d.Rewind(ctx); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestSenseAndBounds(t *testing.T) {
	sense := make([]byte, 18)
	sense[0] = 0x70
	sense[2] = 0xc3
	sense[12] = 0x11
	e := DecodeSense(2, sense).(*SenseError)
	if !e.Filemark || !e.EOM || e.Key != 3 || e.ASC != 0x11 {
		t.Fatal(e)
	}
	b := &bench{fail: 255, records: [][]byte{[]byte("oversized")}}
	d := &Device{IO: b}
	if e := d.ReadFile(context.Background(), io.Discard, 2); e == nil {
		t.Fatal("unbounded restore")
	}
	if e := d.SpaceFiles(context.Background(), 8388608); e == nil {
		t.Fatal("count overflow")
	}
}

type partialBlankRead struct{}

func (partialBlankRead) Execute(_ context.Context, _ []byte, p []byte, _ Direction) (int, error) {
	p[0] = 1
	return 1, &SenseError{Status: 2, Key: 8}
}

func TestPartialDataCannotBeMistakenForBlankEnd(t *testing.T) {
	d := &Device{IO: partialBlankRead{}}
	var out bytes.Buffer
	err := d.ReadFile(context.Background(), &out, 1024)
	var sense *SenseError
	if err == nil || errors.As(err, &sense) {
		t.Fatalf("partial transfer must not expose a clean blank-end result: %v", err)
	}
}
