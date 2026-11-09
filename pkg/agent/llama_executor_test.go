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

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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
