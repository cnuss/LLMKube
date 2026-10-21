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
	"path/filepath"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// InferenceService side of prefetch (#2136). A service with
// spec.modelCache.prefetch asks for its Model to be staged into the cache
// claim its pods mount before any of them runs. The Model controller owns the
// Job and reports per-claim progress in Model.status.prefetch; this side reads
// that entry, holds the workload until it is current, and once it is, renders
// pods that only consume the cache: a read-only mount, no downloader, a check
// that the staged files are present.

const (
	// ConditionModelPrefetched reports whether the weights this service's
	// pods mount were staged by a prefetch for the Model's current spec.
	// Written only when spec.modelCache.prefetch is set.
	ConditionModelPrefetched = "ModelPrefetched"

	// ReasonWaitingForPrefetch: the prefetch into this service's claim has not
	// succeeded yet; the workload is held.
	ReasonWaitingForPrefetch = "WaitingForPrefetch"
	// ReasonPrefetchNotApplicable: the service asked, but there is no cache
	// path to stage into (Metal, caching disabled, a pre-staged source); the
	// workload proceeds as if the field were unset.
	ReasonPrefetchNotApplicable = "PrefetchNotApplicable"
	// ReasonPrefetchTargetMismatch is the event a service gets when its Model
	// was prefetched, but not into the claim this service mounts.
	ReasonPrefetchTargetMismatch = "PrefetchTargetMismatch"
	// ReasonPrefetchedFilesMissing: a serving pod built to consume a
	// prefetched cache found a staged file absent from the claim.
	ReasonPrefetchedFilesMissing = "PrefetchedFilesMissing"
	// ReasonPrefetchRestaged is the event a service records when it deletes
	// the Model's prefetch Job so the claim its pods found empty is filled
	// again.
	ReasonPrefetchRestaged = "PrefetchRestaged"
	// ReasonPrefetchUnsupported: the service asked, its pods would download
	// at serve time, and prefetch cannot stand in front of that download
	// (caching disabled, a runtime the operator does not stage, a draft
	// model, a source the downloader does not stage). The workload is held
	// rather than rolled out with a downloader: prefetch: true means "serve
	// only from a prefetched cache".
	ReasonPrefetchUnsupported = "PrefetchUnsupported"
)

// prefetchApplicability classifies a service's prefetch request against its
// Model and the operator. Both controllers use it, so the Model side never
// stages for a request the service side cannot consume.
type prefetchApplicability int

const (
	// prefetchApplies: the pods mount a cache claim the Job can fill.
	prefetchApplies prefetchApplicability = iota
	// prefetchNotApplicable: nothing downloads at serve time, so there is
	// nothing to prefetch and nothing to hold for.
	prefetchNotApplicable
	// prefetchUnsupported: the pods would download at serve time and no
	// prefetch can cover it. The request is refused, not ignored.
	prefetchUnsupported
)

// classifyPrefetchRequest is the one place that decides whether a prefetch
// request can be honoured. isvc must already carry its effective modelCache.
func classifyPrefetchRequest(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, modelCachePath string) (prefetchApplicability, string) {
	switch {
	case isMetalModel(model):
		return prefetchNotApplicable, "a Metal service mounts no cache claim; the host metal-agent fetches the model itself"
	case isPVCSource(model.Spec.Source) || isOCISource(model.Spec.Source):
		return prefetchNotApplicable, fmt.Sprintf("source %q is pre-staged and mounted without a download", model.Spec.Source)
	case isLocalSource(model.Spec.Source):
		return prefetchNotApplicable, fmt.Sprintf("source %q is a local path the operator copies; nothing is downloaded on the serving node", model.Spec.Source)
	case modelCachePath == "":
		return prefetchUnsupported, "model caching is disabled on the operator (--model-cache-path unset, chart modelCache.enabled=false), so the pods would download into an emptyDir on the serving node"
	case !operatorStagesModel(isvc, resolveBackend(isvc)):
		return prefetchUnsupported, fmt.Sprintf("runtime %q fetches the model itself on the serving node (skipModelInit, or a runtime that stages only with spec.stageModel); a prefetched claim cannot be put in front of it", resolveServingRuntime(isvc))
	case draftModelRefFor(isvc) != "":
		return prefetchUnsupported, fmt.Sprintf("speculativeDecoding.draftModelRef %q is not prefetched and its downloader would run on the serving node", draftModelRefFor(isvc))
	case !prefetchableSource(model):
		return prefetchUnsupported, fmt.Sprintf("source %q is not one the downloader stages ahead of time, so the pods would download it on the serving node", model.Spec.Source)
	case effectiveModelCacheKey(model) == "":
		return prefetchUnsupported, fmt.Sprintf("Model %q has no cache key yet; it is derived when the Model reconciles", model.Name)
	}
	return prefetchApplies, ""
}

