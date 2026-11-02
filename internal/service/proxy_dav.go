// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"encoding/json"
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/virtualization"
	"io"
)

type proxyDAV struct {
	a *Agent
	v model.Execute
}

func (d *proxyDAV) Create(ctx context.Context, j *virtualization.Journal, restore bool, user string, quota int64) (model.DAVSession, error) {
	for i := range j.Resources {
		if j.Resources[i].Kind == "dav-session" {
			j.Resources[i].Name = d.v.MediaURL
			j.Resources[i].Scope = d.v.Job.MediaID
		}
	}
	if e := j.Save(); e != nil {
		return model.DAVSession{}, e
	}
	var result model.DAVSession
	b, _ := json.Marshal(map[string]any{"user": user, "max_bytes": quota})
	response, e := d.a.media(ctx, d.v, "POST", "/v1/dav/sessions/"+j.ID, b)
	if e != nil {
		return result, e
	}
	defer response.Body.Close()
	e = json.NewDecoder(response.Body).Decode(&result)
	return result, e
}
func (d *proxyDAV) Seal(ctx context.Context, j *virtualization.Journal) (model.VMArchive, error) {
	var result model.VMArchive
	response, e := d.a.media(ctx, d.v, "POST", "/v1/dav/sessions/"+j.ID+"/seal", []byte("{}"))
	if e != nil {
		return result, e
	}
	defer response.Body.Close()
	e = json.NewDecoder(response.Body).Decode(&result)
	return result, e
}
func (d *proxyDAV) Open(ctx context.Context, j *virtualization.Journal, name string) (io.ReadCloser, error) {
	response, e := d.a.media(ctx, d.v, "GET", "/v1/dav/sessions/"+j.ID+"/archive", nil)
	if e != nil {
		return nil, e
	}
	return response.Body, nil
}
func (d *proxyDAV) Close(ctx context.Context, j *virtualization.Journal) error {
	response, e := d.a.media(ctx, d.v, "DELETE", "/v1/dav/sessions/"+j.ID, nil)
	if e != nil {
		return e
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errors.New("WebDAV session close failed")
	}
	return nil
}
