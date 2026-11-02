// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package rescue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
)

type Fetch func(context.Context, string) ([]byte, error)

func DiskImage(m model.Manifest) (model.File, error) {
	if e := repository.Validate(m); e != nil {
		return model.File{}, e
	}
	if m.Consistency != "offline-disk-image" || m.PostgreSQL != nil || len(m.Files) != 1 || m.Files[0].Kind != "file" || m.Files[0].Size < 512 || m.Files[0].Size%512 != 0 {
		return model.File{}, errors.New("для восстановления диска нужна копия RAW-образа типа disk-image")
	}
	return m.Files[0], nil
}

// RestoreImage writes only into an already opened and independently checked destination.
// Every downloaded block is verified before writing; a full read-back follows fsync.
func RestoreImage(ctx context.Context, m model.Manifest, dst *os.File, capacity int64, fetch Fetch, progress func(int64, int64)) error {
	f, e := DiskImage(m)
	if e != nil {
		return e
	}
	if capacity < f.Size {
		return errors.New("целевой диск меньше образа")
	}
	var offset int64
	for _, h := range f.Chunks {
		if e = ctx.Err(); e != nil {
			return e
		}
		b, e := fetch(ctx, h)
		if e != nil {
			return e
		}
		expected := min(int64(repository.ChunkSize), f.Size-offset)
		if int64(len(b)) != expected || repository.Hash(b) != h {
			return errors.New("контрольная сумма или размер блока не совпадает")
		}
		n, e := dst.WriteAt(b, offset)
		if e != nil {
			return fmt.Errorf("запись прервана после %d байт: %w", offset, e)
		}
		if n != len(b) {
			return io.ErrShortWrite
		}
		offset += int64(n)
		if progress != nil {
			progress(offset, f.Size)
		}
	}
	if e = dst.Sync(); e != nil {
		return e
	}
	offset = 0
	for _, h := range f.Chunks {
		if e = ctx.Err(); e != nil {
			return e
		}
		b := make([]byte, min(int64(repository.ChunkSize), f.Size-offset))
		n, e := dst.ReadAt(b, offset)
		if e != nil || n != len(b) {
			return errors.New("не удалось перечитать восстановленный диск")
		}
		if repository.Hash(b) != h {
			return errors.New("проверка записанного диска не пройдена")
		}
		offset += int64(n)
	}
	return nil
}
