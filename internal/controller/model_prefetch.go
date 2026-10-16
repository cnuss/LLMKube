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
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Model prefetch (#904): a Model with spec.prefetch=true and a remote source
// gets its artifact pulled into a model cache PVC ahead of any
// InferenceService, via an owner-ref'd Job whose pod reuses the exact
// init-container download stack the serving path uses
// (buildModelStorageConfig with a nil InferenceService): same cache-prep
// container, same downloader image and ETag handling, same multi-file
// staging plan, same SourceSecretRef env. The first serving pod then starts
// from a cache hit instead of a cold download.
//
// The target PVC is chosen by the resolved policy (#1676): exactly one
// distinct spec.modelCache.claimName among the InferenceServices referencing
// this Model, so the download lands where the serving pod reads it, else the
// shared cache (ModelCacheModeShared) when no service references the Model or
// the services disagree. The field's GoDoc documents that limitation and the
// pvc:// staging alternative.

// defaultPrefetchImage mirrors the --init-container-image flag default for
// the no-op completion container when the reconciler is constructed without
// the field (tests); production wiring always sets InitContainerImage.
const defaultPrefetchImage = "docker.io/curlimages/curl:8.18.0"

// prefetchJobName returns the deterministic Job name for a Model's prefetch.
func prefetchJobName(model *inferencev1alpha1.Model) string {
	return model.Name + "-prefetch"
}

// seedPrefetchCacheKey populates Status.CacheKey (in memory) when empty so
// the storage builder targets the shared cache PVC rather than an emptyDir.
func seedPrefetchCacheKey(model *inferencev1alpha1.Model) {
	if model.Status.CacheKey == "" {
		model.Status.CacheKey = computeCacheKey(model.Spec.Source)
	}
}

// prefetchEligible reports whether this Model should take the prefetch path:
// the field is set and the source is a remote artifact the downloader stack
// can fetch (http/https/hf). Local paths and pvc:// sources have nothing to
// prefetch.
func prefetchEligible(model *inferencev1alpha1.Model) bool {
	if model == nil || !model.Spec.Prefetch {
		return false
	}
	// A glob's expansion is written only by the runtime-resolved reconcile,
	// which runs after prefetch; a Job built before it cannot stage the files.
	if filesHaveGlob(model.Spec.Files) {
		return false
	}
	// A source is prefetchable when it normalizes to a fetchable http(s) URL:
	// hf:// and huggingface.co repo URLs become resolve URLs, direct file URLs
	// stay as-is, while bare org/name (runtime-resolved), local paths, pvc://
	// and s3:// do not. Checking the normalized scheme directly preserves the
	// original semantics without depending on isRemoteHTTPSource's repo-vs-file
	// classification.
	normalized := normalizeHFSource(model.Spec.Source)
	if hasSchemeFold(normalized, "https://") || hasSchemeFold(normalized, "http://") {
		return true
	}
	// A bare repo ID with spec.files is staged by the operator (stagingRepoBase).
	return hasMultiFileStaging(model) && isHFRepoSource(model.Spec.Source)
}

// DefaultPrefetchJobReadTimeout bounds the read of the prefetch Job. Generous
// enough that a legitimate first-time informer sync on a cluster with many
// Jobs completes well inside it, short enough that a permanently unsyncable
// informer cannot monopolise a reconcile worker (#1597).
const DefaultPrefetchJobReadTimeout = 30 * time.Second

// prefetchJobReadTimeout returns the configured bound, or the default.
func (r *ModelReconciler) prefetchJobReadTimeout() time.Duration {
	if r.PrefetchJobReadTimeout > 0 {
		return r.PrefetchJobReadTimeout
	}
	return DefaultPrefetchJobReadTimeout
}

// reconcilePrefetch drives the prefetch state machine. handled=true means
// the prefetch path owns this reconcile and Reconcile should return its
// result; handled=false means fall through to the normal source dispatch
// (not eligible, or the prefetch already completed and the model is Ready).
func (r *ModelReconciler) reconcilePrefetch(ctx context.Context, model *inferencev1alpha1.Model) (bool, ctrl.Result, error) {
	if !prefetchEligible(model) {
		return false, ctrl.Result{}, nil
	}

	// Completed earlier: the cache is warm and status is terminal. Fall
	// through so the ordinary remote-source reconcile keeps owning Ready
	// (revalidation, accelerator checks) without re-running the Job.
	if model.Status.Phase == PhaseReady && model.Status.CacheKey != "" {
		return false, ctrl.Result{}, nil
	}

	logger := logf.FromContext(ctx)

	// Bounded read. The controller does not watch Jobs (see the polling
	// comment below), so this is the first touch of the type and
	// controller-runtime starts the informer lazily and waits for it to sync.
	// When that informer can never list -- an RBAC denial like #1593, a
	// partitioned API server -- the reflector retries forever and an unbounded
	// Get holds the worker. The Model controller's workers are a shared
	// resource, so that stalls every other Model, not just this one (#1597).
	// A timeout turns it into a prompt error and an ordinary backoff requeue,
	// which self-heals once the informer can sync.
	getCtx, cancel := context.WithTimeout(ctx, r.prefetchJobReadTimeout())
	defer cancel()

	job := &batchv1.Job{}
	err := r.Get(getCtx, types.NamespacedName{Name: prefetchJobName(model), Namespace: model.Namespace}, job)
	switch {
	case apierrors.IsNotFound(err):
		result, startErr := r.startPrefetch(ctx, model)
		return true, result, startErr
	case err != nil:
		return true, ctrl.Result{}, fmt.Errorf("checking prefetch job: %w", err)
	}

	switch {
	case jobSucceeded(job):
		return true, ctrl.Result{}, r.completePrefetch(ctx, model)
	case jobFailed(job):
		logger.Info("Prefetch job failed", "job", job.Name)
		model.Status.Phase = PhaseFailed
		return true, ctrl.Result{}, r.updateStatus(ctx, model, ConditionProgressing, metav1.ConditionFalse,
			"PrefetchFailed", fmt.Sprintf("prefetch job %q failed; see its pod logs", job.Name))
	default:
		// Still running: reflect progress and poll. The Job's completion
		// does not generate a Model event, so a modest requeue keeps the
		// status honest without watching Jobs from this controller.
		if model.Status.Phase != PhaseDownloading {
			model.Status.Phase = PhaseDownloading
			seedPrefetchCacheKey(model)
			if err := r.updateStatus(ctx, model, ConditionProgressing, metav1.ConditionTrue,
				"PrefetchRunning", "Prefetch job downloading model into the model cache"); err != nil {
				return true, ctrl.Result{}, err
			}
		}
		return true, ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
}

// resolvePrefetchTarget returns a synthetic InferenceService that carries the
// cache claim prefetch should stage into, or nil for the shared cache, plus a
// reason describing why the override was not honoured (empty when a per-service
// target was resolved).
//
// A Model is 1:N with InferenceServices, so the target is not always
// well-defined. The resolved policy (#1676):
//   - exactly one distinct spec.modelCache.claimName among the referencing
//     services: return a synthetic InferenceService naming that claim, so the
//     download lands in the PVC the serving pod mounts;
//   - no referencing service, or several disagreeing on claimName: return nil
//     (the shared cache), so the behaviour is unchanged and the caller can
//     surface that the override was not honoured.
//
// The nil isvc is a genuine InferenceService-shaped value only when a claim
// was resolved; callers must not assume it is non-nil.
func (r *ModelReconciler) resolvePrefetchTarget(ctx context.Context, model *inferencev1alpha1.Model) (target *inferencev1alpha1.InferenceService, reason string) {
	list := &inferencev1alpha1.InferenceServiceList{}
	if err := r.List(ctx, list, client.InNamespace(model.Namespace)); err != nil {
		// A listing failure (RBAC denial, partitioned API server) must not
		// wedge prefetch: fall back to the shared cache, which is the
		// pre-#1676 behaviour, and let the caller surface the ignore.
		return nil, "prefetch cache listing failed: " + err.Error()
	}

	var (
		claim    string
		seen     bool
		conflict bool
	)
	for i := range list.Items {
		svc := &list.Items[i]
		if svc.Spec.ModelRef != model.Name {
			continue
		}
		claimName := userModelCacheClaimName(svc)
		if claimName == "" {
			continue // references the model but no claim override to honour
		}
		if !seen {
			seen = true
			claim = claimName
			continue
		}
		if claimName != claim {
			conflict = true
		}
	}
	if seen && !conflict {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: model.Name, Namespace: model.Namespace},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name, ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: claim}},
		}, ""
	}
	if !seen {
		return nil, "no InferenceService references this Model with a modelCache.claimName override; prefetching into the shared cache"
	}
	return nil, "multiple InferenceServices referencing this Model disagree on their modelCache.claimName overrides; prefetching into the shared cache"
}

