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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestWatcherServesOnlyItsAccelerator(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	model := func(name, accel string) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/model.gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: accel},
			},
		}
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(model("on-mac", "metal"), model("on-npu", "npu"), model("on-gpu", "cuda")).
		Build()
	isvc := func(modelRef string) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: modelRef + "-svc", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: modelRef},
		}
	}

	for accel, want := range map[string]map[string]bool{
		"":    {"on-mac": true, "on-npu": false, "on-gpu": false},
		"npu": {"on-mac": false, "on-npu": true, "on-gpu": false},
	} {
		w := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())
		w.SetAccelerator(accel)
		for ref, wantWatch := range want {
			got, err := w.shouldWatch(context.Background(), isvc(ref))
			if err != nil || got != wantWatch {
				t.Errorf("agent serving %q: shouldWatch(%s) = %v, %v; want %v", accel, ref, got, err, wantWatch)
			}
		}
	}
}

func TestBuildExecutorsWithoutLlamaServer(t *testing.T) {
	k8sClient := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	a := NewMetalAgent(MetalAgentConfig{
		K8sClient:      k8sClient,
		Namespace:      "test-ns",
		ModelStorePath: "/tmp/test-models",
		Runtime:        runtimeOllama,
	})
	a.buildExecutors()

	for _, rt := range []string{runtimeLlamaServer, runtimeLlamaCPP} {
		if _, ok := a.executors[rt]; ok {
			t.Errorf("executors[%q] registered with no llama-server binary; want the start refused as unregistered", rt)
		}
	}
	if _, ok := a.executors[runtimeOllama]; !ok {
		t.Errorf("executors[%q] not registered", runtimeOllama)
	}
	orphan := &ManagedProcess{Runtime: runtimeLlamaCPP, PID: 4242}
	if err := a.stopManagedProcess(context.Background(), orphan, true); err == nil {
		t.Error("stopping a llama.cpp process with no executor returned nil; want an error, not a nil dereference")
	}
}
