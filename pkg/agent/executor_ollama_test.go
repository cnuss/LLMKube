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
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// fakeOllama answers the routes OllamaExecutor calls and records the model
// each pull or load names. hailoList non-nil makes it hailo-ollama, serving
// GET /hailo/v1/list with those tags and reporting size for them in /api/tags.
func fakeOllama(t *testing.T, hailoList []string, size uint64) (port int, models func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	loaded := ""
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	record := func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body.Model)
		loaded = body.Model
		mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	}
	mux.HandleFunc("POST /api/pull", record)
	mux.HandleFunc("POST /api/generate", record)
	mux.HandleFunc("GET /api/ps", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(ollamaPsResponse{Models: []ollamaPsModel{{Name: loaded, Model: loaded}}})
	})
	if hailoList != nil {
		mux.HandleFunc("GET /hailo/v1/list", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(hailoListResponse{Models: hailoList})
		})
		mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, _ *http.Request) {
			var out ollamaTagsResponse
			for _, m := range hailoList {
				out.Models = append(out.Models, struct {
					Name  string `json:"name"`
					Model string `json:"model"`
					Size  uint64 `json:"size"`
				}{Name: m, Model: m, Size: size})
			}
			_ = json.NewEncoder(w).Encode(out)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestOllamaExecutorModelTag(t *testing.T) {
	hailo := []string{"qwen2.5:1.5b", "llama3.2:1b"}
	for name, tc := range map[string]struct {
		cfg  ExecutorConfig
		want string
	}{
		"hailo source":         {ExecutorConfig{ModelName: "qwen25", ModelSource: "hailo://qwen2.5:1.5b"}, "qwen2.5:1.5b"},
		"mapped name":          {ExecutorConfig{ModelName: "llama-3.2-3b", ModelSource: "https://x/m.gguf"}, "llama3.2:3b"},
		"unmapped name as tag": {ExecutorConfig{ModelName: "tinyllama"}, "tinyllama"},
	} {
		t.Run(name, func(t *testing.T) {
			port, models := fakeOllama(t, hailo, 0)
			e := NewOllamaExecutor(port, zap.NewNop().Sugar())
			p, err := e.StartProcess(context.Background(), tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if p.ModelID != tc.want {
				t.Errorf("ModelID = %q, want %q (the tag unload sends)", p.ModelID, tc.want)
			}
			for _, m := range models() {
				if m != tc.want {
					t.Errorf("request named %q, want %q", m, tc.want)
				}
			}
		})
	}
}

func TestOllamaExecutorRefusesHailoSourceItCannotServe(t *testing.T) {
	for name, tc := range map[string]struct {
		hailoList []string
		source    string
		wantErr   string
	}{
		"stock ollama":    {hailoList: nil, source: "hailo://qwen2.5:1.5b", wantErr: "not hailo-ollama"},
		"catch-all 200":   {hailoList: []string{}, source: "hailo://qwen2.5:1.5b", wantErr: "does not list"},
		"tag hailo lacks": {hailoList: []string{"qwen3:1.7b"}, source: "hailo://qwen2.5:1.5b", wantErr: "does not list"},
		"registry in the tag": {
			hailoList: []string{"x"}, source: "hailo://evil.example.com/x:1", wantErr: "not a Hailo model tag",
		},
	} {
		t.Run(name, func(t *testing.T) {
			port, models := fakeOllama(t, tc.hailoList, 0)
			e := NewOllamaExecutor(port, zap.NewNop().Sugar())
			_, err := e.StartProcess(context.Background(), ExecutorConfig{ModelName: "m", ModelSource: tc.source})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if got := models(); len(got) != 0 {
				t.Errorf("daemon saw %v; want no pull or load for a refused source", got)
			}
		})
	}
}

func TestHailoModelSize(t *testing.T) {
	port, models := fakeOllama(t, []string{"qwen2.5:1.5b"}, 2495056733)
	e := NewOllamaExecutor(port, zap.NewNop().Sugar())
	size, err := e.HailoModelSize(context.Background(), "qwen2.5:1.5b")
	if err != nil || size != 2495056733 {
		t.Fatalf("HailoModelSize = %d, %v; want the HEF size the daemon reports", size, err)
	}
	if got := models(); len(got) != 1 || got[0] != "qwen2.5:1.5b" {
		t.Errorf("daemon saw %v; want one pull of the tag before it is sized", got)
	}

	stock, _ := fakeOllama(t, nil, 0)
	stockExec := NewOllamaExecutor(stock, zap.NewNop().Sugar())
	if _, err := stockExec.HailoModelSize(context.Background(), "qwen2.5:1.5b"); err == nil {
		t.Error("sizing a hailo tag on stock Ollama: want an error, not the registry model's size")
	}
}
