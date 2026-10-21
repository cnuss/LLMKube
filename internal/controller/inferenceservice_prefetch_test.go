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
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// createCachePVC makes a user-owned cache claim a prefetch can target, and
// removes it with the spec.
func createCachePVC(ctx context.Context, ns, name string) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, pvc))).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(ctx, pvc) })
}

// markJobComplete marks a Job Complete. k8s >=1.31 validates the Job status
// subresource: a finished Job needs startTime/completionTime, and
// Complete=True requires SuccessCriteriaMet=True first.
func markJobComplete(ctx context.Context, key types.NamespacedName) {
	job := &batchv1.Job{}
	Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.CompletionTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	job.Status.Succeeded = 1
	Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
}

// markJobFailed marks a Job Failed. Failed=True requires FailureTarget=True
// and a startTime under the same validation.
func markJobFailed(ctx context.Context, key types.NamespacedName) {
	job := &batchv1.Job{}
	Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "PodFailurePolicy", Message: "test"},
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "PodFailurePolicy", Message: "test"},
	}
	job.Status.Failed = 3
	Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
}

// cacheClaimOf returns the PersistentVolumeClaim the pod's model-cache volume
// mounts, or "".
func cacheClaimOf(spec *corev1.PodSpec) string {
	for _, v := range spec.Volumes {
		if v.Name == "model-cache" && v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

func initContainerNames(spec *corev1.PodSpec) []string {
	names := make([]string, 0, len(spec.InitContainers))
	for _, c := range spec.InitContainers {
		names = append(names, c.Name)
	}
	return names
}

var _ = Describe("InferenceService modelCache.prefetch (#2136)", func() {
	ctx := context.Background()
	const ns = "prefetch-isvc-test"

	modelReconciler := func() *ModelReconciler {
		return &ModelReconciler{
			Client:               k8sClient,
			Scheme:               k8sClient.Scheme(),
			InitContainerImage:   "docker.io/curlimages/curl:8.18.0",
			DefaultFSGroup:       102,
			ModelCacheSize:       "10Gi",
			ModelCacheAccessMode: "ReadWriteOnce",
			ModelCacheMode:       ModelCacheModeShared,
		}
	}
	isvcReconciler := func() *InferenceServiceReconciler {
		return &InferenceServiceReconciler{
			Client:               k8sClient,
			Scheme:               k8sClient.Scheme(),
			Recorder:             events.NewFakeRecorder(50),
			ModelCachePath:       "/models",
			ModelCacheMode:       ModelCacheModeShared,
			ModelCacheSize:       "10Gi",
			ModelCacheAccessMode: "ReadWriteOnce",
			InitContainerImage:   "docker.io/curlimages/curl:8.18.0",
			DefaultFSGroup:       102,
		}
	}

	// readyModel creates a Model and writes the Ready status the Model
	// controller would, so the service controller proceeds past its gate.
	readyModel := func(name, source string, mutate func(*inferencev1alpha1.Model)) *inferencev1alpha1.Model {
		m := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       inferencev1alpha1.ModelSpec{Source: source, Format: "gguf"},
		}
		if mutate != nil {
			mutate(m)
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, m) })
		key := types.NamespacedName{Name: name, Namespace: ns}
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		m.Status.Phase = PhaseReady
		if !isPVCSource(source) {
			m.Status.CacheKey = computeCacheKey(source)
		}
		setCondition(m, ConditionAvailable, metav1.ConditionTrue, "Test", "ready")
		Expect(k8sClient.Status().Update(ctx, m)).To(Succeed())
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		return m
	}

	newService := func(name, modelName string, cache *inferencev1alpha1.ModelCacheSpec) *inferencev1alpha1.InferenceService {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: modelName, ModelCache: cache},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, isvc)
			_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
			_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		})
		return isvc
	}

	reconcileService := func(r *InferenceServiceReconciler, name string) *inferencev1alpha1.InferenceService {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		got := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, got)).To(Succeed())
		return got
	}

	// prefetchModel runs the Model controller's prefetch path on a fresh read.
	prefetchModel := func(r *ModelReconciler, name string) (*inferencev1alpha1.Model, bool, time.Duration) {
		m := &inferencev1alpha1.Model{}
		key := types.NamespacedName{Name: name, Namespace: ns}
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		handled, result, err := r.reconcilePrefetch(ctx, m)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		return m, handled, result.RequeueAfter
	}

	// prefetchUntilRunning drives the Model controller through a Job
	// replacement (delete, then create) until the claim's entry runs on a
	// new fingerprint.
	prefetchUntilRunning := func(r *ModelReconciler, name, claim, previousFingerprint string) {
		Eventually(func() bool {
			m, _, _ := prefetchModel(r, name)
			e := findPrefetchStatus(m, claim)
			return e != nil && e.Phase == inferencev1alpha1.PrefetchPhaseRunning && e.Fingerprint != previousFingerprint
		}, 10*time.Second, 200*time.Millisecond).Should(BeTrue())
	}

	BeforeEach(func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, nsObj))).To(Succeed())
	})

	It("holds the first rollout until the prefetch into its claim succeeds, then mounts the cache read-only with no downloader, and holds later rollouts the same way", func() {
		createCachePVC(ctx, ns, "svc-cache")
		model := readyModel("gated-model", "https://example.com/models/llama.gguf", func(m *inferencev1alpha1.Model) {
			m.Spec.PrefetchNodeSelector = map[string]string{"example.com/pool": "downloads"}
		})
		isvc := newService("gated", model.Name, &inferencev1alpha1.ModelCacheSpec{ClaimName: "svc-cache", Prefetch: true})
		mr, ir := modelReconciler(), isvcReconciler()
		depKey := types.NamespacedName{Name: isvc.Name, Namespace: ns}
		jobKey := types.NamespacedName{Name: prefetchTargetJobName(model, "svc-cache"), Namespace: ns}

		By("the Model controller starting a prefetch for the service's claim although the Model does not set spec.prefetch")
		model, handled, requeue := prefetchModel(mr, model.Name)
		Expect(handled).To(BeFalse(), "a service-requested prefetch does not hold the Model's Ready")
		Expect(requeue).To(Equal(prefetchPollInterval))
		Expect(model.Status.Phase).To(Equal(PhaseReady))
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
		Expect(cacheClaimOf(&job.Spec.Template.Spec)).To(Equal("svc-cache"))
		Expect(job.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("example.com/pool", "downloads"))
		entry := findPrefetchStatus(model, "svc-cache")
		Expect(entry).NotTo(BeNil())
		Expect(entry.Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))

		By("the service creating no Deployment while it runs")
		got := reconcileService(ir, isvc.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, depKey, &appsv1.Deployment{}))).To(BeTrue())
		Expect(got.Status.Phase).To(Equal("Pending"))
		Expect(got.Status.SchedulingStatus).To(Equal(ReasonWaitingForPrefetch))
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonWaitingForPrefetch))
		Expect(cond.Message).To(ContainSubstring("svc-cache"))

		By("rolling out onto the prefetched cache once the Job succeeds")
		markJobComplete(ctx, jobKey)
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(findPrefetchStatus(model, "svc-cache").Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
		got = reconcileService(ir, isvc.Name)
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
		pod := &dep.Spec.Template.Spec
		Expect(initContainerNames(pod)).To(Equal([]string{"model-verify"}), "no cache-prep and no downloader on a prefetched cache")
		Expect(envValue(pod.InitContainers[0].Env, "MODEL_FILES")).To(Equal(canonicalModelBasename(model)))
		Expect(envValue(pod.InitContainers[0].Env, "CACHE_DIR")).To(Equal("/models/" + model.Status.CacheKey))
		Expect(cacheClaimOf(pod)).To(Equal("svc-cache"))
		for _, v := range pod.Volumes {
			if v.Name == "model-cache" {
				Expect(v.PersistentVolumeClaim.ReadOnly).To(BeTrue(), "the serving pod only consumes the cache")
			}
		}
		Expect(pod.Containers[0].VolumeMounts).To(ContainElement(corev1.VolumeMount{Name: "model-cache", MountPath: "/models", ReadOnly: true}))
		cond = meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched)
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(ReasonPrefetchComplete))
		Expect(got.Status.SchedulingStatus).To(BeEmpty(), "the hold's diagnosis is cleared when the gate opens")
		hash1 := dep.Annotations[AnnotationDesiredTemplateHash]
		Expect(hash1).NotTo(BeEmpty())
		key1 := model.Status.CacheKey

		By("keeping the live Deployment on its template while a new prefetch for a changed source runs")
		fingerprint1 := findPrefetchStatus(model, "svc-cache").Fingerprint
		model.Spec.Source = "https://example.com/models/llama-v2.gguf"
		Expect(k8sClient.Update(ctx, model)).To(Succeed())
		prefetchUntilRunning(mr, model.Name, "svc-cache", fingerprint1)
		got = reconcileService(ir, isvc.Name)
		Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
		Expect(dep.Annotations[AnnotationDesiredTemplateHash]).To(Equal(hash1), "the old template keeps serving")
		Expect(envValue(dep.Spec.Template.Spec.InitContainers[0].Env, "CACHE_DIR")).To(Equal("/models/" + key1))
		cond = meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonWaitingForPrefetch))

		By("rolling once the new prefetch completes")
		markJobComplete(ctx, jobKey)
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(findPrefetchStatus(model, "svc-cache").Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
		Expect(model.Status.CacheKey).NotTo(Equal(key1))
		got = reconcileService(ir, isvc.Name)
		Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
		Expect(dep.Annotations[AnnotationDesiredTemplateHash]).NotTo(Equal(hash1))
		Expect(envValue(dep.Spec.Template.Spec.InitContainers[0].Env, "CACHE_DIR")).To(Equal("/models/" + model.Status.CacheKey))
		Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched).Status).To(Equal(metav1.ConditionTrue))
		hash2 := dep.Annotations[AnnotationDesiredTemplateHash]

		By("leaving the workload as it is when a later prefetch fails")
		fingerprint2 := findPrefetchStatus(model, "svc-cache").Fingerprint
		model.Spec.Source = "https://example.com/models/llama-v3.gguf"
		Expect(k8sClient.Update(ctx, model)).To(Succeed())
		prefetchUntilRunning(mr, model.Name, "svc-cache", fingerprint2)
		markJobFailed(ctx, jobKey)
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(findPrefetchStatus(model, "svc-cache").Phase).To(Equal(inferencev1alpha1.PrefetchPhaseFailed))
		got = reconcileService(ir, isvc.Name)
		Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
		Expect(dep.Annotations[AnnotationDesiredTemplateHash]).To(Equal(hash2), "a failed prefetch never rolls pods onto an unfilled cache")
		cond = meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonPrefetchFailed))
		Expect(cond.Message).To(ContainSubstring("failed"))
	})

	// A cache clear or a recreated claim empties what the status still
	// records as filled. The pods say so; the service turns that into a
	// re-stage by deleting the Job, once per episode.
	It("deletes the prefetch Job once when its pods report the staged files missing", func() {
		createCachePVC(ctx, ns, "wiped-cache")
		model := readyModel("wiped-model", "https://example.com/models/llama.gguf", nil)
		isvc := newService("wiped", model.Name, &inferencev1alpha1.ModelCacheSpec{ClaimName: "wiped-cache", Prefetch: true})
		mr := modelReconciler()
		ir := isvcReconciler()
		recorder := events.NewFakeRecorder(50)
		ir.Recorder = recorder
		jobKey := types.NamespacedName{Name: prefetchTargetJobName(model, "wiped-cache"), Namespace: ns}

		prefetchModel(mr, model.Name)
		markJobComplete(ctx, jobKey)
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(findPrefetchStatus(model, "wiped-cache").Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
		reconcileService(ir, isvc.Name)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: ns}, &appsv1.Deployment{})).To(Succeed())

		By("a serving pod failing model-verify on the emptied claim")
		// Status times have second resolution; the failure must be seen
		// after the staging completed.
		time.Sleep(1100 * time.Millisecond)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: isvc.Name + "-7f9c4", Namespace: ns,
				Labels: map[string]string{"app": isvc.Name, "inference.llmkube.dev/service": isvc.Name}},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: "model-verify", Image: "busybox"}},
				Containers:     []corev1.Container{{Name: "x", Image: "busybox"}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })
		pod.Status.Phase = corev1.PodPending
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name: "model-verify",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1,
				Message:  "ERROR: PrefetchedFilesMissing - /models/abc/llama.gguf is not on the cache claim.",
			}},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		got := reconcileService(ir, isvc.Name)
		transfer := meta.FindStatusCondition(got.Status.Conditions, ConditionModelTransferHealthy)
		Expect(transfer).NotTo(BeNil())
		Expect(transfer.Reason).To(Equal(ReasonPrefetchedFilesMissing))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &batchv1.Job{}))).To(BeTrue(),
			"the prefetch Job is deleted so the Model stages the claim again")
		Expect(drainEvents(recorder)).To(ContainElement(HavePrefix("Warning " + ReasonPrefetchRestaged)))

		By("the Model re-staging the claim with its Ready untouched")
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(model.Status.Phase).To(Equal(PhaseReady))
		entry := findPrefetchStatus(model, "wiped-cache")
		Expect(entry.Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
		Expect(entry.Message).To(ContainSubstring("re-verifying"))
		Expect(k8sClient.Get(ctx, jobKey, &batchv1.Job{})).To(Succeed())

		By("leaving the new Job alone for the same failure episode")
		markJobComplete(ctx, jobKey)
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(findPrefetchStatus(model, "wiped-cache").Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
		reconcileService(ir, isvc.Name)
		Expect(k8sClient.Get(ctx, jobKey, &batchv1.Job{})).To(Succeed(),
			"a failure observed before the re-stage completed does not trigger another")
	})

	It("reports PrefetchFailed on a first rollout and creates nothing", func() {
		createCachePVC(ctx, ns, "fail-cache")
		model := readyModel("fail-first-model", "https://example.com/models/llama.gguf", nil)
		isvc := newService("fail-first", model.Name, &inferencev1alpha1.ModelCacheSpec{ClaimName: "fail-cache", Prefetch: true})
		mr, ir := modelReconciler(), isvcReconciler()
		jobKey := types.NamespacedName{Name: prefetchTargetJobName(model, "fail-cache"), Namespace: ns}

		prefetchModel(mr, model.Name)
		markJobFailed(ctx, jobKey)
		model, _, _ = prefetchModel(mr, model.Name)
		Expect(findPrefetchStatus(model, "fail-cache").Phase).To(Equal(inferencev1alpha1.PrefetchPhaseFailed))

		got := reconcileService(ir, isvc.Name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: ns}, &appsv1.Deployment{}))).To(BeTrue())
		Expect(got.Status.Phase).To(Equal(PhaseFailed))
		Expect(got.Status.SchedulingStatus).To(Equal(ReasonPrefetchFailed))
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(ReasonPrefetchFailed))
	})

	It("inherits the Model's modelCache on a service without its own, in memory only", func() {
		createCachePVC(ctx, ns, "model-cache")
		model := readyModel("inherit-model", "https://example.com/models/llama.gguf", func(m *inferencev1alpha1.Model) {
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "model-cache"}
		})
		isvc := newService("inherit", model.Name, nil)

		reconcileService(isvcReconciler(), isvc.Name)
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: ns}, dep)).To(Succeed())
		Expect(cacheClaimOf(&dep.Spec.Template.Spec)).To(Equal("model-cache"), "the claim is declared once, on the Model")
		Expect(initContainerNames(&dep.Spec.Template.Spec)).To(ContainElement("model-downloader"), "no prefetch was asked for, so the pod downloads as before")

		stored := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: ns}, stored)).To(Succeed())
		Expect(stored.Spec.ModelCache).To(BeNil(), "inheritance is never written back to the service's spec")
	})

	It("does not hold a service whose Model source is not prefetchable", func() {
		model := readyModel("pvc-model", "pvc://staged-models/llama.gguf", nil)
		isvc := newService("pvc-prefetch", model.Name, &inferencev1alpha1.ModelCacheSpec{Prefetch: true})

		got := reconcileService(isvcReconciler(), isvc.Name)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: ns}, &appsv1.Deployment{})).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionModelPrefetched)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonPrefetchNotApplicable))
	})

	It("rejects modelCache.prefetch with persistence Ephemeral at admission", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "ephemeral-prefetch", Namespace: ns},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:   "any",
				ModelCache: &inferencev1alpha1.ModelCacheSpec{Persistence: inferencev1alpha1.ModelCachePersistenceEphemeral, Prefetch: true},
			},
		}
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("prefetch cannot be set when persistence is Ephemeral"))
	})
})

