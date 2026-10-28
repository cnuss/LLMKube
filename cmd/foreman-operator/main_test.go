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
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
)

// TestAgenticTaskReconciler_CarriesArchiveDir pins the one assignment that
// makes --archive-dir do anything. Deleting `ArchiveDir: archiveDir` from
// the constructed struct still compiles and still passes every other test
// in this repo, while archival goes silently inert on a real cluster: the
// chart renders the flag and mounts the volume, the operator starts
// cleanly, and no bundle is ever written.
//
// Note for whoever maintains this: gofmt is NOT a backstop here. Removing
// the longest key collapses the struct's field alignment, so `gofmt -l`
// flags the file, but a single `gofmt -w` clears that and everything else
// goes green. Only this assertion catches it.
func TestAgenticTaskReconciler_CarriesArchiveDir(t *testing.T) {
	for _, tc := range []struct {
		name       string
		archiveDir string
	}{
		{"an absolute dir reaches the reconciler", "/x"},
		{"a non-default dir is not rewritten", "/mnt/elsewhere"},
		// Empty must stay empty. A seam that substituted a default here
		// would turn archival ON for every install that never asked for
		// it, writing source code and issue text to disk by surprise.
		{"empty stays empty so archival stays off", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := agenticTaskReconciler(nil, nil, tc.archiveDir)
			if r.ArchiveDir != tc.archiveDir {
				t.Fatalf("ArchiveDir = %q, want %q", r.ArchiveDir, tc.archiveDir)
			}
		})
	}
}

// TestAgenticTaskReconciler_CarriesClientAndScheme keeps the seam honest
// about the other two fields, so it cannot become a function that returns
// a reconciler wired to nothing but the archive dir.
func TestAgenticTaskReconciler_CarriesClientAndScheme(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	s := runtime.NewScheme()

	r := agenticTaskReconciler(c, s, "/x")

	if r.Client != c {
		t.Errorf("Client = %v, want the client passed in (%v)", r.Client, c)
	}
	if r.Scheme != s {
		t.Errorf("Scheme = %v, want the scheme passed in (%v)", r.Scheme, s)
	}
}

// The foreman-operator runs no watch canary, so llmkube_watch_canary_up on its
// endpoint is a 0 that fires LLMKubeControllerWatchCanaryDown for its namespace
// (#2294). The canary gauges are unlabelled: registered, they are served.
func TestForemanOperatorServesOnlyItsCollectors(t *testing.T) {
	for _, c := range llmkubemetrics.ForemanCollectors {
		var already prometheus.AlreadyRegisteredError
		if err := ctrlmetrics.Registry.Register(c); !errors.As(err, &already) {
			t.Errorf("collector %T is not registered in controller-runtime's registry: %v", c, err)
		}
	}

	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if strings.HasPrefix(mf.GetName(), "llmkube_watch_canary_") {
			t.Errorf("foreman-operator serves %s, which only the controller sets", mf.GetName())
		}
	}
}
