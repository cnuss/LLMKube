package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const repoBaseMirror = "https://artifactory.corp.example/artifactory/api/huggingfaceml/hf-remote"

// A repo source staged with spec.files downloads from HF_ENDPOINT when the
// Secret names one, as huggingface_hub does, and from huggingface.co otherwise.
// A bare repo ID is the same repo as hf://. A source that already names a URL
// to fetch from, or is not a Hugging Face repo, is left alone.
func TestStagingRepoBase(t *testing.T) {
	tests := []struct {
		name, source, endpoint, want string
	}{
		{"bare repo id via the mirror", "nvidia/GLM-5.3-Flash-NVFP4", repoBaseMirror, repoBaseMirror + "/nvidia/GLM-5.3-Flash-NVFP4/resolve/main/"},
		{"bare repo id without a mirror", "nvidia/GLM-5.3-Flash-NVFP4", "", "https://huggingface.co/nvidia/GLM-5.3-Flash-NVFP4/resolve/main/"},
		{"endpoint with trailing slash", "nvidia/GLM-5.3-Flash-NVFP4", repoBaseMirror + "/", repoBaseMirror + "/nvidia/GLM-5.3-Flash-NVFP4/resolve/main/"},
		{"hf scheme with revision via the mirror", "hf://org/repo@abc123", repoBaseMirror, repoBaseMirror + "/org/repo/resolve/abc123/"},
		{"hf scheme without a mirror", "hf://org/repo", "", "https://huggingface.co/org/repo/resolve/main/"},
		{"huggingface.co repo URL via the mirror", "https://huggingface.co/org/repo/resolve/main/", repoBaseMirror, repoBaseMirror + "/org/repo/resolve/main/"},
		{"mirror repo base is already a URL", repoBaseMirror + "/org/repo/resolve/main/", repoBaseMirror, repoBaseMirror + "/org/repo/resolve/main/"},
		{"s3 is not a repo", "s3://bucket/prefix", repoBaseMirror, "s3://bucket/prefix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stagingRepoBase(tt.source, tt.endpoint); got != tt.want {
				t.Errorf("stagingRepoBase(%q, %q) = %q, want %q", tt.source, tt.endpoint, got, tt.want)
			}
		})
	}
}

// The downloader for a bare repo ID with spec.files fetches from the mirror,
// with the token: the token rule is applied to the URL actually downloaded,
// not to the spec.source spelling, which for a bare ID names no host.
func TestMultiFileInitContainer_BareRepoIDViaMirror(t *testing.T) {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "glm", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:          "nvidia/GLM-5.3-Flash-NVFP4",
			Format:          "safetensors",
			Files:           []string{"config.json", "tokenizer.json"},
			SourceSecretRef: &corev1.LocalObjectReference{Name: "huggingface"},
		},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "glm", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "glm", Runtime: "vllm"},
	}

	for _, useCache := range []bool{true, false} {
		cfg := buildModelStorageConfig(model, isvc, "default", useCache, "", "", defaultPrefetchImage, 0, nil, repoBaseMirror)
		var downloader *corev1.Container
		for i := range cfg.initContainers {
			if cfg.initContainers[i].Name == "model-downloader" {
				downloader = &cfg.initContainers[i]
			}
		}
		if downloader == nil {
			t.Fatalf("useCache=%v: no model-downloader init container", useCache)
		}
		source := ""
		for _, e := range downloader.Env {
			if e.Name == "MODEL_SOURCE" {
				source = e.Value
			}
		}
		if want := repoBaseMirror + "/nvidia/GLM-5.3-Flash-NVFP4/resolve/main/"; source != want {
			t.Errorf("useCache=%v: MODEL_SOURCE = %q, want %q", useCache, source, want)
		}
		script := strings.Join(downloader.Command, " ") + strings.Join(downloader.Args, " ")
		if !strings.Contains(script, `-H "Authorization: Bearer ${HF_TOKEN}"`) {
			t.Errorf("useCache=%v: downloader sends no bearer header to the mirror", useCache)
		}
	}
}

// Prefetch stages a Model before any InferenceService exists. A bare repo ID
// with spec.files is staged by the operator, so it is prefetchable; without
// spec.files it is still the runtime's to fetch.
func TestPrefetchEligible_BareRepoID(t *testing.T) {
	model := &inferencev1alpha1.Model{Spec: inferencev1alpha1.ModelSpec{
		Source: "nvidia/GLM-5.3-Flash-NVFP4", Prefetch: true, Files: []string{"config.json"},
	}}
	if !prefetchEligible(model) {
		t.Error("bare repo ID with spec.files is not prefetchable")
	}
	model.Spec.Files = nil
	if prefetchEligible(model) {
		t.Error("bare repo ID without spec.files became prefetchable")
	}
}