// resolveServingRuntime names the runtime for messages.
func resolveServingRuntime(isvc *inferencev1alpha1.InferenceService) string {
	if isvc.Spec.Runtime == "" {
		return "llamacpp"
	}
	return isvc.Spec.Runtime
}

// inheritModelCache gives a service with no modelCache block of its own the
// Model's, in memory only: the claim is declared once on the Model and every
// referencing service resolves it, while a service that sets its own block
// keeps it. The controller never writes an InferenceService spec, so the copy
// is never persisted.
//
// A status write mid-reconcile decodes the server's object back into isvc and
// drops the block again, so the helpers that read it after such a write go
// through effectiveCacheService instead of trusting the mutation, and
// reconcileDeployment re-applies it after its own status writes.
func inheritModelCache(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) {
	if isvc == nil || model == nil || isvc.Spec.ModelCache != nil || model.Spec.ModelCache == nil {
		return
	}
	isvc.Spec.ModelCache = model.Spec.ModelCache.DeepCopy()
}

// effectiveCacheService returns isvc as the cache helpers should read it: the
// service itself when it has a modelCache block, else a shallow copy carrying
// the Model's. The copy is never written anywhere.
func effectiveCacheService(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) *inferencev1alpha1.InferenceService {
	if isvc == nil || isvc.Spec.ModelCache != nil || model == nil || model.Spec.ModelCache == nil {
		return isvc
	}
	eff := *isvc
	eff.Spec.ModelCache = model.Spec.ModelCache.DeepCopy()
	return &eff
}

// prefetchGate is what a service's modelCache.prefetch resolves to on this
// reconcile.
type prefetchGate struct {
	// requested: spec.modelCache.prefetch is set.
	requested bool
	// applies: and there is a cache claim to stage into.
	applies bool
	claim   string
	// satisfied: the Model's entry for claim is Succeeded and current.
	satisfied bool
	// failed: the Model's entry for claim is Failed.
	failed bool
	// unsupported: the request cannot be honoured and the workload is held.
	unsupported bool
	// entry is the Model's record for claim, when there is one.
	entry   *inferencev1alpha1.PrefetchStatus
	reason  string
	message string
}

