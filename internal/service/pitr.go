// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gdata/srk/internal/config"
	"gdata/srk/internal/model"
	"gdata/srk/internal/repository"
)

func validatePITR(p *model.PITRRequest, base *model.PostgreSQLBackup) error {
	if p == nil {
		return nil
	}
	p.TargetTime = p.TargetTime.UTC().Truncate(time.Microsecond)
	inclusive := p.IncludeTarget()
	p.Inclusive = &inclusive
	if base == nil || base.ArchiveID == "" || base.SystemID == "" || len(base.WALRanges) != 1 || !repository.WALSegmentSize(base.SegmentSize) {
		return errors.New("PITR requires a new PostgreSQL base backup linked to a WAL archive")
	}
	if p.TargetTime.IsZero() || !p.TargetTime.After(base.FinishedAt) || p.TargetTime.After(time.Now()) {
		return errors.New("PITR target must be after the base backup and not in the future")
	}
	return nil
}
func postgresLiteral(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "'", "''") + "'"
}
func shellLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func (a *Agent) recoverPITR(ctx context.Context, v model.Execute, m model.Manifest, res *model.Result) error {
	meta := m.PostgreSQL
	if e := validatePITR(v.PITR, meta); e != nil {
		return e
	}
	p, e := a.postgreSQLRestoreProfile(meta)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	res.Phase = "postgresql-wal-download"
	res.ProgressScope = "wal-download"
	res.ProgressPercent = nil
	a.progressUpdate(v.RunID, *res)
	response, e := a.media(ctx, v, "GET", "/v1/wal/"+meta.ArchiveID, nil)
	if e != nil {
		return e
	}
	var records []model.WALRecord
	e = json.NewDecoder(io.LimitReader(response.Body, 64<<20)).Decode(&records)
	response.Body.Close()
	if e != nil {
		return e
	}
	names := []string{}
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, "pg_wal/") && !strings.Contains(strings.TrimPrefix(f.Path, "pg_wal/"), "/") {
			names = append(names, filepath.Base(f.Path))
		}
	}
	for _, r := range records {
		if e = repository.ValidateWALRecord(r); e != nil {
			return e
		}
		if r.ArchiveID != meta.ArchiveID || r.SystemID != meta.SystemID || r.Major != meta.Major || r.SegmentSize != meta.SegmentSize {
			return errors.New("WAL archive identity or version does not match base")
		}
		names = append(names, r.Name)
	}
	continuousEnd, _, e := repository.WALPrefix(meta, names)
	if e != nil {
		return e
	}
	calendarResponse, e := a.media(ctx, v, "GET", "/v1/wal/"+meta.ArchiveID+"/calendar", nil)
	if e != nil {
		return e
	}
	var calendar model.PITRCalendar
	e = json.NewDecoder(io.LimitReader(calendarResponse.Body, 64<<20)).Decode(&calendar)
	calendarResponse.Body.Close()
	if e != nil {
		return e
	}
	found := false
	for _, window := range calendar.Windows {
		if window.BaseID != m.ID {
			continue
		}
		found = true
		var hi, lo uint64
		if _, e = fmt.Sscanf(window.ContiguousEnd, "%X/%X", &hi, &lo); e != nil {
			return e
		}
		if end := hi<<32 | lo; end < continuousEnd {
			continuousEnd = end
		}
	}
	if !found {
		return errors.New("base is not present in the PITR calendar")
	}
	if len(records) == 0 {
		return errors.New("no archived WAL is available beyond the base backup")
	}
	archive := filepath.Join(v.Destination, "srk_wal_archive")
	if e = os.Mkdir(archive, 0700); e != nil {
		return e
	}
	// Only the base timeline is replayed. Other branches are rejected by WALContinuity.
	timeline := fmt.Sprintf("%08X", meta.WALRanges[0].Timeline)
	total := int64(0)
	selected := []model.WALRecord{}
	start, e := repository.WALBasePosition(meta)
	if e != nil {
		return e
	}
	for _, r := range records {
		if len(r.Name) == 24 && strings.HasPrefix(r.Name, timeline) {
			position, e := repository.WALPosition(r.Name, meta.SegmentSize)
			if e != nil {
				return e
			}
			if position < start {
				continue
			}
			if position >= continuousEnd {
				continue
			}
			selected = append(selected, r)
			total += r.Bytes
		}
	}
	if len(selected) == 0 {
		return errors.New("no WAL segments for the base timeline")
	}
	res.TotalBytes = total
	res.ProgressBytes = 0
	for _, r := range selected {
		if e = postgresSpace(v.Destination, a.Config.ReserveBytes, r.Bytes); e != nil {
			return e
		}
		f, e := os.OpenFile(filepath.Join(archive, r.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		response, e := a.media(ctx, v, "GET", "/v1/wal/"+meta.ArchiveID+"/"+r.Name, nil)
		if e != nil {
			f.Close()
			return e
		}
		h := sha256.New()
		n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(response.Body, r.Bytes+1))
		response.Body.Close()
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		if n != r.Bytes || hex.EncodeToString(h.Sum(nil)) != r.SHA256 {
			return errors.New("downloaded WAL checksum mismatch")
		}
		res.ProgressBytes += n
		percent := min(99, 100*float64(res.ProgressBytes)/float64(total))
		res.ProgressPercent = &percent
		a.progressUpdate(v.RunID, *res)
	}
	res.Phase = "postgresql-pitr-recovery"
	res.ProgressScope = "recovery"
	res.ProgressPercent = nil
	a.progressUpdate(v.RunID, *res)
	if e = runPITRRecovery(ctx, p, v.Destination, archive, v.PITR, meta); e != nil {
		return e
	}
	res.PITR = &model.PITRResult{TargetTime: v.PITR.TargetTime, Inclusive: v.PITR.IncludeTarget(), ArchiveID: meta.ArchiveID, Timeline: meta.WALRanges[0].Timeline, Segments: len(selected), Recovered: true}
	return nil
}

