// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package tape

import (
	"context"
	"encoding/json"
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"os"
	"path/filepath"
)

type operation struct {
	Request model.TapeRequest `json:"request"`
	Phase   string            `json:"phase"`
	Result  model.Result      `json:"result"`
	Error   string            `json:"error,omitempty"`
}

// The journal prevents replaying a destructive command after a lost HTTP reply.
func (s *Store) Execute(ctx context.Context, id string, v model.TapeRequest, r *repository.Repository, report func(model.Result)) (model.Result, error) {
	if !idPattern.MatchString(id) {
		return model.Result{}, errors.New("invalid operation ID")
	}
	path := filepath.Join(s.root, "operations", id+".json")
	s.mu.Lock()
	var old operation
	e := read(path, &old)
	if e == nil {
		s.mu.Unlock()
		a, _ := json.Marshal(old.Request)
		b, _ := json.Marshal(v)
		if string(a) != string(b) {
			return model.Result{}, errors.New("operation identity conflict")
		}
		if old.Phase != "complete" {
			return old.Result, errors.New("previous tape operation incomplete; inspect cartridge before retrying with a new operation ID")
		}
		return old.Result, nil
	}
	if !os.IsNotExist(e) {
		s.mu.Unlock()
		return model.Result{}, e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e == nil {
		e = save(path, operation{Request: v, Phase: "started"})
	}
	s.mu.Unlock()
	if e != nil {
		return model.Result{}, e
	}
	res, e := s.execute(ctx, id, v, r, report)
	op := operation{Request: v, Phase: "complete", Result: res}
	if e != nil {
		op.Phase = "failed"
		op.Error = e.Error()
	}
	if persist := save(path, op); persist != nil {
		return res, persist
	}
	return res, e
}