// prefetchGate reads the Model's prefetch entry for the claim this service's
// pods mount. Call after inheritModelCache.
func (r *InferenceServiceReconciler) prefetchGate(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) prefetchGate {
	g := prefetchGate{}
	isvc = effectiveCacheService(isvc, model)
	if !servicePrefetchRequested(isvc) {
		return g
	}
	g.requested = true
	switch kind, why := classifyPrefetchRequest(isvc, model, r.ModelCachePath); kind {
	case prefetchNotApplicable:
		g.reason, g.message = ReasonPrefetchNotApplicable, why
		return g
	case prefetchUnsupported:
		g.applies, g.unsupported = true, true
		g.reason, g.message = ReasonPrefetchUnsupported, why+"; the workload is held until the spec changes"
		return g
	}
	g.applies = true
	g.claim = modelCachePVCName(isvc, r.ModelCacheMode)
	entry := findPrefetchStatus(model, g.claim)
	g.entry = entry
	switch {
	case entry == nil:
		g.reason = ReasonWaitingForPrefetch
		g.message = fmt.Sprintf("no prefetch of Model %q into cache claim %q recorded yet; the Model controller starts one", model.Name, g.claim)
	case entry.Phase == inferencev1alpha1.PrefetchPhaseFailed:
		g.failed = true
		g.reason = ReasonPrefetchFailed
		g.message = fmt.Sprintf("prefetch of Model %q into cache claim %q failed: %s", model.Name, g.claim, entry.Message)
	case entry.Phase != inferencev1alpha1.PrefetchPhaseSucceeded:
		g.reason = ReasonWaitingForPrefetch
		g.message = fmt.Sprintf("prefetch of Model %q into cache claim %q is %s (job %q): %s", model.Name, g.claim, entry.Phase, entry.JobName, entry.Message)
	case entry.ObservedGeneration != model.Generation || entry.CacheKey != effectiveModelCacheKey(model):
		g.reason = ReasonWaitingForPrefetch
		g.message = fmt.Sprintf("prefetch of Model %q into cache claim %q was confirmed for generation %d; waiting for generation %d", model.Name, g.claim, entry.ObservedGeneration, model.Generation)
	default:
		// The pods stage the recorded expansion of a glob; a Model whose
		// expansion is not current for its generation is not ready to be
		// consumed even though its entry is.
		if _, err := modelStagingPlan(model); err != nil {
			g.reason = ReasonWaitingForPrefetch
			g.message = fmt.Sprintf("Model %q has not recorded its staged file set for generation %d yet", model.Name, model.Generation)
			return g
		}
		g.satisfied = true
		g.reason = ReasonPrefetchComplete
		g.message = fmt.Sprintf("Model %q is prefetched into cache claim %q (job %q); pods mount it read-only", model.Name, g.claim, entry.JobName)
	}
	return g
}

// prefetchSatisfied reports whether the pods should be rendered to consume a
// prefetched cache: the service asked, and the prefetch is current.
func (r *InferenceServiceReconciler) prefetchSatisfied(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) bool {
	g := r.prefetchGate(isvc, model)
	return g.applies && g.satisfied
}

// setModelPrefetchedCondition records the gate on the service. The condition
// exists only while the field is set; it is removed, not flipped, when the
// user drops the request.
func setModelPrefetchedCondition(isvc *inferencev1alpha1.InferenceService, g prefetchGate) {
	if !g.requested {
		meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionModelPrefetched)
		return
	}
	status := metav1.ConditionFalse
	if g.satisfied {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type:               ConditionModelPrefetched,
		Status:             status,
		ObservedGeneration: isvc.Generation,
		Reason:             g.reason,
		Message:            g.message,
	})
}

