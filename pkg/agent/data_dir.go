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
	"os"
	"path/filepath"
	"runtime"
)

// DefaultDataDir is the parent of the agent's default model store and state
// directory: ~/Library/Application Support/llmkube on macOS, and
// $XDG_DATA_HOME/llmkube (~/.local/share/llmkube) elsewhere.
func DefaultDataDir(home string) string {
	return defaultDataDir(runtime.GOOS, home, os.Getenv("XDG_DATA_HOME"))
}

func defaultDataDir(goos, home, xdgDataHome string) string {
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "llmkube")
	}
	if filepath.IsAbs(xdgDataHome) {
		return filepath.Join(xdgDataHome, "llmkube")
	}
	return filepath.Join(home, ".local", "share", "llmkube")
}
