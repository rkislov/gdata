// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package tape

import (
	"context"
	"encoding/binary"
	"errors"
	"gdata/srk/internal/model"
	"gdata/srk/internal/tape/scsi"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type commandLibrary struct {
	erases         int
	eraseFlags     byte
	failErase      bool
	mediumType     byte
	writeProtected bool
	failMove       bool
	failLabel      bool
	loaded         bool
	files          [][][]byte
	pending        [][]byte
	file, record   int
	failRead       bool
	writes         int
}
type commandDevice struct {
	lib    *commandLibrary
	kind   byte
	serial string
}

func (x commandDevice) Execute(ctx context.Context, c, b []byte, dir scsi.Direction) (int, error) {
	if e := ctx.Err(); e != nil {
		return 0, e
	}
	l := x.lib
	switch c[0] {
	case 0x12:
		if c[1] == 1 {
			b[1] = 0x80
			binary.BigEndian.PutUint16(b[2:4], uint16(len(x.serial)))
			copy(b[4:], x.serial)
			return 4 + len(x.serial), nil
		}
		b[0] = x.kind
		return 36, nil
	case 0x1a:
		b[1] = 0x98
		if l.mediumType != 0 {
			b[1] = l.mediumType
		}
		if l.writeProtected {
			b[2] = 0x80
		}
		b[3] = 8
		return 12, nil
	case 0xb8:
		n := 120
		b[7] = 112
		for i := 0; i < 2; i++ {
			p := 8 + i*56
			b[p] = byte(2 + i*2)
			b[p+1] = 128
			b[p+3] = 48
			b[p+7] = 48
			binary.BigEndian.PutUint16(b[p+8:p+10], uint16(100+i*100))
			full := (i == 0 && !l.loaded) || (i == 1 && l.loaded)
			if full {
				b[p+10] = 1
				for k := p + 20; k < p+52; k++ {
					b[k] = ' '
				}
				copy(b[p+20:p+52], "SRK001L9")
			}
		}
		return n, nil
	case 0x19:
		if l.file != 0 {
			return 0, errors.New("erase not at BOT")
		}
		l.erases++
		l.eraseFlags = c[1]
		l.files = nil
		l.pending = nil
		l.file = 0
		l.record = 0
		if l.failErase {
			return 0, errors.New("lost erase completion")
		}
		return 0, nil
	case 0xa5:
		dest := binary.BigEndian.Uint16(c[6:8])
		l.loaded = dest == 200
		if l.failMove {
			return 0, errors.New("lost MOVE reply")
		}
		return 0, nil
	case 1:
		l.file = 0
		l.record = 0
		l.pending = nil
		return 0, nil
	case 0x11:
		l.file += int(c[2])<<16 | int(c[3])<<8 | int(c[4])
		l.record = 0
		return 0, nil
	case 0x0a:
		l.pending = append(l.pending, append([]byte(nil), b...))
		l.writes++
		return len(b), nil
	case 0x10:
		if l.file > len(l.files) {
			return 0, errors.New("invalid position")
		}
		l.files = append(l.files[:l.file], l.pending)
		l.pending = nil
		l.file++
		l.record = 0
		return 0, nil
	case 8:
		if l.failLabel && l.file == 0 && len(l.files) > 0 {
			return 0, errors.New("lost label readback")
		}
		if l.failRead && l.file > 0 {
			return 0, errors.New("simulated lost readback connection")
		}
		if l.file >= len(l.files) {
			return 0, &scsi.SenseError{Status: 2, Key: 8}
		}
		if l.record >= len(l.files[l.file]) {
			l.file++
			l.record = 0
			return 0, &scsi.SenseError{Status: 2, Filemark: true}
		}
		n := copy(b, l.files[l.file][l.record])
		l.record++
		return n, nil
	}
	return 0, nil
}
func TestSCSIBackendCommandRoundTripAndCrashRecovery(t *testing.T) {
	_, repo, manifest := tapeFixture(t)
	cfg := emulatorConfig()
	cfg.Backend = "scsi"
	cfg.Drives = []string{"DRIVE1"}
	cfg.Cartridges = cfg.Cartridges[:1]
	cfg.Cartridges[0].Slot = 100
	cfg.SCSI = &SCSIConfig{Changer: SCSIPath{Path: "/dev/fixture-changer", Serial: "CHANGER"}, Transport: 1, Drives: map[string]SCSIPath{"DRIVE1": {Path: "/dev/fixture-drive", Serial: "DRIVE", Element: 200}}}
	lib := &commandLibrary{}
	open := func(p SCSIPath) (*scsi.Device, func(), error) {
		kind := byte(1)
		if p.Serial == "CHANGER" {
			kind = 8
		}
		return &scsi.Device{IO: commandDevice{lib, kind, p.Serial}}, func() {}, nil
	}
	root := t.TempDir()
	s, e := OpenSCSI(root, cfg, open)
	if e != nil {
		t.Fatal(e)
	}
	run := func(v model.TapeRequest) (model.Result, error) {
		v.Barcode = "SRK001L9"
		v.DriveID = "DRIVE1"
		return s.Execute(context.Background(), model.ID(), v, repo, func(model.Result) {})
	}
	lib.failMove = true
	if _, e = run(model.TapeRequest{Action: "load"}); e == nil {
		t.Fatal("lost move accepted")
	}
	lib.failMove = false
	s, e = OpenSCSI(root, cfg, open)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = run(model.TapeRequest{Action: "recover"}); e != nil {
		t.Fatal(e)
	}
	lib.failLabel = true
	if _, e = run(model.TapeRequest{Action: "label", Label: "Physical test", Confirm: "SRK001L9", Reason: "fixture"}); e == nil {
		t.Fatal("lost label accepted")
	}
	lib.failLabel = false
	s, e = OpenSCSI(root, cfg, open)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = run(model.TapeRequest{Action: "recover"}); e != nil {
		t.Fatal(e)
	}
	lib.failRead = true
	if _, e = run(model.TapeRequest{Action: "write", SnapshotID: manifest.ID, RetentionDays: 30}); e == nil {
		t.Fatal("unconfirmed physical write accepted")
	}
	count := lib.writes
	if _, e = run(model.TapeRequest{Action: "write", SnapshotID: manifest.ID, RetentionDays: 30}); e == nil || lib.writes != count {
		t.Fatal("write after uncertain position")
	}
	s, e = OpenSCSI(root, cfg, open)
	if e != nil {
		t.Fatal(e)
	}
	lib.failRead = false
	if _, e = run(model.TapeRequest{Action: "recover"}); e != nil {
		t.Fatal(e)
	}
	if lib.writes != count {
		t.Fatal("recovery rewrote physical data")
	}
	inv, e := s.Inventory(context.Background())
	if e != nil || inv.Position != nil || len(inv.Cartridges[0].Copies) != 1 || !inv.Cartridges[0].Copies[0].Verified {
		t.Fatal("catalog not recovered", e)
	}
	copyID := inv.Cartridges[0].Copies[0].ID
	if e = os.Remove(filepath.Join(s.stage.volumeDir("SRK001L9"), copyID+".srktape")); e != nil {
		t.Fatal(e)
	}
	out, e := run(model.TapeRequest{Action: "restore", CopyID: copyID, RetentionDays: 7})
	if e != nil {
		t.Fatal(e)
	}
	if e = repo.Verify(out.Tape.Snapshot.ID); e != nil {
		t.Fatal(e)
	}
	writeID := model.ID()
	request := model.TapeRequest{Action: "write", Barcode: "SRK001L9", DriveID: "DRIVE1", SnapshotID: manifest.ID, RetentionDays: 30}
	callback := func(v model.Result) {
		if v.Verified {
			t.Error("staging advertised as physical verification")
		}
	}
	if _, e = s.Execute(context.Background(), writeID, request, repo, callback); e != nil {
		t.Fatal(e)
	}
	count = lib.writes
	s, e = OpenSCSI(root, cfg, open)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Execute(context.Background(), writeID, request, repo, callback); e != nil || lib.writes != count {
		t.Fatal("write replay", e)
	}
	request.RetentionDays = 31
	if _, e = s.Execute(context.Background(), writeID, request, repo, callback); e == nil {
		t.Fatal("request identity changed")
	}
	// Corruption must fail even if a verified staging archive is still present.
	lib.files[1][0][0] ^= 1
	if _, e = run(model.TapeRequest{Action: "verify"}); e == nil {
		t.Fatal("physical corruption ignored")
	}
	lib.files[1][0][0] ^= 1
	if _, e = run(model.TapeRequest{Action: "unload"}); e != nil || lib.loaded {
		t.Fatal("unload", e)
	}
}

func TestSCSIImportEraseRecycleLifecycle(t *testing.T) {
	_, repo, manifest := tapeFixture(t)
	cfg := emulatorConfig()
	cfg.Backend = "scsi"
	cfg.Drives = []string{"DRIVE1"}
	cfg.Cartridges = cfg.Cartridges[:1]
	cfg.Cartridges[0].Slot = 100
	cfg.SCSI = &SCSIConfig{Changer: SCSIPath{Path: "/dev/changer", Serial: "CHANGER"}, Transport: 1, Drives: map[string]SCSIPath{"DRIVE1": {Path: "/dev/drive", Serial: "DRIVE", Element: 200}}}
	lib := &commandLibrary{}
	open := func(p SCSIPath) (*scsi.Device, func(), error) {
		kind := byte(1)
		if p.Serial == "CHANGER" {
			kind = 8
		}
		return &scsi.Device{IO: commandDevice{lib, kind, p.Serial}}, func() {}, nil
	}
	root := t.TempDir()
	s, e := OpenSCSI(root, cfg, open)
	if e != nil {
		t.Fatal(e)
	}
	run := func(v model.TapeRequest) (model.Result, error) {
		v.Barcode = "SRK001L9"
		v.DriveID = "DRIVE1"
		return s.Execute(context.Background(), model.ID(), v, repo, func(model.Result) {})
	}
	must := func(v model.TapeRequest) model.Result {
		t.Helper()
		res, e := run(v)
		if e != nil {
			t.Fatal(v.Action, e)
		}
		return res
	}
	must(model.TapeRequest{Action: "load"})
	must(model.TapeRequest{Action: "label", Label: "Archive", Confirm: "SRK001L9", Reason: "init"})
	out := must(model.TapeRequest{Action: "write", SnapshotID: manifest.ID, RetentionDays: 30})
	copyID := out.Tape.CopyID
	// A second media server has neither local catalog nor cached archives.
	root = t.TempDir()
	s, e = OpenSCSI(root, cfg, open)
	if e != nil {
		t.Fatal(e)
	}
	erase := model.TapeRequest{Action: "erase", EraseMode: "long", Confirm: "SRK001L9", Reason: "expired"}
	if _, e = run(erase); e == nil || lib.erases != 0 {
		t.Fatal("unknown medium erased")
	}
	must(model.TapeRequest{Action: "import"})
	out = must(model.TapeRequest{Action: "restore", CopyID: copyID, RetentionDays: 7})
	if e = repo.Verify(out.Tape.Snapshot.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = run(erase); e == nil || lib.erases != 0 {
		t.Fatal("retained copy erased")
	}
	s.stage.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	lib.mediumType = 0x9c
	if _, e = run(erase); e == nil || lib.erases != 0 {
		t.Fatal("hardware WORM erased")
	}
	lib.mediumType = 0x98
	lib.writeProtected = true
	if _, e = run(erase); e == nil || lib.erases != 0 {
		t.Fatal("write-protected tape erased")
	}
	lib.writeProtected = false
	// Corrupt physical data cannot pass preflight or silently shorten the catalog.
	lib.files[1][0][0] ^= 1
	if _, e = run(erase); e == nil || lib.erases != 0 {
		t.Fatal("corrupt medium erased")
	}
	lib.files[1][0][0] ^= 1
	must(model.TapeRequest{Action: "recover"})
	lib.failErase = true
	if _, e = run(erase); e == nil || lib.erases != 1 || lib.eraseFlags != 1 {
		t.Fatal("erase fault", e)
	}
	if _, e = run(model.TapeRequest{Action: "write", SnapshotID: manifest.ID, RetentionDays: 1}); e == nil {
		t.Fatal("write allowed during uncertain erase")
	}
	s, e = OpenSCSI(root, cfg, open)
	if e != nil {
		t.Fatal(e)
	}
	lib.failErase = false
	must(model.TapeRequest{Action: "recover"})
	if lib.erases != 1 {
		t.Fatal("ERASE repeated by recovery")
	}
	must(model.TapeRequest{Action: "write", SnapshotID: manifest.ID, RetentionDays: 1})
	s.stage.now = func() time.Time { return time.Now().Add(2 * 24 * time.Hour) }
	must(model.TapeRequest{Action: "recycle", Confirm: "SRK001L9", Reason: "reuse"})
	if lib.erases != 2 || lib.eraseFlags != 0 || len(lib.files) != 1 {
		t.Fatal("short erase lifecycle")
	}
	// Embedded hold survives a move to another media server and blocks erase.
	must(model.TapeRequest{Action: "write", SnapshotID: manifest.ID, RetentionDays: 1, Hold: true})
	s, e = OpenSCSI(t.TempDir(), cfg, open)
	if e != nil {
		t.Fatal(e)
	}
	must(model.TapeRequest{Action: "import"})
	s.stage.now = func() time.Time { return time.Now().Add(10 * 24 * time.Hour) }
	if _, e = run(erase); e == nil || lib.erases != 2 {
		t.Fatal("hold lost during import")
	}
}
