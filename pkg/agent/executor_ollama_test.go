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
	"sync"
	"testing"

	"go.uber.org/zap"
)

// fakeOllama answers the routes OllamaExecutor.StartProcess calls and records
// the model each request names.
func fakeOllama(t *testing.T) (port int, models func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	loaded := ""
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
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
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestOllamaExecutorRefusesNonLibraryTag(t *testing.T) {
	port, models := fakeOllama(t)
	e := NewOllamaExecutor(port, zap.NewNop().Sugar())
	for _, tag := range []string{
		"attacker.example.com/x/backdoor:latest",
		"someone/model",
		"qwen2.5:1.5b@sha256:abc",
		"Qwen:1.5b",
		":1.5b",
	} {
		if _, err := e.StartProcess(context.Background(), ExecutorConfig{ModelName: "m", OllamaModel: tag}); err == nil {
			t.Errorf("tag %q started; want it refused before any request to the daemon", tag)
		}
	}
	if got := models(); len(got) != 0 {
		t.Errorf("daemon saw %v; want no pull or load for a refused tag", got)
	}
}

func TestOllamaExecutorModelTag(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  ExecutorConfig
		want string
	}{
		"annotation wins":       {ExecutorConfig{ModelName: "qwen25", OllamaModel: "qwen2.5:1.5b"}, "qwen2.5:1.5b"},
		"mapped name":           {ExecutorConfig{ModelName: "llama-3.2-3b"}, "llama3.2:3b"},
		"unmapped name as tag":  {ExecutorConfig{ModelName: "tinyllama"}, "tinyllama"},
		"empty annotation maps": {ExecutorConfig{ModelName: "llama-3.2-3b", OllamaModel: ""}, "llama3.2:3b"},
	} {
		t.Run(name, func(t *testing.T) {
			port, models := fakeOllama(t)
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