// holdForPrefetch keeps the workload as it is while a requested prefetch is
// pending or failed. held=true means the caller returns what it is handed:
// the live Deployment and its counts when one exists (so a rollout waits on
// the new weights while the old template keeps serving), or a status write
// and no workload when nothing serves yet. A failed prefetch holds the same
// way and shows on the service; it never rolls pods onto a cache that was
// not filled. A workload scaled to zero is not held: there is nothing to
// protect and the scale-down must land.
func (r *InferenceServiceReconciler) holdForPrefetch(ctx context.Context, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, desiredReplicas int32, modelReady bool) (held bool, dep *appsv1.Deployment, counts replicaCounts, result *ctrl.Result, err error) {
	g := r.prefetchGate(isvc, model)
	setModelPrefetchedCondition(isvc, g)
	if !g.applies || g.satisfied || desiredReplicas == 0 {
		// The hold's diagnosis is this controller's to clear: the Ready path
		// clears scheduling fields only once pods serve, and a first rollout
		// that was held has none yet.
		if isvc.Status.SchedulingStatus == ReasonWaitingForPrefetch || isvc.Status.SchedulingStatus == ReasonPrefetchFailed {
			isvc.Status.SchedulingStatus = ""
			isvc.Status.SchedulingMessage = ""
			isvc.Status.WaitingFor = ""
		}
		return false, nil, replicaCounts{}, nil, nil
	}
	log := logf.FromContext(ctx)

	if isvc.Spec.MultiNode != nil {
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.InNamespace(isvc.Namespace), client.MatchingLabels{LabelMultiNodeGroup: isvc.Name}); err != nil {
			return true, nil, replicaCounts{}, nil, err
		}
		if len(pods.Items) > 0 {
			log.Info("Holding the multiNode group on its current members while the prefetch runs", "claim", g.claim, "reason", g.reason)
			return true, nil, replicaCounts{Ready: isvc.Status.ReadyReplicas, Observed: isvc.Status.Replicas}, nil, nil
		}
	} else {
		existing := &appsv1.Deployment{}
		err := r.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: isvc.Namespace}, existing)
		switch {
		case err == nil && ensureDeploymentOwned(isvc, existing) == nil:
			log.Info("Holding the Deployment on its current template while the prefetch runs", "claim", g.claim, "reason", g.reason)
			return true, existing, replicaCounts{Ready: existing.Status.ReadyReplicas, Observed: existing.Status.Replicas}, nil, nil
		case err != nil && !apierrors.IsNotFound(err):
			return true, nil, replicaCounts{}, nil, err
		}
	}

	// Nothing serves yet: no pods until the weights are in place, and none
	// at all for a request that cannot be honoured.
	phase := "Pending"
	if g.failed || g.unsupported {
		phase = PhaseFailed
	}
	log.Info("Not creating the workload until the prefetch completes", "claim", g.claim, "reason", g.reason, "phase", phase)
	res, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, phase, modelReady, 0, isvc.Status.Replicas, desiredReplicas, "",
		g.message, &SchedulingInfo{Status: g.reason, Message: g.message})
	return true, nil, replicaCounts{}, &res, updateErr
}

// restagePrefetchOnMissingFiles reacts to serving pods that found the staged
// files gone from a claim the Model's status still records as filled (a
// cache clear, a claim recreated under the same name). The service deletes
// the Model's prefetch Job for its claim; the Model controller, which owns
// the Job, recreates it as a re-verification, and the failing pods pass
// model-verify on their next init retry with no rollout. Once per episode:
// the condition must have transitioned after the entry last succeeded, so a
// claim the serving node cannot reach at all is re-staged once and then
// stays reported, not re-staged in a loop.
func (r *InferenceServiceReconciler) restagePrefetchOnMissingFiles(ctx context.Context, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) {
	g := r.prefetchGate(isvc, model)
	if !g.applies || !g.satisfied {
		return
	}
	cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionModelTransferHealthy)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonPrefetchedFilesMissing {
		return
	}
	entry := findPrefetchStatus(model, g.claim)
	if entry == nil || entry.JobName == "" {
		return
	}
	if entry.LastTransitionTime != nil && !cond.LastTransitionTime.After(entry.LastTransitionTime.Time) {
		return
	}
	log := logf.FromContext(ctx)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: entry.JobName, Namespace: model.Namespace}, job); err != nil {
		// Gone already: the Model controller re-verifies on its own.
		if !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to read the prefetch job to re-stage", "job", entry.JobName)
		}
		return
	}
	if !metav1.IsControlledBy(job, model) {
		return
	}
	// A staging already in flight is the remedy, not a stale Job; and one
	// that finished after the failure was observed has already refilled the
	// claim. Either way deleting it would only start the download over.
	if !jobSucceeded(job) && !jobFailed(job) {
		return
	}
	if job.Status.CompletionTime != nil && !cond.LastTransitionTime.After(job.Status.CompletionTime.Time) {
		return
	}
	if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "Failed to delete the prefetch job to re-stage", "job", job.Name)
		return
	}
	log.Info("Serving pods found the prefetched files missing; deleted the prefetch job so the Model stages the claim again",
		"job", job.Name, "claim", g.claim, "model", model.Name)
	if r.Recorder != nil {
		r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, ReasonPrefetchRestaged, "Reconcile",
			"pods report PrefetchedFilesMissing on claim %q; deleted prefetch job %q so Model %q stages it again", g.claim, job.Name, model.Name)
	}
}

