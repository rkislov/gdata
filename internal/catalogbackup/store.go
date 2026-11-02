// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package catalogbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"gdata/srk/internal/catalog"
	"gdata/srk/internal/config"
	"gdata/srk/internal/model"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Event struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
}
type Entry struct {
	ID         string     `json:"id"`
	State      string     `json:"state"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Rows       int64      `json:"rows"`
	TotalRows  int64      `json:"total_rows"`
	Progress   int        `json:"progress_percent"`
	Bytes      int64      `json:"bytes"`
	SHA256     string     `json:"sha256"`
	Manifest   *Manifest  `json:"manifest,omitempty"`
	Events     []Event    `json:"events"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
}
type Policy struct {
	TargetMediaID        string `json:"target_media_id"`
	LocalRetentionDays   int    `json:"local_retention_days"`
	RemoteRetentionDays  int    `json:"remote_retention_days"`
	DrillIntervalSeconds int    `json:"drill_interval_seconds"`
	Version              int    `json:"version"`
	IntervalSeconds      int    `json:"interval_seconds"`
}
type Store struct {
	resolve         Resolver
	maintenanceBusy bool
	operations      []Operation
	lockFile        *os.File
	closeOnce       sync.Once
	mu              sync.Mutex
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	closed, busy    bool
	dir             string
	key             []byte
	db              *catalog.Catalog
	cfg             config.Config
	entries         map[string]Entry
	policy          Policy
	lastStart       time.Time
}

func OpenStore(ctx context.Context, db *catalog.Catalog, c config.Config) (*Store, error) {
	if c.CatalogBackup == nil {
		return nil, nil
	}
	key, e := ReadKey(c.CatalogBackup.KeyFile)
	if e != nil {
		return nil, e
	}
	dir := c.CatalogBackup.Directory
	if e = os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	st, e := os.Stat(dir)
	if e != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("catalog backup directory must be private (0700)")
	}
	lock, e := os.OpenFile(filepath.Join(dir, ".writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		lock.Close()
		return nil, errors.New("catalog backup storage already has a writer")
	}
	owned := false
	defer func() {
		if !owned {
			syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			lock.Close()
		}
	}()
	identityFile := filepath.Join(dir, ".installation")
	previous, e := os.ReadFile(identityFile)
	if os.IsNotExist(e) {
		if e = privateWrite(identityFile, []byte(c.Installation)); e != nil {
			return nil, e
		}
	} else if e != nil || string(previous) != c.Installation {
		return nil, errors.New("catalog backup storage belongs to a different installation")
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Store{lockFile: lock, ctx: ctx, cancel: cancel, dir: dir, key: key, db: db, cfg: c, entries: map[string]Entry{}}
	b, e := os.ReadFile(filepath.Join(dir, "policy.json"))
	if e == nil {
		if e = json.Unmarshal(b, &s.policy); e != nil {
			cancel()
			return nil, e
		}
	} else if !os.IsNotExist(e) {
		cancel()
		return nil, e
	}
	files, e := os.ReadDir(dir)
	if e != nil {
		cancel()
		return nil, e
	}
	for _, f := range files {
		if !f.IsDir() && strings.HasPrefix(f.Name(), ".drill-") {
			if e = os.Remove(filepath.Join(dir, f.Name())); e != nil {
				cancel()
				return nil, e
			}
			continue
		}
		if filepath.Ext(f.Name()) != ".json" || f.Name() == "policy.json" {
			continue
		}
		b, e := os.ReadFile(filepath.Join(dir, f.Name()))
		if e != nil {
			cancel()
			return nil, e
		}
		var v Entry
		if json.Unmarshal(b, &v) != nil || !validID(v.ID) || f.Name() != v.ID+".json" {
			continue
		}
		if v.State == "running" {
			v.State = "error"
			v.Events = append(v.Events, Event{At: time.Now().UTC(), Message: "Создание прервано перезапуском; неполный архив не используется"})
		}
		s.entries[v.ID] = v
		if v.CreatedAt.After(s.lastStart) {
			s.lastStart = v.CreatedAt
		}
	}
	if e = s.loadOperations(); e != nil {
		cancel()
		return nil, e
	}
	owned = true
	return s, nil
}
func validID(id string) bool { return regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(id) }
func (s *Store) save(name string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(s.dir, ".metadata-")
	if e != nil {
		return e
	}
	path := f.Name()
	defer os.Remove(path)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(path, filepath.Join(s.dir, name)); e != nil {
		return e
	}
	return syncDir(s.dir)
}
func (s *Store) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.entries))
	for _, v := range s.entries {
		v.Events = append([]Event{}, v.Events...)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}
