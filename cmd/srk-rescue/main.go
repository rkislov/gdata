// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gdata/srk/internal/model"
	"gdata/srk/internal/rescue"
)

type disk struct {
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	Type        string    `json:"type"`
	SectorSize  int       `json:"log-sec"`
	Size        int64     `json:"size"`
	Model       string    `json:"model"`
	Serial      string    `json:"serial"`
	RO          bool      `json:"ro"`
	RM          bool      `json:"rm"`
	Transport   string    `json:"tran"`
	Filesystem  string    `json:"fstype"`
	Mountpoints []*string `json:"mountpoints"`
	Children    []disk    `json:"children"`
}

func safeDisk(d disk, write bool) bool {
	if (write && d.RO) || d.RM || d.Transport == "usb" || d.Filesystem == "iso9660" || d.Filesystem == "squashfs" {
		return false
	}
	for _, p := range d.Mountpoints {
		if p != nil && *p != "" {
			return false
		}
	}
	// Active device-mapper / RAID dependencies are represented as holders.
	holders, e := os.ReadDir(filepath.Join("/sys/class/block", d.Name, "holders"))
	if e != nil || len(holders) != 0 {
		return false
	}
	for _, child := range d.Children {
		if !safeDisk(child, write) {
			return false
		}
	}
	return true
}
func disks(write bool) ([]disk, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("disk operations require Linux rescue environment")
	}
	b, e := exec.Command("lsblk", "--json", "--bytes", "--output", "NAME,PATH,TYPE,SIZE,MODEL,SERIAL,RO,RM,TRAN,FSTYPE,MOUNTPOINTS,LOG-SEC").Output()
	if e != nil {
		return nil, e
	}
	var data struct {
		Devices []disk `json:"blockdevices"`
	}
	if e = json.Unmarshal(b, &data); e != nil {
		return nil, e
	}
	out := []disk{}
	for _, d := range data.Devices {
		diskIdentity(&d)
		if d.Type == "disk" && strings.HasPrefix(d.Path, "/dev/") && safeDisk(d, write) {
			out = append(out, d)
		}
	}
	return out, nil
}

// mdev-based rescue environments do not maintain a udev database. Read the
// kernel's read-only identity attributes when lsblk cannot obtain those fields.
func diskIdentity(d *disk) {
	if filepath.Base(d.Name) != d.Name {
		return
	}
	if d.Serial == "" {
		for _, suffix := range []string{"serial", "device/serial"} {
			if b, e := os.ReadFile(filepath.Join("/sys/class/block", d.Name, suffix)); e == nil {
				d.Serial = strings.TrimSpace(string(b))
				if d.Serial != "" {
					break
				}
			}
		}
	}
	if d.Model == "" {
		if b, e := os.ReadFile(filepath.Join("/sys/class/block", d.Name, "device/model")); e == nil {
			d.Model = strings.TrimSpace(string(b))
		}
	}
	for i := range d.Children {
		diskIdentity(&d.Children[i])
	}
}

var input = bufio.NewReader(os.Stdin)
var inputEnded bool

func ask(prompt string) string {
	fmt.Print(prompt)
	s, e := input.ReadString('\n')
	if e != nil {
		inputEnded = true
		return ""
	}
	return strings.TrimSpace(s)
}
func askCode() string {
	state := exec.Command("stty", "-g")
	state.Stdin = os.Stdin
	original, e := state.Output()
	if e == nil {
		off := exec.Command("stty", "-echo")
		off.Stdin = os.Stdin
		_ = off.Run()
		defer func() {
			restore := exec.Command("stty", strings.TrimSpace(string(original)))
			restore.Stdin = os.Stdin
			_ = restore.Run()
			fmt.Println()
		}()
	}
	return ask("Paste one-time SRK1 code (hidden): ")
}
func disconnect(c *rescue.Client) {
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.JSON(ctx, "POST", "/v1/rescue/disconnect", nil, new(any))
	c.HTTP.CloseIdleConnections()
}
func choice(n int) int {
	v, e := strconv.Atoi(ask("Number (0 = cancel): "))
	if e != nil || v < 1 || v > n {
		return -1
	}
	return v - 1
}
func network() {
	fmt.Println("Network status:")
	cmd := exec.Command("ip", "-brief", "address")
	cmd.Stdout = os.Stdout
	_ = cmd.Run()
	fmt.Println("1 DHCP  2 Static IPv4  0 Back")
	switch ask("> ") {
	case "1":
		iface := ask("Interface: ")
		if _, e := net.InterfaceByName(iface); e != nil {
			fmt.Println(e)
			return
		}
		run("ip", "link", "set", iface, "up")
		run("udhcpc", "-i", iface, "-n", "-q", "-t", "3")
	case "2":
		iface := ask("Interface: ")
		if _, e := net.InterfaceByName(iface); e != nil {
			fmt.Println(e)
			return
		}
		cidr := ask("IPv4/prefix: ")
		gw := ask("Gateway: ")
		dns := ask("DNS IPv4: ")
		if _, _, e := net.ParseCIDR(cidr); e != nil || net.ParseIP(gw) == nil || net.ParseIP(dns) == nil {
			fmt.Println("Invalid IPv4 parameters")
			return
		}
		run("ip", "link", "set", iface, "up")
		run("ip", "address", "replace", cidr, "dev", iface)
		run("ip", "route", "replace", "default", "via", gw, "dev", iface)
		if strings.ContainsAny(dns, "\r\n \t") {
			fmt.Println("Invalid DNS")
			return
		}
		if e := os.WriteFile("/etc/resolv.conf", []byte("nameserver "+dns+"\n"), 0644); e != nil {
			fmt.Println(e)
		}
	}
}
func run(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if e := cmd.Run(); e != nil {
		fmt.Println(e)
	}
}
func restore(ctx context.Context, c *rescue.Client) error {
	backups, e := c.Backups(ctx)
	if e != nil {
		return e
	}
	if len(backups) == 0 {
		return errors.New("no authorized backups")
	}
	fmt.Println("Authorized backups:")
	for i, b := range backups {
		fmt.Printf("%d  %s  %s  %s\n", i+1, b.CreatedAt.Format(time.RFC3339), b.Name, b.Consistency)
	}
	pick := choice(len(backups))
	if pick < 0 {
		return nil
	}
	id := backups[pick].ID
	fmt.Println("Checking backup integrity on media server...")
	m, e := c.Manifest(ctx, id)
	if e != nil {
		return e
	}
	image, e := rescue.DiskImage(m)
	if e != nil {
		return e
	}
	available, e := disks(true)
	if e != nil {
		return e
	}
	candidates := []disk{}
	for _, d := range available {
		if d.Size >= image.Size && d.SectorSize == expectedSector(m) {
			candidates = append(candidates, d)
		}
	}
	if len(candidates) == 0 {
		return errors.New("no unmounted writable non-USB disk large enough")
	}
	fmt.Printf("RAW image: %s, %d bytes. ALL target partitions and data will be overwritten.\n", image.Path, image.Size)
	for i, d := range candidates {
		fmt.Printf("%d  %s  %s  serial=%s  bytes=%d\n", i+1, d.Path, d.Model, d.Serial, d.Size)
	}
	pick = choice(len(candidates))
	if pick < 0 {
		return nil
	}
	target := candidates[pick]
	confirmation := "ERASE " + target.Path + " " + strings.TrimSpace(target.Serial)
	confirmation = strings.TrimSpace(confirmation)
	if ask("Type exactly ["+confirmation+"]: ") != confirmation {
		return errors.New("confirmation does not match; nothing written")
	}
	// Re-evaluate immediately after confirmation, before opening with exclusive access.
	current, e := disks(true)
	if e != nil {
		return e
	}
	found := false
	for _, d := range current {
		if d.Path == target.Path && d.Serial == target.Serial && d.Model == target.Model && d.Size == target.Size && d.SectorSize == target.SectorSize {
			found = true
		}
	}
	if !found {
		return errors.New("disk state changed; nothing written")
	}
	before, e := os.Lstat(target.Path)
	if e != nil {
		return e
	}
	if before.Mode()&os.ModeDevice == 0 || before.Mode()&os.ModeCharDevice != 0 {
		return errors.New("block device required")
	}
	fd, e := os.OpenFile(target.Path, os.O_RDWR|syscall.O_EXCL|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return e
	}
	defer fd.Close()
	after, e := fd.Stat()
	if e != nil || !os.SameFile(before, after) {
		return errors.New("disk identity changed")
	}
	_ = c.JSON(ctx, "POST", "/v1/rescue/events", strings.NewReader(`{"run_id":"`+id+`","status":"started"}`), new(any))
	e = rescue.RestoreImage(ctx, m, fd, target.Size, func(ctx context.Context, h string) ([]byte, error) { return c.Chunk(ctx, id, h) }, func(done, total int64) {
		fmt.Printf("\rWriting: %.1f%%  %d / %d bytes", float64(done)*100/float64(total), done, total)
	})
	fmt.Println()
	status := "verified"
	if e != nil {
		status = "failed"
	}
	_ = c.JSON(context.Background(), "POST", "/v1/rescue/events", strings.NewReader(`{"run_id":"`+id+`","status":"`+status+`"}`), new(any))
	if e != nil {
		return fmt.Errorf("RESTORE FAILED; target may be partially overwritten: %w", e)
	}
	fmt.Println("Restore and complete disk read-back verified. Boot mode and hardware must match the source. Larger GPT disks may require relocating the backup GPT header; no automatic partition edits were performed.")
	return nil
}
func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("SRK Rescue", model.Version)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var client *rescue.Client
	defer func() { disconnect(client) }()
	for ctx.Err() == nil && !inputEnded {
		fmt.Println("\nSRK Rescue | Кислов Роман Сергеевич | Apache-2.0\n1 Network\n2 Connect using one-time code\n3 Find backups and restore RAW disk\n4 Disconnect\n5 Diagnostics shell\n6 Reboot\n7 Back up a physical disk to media server\n0 Exit")
		switch ask("> ") {
		case "1":
			network()
		case "2":
			code, e := rescue.Parse(askCode())
			if e != nil {
				fmt.Println(e)
				continue
			}
			candidate, e := rescue.NewClient(code)
			if e == nil {
				e = candidate.Pair(ctx)
			}
			if e != nil {
				fmt.Println("Connection failed:", e)
				continue
			}
			if client != nil {
				disconnect(client)
			}
			client = candidate
			fmt.Println("Connected to", code.URL, "until", client.Pairing.Scope.ExpiresAt.Format(time.RFC3339))
		case "3":
			if client == nil {
				fmt.Println("Connect first")
				continue
			}
			if e := restore(ctx, client); e != nil {
				fmt.Println(e)
			}
		case "4":
			if client != nil {
				disconnect(client)
				client = nil
			}
			fmt.Println("Disconnected")
		case "5":
			cmd := exec.Command("/bin/sh")
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			_ = cmd.Run()
		case "7":
			if client == nil {
				fmt.Println("Connect with a backup code first")
				continue
			}
			if e := backupDisk(ctx, client); e != nil {
				fmt.Println("Backup failed:", e)
			}
		case "6":
			if ask("Type REBOOT: ") == "REBOOT" {
				run("reboot", "-f")
			}
		case "0":
			return
		}
	}
}