// warnPrefetchTargetMismatch tells a service that did not ask for its own
// prefetch when its Model was prefetched, but into another claim than the
// one this service mounts: its pods download at serve time, which is what
// the Model's spec.prefetch was meant to avoid.
func (r *InferenceServiceReconciler) warnPrefetchTargetMismatch(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) {
	isvc = effectiveCacheService(isvc, model)
	if r.Recorder == nil || servicePrefetchRequested(isvc) || !modelWantsPrefetch(model) || isMetalModel(model) ||
		!modelNeedsCachePVC(model, isvc, r.ModelCachePath) {
		return
	}
	claim := modelCachePVCName(isvc, r.ModelCacheMode)
	var staged []string
	for _, e := range model.Status.Prefetch {
		if e.ClaimName == claim {
			return
		}
		if e.Phase == inferencev1alpha1.PrefetchPhaseSucceeded {
			staged = append(staged, e.ClaimName)
		}
	}
	if len(staged) == 0 {
		return
	}
	r.Recorder.Eventf(isvc, nil, corev1.EventTypeWarning, ReasonPrefetchTargetMismatch, "Reconcile",
		"Model %q was prefetched into %s, not into this service's cache claim %q; the pods download at serve time. "+
			"Set spec.modelCache.prefetch to stage into %q, or point spec.modelCache.claimName at a prefetched claim",
		model.Name, strings.Join(staged, ", "), claim, claim)
}

// buildPrefetchedStorageConfig renders the storage of a pod whose weights a
// prefetch Job already staged: the cache claim mounted read-only, at the same
// paths the download path uses, with a single init container that checks the
// staged files are present (and the primary file's spec.sha256 when one is
// pinned) and fails naming the cause. No cache-prep (the Job prepared the
// claim) and no downloader: a pod that would download on the serving node is
// the failure this exists to prevent.
//
// staged is the file list the Job recorded on the Model's entry. It is
// authoritative over anything derived from the Model now: a single-file
// name is derived from GGUF metadata the Model may have parsed only after
// the Job ran, and the Job wrote the name it had then.
func buildPrefetchedStorageConfig(model *inferencev1alpha1.Model, isvc *inferencev1alpha1.InferenceService, cacheMode string, initContainerImage string, staged []string) (cfg modelStorageConfig) {
	defer func() { applyInitContainerDiagnostics(cfg.initContainers) }()
	cacheDir := fmt.Sprintf("/models/%s", effectiveModelCacheKey(model))
	volumes := []corev1.Volume{{
		Name: "model-cache",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: modelCachePVCName(isvc, cacheMode),
				ReadOnly:  true,
			},
		},
	}}
	mounts := []corev1.VolumeMount{{Name: "model-cache", MountPath: "/models", ReadOnly: true}}

	plan, err := modelStagingPlan(model)
	if err != nil {
		return modelStorageConfig{
			modelPath:      stagedCachePath(cacheDir, "model.gguf"),
			initContainers: []corev1.Container{modelStagingErrorInitContainer(err, model, initContainerImage)},
			volumes:        volumes,
			volumeMounts:   mounts,
		}
	}
	var (
		modelPath string
		stagedDir string
		files     []string
	)
	switch {
	case len(staged) > 0:
		files = staged
		modelPath = stagedCachePath(cacheDir, staged[0])
		if plan != nil {
			stagedDir = cacheDir
		}
	case plan != nil:
		modelPath = stagedCachePath(cacheDir, plan.Primary)
		stagedDir = cacheDir
		files = plan.Files
	default:
		// The basename the Job's downloader wrote: see buildCachedStorageConfig.
		basename := singleFileStagedBasename(model)
		modelPath = fmt.Sprintf("%s/%s", cacheDir, basename)
		files = []string{basename}
	}

	env := []corev1.EnvVar{
		{Name: "CACHE_DIR", Value: cacheDir},
		{Name: "MODEL_FILES", Value: strings.Join(files, "\n")},
		{Name: "MODEL_NAME", Value: model.Name},
	}
	// A pinned single-file digest is enforced here as it is on a pvc://
	// mount (#1979): the downloader that would have checked it did not run
	// in this pod. Per-file digests of a multi-file set were checked by the
	// Job as it staged each file.
	digest := ""
	if plan == nil && model.Spec.SHA256 != "" {
		digest = strings.ToLower(model.Spec.SHA256)
		env = append(env, corev1.EnvVar{Name: "MODEL_PATH", Value: modelPath}, corev1.EnvVar{Name: "MODEL_SHA256", Value: digest})
	}

	return modelStorageConfig{
		modelPath: modelPath,
		stagedDir: stagedDir,
		initContainers: []corev1.Container{{
			Name:            "model-verify",
			Image:           initContainerImage,
			Command:         []string{"sh", "-c", buildPrefetchVerifyCommand(digest != "")},
			Env:             env,
			VolumeMounts:    mounts,
			SecurityContext: initContainerSecurityContext(isvc),
		}},
		volumes:      volumes,
		volumeMounts: mounts,
	}
}

