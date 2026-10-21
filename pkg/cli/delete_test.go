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

package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/cachekey"
)

func TestNewDeleteCommand(t *testing.T) {
	cmd := NewDeleteCommand()

	if cmd.Use != "delete [NAME]" {
		t.Errorf("Use = %q, want %q", cmd.Use, "delete [NAME]")
	}

	expectedAliases := []string{"del", "rm"}
	if len(cmd.Aliases) != len(expectedAliases) {
		t.Errorf("Aliases count = %d, want %d", len(cmd.Aliases), len(expectedAliases))
	}
	for i, alias := range expectedAliases {
		if i < len(cmd.Aliases) && cmd.Aliases[i] != alias {
			t.Errorf("Aliases[%d] = %q, want %q", i, cmd.Aliases[i], alias)
		}
	}

	if f := cmd.Flags().Lookup("namespace"); f == nil {
		t.Error("Missing --namespace flag")
	} else {
		if f.Shorthand != "n" {
			t.Errorf("namespace shorthand = %q, want %q", f.Shorthand, "n")
		}
		if f.DefValue != testDefaultNamespace {
			t.Errorf("namespace default = %q, want %q", f.DefValue, testDefaultNamespace)
		}
	}
}

func TestDeleteCommandRequiresArg(t *testing.T) {
	cmd := NewDeleteCommand()
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("Expected error when no argument provided")
	}
}

func TestNewDeleteCommand_PurgeCacheFlag(t *testing.T) {
	cmd := NewDeleteCommand()

	if f := cmd.Flags().Lookup("purge-cache"); f == nil {
		t.Error("Missing --purge-cache flag")
	} else {
		if f.DefValue != "false" {
			t.Errorf("purge-cache default = %q, want %q", f.DefValue, "false")
		}
	}
}

func TestDeleteOptions_PurgeCacheDefault(t *testing.T) {
	opts := &deleteOptions{
		name:      "test-model",
		namespace: testDefaultNamespace,
	}

	if opts.purgeCache {
		t.Error("purgeCache should be false by default")
	}
}

func TestDeleteOptions_PurgeCacheSet(t *testing.T) {
	opts := &deleteOptions{
		name:       "test-model",
		namespace:  testDefaultNamespace,
		purgeCache: true,
	}

	if !opts.purgeCache {
		t.Error("purgeCache should be true")
	}
}