func runPITRRecovery(ctx context.Context, p config.PostgreSQLProfile, dest, archive string, target *model.PITRRequest, meta *model.PostgreSQLBackup) error {
	if strings.ContainsAny(dest, "\x00\r\n") {
		return errors.New("PITR destination contains unsupported control characters")
	}
	for _, tool := range []string{"postgres", "pg_ctl", "pg_controldata"} {
		info, e := os.Stat(filepath.Join(p.BinDir, tool))
		if e != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("PITR requires native %s", tool)
		}
	}
	// Never execute includes, preload libraries or external commands from the source configuration.
	original := filepath.Join(dest, "srk_original_config")
	if e := os.Mkdir(original, 0700); e != nil {
		return e
	}
	for _, name := range []string{"postgresql.conf", "postgresql.auto.conf", "pg_hba.conf", "pg_ident.conf"} {
		if e := os.Rename(filepath.Join(dest, name), filepath.Join(original, name)); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	safe := "listen_addresses = ''\nunix_socket_directories = ''\nssl = off\nlogging_collector = off\nlog_destination = 'stderr'\nlc_messages = 'C'\narchive_mode = off\narchive_command = ''\nshared_preload_libraries = ''\nhot_standby = off\nexternal_pid_file = ''\n"
	if e := repository.Atomic(filepath.Join(dest, "postgresql.conf"), []byte(safe), 0600); e != nil {
		return e
	}
	for _, name := range []string{"postgresql.auto.conf", "pg_ident.conf"} {
		if e := repository.Atomic(filepath.Join(dest, name), []byte("# Generated by GData SRK PITR\n"), 0600); e != nil {
			return e
		}
	}
	if e := repository.Atomic(filepath.Join(dest, "pg_hba.conf"), []byte("local all all reject\nhost all all 0.0.0.0/0 reject\nhost all all ::0/0 reject\n"), 0600); e != nil {
		return e
	}
	if e := os.Remove(filepath.Join(dest, "standby.signal")); e != nil && !os.IsNotExist(e) {
		return e
	}
	if e := repository.Atomic(filepath.Join(dest, "recovery.signal"), nil, 0600); e != nil {
		return e
	}
	// Literal percent signs in paths must survive PostgreSQL's restore_command substitutions.
	command := "/bin/cp " + shellLiteral(strings.ReplaceAll(archive, "%", "%%")+"/%f") + " " + shellLiteral("%p")
	// PostgreSQL requires a listening socket. Use a private local Unix socket, reject all SQL clients, and expose no TCP port.
	socket, e := os.MkdirTemp("/tmp", "srk-pitr-socket-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(socket)
	args := []string{"-D", dest, "-c", "config_file=" + filepath.Join(dest, "postgresql.conf"), "-c", "data_directory=" + dest, "-c", "hba_file=" + filepath.Join(dest, "pg_hba.conf"), "-c", "ident_file=" + filepath.Join(dest, "pg_ident.conf"), "-c", "restore_command=" + command, "-c", "recovery_target_time=" + target.TargetTime.UTC().Format("2006-01-02 15:04:05.999999-07:00"), "-c", fmt.Sprintf("recovery_target_inclusive=%t", target.IncludeTarget()), "-c", fmt.Sprintf("recovery_target_timeline=%d", meta.WALRanges[0].Timeline), "-c", "recovery_target_action=promote"}
	args = append(args, "-c", "unix_socket_directories="+socket, "-c", "unix_socket_permissions=0700")
	log, e := os.OpenFile(filepath.Join(dest, "srk-pitr.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer log.Close()
	cmd := exec.CommandContext(ctx, filepath.Join(p.BinDir, "postgres"), args...)
	cmd.Env = postgresEnvironment(p)
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if e = cmd.Start(); e != nil {
		return e
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	exited := false
	defer func() {
		if !exited {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			exited = true
			return errors.New("PostgreSQL did not reach the PITR target; inspect srk-pitr.log in the new destination")
		case <-ticker.C:
			info, e := log.Stat()
			if e != nil {
				return e
			}
			if info.Size() > 16<<20 {
				return errors.New("PITR recovery log limit exceeded")
			}
			b, e := os.ReadFile(log.Name())
			if e != nil {
				return e
			}
			reached := strings.Contains(string(b), "recovery stopping before") || strings.Contains(string(b), "recovery stopping after")
			if !reached || !strings.Contains(string(b), "database system is ready to accept connections") {
				continue
			}
			if e = postgresCommand(ctx, p, "pg_ctl", "-D", dest, "-m", "fast", "-w", "stop"); e != nil {
				return e
			}
			e = <-done
			exited = true
			if e != nil {
				return errors.New("PITR PostgreSQL shutdown failed")
			}
			control, e := readPostgresControl(ctx, p, dest)
			if e != nil {
				return e
			}
			if control.SystemID != meta.SystemID || control.State != "shut down" {
				return errors.New("PITR did not produce a cleanly stopped recovered cluster")
			}
			if _, e = os.Stat(filepath.Join(dest, "recovery.signal")); !os.IsNotExist(e) {
				return errors.New("PITR recovery signal remains after promotion")
			}
			report := model.PITRResult{TargetTime: target.TargetTime, Inclusive: target.IncludeTarget(), ArchiveID: meta.ArchiveID, Timeline: meta.WALRanges[0].Timeline, Recovered: true}
			b, _ = json.MarshalIndent(report, "", "  ")
			return repository.Atomic(filepath.Join(dest, "srk-pitr-result.json"), b, 0600)
		}
	}
}
