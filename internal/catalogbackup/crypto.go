// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalogbackup

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"golang.org/x/crypto/chacha20poly1305"
	"io"
	"os"
)

const magic = "SRKCAT01"
const blockSize = 64 * 1024

func ReadKey(path string) ([]byte, error) {
	st, e := os.Stat(path)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("recovery key must be a private regular file (0600/0400)")
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	if len(b) != 32 {
		return nil, errors.New("recovery key must contain exactly 32 random bytes")
	}
	return b, nil
}
func GenerateKey(path string) error {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	if _, e = f.Write(b); e != nil {
		return e
	}
	return f.Sync()
}

type encryptWriter struct {
	w      io.Writer
	a      cipher.AEAD
	prefix []byte
	seq    uint64
	buf    []byte
	closed bool
}

func encrypt(w io.Writer, key []byte) (*encryptWriter, error) {
	a, e := chacha20poly1305.NewX(key)
	if e != nil {
		return nil, e
	}
	p := make([]byte, 16)
	if _, e = rand.Read(p); e != nil {
		return nil, e
	}
	if _, e = w.Write(append([]byte(magic), p...)); e != nil {
		return nil, e
	}
	return &encryptWriter{w: w, a: a, prefix: p}, nil
}
func nonce(p []byte, n uint64) []byte {
	b := make([]byte, 24)
	copy(b, p)
	binary.BigEndian.PutUint64(b[16:], n)
	return b
}
func (w *encryptWriter) frame(b []byte) error {
	v := w.a.Seal(nil, nonce(w.prefix, w.seq), b, []byte(magic))
	w.seq++
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(v)))
	if _, e := w.w.Write(size[:]); e != nil {
		return e
	}
	_, e := w.w.Write(v)
	return e
}
func (w *encryptWriter) Write(b []byte) (int, error) {
	if w.closed {
		return 0, errors.New("closed archive")
	}
	total := len(b)
	for len(b) > 0 {
		n := min(blockSize-len(w.buf), len(b))
		w.buf = append(w.buf, b[:n]...)
		b = b[n:]
		if len(w.buf) == blockSize {
			if e := w.frame(w.buf); e != nil {
				return total - len(b), e
			}
			w.buf = w.buf[:0]
		}
	}
	return total, nil
}
func (w *encryptWriter) Close() error {
	if w.closed {
		return errors.New("archive already closed")
	}
	w.closed = true
	if len(w.buf) > 0 {
		if e := w.frame(w.buf); e != nil {
			return e
		}
	}
	return w.frame(nil)
}

type decryptReader struct {
	r      io.Reader
	a      cipher.AEAD
	prefix []byte
	seq    uint64
	buf    []byte
	done   bool
}

func decrypt(r io.Reader, key []byte) (*decryptReader, error) {
	a, e := chacha20poly1305.NewX(key)
	if e != nil {
		return nil, e
	}
	b := make([]byte, len(magic)+16)
	if _, e = io.ReadFull(r, b); e != nil || string(b[:len(magic)]) != magic {
		return nil, errors.New("invalid encrypted catalog archive")
	}
	return &decryptReader{r: r, a: a, prefix: b[len(magic):]}, nil
}
func (r *decryptReader) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if len(r.buf) == 0 {
		if r.done {
			return 0, io.EOF
		}
		var size [4]byte
		if _, e := io.ReadFull(r.r, size[:]); e != nil {
			return 0, errors.New("truncated encrypted archive")
		}
		n := binary.BigEndian.Uint32(size[:])
		if n < uint32(r.a.Overhead()) || n > blockSize+uint32(r.a.Overhead()) {
			return 0, errors.New("invalid encrypted frame size")
		}
		data := make([]byte, n)
		if _, e := io.ReadFull(r.r, data); e != nil {
			return 0, errors.New("truncated encrypted frame")
		}
		plain, e := r.a.Open(nil, nonce(r.prefix, r.seq), data, []byte(magic))
		if e != nil {
			return 0, errors.New("archive authentication failed")
		}
		r.seq++
		if len(plain) == 0 {
			var extra [1]byte
			n, e := r.r.Read(extra[:])
			if n != 0 || e != io.EOF {
				return 0, errors.New("trailing archive data")
			}
			r.done = true
			return 0, io.EOF
		}
		r.buf = plain
	}
	n := copy(b, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
