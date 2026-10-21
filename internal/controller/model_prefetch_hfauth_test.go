package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The prefetch Job reuses the serving path's downloader through
// buildModelStorageConfig, so it inherits Hugging Face auth rather than
// implementing its own. That inheritance is the thing worth pinning: a future
// refactor that gives prefetch a bespoke container would silently reintroduce
// the 401 on gated repositories that #1750 fixed for the serving path.
func TestPrefetchJob_CarriesHFAuth(t *testing.T) {
	r := &ModelReconciler{InitContainerImage: defaultPrefetchImage}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "llama-guard", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:          "hf://meta-llama/Llama-Guard-4-12B",
			Format:          "safetensors",
			Files:           []string{"model.safetensors", "config.json"},
			SourceSecretRef: &corev1.LocalObjectReference{Name: "hf-token"},
		},
		Status: inferencev1alpha1.ModelStatus{CacheKey: "testcachekey"},
	}

	job, err := r.buildPrefetchJob(context.Background(), model, "", "")
	if err != nil {
		t.Fatalf("buildPrefetchJob: %v", err)
	}

	var downloader *corev1.Container
	for i := range job.Spec.Template.Spec.InitContainers {
		c := &job.Spec.Template.Spec.InitContainers[i]
		if c.Name == "model-downloader" {
			downloader = c
		}
	}
	if downloader == nil {
		t.Fatal("prefetch Job has no downloader init container")
	}

	var sawSecret bool
	for _, ef := range downloader.EnvFrom {
		if ef.SecretRef != nil && ef.SecretRef.Name == "hf-token" {
			sawSecret = true
		}
	}
	if !sawSecret {
		t.Errorf("downloader does not mount sourceSecretRef, so HF_TOKEN never reaches it; envFrom=%+v", downloader.EnvFrom)
	}

	script := strings.Join(downloader.Command, " ")
	if !strings.Contains(script, authHeader) {
		t.Errorf("prefetch downloader sends no bearer header:\n%s", script)
	}
}

// The same Job for a non-Hugging-Face source must not carry the header, so a
// secret shared between an S3 Model and an HF Model cannot leak the token to
// the object store. A source on the host named by HF_ENDPOINT IS authenticated
// (the mirror case, #1900); this Model names no such Secret, so the mirror
// branch cannot fire.
func TestPrefetchJob_NoHFAuthForOtherHosts(t *testing.T) {
	r := &ModelReconciler{InitContainerImage: defaultPrefetchImage}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "mirrored", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:          "https://mirror.internal/models/model.gguf",
			Format:          "gguf",
			SourceSecretRef: &corev1.LocalObjectReference{Name: "hf-token"},
		},
		Status: inferencev1alpha1.ModelStatus{CacheKey: "testcachekey"},
	}

	job, err := r.buildPrefetchJob(context.Background(), model, "", "")
	if err != nil {
		t.Fatalf("buildPrefetchJob: %v", err)
	}
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if strings.Contains(strings.Join(c.Command, " "), "HF_TOKEN") {
			t.Errorf("non-HF source leaks the token into %s:\n%s", c.Name, strings.Join(c.Command, " "))
		}
	}
}

// The prefetch Job inherits the mirror gate from buildModelStorageConfig, so a
// Model whose source is on the host named by HF_ENDPOINT in its sourceSecretRef
// must carry the bearer header. Before #1900 the gate was huggingface.co only,
// so a prefetch from a mirror went out anonymous and the mirror answered 401.
func TestPrefetchJob_CarriesHFAuthForMirror(t *testing.T) {
	const endpoint = "https://artifactory.corp.example/artifactory/api/huggingfaceml/repo"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "hf-token", Namespace: "default"},
		Data: map[string][]byte{
			"HF_TOKEN":    []byte("hf_secret"),
			"HF_ENDPOINT": []byte(endpoint),
		},
	}
	r := newHFSecretClient(t, secret)
	r.InitContainerImage = defaultPrefetchImage
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "mirrored", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:          endpoint + "/unsloth/Qwen3-GGUF/resolve/main/m.gguf",
			Format:          "gguf",
			SourceSecretRef: &corev1.LocalObjectReference{Name: "hf-token"},
		},
		Status: inferencev1alpha1.ModelStatus{CacheKey: "testcachekey"},
	}

	job, err := r.buildPrefetchJob(context.Background(), model, "", "")
	if err != nil {
		t.Fatalf("buildPrefetchJob: %v", err)
	}
	script := ""
	for _, c := range job.Spec.Template.Spec.InitContainers {
		script += strings.Join(c.Command, " ")
	}
	if !strings.Contains(script, authHeader) {
		t.Errorf("mirror prefetch downloader sends no bearer header:\n%s", script)
	}

	// The lookalike host must not authenticate even with the same Secret.
	lookalike := model.DeepCopy()
	lookalike.Spec.Source = "https://artifactory.corp.example.evil.example/repo/m.gguf"
	job, err = r.buildPrefetchJob(context.Background(), lookalike, "", "")
	if err != nil {
		t.Fatalf("buildPrefetchJob(lookalike): %v", err)
	}
	script = ""
	for _, c := range job.Spec.Template.Spec.InitContainers {
		script += strings.Join(c.Command, " ")
	}
	if strings.Contains(script, "HF_TOKEN") {
		t.Errorf("lookalike host leaked the token:\n%s", script)
	}
}
