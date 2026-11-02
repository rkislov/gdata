// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"golang.org/x/net/webdav"
	"io"
	"os"
	"path"
	"sync"
	"syscall"
	"time"
)

type davFS struct {
	s    *davStore
	user string
}

func (f *davFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	_, clean, e := f.s.session(f.user, name, true)
	if e != nil {
		return e
	}
	return f.s.root.Mkdir(clean, 0700)
}
func (f *davFS) RemoveAll(ctx context.Context, name string) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	session, clean, e := f.s.session(f.user, name, true)
	if e != nil {
		return e
	}
	if clean == "staging/"+session.ID {
		return os.ErrPermission
	}
	if e = f.s.root.RemoveAll(clean); e != nil {
		return e
	}
	return f.s.recount(session)
}
func (f *davFS) Rename(ctx context.Context, old, new string) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	a, from, e := f.s.session(f.user, old, true)
	if e != nil {
		return e
	}
	b, to, e := f.s.session(f.user, new, true)
	if e != nil {
		return e
	}
	if a.ID != b.ID || from == "staging/"+a.ID || to == "staging/"+a.ID {
		return os.ErrPermission
	}
	return f.s.root.Rename(from, to)
}
func (f *davFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	session, clean, e := f.s.session(f.user, name, false)
	if e != nil {
		return nil, e
	}
	if session != nil && session.Operation == "restore" && clean == "restore/"+session.ID+"/"+session.Archive.Name {
		return davInfo{name: session.Archive.Name, size: session.Archive.Bytes}, nil
	}
	return f.s.root.Stat(cleanOrDot(clean))
}
func cleanOrDot(s string) string {
	if s == "" {
		return "."
	}
	return s
}
func (f *davFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	write := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
	session, clean, e := f.s.session(f.user, name, write)
	if e != nil {
		return nil, e
	}
	if session != nil && session.Operation == "restore" && clean == "restore/"+session.ID+"/"+session.Archive.Name {
		if write {
			return nil, os.ErrPermission
		}
		m, e := f.s.m.Repo.Manifest(session.SnapshotID)
		if e != nil {
			return nil, e
		}
		for _, file := range m.Files {
			if file.Path == session.Archive.File {
				return &davSnapshotFile{s: f.s, id: session.ID, file: file, info: davInfo{name: session.Archive.Name, size: session.Archive.Bytes}, prefix: session.header, oldHeader: session.oldHeader}, nil
			}
		}
		return nil, os.ErrNotExist
	}
	var oldSize int64
	if write {
		if stat, err := f.s.root.Stat(clean); err == nil {
			if !stat.Mode().IsRegular() {
				return nil, os.ErrPermission
			}
			oldSize = stat.Size()
		}
	}
	fd, e := f.s.root.OpenFile(cleanOrDot(clean), flag, 0600)
	if e != nil {
		return nil, e
	}
	if write {
		session.writers++
		if flag&os.O_TRUNC != 0 {
			session.used -= oldSize
		}
	}
	return &davFile{File: fd, fs: f, session: session, path: clean, write: write}, nil
}

type davFile struct {
	*os.File
	fs      *davFS
	session *davSession
	path    string
	write   bool
	closed  bool
	mu      sync.Mutex
}

