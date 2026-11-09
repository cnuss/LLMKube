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

// LinuxMemoryProvider implements MemoryProvider from /proc, capped by the
// cgroup v2 memory limit of the agent's own cgroup and its ancestors. Engines
// the agent starts inherit that cgroup; a separately managed daemon (Ollama)
// does not.
type LinuxMemoryProvider struct {
	// ProcRoot is the procfs mount; empty means /proc.
	ProcRoot string
	// CgroupRoot is the cgroup v2 mount; empty means /sys/fs/cgroup.
	CgroupRoot string
}

// cgroupLimit is the tightest memory.max on the path from the agent's cgroup
// to the root, and the room left under it (limit minus usage, with inactive
// file cache counted as reclaimable). ok is false when no limit is set or
// cgroup v2 is not mounted.
func (p *LinuxMemoryProvider) cgroupLimit() (limit, room uint64, ok bool) {
	root := p.CgroupRoot
	if root == "" {
		root = "/sys/fs/cgroup"
	}
	rel := "/"
	if data, err := os.ReadFile(p.procPath("self", "cgroup")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if path, found := strings.CutPrefix(line, "0::"); found {
				rel = path
				break
			}
		}
	}
	for dir := filepath.Join(root, rel); ; dir = filepath.Dir(dir) {
		if l, r, set := readCgroupMemory(dir); set && (!ok || r < room) {
			limit, room, ok = l, r, true
		}
		if dir == root || !strings.HasPrefix(dir, root) {
			return limit, room, ok
		}
	}
}

// readCgroupMemory reads one cgroup's memory.max, memory.current and
// memory.stat inactive_file. set is false for "max" or a missing file.
func readCgroupMemory(dir string) (limit, room uint64, set bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "memory.max"))
	if err != nil {
		return 0, 0, false
	}
	limit, err = strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	var used uint64
	if raw, err := os.ReadFile(filepath.Join(dir, "memory.current")); err == nil {
		used, _ = strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "memory.stat")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if v, found := strings.CutPrefix(line, "inactive_file "); found {
				if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil && n <= used {
					used -= n
				}
				break
			}
		}
	}
	if used >= limit {
		return limit, 0, true
	}
	return limit, limit - used, true
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

// TotalMemory is MemTotal, or the cgroup limit when that is lower.
func (p *LinuxMemoryProvider) TotalMemory() (uint64, error) {
	total, err := p.meminfoField("MemTotal")
	if err != nil {
		return 0, err
	}
	if limit, _, ok := p.cgroupLimit(); ok && limit < total {
		return limit, nil
	}
	return total, nil
}

// AvailableMemory is MemAvailable, or the room under the cgroup limit when
// that is lower. A missing MemAvailable is an error, not zero: the watchdog
// reads zero as Critical pressure.
func (p *LinuxMemoryProvider) AvailableMemory() (uint64, error) {
	avail, err := p.meminfoField("MemAvailable")
	if err != nil {
		return 0, err
	}
	if _, room, ok := p.cgroupLimit(); ok && room < avail {
		return room, nil
	}
	return avail, nil
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