// TestRunDelete_PurgeResolvesClaimsBeforeDeleting pins the delete order: the
// cache claims and the key come from state read before the InferenceService
// delete, because in perService mode the claim is owner-ref'd to the service
// and is collected with it. Resolving afterwards would print "nothing to
// purge" on a cluster whose GC has already removed the claim.
func TestRunDelete_PurgeResolvesClaimsBeforeDeleting(t *testing.T) {
	const (
		namespace = "default"
		modelName = "my-model"
	)
	key := cachekey.Compute("hf://org/repo")

	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "hf://org/repo",
			Files:  []string{"model.gguf", "model-00001-of-00002.gguf"},
		},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:   modelName,
			ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "my-cache-claim"},
		},
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cache-claim", Namespace: namespace},
	}

	// The mock server keeps the claim after the InferenceService delete, so the
	// probe sees exactly whether resolution happened before that delete.
	probe := &deleteProbeClient{
		Client: fake.NewClientBuilder().
			WithScheme(newCLIScheme()).
			WithObjects(model, isvc, claim).
			Build(),
	}

	origKubeConfig, origClientset, origNew := deleteKubeConfig, deleteClientset, newDeleteClient
	t.Cleanup(func() {
		deleteKubeConfig, deleteClientset, newDeleteClient = origKubeConfig, origClientset, origNew
	})
	deleteKubeConfig = func() (*rest.Config, error) { return nilRESTConfig, nil }
	clientset := fakeclientset.NewClientset()
	deleteClientset = func(*rest.Config) (kubernetes.Interface, error) { return clientset, nil }
	newDeleteClient = func(*rest.Config) (deleteCRClient, error) { return probe, nil }

	var cmds [][]string
	fakePurgeExec(t, func(command []string) (string, error) {
		if len(command) < 3 || command[1] != "-c" {
			return "", nil
		}
		return "removed /models/" + key + "\n", nil
	}, &cmds)

	if err := runDelete(&deleteOptions{name: modelName, namespace: namespace, purgeCache: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if probe.claimReads == 0 {
		t.Fatal("the cache claim was never read; the purge had no target")
	}
	if probe.claimReadsAfterServiceDelete > 0 {
		t.Errorf("%d cache claim reads happened after the InferenceService delete; resolve them first",
			probe.claimReadsAfterServiceDelete)
	}
	if len(cmds) == 0 {
		t.Fatal("no purge ran")
	}
	if !strings.Contains(cmds[0][2], "'/models/"+key+"'") {
		t.Errorf("purge command = %q, want the model's key", cmds[0][2])
	}
}

func stubDeleteSeams(t *testing.T, crClient deleteCRClient, clientset kubernetes.Interface) {
	t.Helper()
	origKubeConfig, origClientset, origNew := deleteKubeConfig, deleteClientset, newDeleteClient
	t.Cleanup(func() {
		deleteKubeConfig, deleteClientset, newDeleteClient = origKubeConfig, origClientset, origNew
	})
	deleteKubeConfig = func() (*rest.Config, error) { return nilRESTConfig, nil }
	deleteClientset = func(*rest.Config) (kubernetes.Interface, error) { return clientset, nil }
	newDeleteClient = func(*rest.Config) (deleteCRClient, error) { return crClient, nil }
}

// TestRunDelete_PurgeSharedClaimOnDefaultInstall pins the default install:
// shared mode has no per-service claim, so the model's files are on
// llmkube-model-cache and a purge that skips it removes nothing while
// reporting success (#2127).
func TestRunDelete_PurgeSharedClaimOnDefaultInstall(t *testing.T) {
	const (
		namespace = "default"
		modelName = "llama"
	)
	key := cachekey.Compute("hf://org/llama")

	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "hf://org/llama",
			Files:  []string{"model.gguf", "model-00001-of-00002.gguf"},
		},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "chat-api", Namespace: namespace},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: modelName},
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelCachePVCName,
			Namespace: namespace,
			Labels:    map[string]string{modelCacheLabel: modelCacheLabelValue},
		},
	}

	crClient := fake.NewClientBuilder().WithScheme(newCLIScheme()).WithObjects(model, isvc, claim).Build()
	clientset := fakeclientset.NewClientset()
	stubDeleteSeams(t, crClient, clientset)

	var cmds [][]string
	fakePurgeExec(t, func(command []string) (string, error) {
		if len(command) < 3 || command[1] != "-c" {
			return "", nil
		}
		return "removed /models/" + key + "\n", nil
	}, &cmds)

	if err := runDelete(&deleteOptions{name: modelName, namespace: namespace, purgeCache: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cmds) == 0 {
		t.Fatal("no purge ran; the model's key is still on the shared claim")
	}
	if !strings.Contains(cmds[0][2], "'/models/"+key+"'") {
		t.Errorf("purge command = %q, want the model's key", cmds[0][2])
	}
	var purged []string
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "pods" {
			purged = append(purged, action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Name)
		}
	}
	// One pod, on the shared claim: a pod on a per-service claim would stall
	// on attach in shared mode, where no service mounts one.
	if want := purgePodName(modelCachePVCName); len(purged) != 1 || purged[0] != want {
		t.Errorf("purge pods = %v, want only %s", purged, want)
	}
}

