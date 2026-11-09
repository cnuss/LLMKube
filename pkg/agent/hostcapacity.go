/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	"math"
	"strconv"
	"strings"

	"github.com/defilantech/llmkube/pkg/metalnode"
)

// maxChipBytes matches the bound the MetalNode controller enforces on
// free-text report fields.
const maxChipBytes = 128

// HostCapacity is the hardware the agent reports to its MetalNode.
type HostCapacity struct {
	MemoryBytes  int64
	Chip         string
	GPUCores     int32
	MetalVersion string
}

// hostCapacityFromSysctl reads unified memory (hw.memsize) and the chip name
// (machdep.cpu.brand_string). A value that cannot be read or parsed is left
// empty rather than guessed.
func hostCapacityFromSysctl(sysctl func(name string) (string, error)) HostCapacity {
	var c HostCapacity
	if out, err := sysctl("hw.memsize"); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil && n > 0 {
			c.MemoryBytes = n
		}
	}
	if out, err := sysctl("machdep.cpu.brand_string"); err == nil {
		c.Chip = printablePrefix(strings.TrimSpace(out), maxChipBytes)
	}
	return c
}

// printablePrefix drops every character the controller would reject
// (metalnode.Printable: controls, bidi and other format characters) and
// truncates to limit bytes on a rune boundary.
func printablePrefix(s string, limit int) string {
	var b strings.Builder
	for _, r := range s {
		if !metalnode.Printable(r) {
			continue
		}
		if b.Len()+len(string(r)) > limit {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// MetalVersionString renders platform.Capabilities.MetalVersion ("Metal 4");
// zero (unknown) is empty.
func MetalVersionString(v int) string {
	if v <= 0 {
		return ""
	}
	return "Metal " + strconv.Itoa(v)
}

// hostCapacityFromProc reads memory (MemTotal) and a chip name: the
// device-tree model when the board has one (Raspberry Pi and other ARM
// boards), else the first cpuinfo "model name" or "Model" line.
func hostCapacityFromProc(readFile func(path string) ([]byte, error)) HostCapacity {
	var c HostCapacity
	if data, err := readFile("/proc/meminfo"); err == nil {
		if n, ok := parseProcKB(string(data))["MemTotal"]; ok && n > 0 && n <= math.MaxInt64 {
			c.MemoryBytes = int64(n)
		}
	}
	if data, err := readFile("/proc/device-tree/model"); err == nil {
		c.Chip = printablePrefix(strings.TrimSpace(strings.TrimRight(string(data), "\x00")), maxChipBytes)
	}
	if c.Chip != "" {
		return c
	}
	if data, err := readFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			key, val, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			if k := strings.TrimSpace(key); k == "model name" || k == "Model" {
				c.Chip = printablePrefix(strings.TrimSpace(val), maxChipBytes)
				break
			}
		}
	}
	return c
}
