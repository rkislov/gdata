// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var opID = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Resource struct {
	Scope   string `json:"scope,omitempty"`
	Kind    string `json:"kind"`
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Keep    bool   `json:"keep"`
	Deleted bool   `json:"deleted"`
}
type Journal struct {
	SourceServer  string                 `json:"source_server,omitempty"`
	SourceVolumes []string               `json:"source_volumes,omitempty"`
	VolumeBacked  bool                   `json:"volume_backed"`
	Version       int                    `json:"version"`
	ID            string                 `json:"id"`
	Owner         string                 `json:"owner"`
	Cloud         string                 `json:"cloud"`
	Kind          string                 `json:"kind"`
	StartedAt     time.Time              `json:"started_at"`
	Success       bool                   `json:"success"`
	Done          bool                   `json:"done"`
	Resources     []Resource             `json:"resources"`
	Restore       *model.VMRestoreResult `json:"restore,omitempty"`
	LastError     string                 `json:"last_error,omitempty"`
	path          string
	lock          *os.File
}

func cloudFingerprint(p Profile) string {
	identity := p.Endpoint + "\x00" + p.GlanceEndpoint + "\x00" + p.CinderEndpoint + "\x00" + p.ProjectID
	if NativeProvider(p.Kind) {
		b, _ := json.Marshal(struct {
			DAVPath, DAVUser, Storage, PBSEndpoint, PBSStore, PBSNamespace, BackupDatastore string
			OneBEX                                                                          map[string]string
		}{p.DAVMountPath, p.DAVUser, p.BackupStorage, p.PBSEndpoint, p.PBSStore, p.PBSNamespace, p.BackupDatastore, p.OneBEXEndpoints})
		identity += "\x00" + string(b)
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func OpenJournal(p Profile, id, owner, kind string) (*Journal, error) {
	if !opID.MatchString(id) || owner == "" || (kind != "backup" && kind != "restore") {
		return nil, errors.New("invalid VM operation identity")
	}
	if e := os.MkdirAll(p.StateDir, 0700); e != nil {
		return nil, e
	}
	st, e := os.Lstat(p.StateDir)
	if e != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("VM journal directory must be private, without symlinks")
	}
	lock, e := os.OpenFile(filepath.Join(p.StateDir, id+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		lock.Close()
		return nil, errors.New("VM operation is already active")
	}
	j := &Journal{Version: 1, ID: id, Owner: owner, Cloud: cloudFingerprint(p), Kind: kind, StartedAt: time.Now().UTC(), Resources: []Resource{}, path: filepath.Join(p.StateDir, id+".json"), lock: lock}
	b, e := os.ReadFile(j.path)
	if e == nil {
		file, lk := j.path, j.lock
		if e = json.Unmarshal(b, j); e != nil {
			lock.Close()
			return nil, e
		}
		j.path = file
		j.lock = lk
		if j.Version != 1 || j.ID != id || j.Owner != owner || j.Cloud != cloudFingerprint(p) || j.Kind != kind {
			lock.Close()
			return nil, errors.New("VM journal belongs to a different operation or cloud")
		}
	} else if !os.IsNotExist(e) {
		lock.Close()
		return nil, e
	} else if e = j.Save(); e != nil {
		lock.Close()
		return nil, e
	}
	return j, nil
}
func (j *Journal) Close() {
	if j.lock != nil {
		_ = j.lock.Close()
		j.lock = nil
	}
}
func (j *Journal) Save() error {
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	return repository.Atomic(j.path, b, 0600)
}
func (j *Journal) Intent(kind, name string, keep bool) (int, error) {
	j.Resources = append(j.Resources, Resource{Kind: kind, Name: name, Keep: keep})
	return len(j.Resources) - 1, j.Save()
}
func (j *Journal) Record(index int, id string) error {
	if !model.CloudUUID(id) || index < 0 || index >= len(j.Resources) {
		return errors.New("invalid cloud resource receipt")
	}
	j.Resources[index].ID = id
	return j.Save()
}
func (j *Journal) MarkSuccess() error { j.Success = true; return j.Save() }
func (j *Journal) tags() map[string]string {
	return map[string]string{"srk_run": j.ID, "srk_proxy": j.Owner}
}
func (j *Journal) Name(suffix string) string { return "srk-" + j.ID + "-" + suffix }
func owned(meta map[string]string, j *Journal) bool {
	return meta["srk_run"] == j.ID && meta["srk_proxy"] == j.Owner
}
func (c *OpenStack) resourceID(ctx context.Context, j *Journal, r Resource) (string, error) {
	if r.ID != "" {
		return r.ID, nil
	}
	var ids []string
	switch r.Kind {
	case "image":
		var out struct {
			Images []Image `json:"images"`
		}
		_, e := c.json(ctx, "Glance", "GET", "/images?name="+urlQuery(r.Name), nil, &out)
		if e != nil {
			return "", e
		}
		for _, im := range out.Images {
			if im.Name == r.Name && im.Owner == c.base.profile.ProjectID && (im.Property("srk_run") == j.ID && im.Property("srk_proxy") == j.Owner || strings.HasPrefix(r.Name, j.Name("export-")) && im.Property("srk_run") == "") {
				ids = append(ids, im.ID)
			}
		}
	case "volume":
		var out struct {
			Volumes []CloudVolume `json:"volumes"`
		}
		_, e := c.json(ctx, "Cinder", "GET", "/volumes/detail?name="+urlQuery(r.Name), nil, &out)
		if e != nil {
			return "", e
		}
		for _, v := range out.Volumes {
			if v.Name == r.Name && owned(v.Metadata, j) {
				ids = append(ids, v.ID)
			}
		}
	case "snapshot":
		var out struct {
			Snapshots []CloudSnapshot `json:"snapshots"`
		}
		_, e := c.json(ctx, "Cinder", "GET", "/snapshots/detail?name="+urlQuery(r.Name), nil, &out)
		if e != nil {
			return "", e
		}
		for _, v := range out.Snapshots {
			if v.Name == r.Name && owned(v.Metadata, j) {
				ids = append(ids, v.ID)
			}
		}
	case "server":
		var out struct {
			Servers []Server `json:"servers"`
		}
		_, e := c.json(ctx, "Nova", "GET", "/servers/detail?name="+urlQuery(regexp.QuoteMeta(r.Name)), nil, &out)
		if e != nil {
			return "", e
		}
		for _, v := range out.Servers {
			if v.Name == r.Name && v.Project == c.base.profile.ProjectID && owned(v.Metadata, j) {
				ids = append(ids, v.ID)
			}
		}
	default:
		return "", errors.New("unknown journal resource kind")
	}
	if len(ids) > 1 {
		return "", errors.New("ambiguous resource reconciliation; automatic deletion refused")
	}
	if len(ids) == 0 {
		return "", nil
	}
	if !model.CloudUUID(ids[0]) {
		return "", errors.New("invalid reconciled resource ID")
	}
	return ids[0], nil
}
func (c *OpenStack) deleteResource(ctx context.Context, j *Journal, r Resource) error {
	var service, path string
	switch r.Kind {
	case "image":
		im, e := c.Image(ctx, r.ID)
		if isMissing(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if im.Property("srk_run") == "" && strings.HasPrefix(r.Name, j.Name("export-")) && im.Name == r.Name {
			patch, _ := json.Marshal([]map[string]any{{"op": "add", "path": "/srk_run", "value": j.ID}, {"op": "add", "path": "/srk_proxy", "value": j.Owner}})
			body, e := c.patchImage(ctx, im.ID, patch)
			if e != nil {
				return e
			}
			body.Close()
			im, e = c.Image(ctx, im.ID)
			if e != nil {
				return e
			}
		}
		if im.Property("srk_run") != j.ID || im.Property("srk_proxy") != j.Owner {
			return errors.New("foreign image; cleanup refused")
		}
		service = "Glance"
		path = "/images/" + r.ID
	case "volume":
		v, e := c.Volume(ctx, r.ID)
		if isMissing(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if !owned(v.Metadata, j) {
			return errors.New("foreign volume; cleanup refused")
		}
		service = "Cinder"
		path = "/volumes/" + r.ID
	case "snapshot":
		v, e := c.Snapshot(ctx, r.ID)
		if isMissing(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if !owned(v.Metadata, j) {
			return errors.New("foreign snapshot; cleanup refused")
		}
		service = "Cinder"
		path = "/snapshots/" + r.ID
	case "server":
		v, e := c.Server(ctx, r.ID)
		if isMissing(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if !owned(v.Metadata, j) {
			return errors.New("foreign VM; cleanup refused")
		}
		service = "Nova"
		path = "/servers/" + r.ID
	default:
		return errors.New("invalid journal kind")
	}
	if _, e := c.json(ctx, service, "DELETE", path, nil, nil); e != nil && !isMissing(e) {
		return e
	}
	return c.wait(ctx, func() (bool, error) {
		_, e := c.json(ctx, service, "GET", path, nil, new(any))
		if isMissing(e) {
			return true, nil
		}
		return false, e
	})
}
func (c *OpenStack) Cleanup(ctx context.Context, j *Journal, log func(string)) error {
	// Resolve all outstanding intents before deletion. Nova's volume-backed anchor
	// may own snapshots that were created before its response was received.
	for i := range j.Resources {
		if j.Resources[i].Deleted || j.Resources[i].ID != "" {
			continue
		}
		id, e := c.resourceID(ctx, j, j.Resources[i])
		if e != nil {
			return e
		}
		if id == "" {
			j.LastError = "unresolved create intent; resource may still appear; retry cleanup after cloud reconciliation"
			_ = j.Save()
			return errors.New(j.LastError)
		}
		if id != "" {
			if e = j.Record(i, id); e != nil {
				return e
			}
		}
	}
	if e := c.adoptSnapshotMapping(ctx, j); e != nil {
		return e
	}
	var failures []string
	for i := len(j.Resources) - 1; i >= 0; i-- {
		r := &j.Resources[i]
		if r.Deleted || j.Success && r.Keep {
			continue
		}
		if r.ID != "" {
			if e := c.deleteResource(ctx, j, *r); e != nil {
				failures = append(failures, fmt.Sprintf("%s %s: %v", r.Kind, r.ID, e))
				continue
			}
		}
		r.Deleted = true
		if e := j.Save(); e != nil {
			return e
		}
		if log != nil {
			log("removed " + r.Kind + " " + r.ID)
		}
	}
	if len(failures) > 0 {
		j.LastError = strings.Join(failures, "; ")
		_ = j.Save()
		return errors.New(j.LastError)
	}
	j.Done = true
	j.LastError = ""
	return j.Save()
}
func Pending(p Profile) ([]Journal, error) {
	entries, e := os.ReadDir(p.StateDir)
	if os.IsNotExist(e) {
		return []Journal{}, nil
	}
	if e != nil {
		return nil, e
	}
	out := []Journal{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !opID.MatchString(id) || !entry.Type().IsRegular() {
			return nil, errors.New("unexpected VM journal entry")
		}
		b, e := os.ReadFile(filepath.Join(p.StateDir, entry.Name()))
		if e != nil {
			return nil, e
		}
		var j Journal
		if e = json.Unmarshal(b, &j); e != nil {
			return nil, e
		}
		if !j.Done {
			out = append(out, j)
		}
	}
	return out, nil
}

func urlQuery(s string) string { return url.QueryEscape(s) }

// Outstanding excludes journals whose live worker still holds the flock. This
// blocks abandoned cleanup without serializing independent VM backups.
func Outstanding(p Profile) ([]Journal, error) {
	pending, e := Pending(p)
	if e != nil {
		return nil, e
	}
	out := []Journal{}
	for _, j := range pending {
		f, e := os.OpenFile(filepath.Join(p.StateDir, j.ID+".lock"), os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			return nil, e
		}
		e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if e == nil {
			out = append(out, j)
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		} else if e != syscall.EWOULDBLOCK && e != syscall.EAGAIN {
			f.Close()
			return nil, e
		}
		f.Close()
	}
	return out, nil
}
