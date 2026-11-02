// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalogbackup

import (
	"bytes"
	"io"
	"testing"
)

func TestEncryptedStreamRejectsTamperingAndTruncation(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	plain := bytes.Repeat([]byte("secret-password-and-private-key"), 10000)
	var out bytes.Buffer
	w, e := encrypt(&out, key)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = w.Write(plain); e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	raw := out.Bytes()
	if bytes.Contains(raw, []byte("secret-password")) {
		t.Fatal("plaintext leaked")
	}
	r, e := decrypt(bytes.NewReader(raw), key)
	if e != nil {
		t.Fatal(e)
	}
	got, e := io.ReadAll(r)
	if e != nil || !bytes.Equal(got, plain) {
		t.Fatal("roundtrip", e)
	}
	tampered := append([]byte{}, raw...)
	tampered[len(tampered)-1] ^= 1
	for _, bad := range [][]byte{raw[:len(raw)-20], raw[:len(raw)-1], tampered, append(append([]byte{}, raw...), 1)} {
		r, e := decrypt(bytes.NewReader(bad), key)
		if e == nil {
			_, e = io.ReadAll(r)
		}
		if e == nil {
			t.Fatal("corrupt archive accepted")
		}
	}
	r, e = decrypt(bytes.NewReader(raw), bytes.Repeat([]byte{8}, 32))
	if e == nil {
		_, e = io.ReadAll(r)
	}
	if e == nil {
		t.Fatal("wrong key accepted")
	}
}
