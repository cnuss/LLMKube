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
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A Raspberry Pi 5 8GB, trimmed.
const piMeminfo = `MemTotal:        8245328 kB
MemFree:         5120000 kB
MemAvailable:    7012340 kB
Buffers:           61234 kB
Mlocked:           16384 kB
HugePages_Total:       0
Hugepagesize:       2048 kB
`

func writeProcFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLinuxMemoryProvider(t *testing.T) {
	root := writeProcFixture(t, map[string]string{
		"meminfo":   piMeminfo,
		"42/status": "Name:\tllama-server\nVmRSS:\t  204800 kB\nThreads:\t4\n",
		"43/status": "Name:\tkthreadd\nThreads:\t1\n",
	})
	p := &LinuxMemoryProvider{ProcRoot: root}

	for name, tc := range map[string]struct {
		fn   func() (uint64, error)
		want uint64
	}{
		"total":     {p.TotalMemory, 8245328 * 1024},
		"available": {p.AvailableMemory, 7012340 * 1024},
		"wired":     {p.WiredMemory, 16384 * 1024},
		"rss":       {func() (uint64, error) { return p.ProcessRSS(42) }, 204800 * 1024},
	} {
		got, err := tc.fn()
		if err != nil || got != tc.want {
			t.Errorf("%s = %d, %v; want %d", name, got, err, tc.want)
		}
	}

	if _, err := p.ProcessRSS(43); err == nil {
		t.Error("rss of a process without VmRSS: want an error, not zero")
	}
	if _, err := p.ProcessRSS(99); err == nil {
		t.Error("rss of a missing process: want an error")
	}
}

func TestLinuxMemoryProviderMissingAvailable(t *testing.T) {
	root := writeProcFixture(t, map[string]string{"meminfo": "MemTotal: 1024 kB\n"})
	if got, err := (&LinuxMemoryProvider{ProcRoot: root}).AvailableMemory(); err == nil {
		t.Errorf("available = %d with no MemAvailable; want an error (zero reads as Critical)", got)
	}
}

func TestHostCapacityFromProc(t *testing.T) {
	files := map[string]string{
		"/proc/meminfo":           piMeminfo,
		"/proc/device-tree/model": "Raspberry Pi 5 Model B Rev 1.0\x00",
		"/proc/cpuinfo":           "processor\t: 0\nmodel name\t: Cortex-A76\n",
	}
	read := func(p string) ([]byte, error) {
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("no such file")
	}

	got := hostCapacityFromProc(read)
	if got.MemoryBytes != 8245328*1024 || got.Chip != "Raspberry Pi 5 Model B Rev 1.0" {
		t.Errorf("capacity = %+v, want MemTotal and the device-tree model without its NUL", got)
	}

	delete(files, "/proc/device-tree/model")
	if got := hostCapacityFromProc(read).Chip; got != "Cortex-A76" {
		t.Errorf("chip without device tree = %q, want the cpuinfo model name", got)
	}

	noProc := func(string) ([]byte, error) { return nil, errors.New("no proc") }
	if got := hostCapacityFromProc(noProc); got != (HostCapacity{}) {
		t.Errorf("capacity without /proc = %+v, want empty rather than a guess", got)
	}
}
