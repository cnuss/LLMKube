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
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// NewHostMemoryProvider returns the MemoryProvider for the OS the agent runs
// on: /proc on Linux, sysctl and vm_stat otherwise.
func NewHostMemoryProvider() MemoryProvider {
	if runtime.GOOS == "linux" {
		return &LinuxMemoryProvider{}
	}
	return &DarwinMemoryProvider{}
}

// LinuxMemoryProvider implements MemoryProvider from /proc.
type LinuxMemoryProvider struct {
	// ProcRoot is the procfs mount; empty means /proc.
	ProcRoot string
}

func (p *LinuxMemoryProvider) procPath(elem ...string) string {
	root := p.ProcRoot
	if root == "" {
		root = "/proc"
	}
	return filepath.Join(append([]string{root}, elem...)...)
}

func (p *LinuxMemoryProvider) meminfoField(key string) (uint64, error) {
	data, err := os.ReadFile(p.procPath("meminfo"))
	if err != nil {
		return 0, err
	}
	fields := parseProcKB(string(data))
	v, ok := fields[key]
	if !ok {
		return 0, fmt.Errorf("meminfo: no %s field", key)
	}
	return v, nil
}

// TotalMemory is MemTotal.
func (p *LinuxMemoryProvider) TotalMemory() (uint64, error) {
	return p.meminfoField("MemTotal")
}

// AvailableMemory is MemAvailable. A missing field is an error, not zero:
// the watchdog reads zero as Critical pressure.
func (p *LinuxMemoryProvider) AvailableMemory() (uint64, error) {
	return p.meminfoField("MemAvailable")
}

// WiredMemory is Mlocked, the closest Linux analogue of macOS wired pages.
func (p *LinuxMemoryProvider) WiredMemory() (uint64, error) {
	return p.meminfoField("Mlocked")
}

// ProcessRSS is VmRSS from /proc/<pid>/status.
func (p *LinuxMemoryProvider) ProcessRSS(pid int) (uint64, error) {
	data, err := os.ReadFile(p.procPath(strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, fmt.Errorf("rss for pid %d: %w", pid, err)
	}
	v, ok := parseProcKB(string(data))["VmRSS"]
	if !ok {
		return 0, fmt.Errorf("rss for pid %d: no VmRSS field", pid)
	}
	return v, nil
}

// parseProcKB parses "Key:   123 kB" lines (/proc/meminfo, /proc/<pid>/status)
// into bytes. Lines without a numeric kB value are skipped.
func parseProcKB(data string) map[string]uint64 {
	out := make(map[string]uint64)
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		out[strings.TrimSpace(key)] = n * 1024
	}
	return out
}