func (s *Store) Policy() Policy { s.mu.Lock(); defer s.mu.Unlock(); return s.policy }
func (s *Store) SetPolicy(v Policy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.IntervalSeconds != 0 && (v.IntervalSeconds < 60 || v.IntervalSeconds > 30*86400) {
		return errors.New("interval must be 0 or 60–2592000 seconds")
	}
	if v.LocalRetentionDays < 0 || v.LocalRetentionDays > 36500 || v.RemoteRetentionDays < 0 || v.RemoteRetentionDays > 36500 || (v.TargetMediaID != "" && v.RemoteRetentionDays < 1) {
		return errors.New("invalid retention days")
	}
	if v.DrillIntervalSeconds != 0 && (v.DrillIntervalSeconds < 60 || v.DrillIntervalSeconds > 30*86400) {
		return errors.New("invalid drill interval")
	}
	if v.DrillIntervalSeconds > 0 && s.cfg.CatalogBackup.DrillDatabaseURLFile == "" {
		return errors.New("dedicated drill database not configured")
	}
	if v.Version != s.policy.Version {
		return errors.New("policy changed; reload before saving")
	}
	v.Version++
	if e := s.save("policy.json", v); e != nil {
		return e
	}
	s.policy = v
	return nil
}
func (s *Store) Start(actor string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.busy {
		return Entry{}, errors.New("catalog backup already running or manager stopping")
	}
	v := Entry{ID: model.ID(), State: "running", CreatedAt: time.Now().UTC(), Events: []Event{{At: time.Now().UTC(), Message: "Создание согласованной зашифрованной копии"}}}
	if e := s.save(v.ID+".json", v); e != nil {
		return v, e
	}
	s.entries[v.ID] = v
	s.lastStart = v.CreatedAt
	s.busy = true
	s.wg.Add(1)
	go s.create(v, actor)
	return v, nil
}
func (s *Store) create(v Entry, actor string) {
	defer s.wg.Done()
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Hour)
	defer cancel()
	path := filepath.Join(s.dir, v.ID+".partial")
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	var manifest Manifest
	sum := sha256.New()
	last := time.Time{}
	lastTable := ""
	if e == nil {
		manifest, e = Export(ctx, s.db, s.cfg, s.key, io.MultiWriter(f, sum), v.ID, func(table string, rows, total int64) {
			if time.Since(last) < 500*time.Millisecond && table == lastTable {
				return
			}
			last = time.Now()
			s.mu.Lock()
			v.Rows = rows
			v.TotalRows = total
			if total > 0 {
				v.Progress = int(90 * rows / total)
			}
			if table != lastTable {
				v.Events = append(v.Events, Event{At: time.Now().UTC(), Message: "Снимок таблицы " + table})
				lastTable = table
			}
			s.entries[v.ID] = v
			s.mu.Unlock()
		})
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e == nil {
			e = ce
		}
	}
	if e == nil {
		var check *os.File
		check, e = os.Open(path)
		if e == nil {
			_, e = Verify(ctx, check, s.key)
			check.Close()
		}
	}
	if e == nil {
		e = os.Rename(path, filepath.Join(s.dir, v.ID+".srkcat"))
		if e == nil {
			e = syncDir(s.dir)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = false
	now := time.Now().UTC()
	v.FinishedAt = &now
	if e != nil {
		v.State = "error"
		v.Events = append(v.Events, Event{At: now, Message: "Копия не создана или не прошла проверку; повторите после проверки каталога, ключа и свободного места"})
		os.Remove(path)
	} else {
		v.State = "success"
		v.Progress = 100
		v.Manifest = &manifest
		v.VerifiedAt = &now
		v.SHA256 = hex.EncodeToString(sum.Sum(nil))
		v.Rows = 0
		for _, n := range manifest.Counts {
			v.Rows += n
		}
		v.TotalRows = v.Rows
		if st, err := os.Stat(filepath.Join(s.dir, v.ID+".srkcat")); err == nil {
			v.Bytes = st.Size()
		}
		v.Events = append(v.Events, Event{At: now, Message: "Архив записан и полностью проверен; ключ хранится отдельно"})
	}
	if err := s.save(v.ID+".json", v); err != nil {
		v.State = "error"
		v.Events = append(v.Events, Event{At: now, Message: "Не удалось сохранить журнал копии"})
	}
	s.entries[v.ID] = v
	_ = s.db.Audit(ctx, actor, "catalog.backup", v.ID, v.State)
}
func (s *Store) Archive(id string) (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.entries[id]
	if !validID(id) || !ok || v.State != "success" {
		return nil, errors.New("completed catalog backup not found")
	}
	root, e := os.OpenRoot(s.dir)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	return root.Open(id + ".srkcat")
}
func (s *Store) Verify(ctx context.Context, id string) (Manifest, error) {
	f, e := s.Archive(id)
	if e != nil {
		return Manifest{}, e
	}
	defer f.Close()
	m, e := Verify(ctx, f, s.key)
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.entries[id]
	now := time.Now().UTC()
	if e == nil {
		v.VerifiedAt = &now
		v.Events = append(v.Events, Event{At: now, Message: "Повторная проверка архива пройдена"})
	} else {
		v.State = "corrupt"
		v.VerifiedAt = nil
		v.Events = append(v.Events, Event{At: now, Message: "Ошибка проверки целостности архива"})
	}
	if len(v.Events) > 100 {
		v.Events = v.Events[len(v.Events)-100:]
	}
	s.entries[id] = v
	if err := s.save(id+".json", v); e == nil {
		e = err
	}
	return m, e
}
func (s *Store) Run() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case now := <-tick.C:
				state, e := s.db.RecoveryState(s.ctx)
				if e != nil || state.Required {
					continue
				}
				s.mu.Lock()
				due := s.policy.IntervalSeconds > 0 && !s.busy && now.Sub(s.lastStart) >= time.Duration(s.policy.IntervalSeconds)*time.Second
				s.mu.Unlock()
				s.maintenanceTick(now)
				if due {
					_, _ = s.Start("manager/catalog-schedule")
				}
			}
		}
	}()
}
func (s *Store) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.cancel()
		s.mu.Unlock()
		s.wg.Wait()
		syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_UN)
		s.lockFile.Close()
	})
}

func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