var _ = Describe("buildPrefetchedStorageConfig", func() {
	multiFileModel := func(generation int64) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "glm", Namespace: "default", Generation: generation},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "hf://org/repo",
				Format: "safetensors",
				Files:  []string{"config.json", "*.safetensors"},
			},
			Status: inferencev1alpha1.ModelStatus{
				Phase:       PhaseReady,
				StagedFiles: []string{"config.json", "a.safetensors", "b.safetensors"},
				Conditions:  []metav1.Condition{{Type: ConditionAvailable, Status: metav1.ConditionTrue, ObservedGeneration: 2, Reason: "x"}},
			},
		}
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "glm", ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "svc-cache", Prefetch: true}},
	}

	It("mounts the claim read-only and verifies the staged file set instead of downloading", func() {
		model := multiFileModel(2)
		cfg := buildPrefetchedStorageConfig(model, isvc, ModelCacheModeShared, "curl:8.18.0")
		key := effectiveModelCacheKey(model)
		Expect(cfg.modelPath).To(Equal("/models/" + key + "/config.json"))
		Expect(cfg.stagedDir).To(Equal("/models/" + key))
		Expect(cfg.volumes).To(HaveLen(1))
		Expect(cfg.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("svc-cache"))
		Expect(cfg.volumes[0].PersistentVolumeClaim.ReadOnly).To(BeTrue())
		Expect(cfg.volumeMounts).To(ConsistOf(corev1.VolumeMount{Name: "model-cache", MountPath: "/models", ReadOnly: true}))
		Expect(cfg.initContainers).To(HaveLen(1))
		verify := cfg.initContainers[0]
		Expect(verify.Name).To(Equal("model-verify"))
		Expect(verify.Image).To(Equal("curl:8.18.0"))
		Expect(envValue(verify.Env, "MODEL_FILES")).To(Equal("config.json\na.safetensors\nb.safetensors"))
		Expect(envValue(verify.Env, "CACHE_DIR")).To(Equal("/models/" + key))
		Expect(strings.Join(verify.Command, " ")).To(ContainSubstring("PrefetchedFilesMissing"))
		Expect(strings.Join(verify.Command, " ")).NotTo(ContainSubstring("curl"), "a verify step downloads nothing")
		Expect(verify.VolumeMounts).To(ConsistOf(corev1.VolumeMount{Name: "model-cache", MountPath: "/models", ReadOnly: true}))
		Expect(verify.TerminationMessagePolicy).To(Equal(corev1.TerminationMessageFallbackToLogsOnError))
	})

	It("fails closed when the glob expansion is not current for the Model's generation", func() {
		cfg := buildPrefetchedStorageConfig(multiFileModel(3), isvc, ModelCacheModeShared, "curl:8.18.0")
		Expect(cfg.initContainers).To(HaveLen(1))
		Expect(strings.Join(cfg.initContainers[0].Command, " ")).To(ContainSubstring("StagedFilesNotCurrent"))
		Expect(cfg.volumes[0].PersistentVolumeClaim.ReadOnly).To(BeTrue())
	})

	It("verifies the canonical basename of a single-file model", func() {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "llama", Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/models/llama.gguf"},
			Status:     inferencev1alpha1.ModelStatus{CacheKey: "abc123"},
		}
		cfg := buildPrefetchedStorageConfig(model, nil, ModelCacheModeShared, "curl:8.18.0")
		Expect(cfg.modelPath).To(Equal("/models/abc123/" + canonicalModelBasename(model)))
		Expect(cfg.stagedDir).To(BeEmpty())
		Expect(cfg.volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(ModelCachePVCName))
		Expect(envValue(cfg.initContainers[0].Env, "MODEL_FILES")).To(Equal(canonicalModelBasename(model)))
	})
})

