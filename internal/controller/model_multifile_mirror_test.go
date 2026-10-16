package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const multiFileMirror = "https://artifactory.corp.example/artifactory/api/huggingfaceml/hf-remote"

// A spec.files Model whose source is a repo base on the origin HF_ENDPOINT names
// is a Hugging Face repo served through a mirror, so the multi-file gate must let
// it through: the init container already fetches "$SOURCE/$rel" with the bearer
// token for that origin. Any other plain https:// base stays rejected, so the
// gate still fails closed for a lookalike host, a Secret without HF_ENDPOINT,
// and a Model that names no Secret at all.
func TestValidateMultiFileStagingSource_HFMirror(t *testing.T) {
	repoBase := multiFileMirror + "/nvidia/GLM-5.3-Flash-NVFP4/resolve/main/"
	withEndpoint := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "huggingface", Namespace: "default"},
		Data: map[string][]byte{
			"HF_TOKEN":    []byte("hf_secret"),
			"HF_ENDPOINT": []byte(multiFileMirror + "\n"),
		},
	}
	tokenOnly := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "token-only", Namespace: "default"},
		Data:       map[string][]byte{"HF_TOKEN": []byte("hf_secret")},
	}

	tests := []struct {
		name       string
		source     string
		secretRef  string
		wantReject bool
	}{
		{"repo base on the HF_ENDPOINT origin", repoBase, "huggingface", false},
		{"lookalike host", "https://artifactory.corp.example.evil.example/nvidia/GLM-5.3-Flash-NVFP4/resolve/main/", "huggingface", true},
		{"same host over http", "http://artifactory.corp.example/artifactory/api/huggingfaceml/hf-remote/nvidia/GLM-5.3-Flash-NVFP4/resolve/main/", "huggingface", true},
		{"Secret without HF_ENDPOINT", repoBase, "token-only", true},
		{"no sourceSecretRef", repoBase, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "glm", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source: tt.source,
					Format: "safetensors",
					Files:  []string{"config.json", "model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"},
				},
			}
			if tt.secretRef != "" {
				model.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: tt.secretRef}
			}
			r := newMultiFileGateReconciler(t, model, withEndpoint, tokenOnly)

			handled, _ := r.validateMultiFileStagingSource(context.Background(), model)
			if handled != tt.wantReject {
				t.Fatalf("validateMultiFileStagingSource handled = %v, want %v", handled, tt.wantReject)
			}

			stored := &inferencev1alpha1.Model{}
			if err := r.Get(context.Background(), types.NamespacedName{Name: "glm", Namespace: "default"}, stored); err != nil {
				t.Fatalf("get Model: %v", err)
			}
			cond := meta.FindStatusCondition(stored.Status.Conditions, ConditionDegraded)
			failed := stored.Status.Phase == PhaseFailed && cond != nil && cond.Reason == "InvalidFileSet"
			if failed != tt.wantReject {
				t.Errorf("stored Model failed with InvalidFileSet = %v, want %v (phase=%q, degraded=%+v)",
					failed, tt.wantReject, stored.Status.Phase, cond)
			}
		})
	}
}

// The gate only admits the mirror source; the download is the init container's.
// This pins what that container receives for such a Model: the mirror repo base
// as MODEL_SOURCE (not rewritten to huggingface.co), each file under it, and the
// bearer header, so a later change to source normalization or the auth gate that
// would send these downloads elsewhere or anonymous fails here.
func TestMultiFileInitContainer_HFMirrorRepoBase(t *testing.T) {
	repoBase := multiFileMirror + "/nvidia/GLM-5.3-Flash-NVFP4/resolve/main/"
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "glm", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:          repoBase,
			Format:          "safetensors",
			Files:           []string{"config.json", "tokenizer.json"},
			SourceSecretRef: &corev1.LocalObjectReference{Name: "huggingface"},
		},
		Status: inferencev1alpha1.ModelStatus{CacheKey: "glmkey"},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "glm", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "glm", Runtime: "vllm"},
	}

	cfg := buildModelStorageConfig(model, isvc, "default", true, "", "", defaultPrefetchImage, 0, nil, multiFileMirror)

	var downloader *corev1.Container
	for i := range cfg.initContainers {
		if cfg.initContainers[i].Name == "model-downloader" {
			downloader = &cfg.initContainers[i]
		}
	}
	if downloader == nil {
		t.Fatal("no model-downloader init container")
	}
	env := map[string]string{}
	for _, e := range downloader.Env {
		env[e.Name] = e.Value
	}
	if env["MODEL_SOURCE"] != repoBase {
		t.Errorf("MODEL_SOURCE = %q, want the mirror repo base %q", env["MODEL_SOURCE"], repoBase)
	}
	if env["MODEL_FILES"] != "config.json\ntokenizer.json" {
		t.Errorf("MODEL_FILES = %q, want both files", env["MODEL_FILES"])
	}
	script := ""
	for _, c := range downloader.Command {
		script += c
	}
	for _, a := range downloader.Args {
		script += a
	}
	if !containsAll(script, `-H "Authorization: Bearer ${HF_TOKEN}"`, `url="${SOURCE%/}/$rel"`) {
		t.Errorf("downloader does not fetch $SOURCE/$rel with the bearer header:\n%s", script)
	}
}

func newMultiFileGateReconciler(t *testing.T, model *inferencev1alpha1.Model, secrets ...*corev1.Secret) *ModelReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add batch scheme: %v", err)
	}
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(model).
		WithStatusSubresource(&inferencev1alpha1.Model{})
	for _, s := range secrets {
		builder = builder.WithObjects(s)
	}
	return &ModelReconciler{Client: builder.Build()}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