// startPrefetch ensures the shared cache PVC exists (unless prefetch has
// resolved a per-service target) and creates the owner-ref'd prefetch Job.
func (r *ModelReconciler) startPrefetch(ctx context.Context, model *inferencev1alpha1.Model) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// Seed the cache key BEFORE building the Job: effectiveModelCacheKey
	// reads Status.CacheKey for single-file models, and an empty key makes
	// buildModelStorageConfig fall back to an emptyDir — the prefetch would
	// download and then discard the artifact. Same derivation as the remote
	// branch of reconcileBySourceType, so the serving pod later computes the
	// identical key and takes the cache-hit path.
	seedPrefetchCacheKey(model)

	// Resolve the PVC the serving pod will read (#1676). A nil isvc means
	// the shared cache; a resolved one means prefetch stages into that
	// service's PVC, so the shared PVC must not be provisioned here.
	target, ignoreReason := r.resolvePrefetchTarget(ctx, model)
	if target == nil {
		if err := ensureSharedModelCachePVC(ctx, r.Client, model.Namespace,
			r.ModelCacheSize, r.ModelCacheClass, r.ModelCacheAccessMode); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring shared model cache PVC: %w", err)
		}
	}

	job, err := r.buildPrefetchJob(ctx, model, target)
	if err != nil {
		model.Status.Phase = PhaseFailed
		return ctrl.Result{}, r.updateStatus(ctx, model, ConditionProgressing, metav1.ConditionFalse,
			"PrefetchInvalid", err.Error())
	}

	// The Job pod mounts the CA ConfigMap from the Model's namespace like the
	// serving pod does. Without it the pod wedges in ContainerCreating, the
	// Job neither completes nor fails, and the Model reads Downloading
	// forever. Hold the Job and name the cause instead.
	required, caMissing, err := caCertConfigMapMissing(ctx, r.Client, r.CACertConfigMap, model.Namespace, &job.Spec.Template.Spec)
	if err != nil {
		return ctrl.Result{}, err
	}
	transitioned := setCACertConfigMapCondition(&model.Status.Conditions, model.Generation, r.CACertConfigMap, model.Namespace, required, caMissing)
	if caMissing {
		msg := caCertConfigMapMissingMessage(r.CACertConfigMap, model.Namespace)
		if transitioned && r.Recorder != nil {
			r.Recorder.Eventf(model, nil, corev1.EventTypeWarning, ReasonCACertConfigMapMissing, "Prefetch", "%s", msg)
		}
		logger.Info("CA ConfigMap missing in the Model namespace; not creating the prefetch Job",
			"configMap", r.CACertConfigMap, "namespace", model.Namespace)
		model.Status.Phase = "Pending"
		return ctrl.Result{RequeueAfter: caCertConfigMapMissingRequeue}, r.updateStatus(ctx, model, ConditionProgressing, metav1.ConditionFalse,
			ReasonCACertConfigMapMissing, msg)
	}

	// The override could not be honoured: keep today's shared-cache
	// behaviour but surface the condition so the no-op is visible rather
	// than silent (#1676). Written only once the Job is about to be created.
	// The Model controller has no predicate, so on the CA-hold and
	// PrefetchInvalid paths above this Progressing=True write would flip the
	// condition against that path's own Progressing=False write on every
	// reconcile, and each write re-triggers the reconcile.
	if target == nil && ignoreReason != "" {
		if err := r.updateStatus(ctx, model, ConditionProgressing, metav1.ConditionTrue,
			"PrefetchCacheIgnored", ignoreReason); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := controllerutil.SetControllerReference(model, job, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("owner-ref prefetch job: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Raced with a concurrent reconcile; the poll branch takes over.
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("creating prefetch job: %w", err)
	}

	logger.Info("Created prefetch job", "job", job.Name)
	model.Status.Phase = PhaseDownloading
	if err := r.updateStatus(ctx, model, ConditionProgressing, metav1.ConditionTrue,
		"PrefetchStarted", prefetchStartedMessage(target)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// completePrefetch marks the Model Ready with its cache key so the first
// InferenceService takes the cache-hit path.
func (r *ModelReconciler) completePrefetch(ctx context.Context, model *inferencev1alpha1.Model) error {
	model.Status.Phase = PhaseReady
	seedPrefetchCacheKey(model)
	model.Status.AcceleratorReady = r.checkAcceleratorAvailability(ctx, model)
	now := metav1.Now()
	model.Status.LastUpdated = &now
	return r.updateStatus(ctx, model, "Available", metav1.ConditionTrue,
		"ModelPrefetched", "Model prefetched into the model cache")
}

// prefetchTargetName returns the PVC name prefetch will stage into, for status
// messages: the resolved per-service claim, or the shared cache PVC.
func prefetchTargetName(target *inferencev1alpha1.InferenceService) string {
	if claim := userModelCacheClaimName(target); claim != "" {
		return claim
	}
	return ModelCachePVCName
}

// prefetchStartedMessage renders the Job's target PVC into the started status
// so operators can see where the download lands (#1676).
func prefetchStartedMessage(target *inferencev1alpha1.InferenceService) string {
	return fmt.Sprintf("Started prefetch job downloading model into %q", prefetchTargetName(target))
}

// buildPrefetchJob assembles the download Job. The pod reuses the serving
// path's init containers and volumes verbatim (nil InferenceService: the
// only isvc-derived input is an optional fsGroup override) and adds a no-op
// completion container, since a Job pod must have at least one non-init
// container.
//
// target is the resolved per-InferenceService cache claim (#1676), or nil for
// the shared cache; buildModelStorageConfig reads userModelCacheClaimName off
// it to pick the PVC, so the download lands where the serving pod reads.
//
// ctx is needed only to read HF_ENDPOINT from the Model's sourceSecretRef, so
// a prefetch from a mirror carries the bearer token as the serving pod does
// (#1900).
func (r *ModelReconciler) buildPrefetchJob(ctx context.Context, model *inferencev1alpha1.Model, target *inferencev1alpha1.InferenceService) (*batchv1.Job, error) {
	if effectiveModelCacheKey(model) == "" {
		return nil, fmt.Errorf("prefetch: model has no cache key; refusing to build a Job that would download into an emptyDir")
	}
	storage := buildModelStorageConfig(model, target, model.Namespace, true, ModelCacheModeShared,
		r.CACertConfigMap, r.InitContainerImage, r.DefaultFSGroup, r.AllowedHostPathRoots, hfEndpointFromSecret(ctx, r.Client, model))
	if len(storage.initContainers) == 0 {
		return nil, fmt.Errorf("prefetch: source %q produced no downloader containers", model.Spec.Source)
	}

	backoff := int32(2)
	ttl := int32(24 * 60 * 60) // keep a day for log triage, then self-clean

	var podSecurity *corev1.PodSecurityContext
	if r.DefaultFSGroup > 0 {
		fs := r.DefaultFSGroup
		podSecurity = &corev1.PodSecurityContext{FSGroup: &fs}
	}

	image := r.InitContainerImage
	if image == "" {
		image = defaultPrefetchImage
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      prefetchJobName(model),
			Namespace: model.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "llmkube",
				"app.kubernetes.io/component":  "model-prefetch",
				"app.kubernetes.io/managed-by": "llmkube-controller",
				"inference.llmkube.dev/model":  model.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/component": "model-prefetch",
						"inference.llmkube.dev/model": model.Name,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:   corev1.RestartPolicyNever,
					SecurityContext: podSecurity,
					// The Model's own tolerations, so the download can reach
					// tainted nodes whose local storage backs the shared
					// cache (#1621).
					Tolerations: model.Spec.PrefetchTolerations,
					// A node-pinned claim (#1676) binds wherever this pod
					// schedules, so placement here decides which node the cache
					// lands on (#1964).
					NodeSelector:   model.Spec.PrefetchNodeSelector,
					InitContainers: storage.initContainers,
					Containers: []corev1.Container{{
						Name:    "prefetch-done",
						Image:   image,
						Command: []string{"sh", "-c", "echo prefetch complete"},
					}},
					Volumes: storage.volumes,
				},
			},
		},
	}, nil
}

// jobSucceeded / jobFailed read the Job's terminal conditions.
func jobSucceeded(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