// singleFileStagedBasename is the name the download path writes a
// single-file Model under: see buildCachedStorageConfig.
func singleFileStagedBasename(model *inferencev1alpha1.Model) string {
	if model.Status.Path != "" {
		return filepath.Base(model.Status.Path)
	}
	return canonicalModelBasename(model)
}

// prefetchStagedFileNames lists what a prefetch Job built from jobModel
// writes under the cache key, primary first: the staging plan's files, or
// the single-file basename.
func prefetchStagedFileNames(jobModel *inferencev1alpha1.Model) []string {
	if plan, err := modelStagingPlan(jobModel); err == nil && plan != nil {
		return append([]string(nil), plan.Files...)
	}
	return []string{singleFileStagedBasename(jobModel)}
}

// buildPrefetchVerifyCommand checks every staged file is present on the
// read-only cache mount, and with verifyDigest hashes the primary file
// against MODEL_SHA256 as buildPVCVerifyCommand does (no stamp: the mount is
// read-only). The file list and the Model name arrive through env and are
// printed as printf arguments, never interpolated into the shell. The loop
// runs in a pipeline subshell, so a missing file exits that subshell and the
// trailing `|| exit 1` fails the container.
func buildPrefetchVerifyCommand(verifyDigest bool) string {
	cmd := `printf '%s\n' "$MODEL_FILES" | while IFS= read -r rel; do ` +
		`[ -n "$rel" ] || continue; ` +
		`if [ ! -f "$CACHE_DIR/$rel" ]; then ` +
		`printf 'ERROR: PrefetchedFilesMissing - %s is not on the cache claim. Model %s was expected to be staged there by its prefetch job (see the Model status.prefetch); not downloading on the serving node.\n' "$CACHE_DIR/$rel" "$MODEL_NAME" >&2; ` +
		`exit 1; fi; done || exit 1; `
	if verifyDigest {
		cmd = sha256HashFns + cmd +
			`llmkube_precheck_sha256 || exit 1; ` +
			`actual=$(llmkube_sha256_hash "$MODEL_PATH") || exit 1; ` +
			`if [ "$actual" != "$MODEL_SHA256" ]; then echo "ERROR: SHA256 mismatch for the prefetched $MODEL_PATH: expected $MODEL_SHA256, computed $actual" >&2; exit 1; fi; ` +
			`echo "Prefetched model verified against spec.sha256"; `
	}
	return cmd + `echo "Prefetched model files present under $CACHE_DIR"`
}
