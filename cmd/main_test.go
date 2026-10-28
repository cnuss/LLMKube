/*
Copyright 2025.

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
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/prometheus/client_golang/prometheus"

	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
)

// TestClientsetPodLogReader_ReadPodLogs covers the production PodLogReader
// implementation (cmd's clientsetPodLogReader) that backs
// status.acceleration (#1385). It is the only code path that turns a
// kubernetes clientset into the log text the controller parses, so a test that
// deletes it must fail. The fake clientset's Pods.GetLogs returns a canned
// "fake logs" body for a matched action and honors a reactor's error, so both
// the happy and error paths are exercised without a live cluster.
func TestClientsetPodLogReader_ReadPodLogs(t *testing.T) {
	ctx := context.Background()
	ns, pod, container := "default", "svc-1", "llama-server"

	t.Run("returns the log body", func(t *testing.T) {
		cs := fakeclientset.NewSimpleClientset()
		r := &clientsetPodLogReader{cs: cs}

		got, err := r.ReadPodLogs(ctx, ns, pod, container, 100)
		if err != nil {
			t.Fatalf("ReadPodLogs returned error: %v", err)
		}
		if got != "fake logs\n" {
			t.Errorf("ReadPodLogs = %q, want %q", got, "fake logs\n")
		}
	})

	t.Run("propagates the clientset error", func(t *testing.T) {
		cs := fakeclientset.NewSimpleClientset()
		boom := errors.New("log stream unavailable")
		cs.PrependReactor("get", "pods", func(action clientgotesting.Action) (bool, runtime.Object, error) {
			return true, nil, boom
		})
		r := &clientsetPodLogReader{cs: cs}

		got, err := r.ReadPodLogs(ctx, ns, pod, container, 100)
		if !errors.Is(err, boom) {
			t.Errorf("ReadPodLogs error = %v, want %v", err, boom)
		}
		if got != "" {
			t.Errorf("ReadPodLogs = %q, want empty string on error", got)
		}
	})

	t.Run("reads the head, never the tail", func(t *testing.T) {
		// #1585: the engine's load-time offload result is the first thing in the
		// log, so a pod that has served traffic loses it through a TailLines
		// read. Asserting the absence of TailLines is what keeps this from
		// regressing back; TestReadPodLogsTruncatesToTheHead covers the size.
		cs := fakeclientset.NewSimpleClientset()
		r := &clientsetPodLogReader{cs: cs}

		_, _ = r.ReadPodLogs(ctx, ns, pod, container, 2048)

		var opts *corev1.PodLogOptions
		for _, a := range cs.Actions() {
			impl, ok := a.(clientgotesting.GenericActionImpl)
			if !ok || impl.Verb != "get" || impl.Subresource != "log" {
				continue
			}
			if o, ok := impl.Value.(*corev1.PodLogOptions); ok {
				opts = o
			}
		}
		if opts == nil {
			t.Fatal("no 'get pods/log' action with PodLogOptions recorded; the reader did not call the expected subresource")
		}
		if opts.Container != container {
			t.Errorf("PodLogOptions.Container = %q, want %q", opts.Container, container)
		}
		if opts.TailLines != nil {
			t.Errorf("PodLogOptions.TailLines = %v, want unset: a tail window loses "+
				"the load-time offload line (#1585)", *opts.TailLines)
		}
	})
}

// TestReadPodLogsTruncatesToTheHead feeds a serving log far longer than the
// requested window through the production reader and checks it hands back the
// first lines — including the load-time offload line — rather than the last.
// The fake clientset's log reactor returns the whole body, so this exercises the
// reader's own head truncation and stream close.
func TestReadPodLogsTruncatesToTheHead(t *testing.T) {
	var b strings.Builder
	b.WriteString("0.00.025.305 I   - Vulkan0 : Radeon 8060S Graphics (RADV STRIX_HALO) (133120 MiB, 132954 MiB free)\n")
	b.WriteString("0.05.998.410 I load_tensors: offloaded 63/63 layers to GPU\n")
	const window = 64
	for i := 1; i <= window*3; i++ {
		fmt.Fprintf(&b, "0.%02d.%03d.000 I srv  log_server_r: request: POST /v1/chat/completions 10.244.0.7 %d\n",
			i%60, i%1000, 200)
	}

	cs := fakeclientset.NewSimpleClientset()
	logs := b.String()
	cs.PrependReactor("get", "pods", func(clientgotesting.Action) (bool, runtime.Object, error) {
		return true, &runtime.Unknown{Raw: []byte(logs)}, nil
	})
	r := &clientsetPodLogReader{cs: cs}

	got, err := r.ReadPodLogs(context.Background(), "default", "svc-1", "llama-server", window)
	if err != nil {
		t.Fatalf("ReadPodLogs: %v", err)
	}
	if !strings.HasPrefix(got, "0.00.025.305 I   - Vulkan0") {
		t.Errorf("reader did not start at the log's head; got %q", firstLine(got))
	}
	if !strings.Contains(got, "offloaded 63/63 layers to GPU") {
		t.Error("reader lost the load-time offload line")
	}
	if n := strings.Count(got, "\n"); n != window {
		t.Errorf("reader returned %d lines, want the requested window of %d", n, window)
	}
}

// firstLine returns s's first line, for a readable failure message.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Importing internal/metrics registers nothing in controller-runtime's
// registry, so the controller's manager serves only what this package's
// init() registers there.
func TestControllerRegistersItsCollectors(t *testing.T) {
	for _, c := range llmkubemetrics.ControllerCollectors {
		var already prometheus.AlreadyRegisteredError
		if err := ctrlmetrics.Registry.Register(c); !errors.As(err, &already) {
			t.Errorf("collector %T is not registered in controller-runtime's registry: %v", c, err)
		}
	}
}
