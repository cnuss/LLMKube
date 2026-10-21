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

package controller

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestOfflineEnv drives constructDeployment and asserts on the serving
// container's env: spec.offline alone puts the Hub-offline variables on the
// pod, and an explicit spec.env entry of the same name lands after them.
func TestOfflineEnv(t *testing.T) {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "Qwen/Qwen2.5-0.5B-Instruct"},
	}
	isvc := func(offline bool, env ...corev1.EnvVar) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{Runtime: RuntimeVLLM, ModelRef: "qwen", Offline: offline, Env: env},
		}
	}
	mirrored := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "Qwen/Qwen2.5-0.5B-Instruct", SourceSecretRef: &corev1.LocalObjectReference{Name: "hf-mirror"}},
	}
	selfFetching := func(offline bool) *inferencev1alpha1.InferenceService {
		s := isvc(offline)
		s.Spec.SkipModelInit = ptr.To(true)
		return s
	}
	cases := []struct {
		name       string
		isvc       *inferencev1alpha1.InferenceService
		model      *inferencev1alpha1.Model
		hfEndpoint string
		want       map[string][]string
	}{
		{
			name:       "a self-fetching runtime gets the mirror when it may reach the Hub",
			isvc:       selfFetching(false),
			model:      mirrored,
			hfEndpoint: testMirror,
			want:       map[string][]string{"HF_ENDPOINT": {testMirror}, "HF_HUB_OFFLINE": nil},
		},
		{
			name:       "offline drops the mirror a self-fetching runtime would get",
			isvc:       selfFetching(true),
			model:      mirrored,
			hfEndpoint: testMirror,
			want:       map[string][]string{"HF_ENDPOINT": nil, "HF_HUB_OFFLINE": {"1"}},
		},
		{
			name: "unset leaves the runtime free to reach the Hub",
			isvc: isvc(false),
			want: map[string][]string{"HF_HUB_OFFLINE": nil, "TRANSFORMERS_OFFLINE": nil},
		},
		{
			name: "offline sets both variables without any prefetch",
			isvc: isvc(true),
			want: map[string][]string{"HF_HUB_OFFLINE": {"1"}, "TRANSFORMERS_OFFLINE": {"1"}},
		},
		{
			name: "an explicit spec.env entry comes after the operator's",
			isvc: isvc(true, corev1.EnvVar{Name: "HF_HUB_OFFLINE", Value: "0"}),
			want: map[string][]string{"HF_HUB_OFFLINE": {"1", "0"}, "TRANSFORMERS_OFFLINE": {"1"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &InferenceServiceReconciler{ModelCachePath: "/models", ModelCacheMode: ModelCacheModePerService}
			m := tc.model
			if m == nil {
				m = model
			}
			dep := r.constructDeployment(tc.isvc, m, nil, 1, tc.hfEndpoint, "")
			for name, want := range tc.want {
				var got []string
				for _, e := range mirrorEnv(dep, name) {
					got = append(got, e.Value)
				}
				if !slices.Equal(got, want) {
					t.Errorf("%s = %v, want %v", name, got, want)
				}
			}
		})
	}
}