// TestRunDelete_PurgeTargetsClaimDeclaredOnModel pins the GitOps shape: the
// claim is declared on the Model, the service inherits it, and the Model is
// gone by the time the purge runs, so its claim must be read before the
// delete or the purge lands on the shared cache and leaves the files behind.
func TestRunDelete_PurgeTargetsClaimDeclaredOnModel(t *testing.T) {
	const (
		namespace = "default"
		modelName = "qwen"
	)
	key := cachekey.Compute("hf://org/qwen")

	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
		Spec: inferencev1alpha1.ModelSpec{
			Source:     "hf://org/qwen",
			Files:      []string{"model.gguf"},
			ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "model-claim"},
		},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "chat-api", Namespace: namespace},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: modelName},
	}
	shared := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelCachePVCName,
			Namespace: namespace,
			Labels:    map[string]string{modelCacheLabel: modelCacheLabelValue},
		},
	}
	modelClaim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "model-claim", Namespace: namespace},
	}

	crClient := fake.NewClientBuilder().WithScheme(newCLIScheme()).WithObjects(model, isvc, shared, modelClaim).Build()
	clientset := fakeclientset.NewClientset()
	stubDeleteSeams(t, crClient, clientset)

	var cmds [][]string
	fakePurgeExec(t, func(command []string) (string, error) {
		if len(command) < 3 || command[1] != "-c" {
			return "", nil
		}
		return "removed /models/" + key + "\n", nil
	}, &cmds)

	if err := runDelete(&deleteOptions{name: modelName, namespace: namespace, purgeCache: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var purged []string
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "pods" {
			purged = append(purged, action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Name)
		}
	}
	if want := purgePodName("model-claim"); len(purged) != 1 || purged[0] != want {
		t.Errorf("purge pods = %v, want only %s (the claim the Model declared, read before the Model was deleted)",
			purged, want)
	}
}

// TestRunDelete_PurgeTargetsClaimsStatusPrefetchRecords: the controller's
// sticky target staged the Model into the shared cache before the service
// moved to its own claim, so the files are on both and both are purged.
func TestRunDelete_PurgeTargetsClaimsStatusPrefetchRecords(t *testing.T) {
	const (
		namespace = "default"
		modelName = "qwen"
	)
	key := cachekey.Compute("hf://org/qwen")

	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
		Spec:       inferencev1alpha1.ModelSpec{Source: "hf://org/qwen", Files: []string{"model.gguf"}},
		Status: inferencev1alpha1.ModelStatus{Prefetch: []inferencev1alpha1.PrefetchStatus{
			{ClaimName: modelCachePVCName, Own: true, Phase: inferencev1alpha1.PrefetchPhaseSucceeded},
		}},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "chat-api", Namespace: namespace},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:   modelName,
			ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "moved-claim"},
		},
	}
	shared := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelCachePVCName,
			Namespace: namespace,
			Labels:    map[string]string{modelCacheLabel: modelCacheLabelValue},
		},
	}
	moved := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "moved-claim", Namespace: namespace},
	}

	crClient := fake.NewClientBuilder().WithScheme(newCLIScheme()).WithObjects(model, isvc, shared, moved).Build()
	clientset := fakeclientset.NewClientset()
	stubDeleteSeams(t, crClient, clientset)

	var cmds [][]string
	fakePurgeExec(t, func(command []string) (string, error) {
		if len(command) < 3 || command[1] != "-c" {
			return "", nil
		}
		return "removed /models/" + key + "\n", nil
	}, &cmds)

	if err := runDelete(&deleteOptions{name: modelName, namespace: namespace, purgeCache: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	purged := map[string]bool{}
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "pods" {
			purged[action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Name] = true
		}
	}
	if len(purged) != 2 || !purged[purgePodName(modelCachePVCName)] || !purged[purgePodName("moved-claim")] {
		t.Errorf("purge pods = %v, want one on %s (status.prefetch) and one on moved-claim (the service)",
			purged, modelCachePVCName)
	}
}