func (f *davFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	if f.write {
		syncErr := f.File.Sync()
		f.fs.s.mu.Lock()
		f.session.writers--
		_ = f.fs.s.recount(f.session)
		f.fs.s.mu.Unlock()
		if syncErr != nil {
			f.File.Close()
			return syncErr
		}
	}
	return f.File.Close()
}
func (f *davFile) Write(b []byte) (int, error) {
	f.fs.s.mu.Lock()
	defer f.fs.s.mu.Unlock()
	if f.session == nil || f.session.Sealed || !time.Now().Before(f.session.ExpiresAt) {
		return 0, os.ErrPermission
	}
	offset, e := f.File.Seek(0, io.SeekCurrent)
	if e != nil {
		return 0, e
	}
	stat, e := f.File.Stat()
	if e != nil {
		return 0, e
	}
	grow := max(int64(0), offset+int64(len(b))-stat.Size())
	if grow > f.session.MaxBytes-f.session.used {
		return 0, errors.New("WebDAV session quota exceeded")
	}
	var disk syscall.Statfs_t
	if e := syscall.Statfs(f.fs.s.m.Config.WebDAV.Root, &disk); e != nil {
		return 0, e
	}
	if uint64(disk.Bavail)*uint64(disk.Bsize) < uint64(grow)+uint64(max(int64(0), f.fs.s.m.Repo.ReserveBytes)) {
		return 0, errors.New("WebDAV filesystem reserve reached")
	}
	n, e := f.File.Write(b)
	if e == nil {
		f.session.used += grow
	}
	return n, e
}
func (f *davFile) Readdir(count int) ([]os.FileInfo, error) {
	items, e := f.File.Readdir(count)
	if e != nil && e != io.EOF {
		return nil, e
	}
	out := []os.FileInfo{}
	f.fs.s.mu.Lock()
	defer f.fs.s.mu.Unlock()
	for _, item := range items {
		if item.Mode()&os.ModeSymlink != 0 {
			continue
		}
		_, _, err := f.fs.s.session(f.fs.user, path.Join(f.path, item.Name()), false)
		if err == nil {
			out = append(out, item)
		}
	}
	if f.session != nil && f.session.Operation == "restore" && f.path == "restore/"+f.session.ID {
		out = append(out, davInfo{name: f.session.Archive.Name, size: f.session.Archive.Bytes})
	}
	return out, e
}
func (s *davStore) recount(session *davSession) error {
	var total int64
	err := walkDAV(s, "staging/"+session.ID, func(info os.FileInfo) {
		if info.Mode().IsRegular() {
			total += info.Size()
		}
	})
	session.used = total
	return err
}
func walkDAV(s *davStore, name string, visit func(os.FileInfo)) error {
	info, e := s.root.Stat(name)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	visit(info)
	if !info.IsDir() {
		return nil
	}
	fd, e := s.root.Open(name)
	if e != nil {
		return e
	}
	items, e := fd.Readdir(-1)
	fd.Close()
	if e != nil {
		return e
	}
	for _, item := range items {
		if item.Mode()&os.ModeSymlink != 0 {
			return os.ErrPermission
		}
		if e = walkDAV(s, path.Join(name, item.Name()), visit); e != nil {
			return e
		}
	}
	return nil
}

type davInfo struct {
	name string
	size int64
}

func (i davInfo) Name() string       { return i.name }
func (i davInfo) Size() int64        { return i.size }
func (i davInfo) Mode() os.FileMode  { return 0400 }
func (i davInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (i davInfo) IsDir() bool        { return false }
func (i davInfo) Sys() any           { return nil }

type davSnapshotFile struct {
	s          *davStore
	id         string
	file       model.File
	info       davInfo
	offset     int64
	chunkIndex int
	chunk      []byte
	mu         sync.Mutex
	closed     bool
	prefix     []byte
	oldHeader  int64
	unchecked  bool
}

func (f *davSnapshotFile) Read(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, os.ErrClosed
	}
	if !f.unchecked {
		f.s.mu.Lock()
		session := f.s.sessions[f.id]
		valid := session != nil && time.Now().Before(session.ExpiresAt)
		f.s.mu.Unlock()
		if !valid {
			return 0, os.ErrPermission
		}
	}
	if f.offset >= f.info.size {
		return 0, io.EOF
	}
	if f.offset < int64(len(f.prefix)) {
		n := copy(b, f.prefix[f.offset:])
		f.offset += int64(n)
		return n, nil
	}
	sourceOffset := f.offset
	if len(f.prefix) > 0 {
		sourceOffset = f.offset - int64(len(f.prefix)) + f.oldHeader
	}
	index := int(sourceOffset / repository.ChunkSize)
	if f.chunk == nil || f.chunkIndex != index {
		hash := f.file.Chunks[index]
		chunk, e := f.s.m.Repo.Get(hash)
		if e != nil {
			return 0, e
		}
		if repository.Hash(chunk) != hash {
			return 0, errors.New("DAV restore block checksum mismatch")
		}
		f.chunk = chunk
		f.chunkIndex = index
	}
	inside := int(sourceOffset % repository.ChunkSize)
	n := copy(b, f.chunk[inside:])
	f.offset += int64(n)
	return n, nil
}
func (f *davSnapshotFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.offset
	case io.SeekEnd:
		offset += f.info.size
	default:
		return 0, os.ErrInvalid
	}
	if offset < 0 {
		return 0, os.ErrInvalid
	}
	f.offset = offset
	return offset, nil
}
func (f *davSnapshotFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.chunk = nil
	return nil
}
func (f *davSnapshotFile) Stat() (os.FileInfo, error)         { return f.info, nil }
func (f *davSnapshotFile) Write([]byte) (int, error)          { return 0, os.ErrPermission }
func (f *davSnapshotFile) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrPermission }
