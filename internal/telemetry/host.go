// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package telemetry

import (
	"context"
	"math"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
)

type Memory struct {
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Available uint64  `json:"available"`
	Percent   float64 `json:"percent"`
}
type Volume struct {
	Path    string  `json:"path"`
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Free    uint64  `json:"free"`
	Percent float64 `json:"percent"`
}
type Disk struct {
	Name        string   `json:"name"`
	ReadBPS     *float64 `json:"read_bps"`
	WriteBPS    *float64 `json:"write_bps"`
	BusyPercent *float64 `json:"busy_percent"`
}
type Network struct {
	Name       string   `json:"name"`
	ReceiveBPS *float64 `json:"receive_bps"`
	SendBPS    *float64 `json:"send_bps"`
	ErrorsIn   uint64   `json:"errors_in"`
	ErrorsOut  uint64   `json:"errors_out"`
}
type Host struct {
	At              time.Time `json:"at"`
	Hostname        string    `json:"hostname"`
	OS              string    `json:"os"`
	CPUPercent      *float64  `json:"cpu_percent"`
	Memory          *Memory   `json:"memory"`
	Volumes         []Volume  `json:"volumes"`
	Disks           []Disk    `json:"disks"`
	Network         []Network `json:"network"`
	IntervalSeconds float64   `json:"interval_seconds"`
	Unavailable     []string  `json:"unavailable"`
}
type collector struct {
	mu           sync.Mutex
	cached       Host
	previousAt   time.Time
	previousCPU  *cpu.TimesStat
	previousDisk map[string]disk.IOCountersStat
	previousNet  map[string]gnet.IOCountersStat
}

var system collector

// Snapshot caches a host sample for two seconds; first interval rates remain unknown.
func Snapshot() Host { return system.snapshot() }
func rate(previous, current uint64, seconds float64) *float64 {
	if seconds <= 0 || current < previous {
		return nil
	}
	v := float64(current-previous) / seconds
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}
func totalCPU(c cpu.TimesStat) float64 {
	return c.User + c.System + c.Nice + c.Idle + c.Iowait + c.Irq + c.Softirq + c.Steal
}
func cpuPercent(before, after cpu.TimesStat) *float64 {
	total := totalCPU(after) - totalCPU(before)
	idle := after.Idle + after.Iowait - before.Idle - before.Iowait
	if total <= 0 || idle < 0 {
		return nil
	}
	v := math.Max(0, math.Min(100, 100*(total-idle)/total))
	return &v
}
func (c *collector) snapshot() Host {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UTC()
	if !c.cached.At.IsZero() && now.Sub(c.cached.At) < 2*time.Second {
		return c.cached
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	h := Host{At: now, OS: runtime.GOOS, Volumes: []Volume{}, Disks: []Disk{}, Network: []Network{}, Unavailable: []string{}}
	h.Hostname, _ = os.Hostname()
	elapsed := 0.0
	if !c.previousAt.IsZero() {
		elapsed = now.Sub(c.previousAt).Seconds()
	}
	h.IntervalSeconds = elapsed
	if v, e := cpu.TimesWithContext(ctx, false); e == nil && len(v) > 0 {
		if c.previousCPU != nil {
			h.CPUPercent = cpuPercent(*c.previousCPU, v[0])
		}
		copy := v[0]
		c.previousCPU = &copy
	} else {
		h.Unavailable = append(h.Unavailable, "cpu")
	}
	if v, e := mem.VirtualMemoryWithContext(ctx); e == nil {
		h.Memory = &Memory{Total: v.Total, Used: v.Used, Available: v.Available, Percent: v.UsedPercent}
	} else {
		h.Unavailable = append(h.Unavailable, "memory")
	}
	paths := map[string]bool{"/": true}
	if volumes, e := disk.PartitionsWithContext(ctx, false); e == nil {
		for _, v := range volumes {
			if len(paths) < 32 {
				paths[v.Mountpoint] = true
			}
		}
	} else {
		h.Unavailable = append(h.Unavailable, "partitions")
	}
	for p := range paths {
		if v, e := disk.UsageWithContext(ctx, p); e == nil && v.Total > 0 {
			h.Volumes = append(h.Volumes, Volume{Path: p, Total: v.Total, Used: v.Used, Free: v.Free, Percent: v.UsedPercent})
		}
	}
	sort.Slice(h.Volumes, func(i, j int) bool { return h.Volumes[i].Path < h.Volumes[j].Path })
	if values, e := disk.IOCountersWithContext(ctx); e == nil {
		for name, v := range values {
			d := Disk{Name: name}
			if old, ok := c.previousDisk[name]; ok {
				d.ReadBPS = rate(old.ReadBytes, v.ReadBytes, elapsed)
				d.WriteBPS = rate(old.WriteBytes, v.WriteBytes, elapsed)
				if r := rate(old.IoTime, v.IoTime, elapsed*10); r != nil {
					v := math.Min(100, *r)
					d.BusyPercent = &v
				}
			}
			h.Disks = append(h.Disks, d)
		}
		c.previousDisk = values
	} else {
		h.Unavailable = append(h.Unavailable, "disk_io")
		c.previousDisk = nil
	}
	sort.Slice(h.Disks, func(i, j int) bool { return h.Disks[i].Name < h.Disks[j].Name })
	if values, e := gnet.IOCountersWithContext(ctx, true); e == nil {
		next := map[string]gnet.IOCountersStat{}
		for _, v := range values {
			n := Network{Name: v.Name, ErrorsIn: v.Errin, ErrorsOut: v.Errout}
			if old, ok := c.previousNet[v.Name]; ok {
				n.ReceiveBPS = rate(old.BytesRecv, v.BytesRecv, elapsed)
				n.SendBPS = rate(old.BytesSent, v.BytesSent, elapsed)
			}
			h.Network = append(h.Network, n)
			next[v.Name] = v
		}
		c.previousNet = next
	} else {
		h.Unavailable = append(h.Unavailable, "network")
		c.previousNet = nil
	}
	sort.Slice(h.Network, func(i, j int) bool { return h.Network[i].Name < h.Network[j].Name })
	c.previousAt = now
	c.cached = h
	return h
}