// TestRunDelete_SkipsClaimCollectedWithService pins perService mode: the
// per-service claim is owner-ref'd to the InferenceService, so garbage
// collection removes it, and a pod mounting a terminating claim never
// schedules. Purging it only waits out the 120s timeout and fails.
func TestRunDelete_SkipsClaimCollectedWithService(t *testing.T) {
	const (
		namespace = "default"
		modelName = "llama"
	)
	controller := true
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "hf://org/llama",
			Files:  []string{"model.gguf", "model-00001-of-00002.gguf"},
		},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: modelName},
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      perServiceCacheClaimName(modelName),
			Namespace: namespace,
			Labels:    map[string]string{modelCacheLabel: modelCacheLabelValue},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: inferencev1alpha1.GroupVersion.String(),
				Kind:       "InferenceService",
				Name:       modelName,
				Controller: &controller,
			}},
		},
	}

	crClient := fake.NewClientBuilder().WithScheme(newCLIScheme()).WithObjects(model, isvc, claim).Build()
	clientset := fakeclientset.NewClientset()
	stubDeleteSeams(t, crClient, clientset)

	var cmds [][]string
	fakePurgeExec(t, func([]string) (string, error) {
		return "", nil
	}, &cmds)

	if err := runDelete(&deleteOptions{name: modelName, namespace: namespace, purgeCache: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cmds) != 0 {
		t.Errorf("purge ran %d command(s) against a claim garbage collection removes: %v", len(cmds), cmds)
	}
	if creates := purgePodCreates(clientset); creates != 0 {
		t.Errorf("created %d purge pod(s) for a claim garbage collection removes", creates)
	}
}

// TestRunDelete_PurgeReportsResolveFailure pins that a purge whose target
// resolution failed exits non-zero. Warning, printing "nothing to purge", and
// returning 0 reports a purge that never happened.
func TestRunDelete_PurgeReportsResolveFailure(t *testing.T) {
	const (
		namespace = "default"
		modelName = "llama"
	)
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "hf://org/llama",
			Files:  []string{"model.gguf", "model-00001-of-00002.gguf"},
		},
	}

	// The InferenceService list is the first read a resolution makes.
	crClient := &failListClient{
		Client: fake.NewClientBuilder().WithScheme(newCLIScheme()).WithObjects(model).Build(),
	}
	clientset := fakeclientset.NewClientset()
	stubDeleteSeams(t, crClient, clientset)

	var cmds [][]string
	fakePurgeExec(t, func([]string) (string, error) {
		return "", nil
	}, &cmds)

	err := runDelete(&deleteOptions{name: modelName, namespace: namespace, purgeCache: true})
	if err == nil {
		t.Fatal("expected an error; the purge resolved no targets and reported success")
	}
	if !strings.Contains(err.Error(), "llmkube cache clear -n "+namespace) {
		t.Errorf("error = %q, want the 'llmkube cache clear -n %s' hint", err, namespace)
	}
}

// failListClient fails every list, which is how resolvePurgeTargets starts.
type failListClient struct {
	client.Client
}

var errTestListFailed = errors.New("the API server is unavailable")

func (c *failListClient) List(
	ctx context.Context, list client.ObjectList, opts ...client.ListOption,
) error {
	return errTestListFailed
}

// purgePodCreates counts the pods a run created through the fake clientset.
func purgePodCreates(clientset *fakeclientset.Clientset) int {
	var creates int
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "pods" {
			creates++
		}
	}
	return creates
}

// deleteProbeClient records whether a claim is read before or after the
// InferenceService delete is issued through it.
type deleteProbeClient struct {
	client.Client
	isvcDeleted                  bool
	claimReads                   int
	claimReadsAfterServiceDelete int
}

func (c *deleteProbeClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if err := c.Client.Delete(ctx, obj, opts...); err != nil {
		return err
	}
	if _, ok := obj.(*inferencev1alpha1.InferenceService); ok {
		c.isvcDeleted = true
	}
	return nil
}

func (c *deleteProbeClient) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
		c.claimReads++
		if c.isvcDeleted {
			c.claimReadsAfterServiceDelete++
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
