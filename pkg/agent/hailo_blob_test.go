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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeHailoLibrary serves HEAD for each path in sizes with that
// Content-Length, and 404 for anything else.
func fakeHailoLibrary(t *testing.T, sizes map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		size, ok := sizes[r.URL.Path]
		if r.Method != http.MethodHead || !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", size)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// writeHailoManifests lays out hailo-ollama's manifests/<name>/<tag>/manifest.json.
func writeHailoManifests(t *testing.T, hefs map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for tag, hef := range hefs {
		name, version, _ := strings.Cut(tag, ":")
		dir := filepath.Join(root, name, version)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"hef_filename": "` + hef + `", "details": {"format": "hef"}}`
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestHailoManifestDirs(t *testing.T) {
	got := hailoManifestDirs("/home/u", "", "")
	want := []string{
		"/home/u/.local/share/hailo-ollama/models/manifests",
		"/usr/share/hailo-ollama/models/manifests",
		"/usr/local/share/hailo-ollama/models/manifests",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("defaults = %v, want %v", got, want)
	}
	got = hailoManifestDirs("/home/u", "/xdg", "/a::/b")
	want = []string{
		"/xdg/hailo-ollama/models/manifests",
		"/a/hailo-ollama/models/manifests",
		"/b/hailo-ollama/models/manifests",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("with XDG = %v, want %v", got, want)
	}
}

func TestHailoHEFFilename(t *testing.T) {
	first := writeHailoManifests(t, map[string]string{"qwen2.5:1.5b": "Qwen2.5-1.5B-Instruct.hef"})
	second := writeHailoManifests(t, map[string]string{"llama3.2:1b": "Llama-3.2-1B-Instruct.hef"})
	dirs := []string{filepath.Join(t.TempDir(), "missing"), first, second}

	if got, err := hailoHEFFilename(dirs, "qwen2.5:1.5b"); err != nil || got != "Qwen2.5-1.5B-Instruct.hef" {
		t.Errorf("qwen2.5:1.5b = %q, %v", got, err)
	}
	if _, err := hailoHEFFilename(dirs, "llama3.2:1b"); err == nil {
		t.Error("a tag only in a later directory: want an error, since hailo-ollama reads only the first that exists")
	}
	if _, err := hailoHEFFilename(dirs, "qwen3"); err == nil {
		t.Error("a tag without :tag: want an error")
	}
	if _, err := hailoHEFFilename([]string{filepath.Join(t.TempDir(), "none")}, "qwen2.5:1.5b"); err == nil {
		t.Error("no manifest directory: want an error")
	}
	bad := writeHailoManifests(t, map[string]string{"x:1": "../../etc/passwd"})
	if _, err := hailoHEFFilename([]string{bad}, "x:1"); err == nil {
		t.Error("an hef_filename with a path: want it refused")
	}
}

func TestHailoBlobVersions(t *testing.T) {
	for in, want := range map[string][]string{
		"5.5.0": {"5.5.0", "5.4.0"},
		"5.5.2": {"5.5.2", "5.5.0"},
		"5.0.0": {"5.0.0"},
	} {
		if got, err := hailoBlobVersions(in); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("hailoBlobVersions(%s) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "5.5", "v5.5.0", "5.x.0"} {
		if _, err := hailoBlobVersions(bad); err == nil {
			t.Errorf("hailoBlobVersions(%q): want an error", bad)
		}
	}
}

func TestHailoBlobSizeNeedsTheCompanion(t *testing.T) {
	lib := fakeHailoLibrary(t, map[string]string{
		"/v5.5.0/blob/M.hef":    "100",
		"/v5.4.0/blob/M.hef":    "90",
		"/v5.4.0/blob/M.sha256": "65",
	})
	client := &http.Client{}
	size, err := hailoBlobSize(context.Background(), client, lib, []string{"5.5.0", "5.4.0"}, "M.hef")
	if err != nil || size != 90 {
		t.Errorf("size = %d, %v; want v5.4.0's, the first version with both the HEF and its .sha256", size, err)
	}
	if _, err := hailoBlobSize(context.Background(), client, lib, []string{"5.5.0"}, "M.hef"); err == nil {
		t.Error("only a HEF without its companion: want an error")
	}
}
