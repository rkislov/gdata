// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalogbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gdata/srk/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
)

type Remote interface {
	Upload(context.Context, string, string, int, io.Reader) (Receipt, error)
	Download(context.Context, string) (io.ReadCloser, error)
	Close()
}
type Resolver func(context.Context, string) (Remote, error)
type Operation struct {
	ID         string     `json:"id"`
	ArchiveID  string     `json:"archive_id"`
	Kind       string     `json:"kind"`
	MediaID    string     `json:"media_id"`
	State      string     `json:"state"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Progress   int        `json:"progress_percent"`
	Bytes      int64      `json:"bytes"`
	Events     []Event    `json:"events"`
	Receipt    *Receipt   `json:"receipt,omitempty"`
	Manifest   *Manifest  `json:"manifest,omitempty"`
	Cleanup    bool       `json:"cleanup_confirmed"`
}

func (s *Store) SetRemoteResolver(fn Resolver) { s.mu.Lock(); defer s.mu.Unlock(); s.resolve = fn }
func (s *Store) Operations() []Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Operation{}, s.operations...)
	for i := range out {
		out[i].Events = append([]Event{}, out[i].Events...)
	}
	return out
}
func (s *Store) loadOperations() error {
	b, e := os.ReadFile(filepath.Join(s.dir, "operations.json"))
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if e = json.Unmarshal(b, &s.operations); e != nil {
		return e
	}
	for i := range s.operations {
		v := &s.operations[i]
		if v.State == "running" {
			v.State = "error"
			v.Events = append(v.Events, Event{At: time.Now().UTC(), Message: "Операция прервана; удалённую копию можно повторно подтвердить, тестовая транзакция откатывается при отключении"})
		}
	}
	return s.save("operations.json", s.operations)
}
func (s *Store) StartOperation(kind, id, media, actor string) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.maintenanceBusy {
		return Operation{}, errors.New("catalog maintenance already running or stopping")
	}
	if kind != "replicate" && kind != "drill" {
		return Operation{}, errors.New("unknown operation")
	}
	v, ok := s.entries[id]
	if !ok || !validID(id) || v.Manifest == nil || v.SHA256 == "" {
		return Operation{}, errors.New("verified archive required")
	}
	if media == "" && v.State != "success" {
		return Operation{}, errors.New("local archive unavailable")
	}
	if kind == "replicate" && (media == "" || s.policy.RemoteRetentionDays < 1 || v.State != "success") {
		return Operation{}, errors.New("local archive, target media and remote retention required")
	}
	if kind == "drill" && s.cfg.CatalogBackup.DrillDatabaseURLFile == "" {
		return Operation{}, errors.New("dedicated drill database URL file not configured")
	}
	if media != "" && s.resolve == nil {
		return Operation{}, errors.New("media transport unavailable")
	}
	op := Operation{ID: model.ID(), ArchiveID: id, Kind: kind, MediaID: media, State: "running", CreatedAt: time.Now().UTC(), Events: []Event{{At: time.Now().UTC(), Message: "Операция принята"}}}
	next := append([]Operation{op}, s.operations...)
	if e := s.save("operations.json", next); e != nil {
		return Operation{}, e
	}
	s.operations = next
	s.maintenanceBusy = true
	s.wg.Add(1)
	go s.maintain(op, v, s.policy, s.resolve, actor)
	return op, nil
}

type progressReader struct {
	io.Reader
	update func(int64)
	n      int64
	last   time.Time
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, e := r.Reader.Read(p)
	r.n += int64(n)
	if time.Since(r.last) > 500*time.Millisecond {
		r.last = time.Now()
		r.update(r.n)
	}
	return n, e
}
func (s *Store) maintain(op Operation, entry Entry, p Policy, resolve Resolver, actor string) {
	defer s.wg.Done()
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Hour)
	defer cancel()
	progress := func(n int64) {
		s.mu.Lock()
		defer s.mu.Unlock()
		op.Bytes = n
		if entry.Bytes > 0 {
			op.Progress = int(n * 80 / entry.Bytes)
			if op.Progress > 80 {
				op.Progress = 80
			}
		}
		s.operations[0] = op
	}
	stage := func(message string, percent int) {
		s.mu.Lock()
		defer s.mu.Unlock()
		op.Progress = percent
		op.Events = append(op.Events, Event{At: time.Now().UTC(), Message: message})
		s.operations[0] = op
	}
	var remote Remote
	var e error
	if op.MediaID != "" {
		remote, e = resolve(ctx, op.MediaID)
		if e == nil {
			defer remote.Close()
		}
	}
	if e == nil && op.Kind == "replicate" {
		stage("Передача зашифрованного архива по mTLS; ожидание подтверждения медиасервера", 5)
		var f *os.File
		f, e = s.Archive(op.ArchiveID)
		if e == nil {
			receipt, err := remote.Upload(ctx, entry.ID, entry.SHA256, p.RemoteRetentionDays, &progressReader{Reader: f, update: progress})
			f.Close()
			e = err
			if e == nil && (receipt.ID != entry.ID || receipt.SHA256 != entry.SHA256 || receipt.Bytes != entry.Bytes || !receipt.ExpiresAt.After(time.Now())) {
				e = errors.New("invalid replica receipt")
			}
			if e == nil {
				op.Receipt = &receipt
			}
		}
	}
	if e == nil && op.Kind == "drill" {
		stage("Получение архива и сверка SHA-256", 5)
		var reader io.ReadCloser
		if remote != nil {
			reader, e = remote.Download(ctx, entry.ID)
		} else {
			reader, e = s.Archive(entry.ID)
		}
		if e == nil {
			// Authenticate the entire ciphertext before importing any rows.
			var f *os.File
			f, e = os.CreateTemp(s.dir, ".drill-")
			if e == nil {
				hash := sha256.New()
				var n int64
				n, e = io.Copy(io.MultiWriter(f, hash), io.LimitReader(&progressReader{Reader: reader, update: progress}, entry.Bytes+1))
				reader.Close()
				if e == nil && (n != entry.Bytes || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256) {
					e = errors.New("replica checksum mismatch")
				}
				if e == nil {
					_, e = f.Seek(0, io.SeekStart)
				}
				if e == nil {
					stage("Импорт в отдельную тестовую БД; проверка режима восстановления и откат", 90)
					var result RestoreResult
					result, e = s.drill(ctx, f)
					if e == nil {
						op.Manifest = &result.Manifest
						op.Cleanup = true
					}
				}
				f.Close()
				if err := os.Remove(f.Name()); err != nil {
					op.Cleanup = false
					if e == nil {
						e = err
					}
				}
			} else {
				reader.Close()
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maintenanceBusy = false
	now := time.Now().UTC()
	op.FinishedAt = &now
	if e != nil {
		op.State = "error"
		op.Events = append(op.Events, Event{At: now, Message: "Операция завершилась ошибкой на указанном выше этапе; успешное восстановление не подтверждено"})
	} else {
		op.State = "success"
		op.Progress = 100
		op.Bytes = entry.Bytes
		message := "Удалённый медиасервер подтвердил SHA-256 и независимый срок хранения"
		if op.Kind == "drill" {
			message = "Каталог импортирован; задания остановлены, ключи и сертификаты проверены; тестовая транзакция отменена, временный архив удалён"
		}
		op.Events = append(op.Events, Event{At: now, Message: message})
	}
	s.operations[0] = op
	if err := s.save("operations.json", s.operations); err != nil {
		s.operations[0].State = "error"
		s.operations[0].Events = append(s.operations[0].Events, Event{At: now, Message: "Не удалось сохранить отчёт"})
	}
	_ = s.db.Audit(ctx, actor, "catalog."+op.Kind, entry.ID, s.operations[0].State)
}
func (s *Store) drill(ctx context.Context, r io.Reader) (RestoreResult, error) {
	path := s.cfg.CatalogBackup.DrillDatabaseURLFile
	st, e := os.Stat(path)
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return RestoreResult{}, errors.New("private drill database URL file required")
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return RestoreResult{}, e
	}
	dsn := strings.TrimSpace(string(b))
	target, e := pgconn.ParseConfig(dsn)
	if e != nil {
		return RestoreResult{}, errors.New("invalid drill database")
	}
	source, e := pgconn.ParseConfig(s.cfg.DatabaseURL)
	if e != nil {
		return RestoreResult{}, e
	}
	// Deliberately conservative: even another server must use a distinct database name.
	if target.Database == source.Database {
		return RestoreResult{}, errors.New("drill database name must differ from production")
	}
	return Restore(ctx, r, s.key, RestoreOptions{DatabaseURL: dsn, Schema: "srk_drill_" + model.ID(), Installation: s.cfg.Installation, Directory: filepath.Join(s.dir, "unused-drill"), Drill: true})
}
func (s *Store) maintenanceTick(now time.Time) {
	s.mu.Lock()
	if s.closed || s.busy || s.maintenanceBusy {
		s.mu.Unlock()
		return
	}
	p := s.policy
	// A receipt must refer to the same archive and destination and remain retained.
	confirmed := func(id, media string) bool {
		for _, op := range s.operations {
			if op.ArchiveID == id && op.MediaID == media && op.Kind == "replicate" && op.State == "success" && op.Receipt != nil && op.Receipt.ExpiresAt.After(now) {
				return true
			}
		}
		return false
	}
	var candidate Entry
	for _, v := range s.entries {
		if p.LocalRetentionDays > 0 && v.State == "success" && now.Sub(v.CreatedAt) >= time.Duration(p.LocalRetentionDays)*24*time.Hour && (p.TargetMediaID == "" || confirmed(v.ID, p.TargetMediaID)) {
			if e := os.Remove(filepath.Join(s.dir, v.ID+".srkcat")); e == nil || os.IsNotExist(e) {
				v.State = "expired"
				v.Events = append(v.Events, Event{At: now, Message: "Локальный срок хранения истёк; удалённая копия имеет свой срок"})
				if s.save(v.ID+".json", v) == nil {
					s.entries[v.ID] = v
				}
			}
		}
		if v.State == "success" && v.CreatedAt.After(candidate.CreatedAt) {
			candidate = v
		}
	}
	// Replicate oldest outstanding archive first; retry failures at most once a minute.
	pending := Entry{}
	if p.TargetMediaID != "" {
		for _, v := range s.entries {
			if v.State != "success" || confirmed(v.ID, p.TargetMediaID) {
				continue
			}
			attempted := false
			for _, op := range s.operations {
				if op.Kind == "replicate" && op.ArchiveID == v.ID && op.MediaID == p.TargetMediaID {
					attempted = true
					if op.State == "error" && now.Sub(op.CreatedAt) >= time.Minute {
						attempted = false
					}
					break
				}
			}
			if !attempted && (pending.ID == "" || v.CreatedAt.Before(pending.CreatedAt)) {
				pending = v
			}
		}
	}
	last := time.Time{}
	for _, op := range s.operations {
		if op.Kind == "drill" {
			last = op.CreatedAt
			break
		}
	}
	// Prefer the latest retained remote copy, including archives already expired locally.
	if p.TargetMediaID != "" {
		candidate = Entry{}
		for _, v := range s.entries {
			if confirmed(v.ID, p.TargetMediaID) && v.CreatedAt.After(candidate.CreatedAt) {
				candidate = v
			}
		}
	}
	due := p.DrillIntervalSeconds > 0 && candidate.ID != "" && now.Sub(last) >= time.Duration(p.DrillIntervalSeconds)*time.Second
	s.mu.Unlock()
	if pending.ID != "" {
		_, _ = s.StartOperation("replicate", pending.ID, p.TargetMediaID, "manager/catalog-schedule")
		return
	}
	if due {
		_, _ = s.StartOperation("drill", candidate.ID, p.TargetMediaID, "manager/catalog-schedule")
	}
}