var _ = Describe("warnPrefetchTargetMismatch", func() {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "prefetched", Namespace: "default"},
		Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/models/llama.gguf", Prefetch: true},
		Status: inferencev1alpha1.ModelStatus{
			Phase:    PhaseReady,
			CacheKey: "abc123",
			Prefetch: []inferencev1alpha1.PrefetchStatus{{ClaimName: ModelCachePVCName, Phase: inferencev1alpha1.PrefetchPhaseSucceeded}},
		},
	}
	service := func(cache *inferencev1alpha1.ModelCacheSpec) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name, ModelCache: cache},
		}
	}
	newReconciler := func() (*InferenceServiceReconciler, *events.FakeRecorder) {
		recorder := events.NewFakeRecorder(10)
		return &InferenceServiceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder,
			ModelCachePath: "/models", ModelCacheMode: ModelCacheModeShared}, recorder
	}

	It("warns a service that mounts a claim its prefetched Model was not staged into", func() {
		r, recorder := newReconciler()
		r.warnPrefetchTargetMismatch(service(&inferencev1alpha1.ModelCacheSpec{ClaimName: "elsewhere"}), model)
		Expect(drainEvents(recorder)).To(ContainElement(HavePrefix("Warning " + ReasonPrefetchTargetMismatch)))
	})

	It("stays quiet for a service on the prefetched claim, or one that asks for its own prefetch", func() {
		r, recorder := newReconciler()
		r.warnPrefetchTargetMismatch(service(nil), model)
		r.warnPrefetchTargetMismatch(service(&inferencev1alpha1.ModelCacheSpec{ClaimName: "elsewhere", Prefetch: true}), model)
		Expect(drainEvents(recorder)).To(BeEmpty())
	})
})
