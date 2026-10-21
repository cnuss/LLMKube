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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

var _ = Describe("Model Prefetch", func() {
	ctx := context.Background()
	const ns = "prefetch-test"

	prefetchReconciler := func() *ModelReconciler {
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

	newPrefetchModel := func(name string) *inferencev1alpha1.Model {
		return &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://example.com/models/llama.gguf",
				Prefetch: true,
			},
		}
	}

	createPVC := func(name string) { createCachePVC(ctx, ns, name) }
	completeJob := func(key types.NamespacedName) { markJobComplete(ctx, key) }
	failJob := func(key types.NamespacedName) { markJobFailed(ctx, key) }
	jobClaim := func(job *batchv1.Job) string { return cacheClaimOf(&job.Spec.Template.Spec) }

	BeforeEach(func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		err := k8sClient.Create(ctx, nsObj)
		if err != nil {
			Expect(client.IgnoreAlreadyExists(err)).To(Succeed())
		}
	})

	Describe("prefetchEligible", func() {
		It("is false when the field is unset", func() {
			m := newPrefetchModel("x")
			m.Spec.Prefetch = false
			Expect(prefetchEligible(m)).To(BeFalse())
		})

		It("is false for pvc:// sources even with prefetch set", func() {
			m := newPrefetchModel("x")
			m.Spec.Source = "pvc://some-pvc/model.gguf"
			Expect(prefetchEligible(m)).To(BeFalse())
		})

		It("is true for hf:// sources (normalized to an https resolve URL, so prefetchable)", func() {
			m := newPrefetchModel("x")
			m.Spec.Source = "hf://org/repo"
			Expect(prefetchEligible(m)).To(BeTrue())
		})

		It("is true for https sources", func() {
			Expect(prefetchEligible(newPrefetchModel("x"))).To(BeTrue())
		})

		// #2136: the prefetch path expands the glob from the repository
		// listing before building the Job, so a glob no longer disables it.
		It("is true with a glob in spec.files", func() {
			m := newPrefetchModel("x")
			m.Spec.Source = "hf://org/repo"
			m.Spec.Files = []string{"config.json", "*.safetensors"}
			Expect(prefetchEligible(m)).To(BeTrue())
		})

		It("is true when only modelCache.prefetch, the alias, is set", func() {
			m := newPrefetchModel("x")
			m.Spec.Prefetch = false
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{Prefetch: true}
			Expect(prefetchEligible(m)).To(BeTrue())
		})
	})

	Describe("buildPrefetchJob scheduling", func() {
		// #1621: a fleet whose GPU nodes are tainted needs the prefetch Job
		// to tolerate them, because the shared cache can live on node-local
		// storage pinned to exactly those nodes. The Model carries its own
		// tolerations: inheriting from an InferenceService is ill-defined
		// here, since prefetch runs before the first one exists by design.
		It("carries the Model's tolerations onto the Job pod", func() {
			m := newPrefetchModel("tolerated")
			m.Spec.PrefetchTolerations = []corev1.Toleration{{
				Key:      "nvidia.com/gpu",
				Operator: corev1.TolerationOpEqual,
				Value:    "present",
				Effect:   corev1.TaintEffectNoSchedule,
			}}
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.Tolerations).To(HaveLen(1))
			Expect(job.Spec.Template.Spec.Tolerations[0].Key).To(Equal("nvidia.com/gpu"))
		})

		It("emits no tolerations when the Model declares none", func() {
			m := newPrefetchModel("plain")
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.Tolerations).To(BeEmpty())
		})

		// #1964: a node-pinned claim (#1676) binds wherever the prefetch pod
		// schedules, so the prefetch placement has to be steerable.
		It("carries the Model's nodeSelector onto the Job pod", func() {
			m := newPrefetchModel("node-selected")
			m.Spec.PrefetchNodeSelector = map[string]string{"example.com/node-pool": "gpu"}
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("example.com/node-pool", "gpu"))
		})

		It("emits no node selector when the Model declares none", func() {
			m := newPrefetchModel("plain-placement")
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.NodeSelector).To(BeEmpty())
		})

		// #2136: a selector cannot say "not on a GPU node"; an affinity can.
		It("carries the Model's prefetchAffinity onto the Job pod", func() {
			m := newPrefetchModel("affine")
			m.Spec.PrefetchAffinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "nvidia.com/gpu.present", Operator: corev1.NodeSelectorOpDoesNotExist,
					}}}},
				},
			}}
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.Affinity).To(Equal(m.Spec.PrefetchAffinity))
		})

		It("emits no affinity when the Model declares none", func() {
			m := newPrefetchModel("plain-affinity")
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.Affinity).To(BeNil())
		})

		It("stamps the fingerprint and claim it was built for", func() {
			m := newPrefetchModel("stamped")
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "node-a-cache", "abc123")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Annotations).To(HaveKeyWithValue(annotationPrefetchFingerprint, "abc123"))
			Expect(job.Annotations).To(HaveKeyWithValue(annotationPrefetchClaim, "node-a-cache"))
			Expect(jobClaim(job)).To(Equal("node-a-cache"))
		})
	})

	// #2058: the prefetch Job mounts the same shared cache as the serving pods,
	// so it gets the same fsGroupChangePolicy whenever it gets an fsGroup.
	Describe("buildPrefetchJob security context", func() {
		It("sets fsGroupChangePolicy OnRootMismatch alongside the default fsGroup", func() {
			m := newPrefetchModel("fsgroup-policy")
			seedPrefetchCacheKey(m)
			job, err := prefetchReconciler().buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			psc := job.Spec.Template.Spec.SecurityContext
			Expect(psc).NotTo(BeNil())
			Expect(psc.FSGroup).To(HaveValue(Equal(int64(102))))
			Expect(psc.FSGroupChangePolicy).To(HaveValue(Equal(corev1.FSGroupChangeOnRootMismatch)))
		})

		It("sets no pod security context when the default fsGroup is disabled", func() {
			m := newPrefetchModel("fsgroup-disabled")
			seedPrefetchCacheKey(m)
			r := prefetchReconciler()
			r.DefaultFSGroup = 0
			job, err := r.buildPrefetchJob(context.Background(), m, "", "")
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.SecurityContext).To(BeNil())
		})
	})

	Describe("reconcilePrefetch", func() {
		It("does not handle models without prefetch", func() {
			m := newPrefetchModel("no-prefetch")
			m.Spec.Prefetch = false
			handled, _, err := prefetchReconciler().reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse())
		})

		It("creates the Job and shared cache PVC, then completes on Job success", func() {
			model := newPrefetchModel("model-prefetch-flow")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			r := prefetchReconciler()

			// First pass: creates PVC + Job, sets Downloading.
			handled, result, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(15 * time.Second))

			job := &batchv1.Job{}
			jobKey := types.NamespacedName{Name: "model-prefetch-flow-prefetch", Namespace: ns}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			Expect(job.OwnerReferences).To(HaveLen(1))
			Expect(job.OwnerReferences[0].Kind).To(Equal("Model"))
			Expect(job.Spec.Template.Spec.InitContainers).NotTo(BeEmpty())
			Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))

			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ModelCachePVCName, Namespace: ns}, pvc)).To(Succeed())

			updated := &inferencev1alpha1.Model{}
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseDownloading))
			Expect(updated.Status.CacheKey).NotTo(BeEmpty())
			Expect(updated.Status.Prefetch).To(HaveLen(1))
			Expect(updated.Status.Prefetch[0].ClaimName).To(Equal(ModelCachePVCName))
			Expect(updated.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
			Expect(updated.Status.Prefetch[0].JobName).To(Equal(jobKey.Name))
			Expect(updated.Status.Prefetch[0].CacheKey).To(Equal(updated.Status.CacheKey))

			// Second pass with the Job still running: no duplicate, polls again.
			handled, result, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(15 * time.Second))

			// Mark the Job complete; next pass promotes the Model to Ready.
			completeJob(jobKey)

			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			handled, _, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseReady))
			Expect(updated.Status.CacheKey).To(Equal(effectiveModelCacheKey(updated)))
			Expect(updated.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
			Expect(updated.Status.Prefetch[0].ObservedGeneration).To(Equal(updated.Generation))
			available := meta.FindStatusCondition(updated.Status.Conditions, ConditionAvailable)
			Expect(available).NotTo(BeNil())
			Expect(available.ObservedGeneration).To(Equal(updated.Generation))

			// Once Ready with a cache key, prefetch steps aside for the
			// ordinary remote-source reconcile.
			handled, _, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse())
		})

		// Pins that the new fields survive the API server round trip (CRD schema +
		// DeepCopy) and land on the created Job, not just the built object.
		It("persists prefetchNodeSelector and prefetchAffinity onto the created Job", func() {
			model := newPrefetchModel("model-prefetch-selector")
			model.Spec.PrefetchNodeSelector = map[string]string{"kubernetes.io/hostname": "worker-1"}
			model.Spec.PrefetchAffinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "nvidia.com/gpu.present", Operator: corev1.NodeSelectorOpDoesNotExist,
					}}}},
				},
			}}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			handled, _, err := prefetchReconciler().reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "model-prefetch-selector-prefetch", Namespace: ns}, job)).To(Succeed())
			Expect(job.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("kubernetes.io/hostname", "worker-1"))
			Expect(job.Spec.Template.Spec.Affinity).NotTo(BeNil())
			terms := job.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
			Expect(terms).To(HaveLen(1))
			Expect(terms[0].MatchExpressions[0].Key).To(Equal("nvidia.com/gpu.present"))
			Expect(terms[0].MatchExpressions[0].Operator).To(Equal(corev1.NodeSelectorOpDoesNotExist))
		})

		It("marks the Model Failed when the Job fails", func() {
			model := newPrefetchModel("model-prefetch-fail")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			r := prefetchReconciler()
			handled, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			failJob(types.NamespacedName{Name: "model-prefetch-fail-prefetch", Namespace: ns})

			updated := &inferencev1alpha1.Model{}
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			handled, _, err = r.reconcilePrefetch(ctx, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			Expect(k8sClient.Get(ctx, modelKey, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseFailed))
			Expect(updated.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseFailed))
			progressing := meta.FindStatusCondition(updated.Status.Conditions, ConditionProgressing)
			Expect(progressing).NotTo(BeNil())
			Expect(progressing.Reason).To(Equal(ReasonPrefetchFailed))
		})

		// A placement that keeps the download off the GPU nodes cannot reach a
		// claim that lives on one of them. The Job then never completes and
		// never fails, so the status must say why instead of "downloading".
		It("names the scheduler's reason while the Job's pod cannot be scheduled", func() {
			model := newPrefetchModel("model-prefetch-unschedulable")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			jobName := prefetchJobName(model)
			r := prefetchReconciler()
			recorder := events.NewFakeRecorder(20)
			r.Recorder = recorder

			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())

			// envtest runs no Job controller or scheduler: stand in for both.
			const schedulerMsg = "0/2 nodes are available: 1 node(s) had volume node affinity conflict, 1 node(s) didn't match Pod's node affinity/selector."
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: jobName + "-abcde", Namespace: ns, Labels: map[string]string{"batch.kubernetes.io/job-name": jobName}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "x", Image: "busybox"}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, pod) }()
			pod.Status.Phase = corev1.PodPending
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: schedulerMsg}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseDownloading))
			Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("cannot be scheduled"))
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("volume node affinity conflict"))
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("delete the Job to retry"))
			progressing := meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing)
			Expect(progressing).NotTo(BeNil())
			Expect(progressing.Reason).To(Equal(ReasonPrefetchUnschedulable))
			Expect(progressing.Message).To(ContainSubstring("volume node affinity conflict"))
			Expect(drainEvents(recorder)).To(ContainElement(HavePrefix("Warning " + ReasonPrefetchUnschedulable)))

			By("clearing once the pod schedules")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: ns}, pod)).To(Succeed())
			pod.Status.Phase = corev1.PodRunning
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Prefetch[0].Message).NotTo(ContainSubstring("cannot be scheduled"))
			progressing = meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing)
			Expect(progressing.Reason).To(Equal(ReasonPrefetchRunning))
		})

		// The status records that a claim was filled; it cannot see the claim.
		// A Job that is gone (its TTL, or a deliberate delete to retry) is
		// run again as a check, with the Model's Ready left standing.
		It("re-verifies the claim when its Job is gone, without leaving Ready", func() {
			model := newPrefetchModel("model-prefetch-reverify")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchJobName(model), Namespace: ns}
			r := prefetchReconciler()

			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			completeJob(jobKey)
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseReady))

			By("the Job going away")
			Expect(k8sClient.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: ns}},
				client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &batchv1.Job{}))
			}, 10*time.Second, 200*time.Millisecond).Should(BeTrue())

			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			handled, result, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(prefetchPollInterval))
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseReady), "a re-verification does not take Ready away")
			Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("re-verifying"))
			progressing := meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing)
			Expect(progressing).NotTo(BeNil())
			Expect(progressing.Status).To(Equal(metav1.ConditionTrue))
			Expect(progressing.Reason).To(Equal(ReasonPrefetchReverifying))
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			Expect(job.Annotations).To(HaveKeyWithValue(annotationPrefetchReverify, "true"))

			By("a later pass keeping the re-verification context")
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseReady))
			Expect(meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing).Reason).To(Equal(ReasonPrefetchReverifying))

			By("the check completing")
			completeJob(jobKey)
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			handled, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseReady))
			Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
			progressing = meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing)
			Expect(progressing.Status).To(Equal(metav1.ConditionFalse))
			Expect(progressing.Reason).To(Equal(ReasonPrefetchComplete))
		})

		// The Job name is deterministic, so a user's Job can sit at it. It is
		// neither adopted as the prefetch nor deleted to make room.
		It("neither adopts nor deletes a Job at the prefetch name that the Model does not own", func() {
			model := newPrefetchModel("model-prefetch-foreign")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			jobKey := types.NamespacedName{Name: prefetchJobName(model), Namespace: ns}
			foreign := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: ns},
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "x", Image: "busybox"}},
				}}},
			}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, foreign) }()
			// Complete it, so adopting it would wrongly mark the Model Ready.
			completeJob(jobKey)

			r := prefetchReconciler()
			for i := 0; i < 2; i++ {
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: model.Name, Namespace: ns}, model)).To(Succeed())
				_, _, err := r.reconcilePrefetch(ctx, model)
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: model.Name, Namespace: ns}, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseFailed))
			Expect(model.Status.Prefetch[0].Message).To(ContainSubstring("not owned by this Model"))
			still := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, still)).To(Succeed())
			Expect(still.UID).To(Equal(foreign.UID), "the foreign Job is left alone")
		})

		// #2136: a prefetch is keyed by what it staged. A source change needs
		// a new download; a placement change does not.
		It("replaces a completed Job when the spec changes what is staged", func() {
			model := newPrefetchModel("model-prefetch-respec")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchJobName(model), Namespace: ns}
			r := prefetchReconciler()

			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			completeJob(jobKey)
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseReady))
			oldFingerprint := model.Status.Prefetch[0].Fingerprint
			oldKey := model.Status.CacheKey

			By("changing the source")
			model.Spec.Source = "https://example.com/models/llama-v2.gguf"
			Expect(k8sClient.Update(ctx, model)).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())

			handled, result, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(prefetchRetryInterval))
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).NotTo(Equal(PhaseReady), "a Model whose source changed is not Ready on the old files")
			Expect(model.Status.CacheKey).NotTo(Equal(oldKey), "the key moves with the source")

			By("creating a new Job for the new source once the old one is gone")
			Eventually(func() error {
				Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
				_, _, err := r.reconcilePrefetch(ctx, model)
				if err != nil {
					return err
				}
				job := &batchv1.Job{}
				if err := k8sClient.Get(ctx, jobKey, job); err != nil {
					return err
				}
				if job.DeletionTimestamp != nil {
					return apierrors.NewNotFound(batchv1.Resource("jobs"), job.Name)
				}
				if job.Annotations[annotationPrefetchFingerprint] == oldFingerprint {
					return apierrors.NewConflict(batchv1.Resource("jobs"), job.Name, nil)
				}
				return nil
			}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseDownloading))
			Expect(model.Status.Prefetch).To(HaveLen(1))
			Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
			Expect(model.Status.Prefetch[0].Fingerprint).NotTo(Equal(oldFingerprint))
		})

		It("re-records the generation without a new Job when the change does not alter the files", func() {
			model := newPrefetchModel("model-prefetch-regen")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchJobName(model), Namespace: ns}
			r := prefetchReconciler()

			_, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			completeJob(jobKey)
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			generationBefore := model.Generation
			jobBefore := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, jobBefore)).To(Succeed())

			model.Spec.PrefetchTolerations = []corev1.Toleration{{Key: "downloads", Operator: corev1.TolerationOpExists}}
			Expect(k8sClient.Update(ctx, model)).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Generation).To(BeNumerically(">", generationBefore))

			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal(PhaseReady))
			Expect(model.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
			Expect(model.Status.Prefetch[0].ObservedGeneration).To(Equal(model.Generation))
			available := meta.FindStatusCondition(model.Status.Conditions, ConditionAvailable)
			Expect(available.ObservedGeneration).To(Equal(model.Generation))
			jobAfter := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, jobAfter)).To(Succeed())
			Expect(jobAfter.UID).To(Equal(jobBefore.UID), "the completed Job is kept, not replaced")
		})
	})

	Describe("Reconcile integration", func() {
		It("intercepts a prefetch model before the remote download path", func() {
			model := newPrefetchModel("model-prefetch-hook")
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			result, err := prefetchReconciler().Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: model.Name, Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(15 * time.Second))

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "model-prefetch-hook-prefetch", Namespace: ns}, job)).To(Succeed())

			updated := &inferencev1alpha1.Model{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: model.Name, Namespace: ns}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseDownloading))
		})
	})

	Describe("PrefetchSkipped", func() {
		It("reports spec.prefetch on a source the downloader does not stage, and clears it when the field goes", func() {
			model := newPrefetchModel("model-prefetch-skipped")
			model.Spec.Source = "pvc://staged-models/llama.gguf"
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()
			modelKey := types.NamespacedName{Name: model.Name, Namespace: ns}
			r := prefetchReconciler()
			recorder := events.NewFakeRecorder(20)
			r.Recorder = recorder

			handled, _, err := r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse(), "an unstageable source is left to its own reconcile")

			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			cond := meta.FindStatusCondition(model.Status.Conditions, ConditionPrefetchSkipped)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal(ReasonSourceNotPrefetchable))
			Expect(cond.Message).To(ContainSubstring("pvc://staged-models/llama.gguf"))
			Expect(drainEvents(recorder)).To(ContainElement(HavePrefix("Warning " + ConditionPrefetchSkipped)))
			var jobs batchv1.JobList
			Expect(k8sClient.List(ctx, &jobs, client.InNamespace(ns), client.MatchingLabels{"inference.llmkube.dev/model": model.Name})).To(Succeed())
			Expect(jobs.Items).To(BeEmpty())

			model.Spec.Prefetch = false
			Expect(k8sClient.Update(ctx, model)).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, model)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, model)).To(Succeed())
			Expect(meta.FindStatusCondition(model.Status.Conditions, ConditionPrefetchSkipped)).To(BeNil())
		})
	})

	Describe("#1676 per-service cache claim", func() {
		// Prefetch must stage into the PVC the InferenceService will actually
		// read, not always the shared cache.

		newIsvc := func(modelName, name, claim string) *inferencev1alpha1.InferenceService {
			return &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: modelName,
					ModelCache: &inferencev1alpha1.ModelCacheSpec{
						ClaimName: claim,
					},
				},
			}
		}

		// cleanupNamespacedObjects drains the InferenceServices and PVCs this
		// block creates. It runs BOTH before each case (so a previous case
		// cannot skew a resolution) and after the block (so these objects do
		// not leak into the shared envtest cluster). The AfterEach is
		// load-bearing: federation_edge_controller.go lists InferenceServices
		// across ALL namespaces, and its spec asserts absolute counts, so a
		// leaked service here fails an unrelated test.
		cleanupNamespacedObjects := func() {
			list := &inferencev1alpha1.InferenceServiceList{}
			Expect(k8sClient.List(ctx, list, client.InNamespace(ns))).To(Succeed())
			for i := range list.Items {
				_ = k8sClient.Delete(ctx, &list.Items[i])
			}
			pvcList := &corev1.PersistentVolumeClaimList{}
			Expect(k8sClient.List(ctx, pvcList, client.InNamespace(ns))).To(Succeed())
			for i := range pvcList.Items {
				_ = k8sClient.Delete(ctx, &pvcList.Items[i])
			}
		}

		AfterEach(cleanupNamespacedObjects)
		BeforeEach(cleanupNamespacedObjects)

		It("stages into the single referencing service's claim", func() {
			createPVC("llmkube-node-a-cache")
			Expect(k8sClient.Create(ctx, newIsvc("per-service-model", "svc-a", "llmkube-node-a-cache"))).To(Succeed())
			m := newPrefetchModel("per-service-model")
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()

			handled, _, err := prefetchReconciler().reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: prefetchJobName(m), Namespace: ns}, job)).To(Succeed())
			Expect(jobClaim(job)).To(Equal("llmkube-node-a-cache"))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: ns}, m)).To(Succeed())
			Expect(m.Status.Prefetch).To(HaveLen(1))
			Expect(m.Status.Prefetch[0].ClaimName).To(Equal("llmkube-node-a-cache"))
		})

		It("resolves the single distinct claim among referencing services", func() {
			r := prefetchReconciler()
			m := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "resolve-model", Namespace: ns}}

			Expect(k8sClient.Create(ctx, newIsvc("resolve-model", "svc-a", "llmkube-cache-a"))).To(Succeed())
			services, err := r.referencingServices(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			claim, reason := resolveOwnPrefetchClaim(m, services, nil, "")
			Expect(reason).To(BeEmpty())
			Expect(claim).To(Equal("llmkube-cache-a"))
		})

		It("falls back to the shared cache when services disagree on claimName", func() {
			r := prefetchReconciler()
			m := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "conflict-model", Namespace: ns}}

			Expect(k8sClient.Create(ctx, newIsvc("conflict-model", "svc-a", "llmkube-cache-a"))).To(Succeed())
			Expect(k8sClient.Create(ctx, newIsvc("conflict-model", "svc-b", "llmkube-cache-b"))).To(Succeed())

			services, err := r.referencingServices(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			claim, reason := resolveOwnPrefetchClaim(m, services, nil, "")
			Expect(claim).To(Equal(ModelCachePVCName))
			Expect(reason).NotTo(BeEmpty())
		})

		It("falls back to the shared cache when no service references the model", func() {
			r := prefetchReconciler()
			m := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "no-svc-model", Namespace: ns}}

			// A service that references a different model must not match.
			Expect(k8sClient.Create(ctx, newIsvc("other-model", "svc-other", "llmkube-cache-a"))).To(Succeed())

			services, err := r.referencingServices(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(services).To(BeEmpty())
			claim, reason := resolveOwnPrefetchClaim(m, services, nil, "")
			Expect(claim).To(Equal(ModelCachePVCName))
			Expect(reason).NotTo(BeEmpty())
		})

		// #2136: once the download has landed, a service that appears later
		// is reported, not re-downloaded for. Its own modelCache.prefetch is
		// the way to ask for that.
		It("keeps the claim already staged when a service appears afterwards, and reports the mismatch", func() {
			m := newPrefetchModel("late-service-model")
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()
			modelKey := types.NamespacedName{Name: m.Name, Namespace: ns}
			r := prefetchReconciler()

			_, _, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			completeJob(types.NamespacedName{Name: prefetchJobName(m), Namespace: ns})
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseReady))

			By("a service naming another claim appearing")
			late := newIsvc(m.Name, "svc-late", "llmkube-cache-late")
			Expect(k8sClient.Create(ctx, late)).To(Succeed())
			handled, _, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse(), "the Model stays Ready on the claim it was staged into")
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Prefetch).To(HaveLen(1))
			Expect(m.Status.Prefetch[0].ClaimName).To(Equal(ModelCachePVCName))
			cond := meta.FindStatusCondition(m.Status.Conditions, ConditionPrefetchTargetMismatch)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Message).To(ContainSubstring("svc-late"))
			Expect(cond.Message).To(ContainSubstring("llmkube-cache-late"))
			var jobs batchv1.JobList
			Expect(k8sClient.List(ctx, &jobs, client.InNamespace(ns), client.MatchingLabels{"inference.llmkube.dev/model": m.Name})).To(Succeed())
			Expect(jobs.Items).To(HaveLen(1), "no second download for a claim nobody asked to prefetch into")

			By("the service asking for its own prefetch")
			createPVC("llmkube-cache-late")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: late.Name, Namespace: ns}, late)).To(Succeed())
			late.Spec.ModelCache.Prefetch = true
			Expect(k8sClient.Update(ctx, late)).To(Succeed())
			handled, result, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse())
			Expect(result.RequeueAfter).To(Equal(prefetchPollInterval), "a service-requested prefetch is polled without holding the Model")
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseReady))
			Expect(meta.FindStatusCondition(m.Status.Conditions, ConditionPrefetchTargetMismatch)).To(BeNil())
			Expect(m.Status.Prefetch).To(HaveLen(2))
			lateEntry := findPrefetchStatus(m, "llmkube-cache-late")
			Expect(lateEntry).NotTo(BeNil())
			Expect(lateEntry.Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: prefetchTargetJobName(m, "llmkube-cache-late"), Namespace: ns}, job)).To(Succeed())
			Expect(jobClaim(job)).To(Equal("llmkube-cache-late"))
			Expect(job.Name).To(Equal(lateEntry.JobName))
		})

		// #2136: when a Model and its service are applied together, the Model
		// can reconcile first and start into the shared cache. The service's
		// claim then moves the Job rather than leaving the download where no
		// pod will read it.
		It("retargets a running Job when the service that will read it appears", func() {
			m := newPrefetchModel("retarget-model")
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()
			modelKey := types.NamespacedName{Name: m.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchJobName(m), Namespace: ns}
			r := prefetchReconciler()

			_, _, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			Expect(jobClaim(job)).To(Equal(ModelCachePVCName))

			createPVC("llmkube-node-a-cache")
			Expect(k8sClient.Create(ctx, newIsvc(m.Name, "svc-a", "llmkube-node-a-cache"))).To(Succeed())

			Eventually(func() string {
				Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
				_, _, err := r.reconcilePrefetch(ctx, m)
				Expect(err).NotTo(HaveOccurred())
				job := &batchv1.Job{}
				if err := k8sClient.Get(ctx, jobKey, job); err != nil || job.DeletionTimestamp != nil {
					return ""
				}
				return jobClaim(job)
			}, 10*time.Second, 200*time.Millisecond).Should(Equal("llmkube-node-a-cache"))

			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseDownloading))
			Expect(m.Status.Prefetch).To(HaveLen(1), "the abandoned shared-cache entry is dropped")
			Expect(m.Status.Prefetch[0].ClaimName).To(Equal("llmkube-node-a-cache"))
			Expect(m.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
		})
	})

	Describe("#2136 spec.modelCache on the Model", func() {
		AfterEach(func() {
			pvcList := &corev1.PersistentVolumeClaimList{}
			Expect(k8sClient.List(ctx, pvcList, client.InNamespace(ns))).To(Succeed())
			for i := range pvcList.Items {
				_ = k8sClient.Delete(ctx, &pvcList.Items[i])
			}
		})

		It("stages into spec.modelCache.claimName with no InferenceService present", func() {
			createPVC("model-cache")
			m := newPrefetchModel("explicit-claim-model")
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "model-cache"}
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()

			handled, result, err := prefetchReconciler().reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(prefetchPollInterval))

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: prefetchJobName(m), Namespace: ns}, job)).To(Succeed())
			Expect(jobClaim(job)).To(Equal("model-cache"))
			Expect(job.Annotations).To(HaveKeyWithValue(annotationPrefetchClaim, "model-cache"))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: ns}, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseDownloading))
			Expect(m.Status.Prefetch).To(HaveLen(1))
			Expect(m.Status.Prefetch[0].ClaimName).To(Equal("model-cache"))
		})

		It("holds the Job while the named claim does not exist, and starts it once it does", func() {
			m := newPrefetchModel("missing-claim-model")
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "not-yet-created"}
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()
			modelKey := types.NamespacedName{Name: m.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchJobName(m), Namespace: ns}
			r := prefetchReconciler()

			handled, result, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(result.RequeueAfter).To(Equal(prefetchClaimWait))
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &batchv1.Job{}))).To(BeTrue(),
				"a Job pod mounting a claim that does not exist sits Pending forever")
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal("Pending"))
			progressing := meta.FindStatusCondition(m.Status.Conditions, ConditionProgressing)
			Expect(progressing).NotTo(BeNil())
			Expect(progressing.Reason).To(Equal(ReasonPrefetchClaimNotFound))
			Expect(progressing.Message).To(ContainSubstring("not-yet-created"))
			Expect(m.Status.Prefetch[0].Phase).To(Equal(inferencev1alpha1.PrefetchPhasePending))

			createPVC("not-yet-created")
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, jobKey, &batchv1.Job{})).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseDownloading))
		})

		It("is honoured over a service that names another claim, which is then reported", func() {
			createPVC("model-cache")
			svc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "svc-elsewhere", Namespace: ns},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:   "honoured-claim-model",
					ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: "somewhere-else"},
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, svc) }()
			m := newPrefetchModel("honoured-claim-model")
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "model-cache"}
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()

			_, _, err := prefetchReconciler().reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: prefetchJobName(m), Namespace: ns}, job)).To(Succeed())
			Expect(jobClaim(job)).To(Equal("model-cache"))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: ns}, m)).To(Succeed())
			cond := meta.FindStatusCondition(m.Status.Conditions, ConditionPrefetchTargetMismatch)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Message).To(ContainSubstring("svc-elsewhere"))
			Expect(cond.Message).To(ContainSubstring("somewhere-else"))
		})

		// A claim the Model was staged into stays recorded, but a Job built for
		// it must never be mistaken for the new claim's: the files are not
		// there.
		It("stages again when spec.modelCache.claimName moves to another claim", func() {
			createPVC("cache-a")
			createPVC("cache-b")
			m := newPrefetchModel("moving-claim-model")
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "cache-a"}
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()
			modelKey := types.NamespacedName{Name: m.Name, Namespace: ns}
			jobKey := types.NamespacedName{Name: prefetchJobName(m), Namespace: ns}
			r := prefetchReconciler()

			_, _, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			completeJob(jobKey)
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			_, _, err = r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseReady))

			m.Spec.ModelCache.ClaimName = "cache-b"
			Expect(k8sClient.Update(ctx, m)).To(Succeed())
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			handled, _, err := r.reconcilePrefetch(ctx, m)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).NotTo(Equal(PhaseReady), "nothing has been staged into cache-b yet")
			entryB := findPrefetchStatus(m, "cache-b")
			Expect(entryB).NotTo(BeNil())
			Expect(entryB.Phase).NotTo(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))

			Eventually(func() string {
				Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
				_, _, err := r.reconcilePrefetch(ctx, m)
				Expect(err).NotTo(HaveOccurred())
				job := &batchv1.Job{}
				if err := k8sClient.Get(ctx, jobKey, job); err != nil || job.DeletionTimestamp != nil {
					return ""
				}
				return jobClaim(job)
			}, 10*time.Second, 200*time.Millisecond).Should(Equal("cache-b"))
			Expect(k8sClient.Get(ctx, modelKey, m)).To(Succeed())
			Expect(m.Status.Phase).To(Equal(PhaseDownloading))
			Expect(findPrefetchStatus(m, "cache-b").Phase).To(Equal(inferencev1alpha1.PrefetchPhaseRunning))
			entryA := findPrefetchStatus(m, "cache-a")
			Expect(entryA).NotTo(BeNil(), "the claim already staged stays on record")
			Expect(entryA.Phase).To(Equal(inferencev1alpha1.PrefetchPhaseSucceeded))
		})

		It("rejects prefetch with modelCache.persistence Ephemeral at admission", func() {
			m := newPrefetchModel("ephemeral-prefetch")
			m.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{Persistence: inferencev1alpha1.ModelCachePersistenceEphemeral}
			err := k8sClient.Create(ctx, m)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("prefetch cannot be combined with modelCache.persistence Ephemeral"))

			alias := newPrefetchModel("ephemeral-alias-prefetch")
			alias.Spec.Prefetch = false
			alias.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{Persistence: inferencev1alpha1.ModelCachePersistenceEphemeral, Prefetch: true}
			err = k8sClient.Create(ctx, alias)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("prefetch cannot be set when persistence is Ephemeral"))

			ok := newPrefetchModel("claim-prefetch")
			ok.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "model-cache", Prefetch: true}
			Expect(k8sClient.Create(ctx, ok)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ok)).To(Succeed())
		})
	})

	Describe("InferenceService watch", func() {
		It("maps a service to the Model it references", func() {
			r := prefetchReconciler()
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: ns},
				Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "the-model"},
			}
			Expect(r.findModelForInferenceService(ctx, isvc)).To(ConsistOf(reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "the-model", Namespace: ns},
			}))
			isvc.Spec.ModelRef = ""
			Expect(r.findModelForInferenceService(ctx, isvc)).To(BeEmpty())
		})
	})
})
