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
		name, goos, flag string
		caps             platform.Capabilities
		glob             func(string) ([]string, error)
		want, wantErr    string
	}{
		{name: "mac default", goos: "darwin", caps: mac, glob: noHat, want: "metal"},
		{name: "linux default", goos: "linux", caps: pi, glob: hat, want: "npu"},
		{name: "linux npu without a device still starts", goos: "linux", flag: "npu", caps: pi, glob: noHat, want: "npu"},
		{name: "metal without Metal", goos: "linux", flag: "metal", caps: pi, glob: noHat,
			wantErr: "metal support not detected"},
		{name: "npu off Linux", goos: "darwin", flag: "npu", caps: mac, glob: hat, wantErr: "only supported on Linux"},
		{name: "unknown accelerator", goos: "linux", flag: "cuda", caps: pi, glob: hat, wantErr: "invalid --accelerator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostGOOS = tc.goos
			detectCapabilities = func() platform.Capabilities { return tc.caps }
			globDevices = tc.glob
			got, caps, err := checkHostPlatform(zap.NewNop().Sugar(), tc.flag)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("checkHostPlatform = %q, %v; want %q", got, err, tc.want)
			}
			if caps != tc.caps {
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