// The MissingSkipModelInit warning tells a user the init container has nothing
// to fetch. With spec.files it does, so the warning would steer them into
// breaking the deployment.
func TestShouldWarnMissingSkipModelInit_MultiFileRepo(t *testing.T) {
	model := &inferencev1alpha1.Model{
		Spec:   inferencev1alpha1.ModelSpec{Source: "nvidia/GLM-5.3-Flash-NVFP4"},
		Status: inferencev1alpha1.ModelStatus{Phase: PhaseReady},
	}
	isvc := &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{Runtime: "vllm"}}
	if !shouldWarnMissingSkipModelInit(model, isvc) {
		t.Fatal("precondition: a bare repo ID without spec.files should warn")
	}
	model.Spec.Files = []string{"config.json"}
	if shouldWarnMissingSkipModelInit(model, isvc) {
		t.Error("warned to skip the init container for a Model it stages")
	}
}

// A bare repo ID with globs expands against the mirror's listing.
func TestReconcileRuntimeResolved_BareRepoIDListsFromMirror(t *testing.T) {
	body, err := os.ReadFile("testdata/hflisting/artifactory-tree-qwen.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/artifactory/api/huggingfaceml/hf-remote/api/models/Qwen/Qwen2.5-0.5B-Instruct/tree/main" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	endpoint := srv.URL + "/artifactory/api/huggingfaceml/hf-remote"

	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default", Generation: 1},
		Spec: inferencev1alpha1.ModelSpec{
			Source:          "Qwen/Qwen2.5-0.5B-Instruct",
			Format:          "safetensors",
			SourceSecretRef: &corev1.LocalObjectReference{Name: "huggingface"},
			Files:           []string{"config.json", "*.json", "*.safetensors"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "huggingface", Namespace: "default"},
		Data:       map[string][]byte{"HF_ENDPOINT": []byte(endpoint)},
	}
	r := newMultiFileGateReconciler(t, model, secret)
	r.AllowedRemoteHosts = []string{"127.0.0.1"}
	if _, err := r.reconcileRuntimeResolvedSource(context.Background(), model, ""); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	stored := getModel(t, r, "qwen")
	want := "config.json generation_config.json tokenizer.json tokenizer_config.json vocab.json model.safetensors"
	if got := strings.Join(stored.Status.StagedFiles, " "); stored.Status.Phase != PhaseReady || got != want {
		t.Errorf("phase = %q, stagedFiles = %q; want Ready and %q (conditions=%+v)", stored.Status.Phase, got, want, stored.Status.Conditions)
	}
}

// The glob expansion is written only by the runtime-resolved reconcile, which
// runs after prefetch. A prefetch Job built before it would stage an
// unexpanded glob and fail, so a Model with a glob and spec.prefetch must skip
// prefetch and still reach Ready with the expansion; its files then stage
// when the serving pod starts.
func TestReconcile_GlobWithPrefetchReachesReady(t *testing.T) {
	body, err := os.ReadFile("testdata/hflisting/artifactory-tree-qwen.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/models/Qwen/Qwen2.5-0.5B-Instruct/tree/main") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	endpoint := srv.URL + "/artifactory/api/huggingfaceml/hf-remote"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "huggingface", Namespace: "default"},
		Data:       map[string][]byte{"HF_ENDPOINT": []byte(endpoint)},
	}

	for _, source := range []string{"Qwen/Qwen2.5-0.5B-Instruct", "hf://Qwen/Qwen2.5-0.5B-Instruct"} {
		t.Run(source, func(t *testing.T) {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default", Generation: 1},
				Spec: inferencev1alpha1.ModelSpec{
					Source:          source,
					Format:          "safetensors",
					Prefetch:        true,
					SourceSecretRef: &corev1.LocalObjectReference{Name: "huggingface"},
					Files:           []string{"config.json", "*.json", "*.safetensors"},
				},
			}
			if prefetchEligible(model) {
				t.Error("a Model with a glob in spec.files is prefetch-eligible")
			}
			r := newMultiFileGateReconciler(t, model, secret)
			r.AllowedRemoteHosts = []string{"127.0.0.1"}
			r.InitContainerImage = defaultPrefetchImage
			r.Scheme = r.Client.Scheme()
			for i := 0; i < 3; i++ {
				if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "qwen", Namespace: "default"}}); err != nil {
					t.Fatalf("reconcile %d: %v", i, err)
				}
			}
			stored := getModel(t, r, "qwen")
			want := "config.json generation_config.json tokenizer.json tokenizer_config.json vocab.json model.safetensors"
			if got := strings.Join(stored.Status.StagedFiles, " "); stored.Status.Phase != PhaseReady || got != want {
				t.Errorf("phase = %q, stagedFiles = %q; want Ready and %q (conditions=%+v)", stored.Status.Phase, got, want, stored.Status.Conditions)
			}
		})
	}
}
