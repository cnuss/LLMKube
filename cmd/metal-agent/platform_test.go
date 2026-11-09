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

package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/defilantech/llmkube/internal/platform"
)

func TestCheckHostPlatform(t *testing.T) {
	origCaps, origGOOS, origGlob := detectCapabilities, hostGOOS, globDevices
	t.Cleanup(func() { detectCapabilities, hostGOOS, globDevices = origCaps, origGOOS, origGlob })

	mac := platform.Capabilities{Metal: true, GPUCores: 40, OS: "darwin", Arch: "arm64"}
	pi := platform.Capabilities{OS: "linux", Arch: "arm64"}
	hat := func(string) ([]string, error) { return []string{"/dev/hailo0"}, nil }
	noHat := func(string) ([]string, error) { return nil, nil }

	for _, tc := range []struct {
		name, goos string
		caps       platform.Capabilities
		glob       func(string) ([]string, error)
		wantErr    bool
	}{
		{name: "mac with Metal", goos: "darwin", caps: mac, glob: noHat},
		{name: "linux with a Hailo device", goos: "linux", caps: pi, glob: hat},
		{name: "linux without a device still starts", goos: "linux", caps: pi, glob: noHat},
		{name: "mac without Metal", goos: "darwin", caps: platform.Capabilities{OS: "darwin"}, glob: noHat, wantErr: true},
		{name: "other OS", goos: "freebsd", caps: platform.Capabilities{OS: "freebsd"}, glob: hat, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostGOOS = tc.goos
			detectCapabilities = func() platform.Capabilities { return tc.caps }
			globDevices = tc.glob
			caps, err := checkHostPlatform(zap.NewNop().Sugar())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if err == nil && caps != tc.caps {
				t.Errorf("caps = %+v, want the detected %+v passed through", caps, tc.caps)
			}
		})
	}
}

func TestResolveDefaultLlamaServerBin(t *testing.T) {
	origStat := statFunc
	t.Cleanup(func() { statFunc = origStat })
	statFunc = func(string) (os.FileInfo, error) { return nil, errors.New("missing") }

	cfg := &AgentConfig{Runtime: "ollama"}
	if err := resolveDefaultLlamaServerBin(cfg, "", zap.NewNop().Sugar()); err != nil || cfg.LlamaServerBin != "" {
		t.Errorf("runtime ollama, no llama-server: bin %q, err %v; want empty and no error", cfg.LlamaServerBin, err)
	}
	cfg = &AgentConfig{Runtime: "llama-server"}
	if err := resolveDefaultLlamaServerBin(cfg, "", zap.NewNop().Sugar()); err == nil {
		t.Error("runtime llama-server, no llama-server: want an error")
	}
	cfg = &AgentConfig{Runtime: "ollama"}
	if err := resolveDefaultLlamaServerBin(cfg, "/opt/llama-server", zap.NewNop().Sugar()); err != nil ||
		cfg.LlamaServerBin != "/opt/llama-server" {
		t.Errorf("override: bin %q, err %v; want the override", cfg.LlamaServerBin, err)
	}
}

func TestLlamaServerInstallHint(t *testing.T) {
	if got := llamaServerInstallHint("darwin"); got != "brew install llama.cpp" {
		t.Errorf("darwin hint = %q", got)
	}
	if got := llamaServerInstallHint("linux"); strings.Contains(got, "brew") {
		t.Errorf("linux hint = %q, want no Homebrew", got)
	}
}
