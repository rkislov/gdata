// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/virtualization"
	"golang.org/x/net/webdav"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type davStore struct {
	m        *Media
	root     *os.Root
	mu       sync.Mutex
	sessions map[string]*davSession
	users    map[string][32]byte
	locks    webdav.LockSystem
}
type davSession struct {
	model.DAVSession
	used      int64
	writers   int
	header    []byte
	oldHeader int64
}

func newDAVStore(m *Media) (*davStore, error) {
	p := m.Config.WebDAV
	if p == nil {
		return nil, nil
	}
	b, e := os.ReadFile(p.CredentialsFile)
	if e != nil {
		return nil, e
	}
	var users map[string]string
	if json.Unmarshal(b, &users) != nil || len(users) == 0 {
		return nil, errors.New("WebDAV credentials must be username/password JSON mapping")
	}
	s := &davStore{m: m, sessions: map[string]*davSession{}, users: map[string][32]byte{}, locks: webdav.NewMemLS()}
	for user, password := range users {
		if user == "" || len(password) < 16 {
			return nil, errors.New("WebDAV password requires at least 16 characters")
		}
		s.users[user] = sha256.Sum256([]byte(password))
	}
	for _, dir := range []string{"staging", "restore"} {
		if e = os.MkdirAll(filepath.Join(p.Root, dir), 0700); e != nil {
			return nil, e
		}
	}
	s.root, e = os.OpenRoot(p.Root)
	if e == nil {
		e = s.load()
	}
	return s, e
}
func (m *Media) WebDAVHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := m.DAV
		if s == nil {
			http.NotFound(w, r)
			return
		}
		if r.TLS == nil {
			http.Error(w, "HTTPS required", 403)
			return
		}
		user, password, ok := r.BasicAuth()
		actual := sha256.Sum256([]byte(password))
		expected, known := s.users[user]
		if !ok || !known || subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="SRK backup transport", charset="UTF-8"`)
			http.Error(w, "authentication required", 401)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		r = r.WithContext(context.WithValue(r.Context(), davUserKey{}, user))
		handler := webdav.Handler{FileSystem: &davFS{s: s, user: user}, LockSystem: s.locks}
		handler.ServeHTTP(w, r)
	})
}

type davUserKey struct{}

func (s *davStore) session(user, name string, write bool) (*davSession, string, error) {
	if strings.Contains(name, "\\") || strings.ContainsRune(name, 0) {
		return nil, "", os.ErrPermission
	}
	clean := strings.TrimPrefix(name, "/")
	if path.Clean("/"+clean) != "/"+clean {
		return nil, "", os.ErrPermission
	}
	parts := strings.Split(clean, "/")
	if clean == "" || clean == "staging" || clean == "restore" {
		if write {
			return nil, "", os.ErrPermission
		}
		return nil, clean, nil
	}
	if len(parts) < 2 || (parts[0] != "staging" && parts[0] != "restore") {
		return nil, "", os.ErrPermission
	}
	session := s.sessions[parts[1]]
	if session == nil || session.User != user || !time.Now().Before(session.ExpiresAt) {
		return nil, "", os.ErrNotExist
	}
	if parts[0] == "restore" && session.Operation != "restore" || parts[0] == "staging" && session.Operation != "backup" {
		return nil, "", os.ErrPermission
	}
	if write && (session.Operation != "backup" || session.Sealed) {
		return nil, "", os.ErrPermission
	}
	return session, clean, nil
}
func (m *Media) davRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/dav/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if m.DAV == nil {
			Fail(w, 409, errors.New("WebDAV transport is disabled"))
			return
		}
		g, e := m.authorize(r)
		if e != nil {
			Fail(w, 403, e)
			return
		}
		peer, e := Role(r, "proxy")
		if e != nil || g.RunID != r.PathValue("id") {
			Fail(w, 403, errors.New("assigned proxy grant required"))
			return
		}
		var req struct {
			User     string `json:"user"`
			MaxBytes int64  `json:"max_bytes"`
		}
		if e = Decode(w, r, &req); e != nil {
			Fail(w, 400, e)
			return
		}
		s := m.DAV
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.users[req.User]; !ok || req.MaxBytes < 1 || req.MaxBytes > model.MaxCaptureBytes {
			Fail(w, 400, errors.New("unknown DAV user or invalid staging quota"))
			return
		}
		if old := s.sessions[g.RunID]; old != nil {
			if old.Subject != peer.ID || old.User != req.User {
				Fail(w, 409, errors.New("DAV session conflict"))
				return
			}
			JSON(w, 200, old.DAVSession)
			return
		}
		session := &davSession{DAVSession: model.DAVSession{ID: g.RunID, Subject: peer.ID, User: req.User, MaxBytes: req.MaxBytes, ExpiresAt: time.Unix(g.Expires, 0).UTC(), Operation: "backup"}}
		prefix := "staging/" + g.RunID
		if g.Permission == "read" {
			snapshot, e := m.Repo.Manifest(g.SnapshotID)
			if e != nil || snapshot.VM == nil || snapshot.VM.Archive == nil {
				Fail(w, 409, errors.New("native archive backup required"))
				return
			}
			session.Operation = "restore"
			session.SnapshotID = g.SnapshotID
			archive := *snapshot.VM.Archive
			session.Archive = &archive
			var artifact model.File
			for _, f := range snapshot.Files {
				if f.Path == archive.File {
					artifact = f
				}
			}
			source := &davSnapshotFile{s: s, id: g.RunID, file: artifact, info: davInfo{name: archive.Name, size: artifact.Size}, unchecked: true}
			first := make([]byte, 60)
			if _, e = io.ReadFull(source, first); e != nil {
				Fail(w, 409, e)
				return
			}
			size := int64(binary.BigEndian.Uint32(first[56:60]))
			if size < 12288 || size > 16<<20 {
				Fail(w, 409, errors.New("invalid VMA header size"))
				return
			}
			header := make([]byte, size)
			copy(header, first)
			if _, e = io.ReadFull(source, header[60:]); e != nil {
				Fail(w, 409, e)
				return
			}
			patched, e := virtualization.RestoreVMAHeader(header, g.RunID, peer.ID)
			if e != nil {
				Fail(w, 409, e)
				return
			}
			session.header = patched
			session.oldHeader = size
			session.Archive.Bytes = artifact.Size - size + int64(len(patched))
			prefix = "restore/" + g.RunID
		} else if g.Permission != "write" {
			Fail(w, 403, errors.New("DAV write/read scope required"))
			return
		}
		if e = s.root.MkdirAll(prefix, 0700); e != nil {
			Fail(w, 500, e)
			return
		}
		if e = s.save(session); e != nil {
			Fail(w, 500, e)
			return
		}
		s.sessions[g.RunID] = session
		JSON(w, 201, session.DAVSession)
	})
	mux.HandleFunc("POST /v1/dav/sessions/{id}/seal", func(w http.ResponseWriter, r *http.Request) {
		g, e := m.grant(r)
		if e != nil || m.DAV == nil || g.Permission != "write" || g.RunID != r.PathValue("id") {
			Fail(w, 403, errors.New("DAV write grant required"))
			return
		}
		s := m.DAV
		s.mu.Lock()
		locked := true
		defer func() {
			if locked {
				s.mu.Unlock()
			}
		}()
		session := s.sessions[g.RunID]
		if session == nil || session.Subject != g.Subject || session.writers != 0 {
			Fail(w, 409, errors.New("DAV session missing or upload still active"))
			return
		}
		var files []string
		dir := "staging/" + g.RunID
		e = fs.WalkDir(s.root.FS(), dir, func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&os.ModeSymlink != 0 {
				return errors.New("symlinks forbidden")
			}
			if !d.IsDir() && strings.HasSuffix(name, ".vma.zst") {
				files = append(files, name)
			}
			return nil
		})
		if e != nil || len(files) != 1 {
			Fail(w, 409, errors.New("exactly one completed .vma.zst archive is required"))
			return
		}
		f, e := s.root.Open(files[0])
		if e != nil {
			Fail(w, 409, e)
			return
		}
		defer f.Close()
		stat, e := f.Stat()
		if e != nil || !stat.Mode().IsRegular() || stat.Size() > session.MaxBytes {
			Fail(w, 409, errors.New("invalid DAV archive"))
			return
		}
		session.Sealed = true
		session.writers++ // Prevent cleanup while the sealed archive is being hashed.
		s.mu.Unlock()
		locked = false
		hash := sha256.New()
		_, e = io.Copy(hash, f)
		s.mu.Lock()
		locked = true
		session.writers--
		if e != nil {
			Fail(w, 500, e)
			return
		}
		session.Sealed = true
		session.Archive = &model.VMArchive{File: files[0], Name: path.Base(files[0]), Format: "vma.zst", Bytes: stat.Size(), SHA256: hex.EncodeToString(hash.Sum(nil))}
		if e = s.save(session); e != nil {
			Fail(w, 500, e)
			return
		}
		JSON(w, 200, session.Archive)
	})
	mux.HandleFunc("GET /v1/dav/sessions/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		g, e := m.grant(r)
		if e != nil || m.DAV == nil || g.Permission != "write" || g.RunID != r.PathValue("id") {
			Fail(w, 403, errors.New("DAV write owner required"))
			return
		}
		s := m.DAV
		s.mu.Lock()
		session := s.sessions[g.RunID]
		if session == nil || !session.Sealed || session.Subject != g.Subject || session.Archive == nil {
			s.mu.Unlock()
			Fail(w, 409, errors.New("sealed DAV session required"))
			return
		}
		name := session.Archive.File
		s.mu.Unlock()
		f, e := s.root.Open(name)
		if e != nil {
			Fail(w, 409, e)
			return
		}
		defer f.Close()
		stat, e := f.Stat()
		if e != nil {
			Fail(w, 500, e)
			return
		}
		http.ServeContent(w, r, path.Base(name), stat.ModTime(), f)
	})
	mux.HandleFunc("DELETE /v1/dav/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		peer, e := Role(r, "proxy")
		id := r.PathValue("id")
		if e != nil || m.DAV == nil || !davID.MatchString(id) {
			Fail(w, 403, errors.New("DAV session owner required"))
			return
		}

		s := m.DAV
		s.mu.Lock()
		defer s.mu.Unlock()
		session := s.sessions[id]
		if session != nil {
			if session.Subject != peer.ID {
				Fail(w, 403, errors.New("foreign DAV session"))
				return
			}
			if session.writers > 0 {
				Fail(w, 409, errors.New("DAV upload is still active"))
				return
			}
			delete(s.sessions, id)
			_ = s.root.RemoveAll("staging/" + id)
			_ = s.root.RemoveAll("restore/" + id)
		}
		if session == nil {
			for _, prefix := range []string{"staging/", "restore/"} {
				if _, err := s.root.Stat(prefix + id); err == nil || !os.IsNotExist(err) {
					Fail(w, 409, errors.New("orphan DAV state requires local administrator inspection"))
					return
				}
			}
		}
		_ = os.Remove(filepath.Join(s.stateDir(), id+".json"))
		JSON(w, 200, map[string]bool{"closed": true})
	})
}
func (m *Media) serveDAV(ctx context.Context) error {
	p := m.Config.WebDAV
	pair, e := tls.LoadX509KeyPair(m.Config.Cert, m.Config.Key)
	if e != nil {
		return e
	}
	listener, e := net.Listen("tcp", p.Listen)
	if e != nil {
		return e
	}
	server := &http.Server{Handler: m.WebDAVHandler(), TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	go m.DAV.expireSessions(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	e = server.ServeTLS(listener, "", "")
	if e == http.ErrServerClosed {
		return nil
	}
	return e
}

func (m *Media) davStatus() map[string]any {
	if m.DAV == nil {
		return map[string]any{"enabled": false}
	}
	s := m.DAV
	s.mu.Lock()
	defer s.mu.Unlock()
	var used int64
	uploads, restores := 0, 0
	for _, session := range s.sessions {
		used += session.used
		if session.Operation == "restore" {
			restores++
		} else {
			uploads++
		}
	}
	return map[string]any{"enabled": true, "url": m.Config.WebDAV.URL, "backup_sessions": uploads, "restore_sessions": restores, "staging_bytes": used}
}
