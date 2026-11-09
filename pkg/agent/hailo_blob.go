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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultHailoLibraryURL is the host hailo-ollama pulls HEFs from.
const DefaultHailoLibraryURL = "https://dev-public.hailo.ai"

// hailoManifestDirs lists the directories hailo-ollama reads its model
// manifests from, in its order: the data home, then each data dir.
// hailo-ollama uses the first one that exists.
func hailoManifestDirs(home, xdgDataHome, xdgDataDirs string) []string {
	const sub = "hailo-ollama/models/manifests"
	dataHome := xdgDataHome
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	dirs := []string{filepath.Join(dataHome, sub)}
	if xdgDataDirs == "" {
		xdgDataDirs = "/usr/share:/usr/local/share"
	}
	for _, d := range strings.Split(xdgDataDirs, ":") {
		if d != "" {
			dirs = append(dirs, filepath.Join(d, sub))
		}
	}
	return dirs
}

// hailoHEFFilename reads the HEF filename hailo-ollama maps tag
// ("name:tag") to, from <dir>/<name>/<tag>/manifest.json in the first of dirs
// that exists.
func hailoHEFFilename(dirs []string, tag string) (string, error) {
	name, version, ok := strings.Cut(tag, ":")
	if !ok {
		return "", fmt.Errorf("hailo model %q has no :tag; hailo-ollama names every model name:tag", tag)
	}
	for _, dir := range dirs {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name, version, "manifest.json"))
		if err != nil {
			return "", fmt.Errorf("hailo-ollama's manifests in %s have no %s: %w", dir, tag, err)
		}
		var m struct {
			HEFFilename string `json:"hef_filename"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", fmt.Errorf("manifest for %s in %s: %w", tag, dir, err)
		}
		if m.HEFFilename == "" || strings.ContainsAny(m.HEFFilename, "/\\") {
			return "", fmt.Errorf("manifest for %s in %s has no usable hef_filename %q", tag, dir, m.HEFFilename)
		}
		return m.HEFFilename, nil
	}
	return "", fmt.Errorf("no hailo-ollama manifest directory found (looked in %s); set --hailo-manifests-dir",
		strings.Join(dirs, ", "))
}

// hailoBlobVersions is the blob versions hailo-ollama tries for its own
// version: that version, then x.y.0 when the patch is not 0, else x.(y-1).0.
// There is no fallback across a major version.
func hailoBlobVersions(version string) ([]string, error) {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("hailo-ollama version %q is not major.minor.patch", version)
	}
	n := make([]int, 3)
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 {
			return nil, fmt.Errorf("hailo-ollama version %q is not major.minor.patch", version)
		}
		n[i] = v
	}
	switch {
	case n[2] != 0:
		return []string{version, fmt.Sprintf("%d.%d.0", n[0], n[1])}, nil
	case n[1] != 0:
		return []string{version, fmt.Sprintf("%d.%d.0", n[0], n[1]-1)}, nil
	default:
		return []string{version}, nil
	}
}

// hailoBlobSize HEADs the HEF hailo-ollama would pull, at the first of
// versions that has both the HEF and its .sha256 companion, and returns the
// HEF's Content-Length. A version missing either is skipped, as hailo-ollama
// skips it; any other failure is returned.
func hailoBlobSize(ctx context.Context, client *http.Client, baseURL string, versions []string,
	hefFilename string) (uint64, error) {
	companion := strings.TrimSuffix(hefFilename, ".hef") + ".sha256"
	base := strings.TrimRight(baseURL, "/")
	for _, v := range versions {
		blob := base + "/v" + v + "/blob/"
		size, err := remoteModelSize(ctx, client, blob+hefFilename)
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("size %s at v%s: %w", hefFilename, v, err)
		}
		if _, err := remoteModelSize(ctx, client, blob+companion); isNotFound(err) {
			continue
		} else if err != nil {
			return 0, fmt.Errorf("check %s at v%s: %w", companion, v, err)
		}
		return size, nil
	}
	return 0, fmt.Errorf("no HEF and .sha256 pair for %s at %s v%s", hefFilename, base, strings.Join(versions, " or v"))
}

func isNotFound(err error) bool {
	var status *sizeProbeStatusError
	return errors.As(err, &status) && status.StatusCode == http.StatusNotFound
}
