// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package telemetry

import (
	"encoding/json"
	"github.com/shirou/gopsutil/v4/cpu"
	"math"
	"testing"
)

func TestCounterRatesAndResets(t *testing.T) {
	if v := rate(100, 400, 2); v == nil || *v != 150 {
		t.Fatal("invalid bytes per second")
	}
	if rate(400, 100, 2) != nil || rate(0, 100, 0) != nil {
		t.Fatal("reset or missing interval represented as a valid rate")
	}
	v := cpuPercent(cpu.TimesStat{User: 10, Idle: 90}, cpu.TimesStat{User: 40, Idle: 160})
	if v == nil || math.Abs(*v-30) > 0.0001 {
		t.Fatal("incorrect CPU interval usage", v)
	}
	if cpuPercent(cpu.TimesStat{User: 40, Idle: 160}, cpu.TimesStat{User: 10, Idle: 90}) != nil {
		t.Fatal("CPU counter reset was not detected")
	}
}
func TestHostSampleCacheAndUnknownFirstInterval(t *testing.T) {
	var c collector
	first := c.snapshot()
	second := c.snapshot()
	if first.At.IsZero() || first.OS == "" || !first.At.Equal(second.At) {
		t.Fatal("host samples not cached or lack identity")
	}
	if first.CPUPercent != nil {
		t.Fatal("first sample claims an interval measurement")
	}
	for _, n := range first.Network {
		if n.ReceiveBPS != nil || n.SendBPS != nil {
			t.Fatal("first sample claims a network rate")
		}
	}
	if _, e := json.Marshal(first); e != nil {
		t.Fatal("metrics are not valid JSON", e)
	}
}
