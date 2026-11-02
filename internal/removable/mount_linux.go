//go:build linux

// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package removable

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
)

func checkMount(c Config) error {
	resolved, e := filepath.EvalSymlinks(c.Mount)
	if e != nil || resolved != filepath.Clean(c.Mount) {
		return errors.New("volume mount absent or indirect")
	}
	var dev, mount, parent unix.Stat_t
	if e = unix.Stat(filepath.Join("/dev/disk/by-uuid", c.UUID), &dev); e != nil {
		return errors.New("expected filesystem UUID not present")
	}
	if dev.Mode&unix.S_IFMT != unix.S_IFBLK {
		return errors.New("UUID does not name a block device")
	}
	if e = unix.Stat(c.Mount, &mount); e != nil {
		return e
	}
	if e = unix.Stat(filepath.Dir(c.Mount), &parent); e != nil {
		return e
	}
	if mount.Dev != dev.Rdev || mount.Dev == parent.Dev {
		return errors.New("mount is not the expected independent filesystem")
	}
	b, e := os.ReadFile("/proc/self/mountinfo")
	if e != nil {
		return e
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 6 && strings.ReplaceAll(f[4], `\040`, " ") == c.Mount {
			return nil
		}
	}
	return errors.New("configured path is not a mountpoint")
}
func syncVolume(c Config) error {
	if e := checkMount(c); e != nil {
		return e
	}
	fd, e := unix.Open(c.Mount, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if e != nil {
		return e
	}
	defer unix.Close(fd)
	return unix.Syncfs(fd)
}

func anchorVolume(c Config) (string, func(), error) {
	f, e := os.Open(c.Mount)
	if e != nil {
		return "", func() {}, e
	}
	var mounted, expected unix.Stat_t
	if e = unix.Fstat(int(f.Fd()), &mounted); e == nil {
		e = unix.Stat(filepath.Join("/dev/disk/by-uuid", c.UUID), &expected)
	}
	if e != nil || mounted.Dev != expected.Rdev {
		f.Close()
		return "", func() {}, errors.New("volume changed while opening")
	}
	return fmt.Sprintf("/proc/self/fd/%d", f.Fd()), func() { f.Close() }, nil
}
