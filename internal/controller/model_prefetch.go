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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Model prefetch (#904): a remote Model is pulled into a model cache PVC ahead
// of its serving pods by an owner-ref'd Job whose pod reuses the exact
// init-container download stack the serving path uses
// (buildModelStorageConfig with a synthetic InferenceService naming the
// claim): same cache-prep container, same downloader image and ETag handling,
// same multi-file staging plan, same SourceSecretRef env.
//
// Prefetch is tracked per cache claim (#2136). A Model can be read through
// more than one claim, so status.prefetch holds one entry per target, and each
// target has its own Job. Targets come from two places:
//
//   - the Model's own prefetch (spec.prefetch or spec.modelCache.prefetch),
//     staged into spec.modelCache.claimName when set, else the single claim
//     the referencing InferenceServices agree on (#1676), else the namespace's
//     shared cache. This target gates the Model's Ready: the Model reads
//     Downloading until it succeeds.
//   - every referencing InferenceService with modelCache.prefetch, staged
//     into the claim that service's pods mount. These never hold the Model's
//     Ready; the service holds its own rollout on its entry instead.
//
// An entry records the fingerprint of what it staged (cache key, file set,
// pinned digests), so a spec change that alters the files replaces the Job
// and a change that does not merely refreshes the entry's generation. A Job
// whose target is no longer wanted is deleted while it runs; a finished one
// is left for its TTL.

// defaultPrefetchImage mirrors the --init-container-image flag default for
// the no-op completion container when the reconciler is constructed without
// the field (tests); production wiring always sets InitContainerImage.
const defaultPrefetchImage = "docker.io/curlimages/curl:8.18.0"

// Annotations the prefetch Job carries so a later reconcile can tell whether
// it was built for the current spec and target.
const (
	annotationPrefetchFingerprint = "inference.llmkube.dev/prefetch-fingerprint"
	annotationPrefetchClaim       = "inference.llmkube.dev/prefetch-claim"
)

// Model conditions and reasons written by the prefetch path.
const (
	// ConditionPrefetchSkipped is True when spec.prefetch is set on a source
	// the downloader does not stage, so the field is not silently ignored.
	ConditionPrefetchSkipped = "PrefetchSkipped"
	// ConditionPrefetchTargetMismatch is True when a referencing
	// InferenceService mounts a cache claim the Model was not prefetched
	// into: that service downloads at serve time.
	ConditionPrefetchTargetMismatch = "PrefetchTargetMismatch"

	ReasonSourceNotPrefetchable = "SourceNotPrefetchable"
	ReasonPrefetchClaimMismatch = "ClaimMismatch"
	ReasonPrefetchClaimNotFound = "PrefetchClaimNotFound"
	ReasonPrefetchStarted       = "PrefetchStarted"
	ReasonPrefetchRunning       = "PrefetchRunning"
	ReasonPrefetchFailed        = "PrefetchFailed"
	ReasonPrefetchComplete      = "PrefetchComplete"
	ReasonModelPrefetched       = "ModelPrefetched"
)

const (
	// prefetchPollInterval is how often a running Job is re-read. The
	// controller does not watch Jobs: a completion generates no Model event.
	prefetchPollInterval = 15 * time.Second
	// prefetchRetryInterval follows a create race or a replaced Job's
	// deletion, both of which resolve within seconds.
	prefetchRetryInterval = 5 * time.Second
	// prefetchClaimWait is the retry while a target claim does not exist yet
	// (a user-owned claim still to be created, or a perService claim the
	// InferenceService controller has not provisioned).
	prefetchClaimWait = 30 * time.Second
)

// prefetchJobName returns the Job name for the Model's own prefetch target.
// It predates per-claim tracking, so a Job a previous controller created is
// found and adopted under this name.
func prefetchJobName(model *inferencev1alpha1.Model) string {
	return model.Name + "-prefetch"
}

// prefetchTargetJobName returns the Job name for a prefetch into claim that
// an InferenceService requested: one Job per claim, named by the claim's
// hash so two services sharing a claim share the Job.
func prefetchTargetJobName(model *inferencev1alpha1.Model, claim string) string {
	sum := sha256.Sum256([]byte(claim))
	return model.Name + "-prefetch-" + hex.EncodeToString(sum[:])[:8]
}

// seedPrefetchCacheKey sets Status.CacheKey (in memory) to the key the source
// hashes to, so the storage builder targets the cache PVC rather than an
// emptyDir. It also moves the key when the source changed: the fingerprint is
// taken from it, and waiting for the runtime-resolved path to catch up would
// let a stale key pass a changed source off as already staged. Every
// prefetchable source derives its key this way, so an equal key is left alone.
func seedPrefetchCacheKey(model *inferencev1alpha1.Model) {
	if key := computeCacheKey(model.Spec.Source); effectiveModelCacheKey(model) != key {
		model.Status.CacheKey = key
	}
}

// modelWantsPrefetch reports whether the Model asks to be staged into its own
// cache claim: spec.prefetch, or its alias spec.modelCache.prefetch.
func modelWantsPrefetch(model *inferencev1alpha1.Model) bool {
	if model == nil {
		return false
	}
	return model.Spec.Prefetch || (model.Spec.ModelCache != nil && model.Spec.ModelCache.Prefetch)
}

// modelCacheClaimName returns spec.modelCache.claimName, or "".
func modelCacheClaimName(model *inferencev1alpha1.Model) string {
	if model == nil || model.Spec.ModelCache == nil {
		return ""
	}
	return model.Spec.ModelCache.ClaimName
}

// prefetchableSource reports whether the Model's source is a remote artifact
// the downloader stack can stage ahead of time (http/https/hf, or a bare
// repo ID with spec.files). Local paths, pvc://, oci:// and s3:// sources
// are not prefetched.
func prefetchableSource(model *inferencev1alpha1.Model) bool {
	if model == nil {
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

// prefetchEligible reports whether this Model's own prefetch runs: the field
// is set and the source is one the downloader stages. A glob in spec.files no
// longer disqualifies it; the prefetch path expands it from the repository
// listing (#2071) before building the Job.
func prefetchEligible(model *inferencev1alpha1.Model) bool {
	return modelWantsPrefetch(model) && prefetchableSource(model)
}

// servicePrefetchRequested reports whether an InferenceService asks for its
// Model to be prefetched into its own cache claim. Read after
// inheritModelCache, so a service that inherits the Model's block inherits
// the request too.
func servicePrefetchRequested(isvc *inferencev1alpha1.InferenceService) bool {
	return isvc != nil && isvc.Spec.ModelCache != nil && isvc.Spec.ModelCache.Prefetch &&
		isvc.Spec.ModelCache.Persistence != inferencev1alpha1.ModelCachePersistenceEphemeral
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

// prefetchTarget is one cache claim the Model is staged into.
type prefetchTarget struct {
	claim string
	// own marks the Model's own target, the one that gates Ready.
	own bool
	// requestedBy lists the InferenceServices whose modelCache.prefetch
	// asked for this claim.
	requestedBy []string
}

// prefetchTargets resolves the claims to stage into: the Model's own target
// when it asks for one, plus one per claim the referencing services with
// modelCache.prefetch mount. ownReason explains why the own target fell back
// to the shared cache (empty when it did not). services must already have
// inherited the Model's modelCache (referencingServices does this).
func (r *ModelReconciler) prefetchTargets(model *inferencev1alpha1.Model, services []*inferencev1alpha1.InferenceService, listErr error, stagedClaim string) (targets []prefetchTarget, ownReason string) {
	index := map[string]int{}
	add := func(claim string, own bool, svc string) {
		i, ok := index[claim]
		if !ok {
			index[claim] = len(targets)
			targets = append(targets, prefetchTarget{claim: claim})
			i = len(targets) - 1
		}
		targets[i].own = targets[i].own || own
		if svc != "" {
			targets[i].requestedBy = append(targets[i].requestedBy, svc)
		}
	}
	if modelWantsPrefetch(model) {
		var claim string
		claim, ownReason = resolveOwnPrefetchClaim(model, services, listErr, stagedClaim)
		add(claim, true, "")
	}
	for _, svc := range services {
		if !servicePrefetchRequested(svc) {
			continue
		}
		add(modelCachePVCName(svc, r.ModelCacheMode), false, svc.Name)
	}
	return targets, ownReason
}

// resolveOwnPrefetchClaim picks the claim the Model's own prefetch stages
// into, and a reason when the choice is a fallback rather than an explicit
// or agreed target.
//
// spec.modelCache.claimName wins outright: it is the declared target, and
// prefetch never falls back from it. Otherwise a claim already staged for the
// current spec (stagedClaim) stays the target: a service that appears after
// the download completed gets a PrefetchTargetMismatch report, not a second
// download. Failing both, the resolved policy (#1676) applies: exactly one
// distinct modelCache.claimName among the referencing services names the PVC
// the serving pods mount; none, or several that disagree, means the shared
// cache, and the caller surfaces that.
func resolveOwnPrefetchClaim(model *inferencev1alpha1.Model, services []*inferencev1alpha1.InferenceService, listErr error, stagedClaim string) (claim string, reason string) {
	if c := modelCacheClaimName(model); c != "" {
		return c, ""
	}
	if stagedClaim != "" {
		return stagedClaim, ""
	}
	if listErr != nil {
		// A listing failure (RBAC denial, partitioned API server) must not
		// wedge prefetch: fall back to the shared cache, which is the
		// pre-#1676 behaviour, and let the caller surface the ignore.
		return ModelCachePVCName, "prefetch cache listing failed: " + listErr.Error()
	}
	var (
		seen     bool
		conflict bool
	)
	for _, svc := range services {
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
	switch {
	case seen && !conflict:
		return claim, ""
	case !seen:
		return ModelCachePVCName, "no InferenceService references this Model with a modelCache.claimName override; prefetching into the shared cache"
	default:
		return ModelCachePVCName, "multiple InferenceServices referencing this Model disagree on their modelCache.claimName overrides; prefetching into the shared cache"
	}
}

// referencingServices lists the InferenceServices whose modelRef names the
// Model, each with the Model's modelCache inherited so its claim resolves as
// the InferenceService controller resolves it.
func (r *ModelReconciler) referencingServices(ctx context.Context, model *inferencev1alpha1.Model) ([]*inferencev1alpha1.InferenceService, error) {
	list := &inferencev1alpha1.InferenceServiceList{}
	if err := r.List(ctx, list, client.InNamespace(model.Namespace)); err != nil {
		return nil, err
	}
	var out []*inferencev1alpha1.InferenceService
	for i := range list.Items {
		svc := &list.Items[i]
		if svc.Spec.ModelRef != model.Name {
			continue
		}
		inheritModelCache(svc, model)
		out = append(out, svc)
	}
	return out, nil
}

// prefetchFingerprint identifies what a prefetch stages: the cache directory
// (which encodes the source), the staged file set, and any pinned digests. A
// change to any of these needs a new download; a change to anything else on
// the Model (placement, tolerations, a label) does not.
func prefetchFingerprint(model *inferencev1alpha1.Model, plan *StagingPlan) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\n", effectiveModelCacheKey(model))
	if plan != nil {
		for _, f := range plan.Files {
			_, _ = fmt.Fprintf(h, "%s\n", f)
		}
	}
	keys := make([]string, 0, len(model.Spec.FileSHA256))
	for k := range model.Spec.FileSHA256 {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(h, "%s=%s\n", k, strings.ToLower(string(model.Spec.FileSHA256[k])))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// prefetchStagingPlan resolves the files a prefetch stages. A glob in
// spec.files is expanded from the repository listing when the Model has not
// recorded a current expansion, as reconcileRuntimeResolvedSource does; a
// Model whose status already carries the expansion for this generation is
// not listed again. nil plan means a single-file Model. listErr reports a
// failed listing and planErr an unresolvable file set; the caller fails the
// Model the same way the runtime-resolved path does.
func (r *ModelReconciler) prefetchStagingPlan(ctx context.Context, model *inferencev1alpha1.Model) (plan *StagingPlan, listErr, planErr error) {
	if !hasMultiFileStaging(model) {
		return nil, nil, nil
	}
	if files, err := stagedFileEntries(model); err == nil {
		plan, planErr = ResolveFileSet(files, model.Spec.Mmproj, nil)
		return plan, nil, planErr
	}
	repoFiles, err := r.repoFileListing(ctx, model)
	if err != nil {
		return nil, err, nil
	}
	plan, planErr = ResolveFileSet(model.Spec.Files, model.Spec.Mmproj, repoFiles)
	return plan, nil, planErr
}

// prefetchJobModel returns the Model the Job is built from: a copy whose
// spec.files is the expanded plan, so the storage builder stages concrete
// paths without consulting status.stagedFiles (which the serving pods trust
// only once the Model is Ready for its generation).
func prefetchJobModel(model *inferencev1alpha1.Model, plan *StagingPlan) *inferencev1alpha1.Model {
	jobModel := model.DeepCopy()
	seedPrefetchCacheKey(jobModel)
	if plan != nil {
		jobModel.Spec.Files = append([]string(nil), plan.Files...)
	}
	return jobModel
}

// findPrefetchStatus returns the entry for claim, or nil.
func findPrefetchStatus(model *inferencev1alpha1.Model, claim string) *inferencev1alpha1.PrefetchStatus {
	for i := range model.Status.Prefetch {
		if model.Status.Prefetch[i].ClaimName == claim {
			return &model.Status.Prefetch[i]
		}
	}
	return nil
}

// ensurePrefetchStatus returns the entry for claim, appending a Pending one.
func ensurePrefetchStatus(model *inferencev1alpha1.Model, claim string) *inferencev1alpha1.PrefetchStatus {
	if e := findPrefetchStatus(model, claim); e != nil {
		return e
	}
	model.Status.Prefetch = append(model.Status.Prefetch, inferencev1alpha1.PrefetchStatus{
		ClaimName: claim,
		Phase:     inferencev1alpha1.PrefetchPhasePending,
	})
	return &model.Status.Prefetch[len(model.Status.Prefetch)-1]
}

// setPrefetchPhase moves an entry to phase, stamping the transition time only
// when the phase changes so a steady state does not rewrite status.
func setPrefetchPhase(entry *inferencev1alpha1.PrefetchStatus, phase, message string) {
	if entry.Phase != phase {
		now := metav1.Now()
		entry.LastTransitionTime = &now
	}
	entry.Phase = phase
	entry.Message = message
}

// prefetchTargetOutcome is what one target's reconcile asks of the Model: a
// requeue to observe progress, and the Progressing reason the own target's
// state maps to.
type prefetchTargetOutcome struct {
	requeue time.Duration
	reason  string
}

// reconcilePrefetch drives the prefetch state machine. handled=true means
// the prefetch path owns this reconcile and Reconcile should return its
// result; handled=false means fall through to the normal source dispatch,
// with result.RequeueAfter set when a service-requested prefetch is still
// running and needs polling.
func (r *ModelReconciler) reconcilePrefetch(ctx context.Context, model *inferencev1alpha1.Model) (bool, ctrl.Result, error) {
	before := model.Status.DeepCopy()
	handled, result, err := r.reconcilePrefetchState(ctx, model)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	if !equality.Semantic.DeepEqual(before, &model.Status) {
		if err := r.Status().Update(ctx, model); err != nil {
			return true, ctrl.Result{}, err
		}
	}
	return handled, result, nil
}

// reconcilePrefetchState mutates the Model's status in memory; the caller
// persists it once if it changed.
func (r *ModelReconciler) reconcilePrefetchState(ctx context.Context, model *inferencev1alpha1.Model) (bool, ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	wantsOwn := modelWantsPrefetch(model)

	// A source the downloader does not stage has nothing to prefetch. Say so
	// when asked, rather than leaving spec.prefetch silently inert.
	if !prefetchableSource(model) {
		if wantsOwn {
			r.setPrefetchSkipped(model)
		} else {
			meta.RemoveStatusCondition(&model.Status.Conditions, ConditionPrefetchSkipped)
		}
		return false, ctrl.Result{}, nil
	}
	meta.RemoveStatusCondition(&model.Status.Conditions, ConditionPrefetchSkipped)

	services, listErr := r.referencingServices(ctx, model)
	if listErr != nil {
		logger.Info("Listing InferenceServices for prefetch targets failed; service-requested targets are unknown this pass", "error", listErr.Error())
	}
	requested := wantsOwn
	for _, svc := range services {
		requested = requested || servicePrefetchRequested(svc)
	}
	if !requested {
		r.pruneStalePrefetch(ctx, model, nil, "")
		meta.RemoveStatusCondition(&model.Status.Conditions, ConditionPrefetchTargetMismatch)
		return false, ctrl.Result{}, nil
	}

	plan, listingErr, planErr := r.prefetchStagingPlan(ctx, model)
	switch {
	case listingErr != nil:
		// The same failure, reported the same way, as the runtime-resolved
		// path: without the listing neither a Job nor a serving pod can stage
		// the files.
		model.Status.Phase = PhaseFailed
		model.Status.StagedFiles = nil
		setCondition(model, ConditionDegraded, metav1.ConditionTrue, "RepoListingFailed",
			"spec.files has a glob and listing the source repository failed: "+listingErr.Error())
		return true, ctrl.Result{RequeueAfter: time.Minute}, nil
	case planErr != nil:
		model.Status.Phase = PhaseFailed
		setCondition(model, ConditionDegraded, metav1.ConditionTrue, "InvalidFileSet", planErr.Error())
		return true, ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	// The key is part of the fingerprint, so it is settled before the
	// fingerprint is taken; the Ready written below carries it too.
	seedPrefetchCacheKey(model)
	fingerprint := prefetchFingerprint(model, plan)
	jobModel := prefetchJobModel(model, plan)
	stagedClaim := ""
	for _, e := range model.Status.Prefetch {
		if e.Phase == inferencev1alpha1.PrefetchPhaseSucceeded && e.Fingerprint == fingerprint {
			stagedClaim = e.ClaimName
			break
		}
	}
	targets, ownReason := r.prefetchTargets(model, services, listErr, stagedClaim)

	var (
		requeue    time.Duration
		ownOutcome prefetchTargetOutcome
		ownEntry   *inferencev1alpha1.PrefetchStatus
	)
	for _, t := range targets {
		entry := ensurePrefetchStatus(model, t.claim)
		entry.CacheKey = effectiveModelCacheKey(model)
		outcome, err := r.reconcilePrefetchTarget(ctx, model, jobModel, t, fingerprint, entry)
		if err != nil {
			return true, ctrl.Result{}, err
		}
		requeue = earliestPositive(requeue, outcome.requeue)
		if t.own {
			ownOutcome = outcome
			ownEntry = findPrefetchStatus(model, t.claim)
		}
	}
	r.pruneStalePrefetch(ctx, model, targets, fingerprint)
	r.setPrefetchTargetMismatch(model, services, targets)

	if !wantsOwn {
		return false, ctrl.Result{RequeueAfter: requeue}, nil
	}
	return r.gateModelOnOwnPrefetch(ctx, model, plan, ownEntry, ownOutcome, ownReason, requeue)
}

// gateModelOnOwnPrefetch maps the Model's own target to the Model's phase: a
// Model that asked to be prefetched is Downloading until its target succeeds,
// Ready after, and Failed or Pending when the Job is.
func (r *ModelReconciler) gateModelOnOwnPrefetch(ctx context.Context, model *inferencev1alpha1.Model, plan *StagingPlan, entry *inferencev1alpha1.PrefetchStatus, outcome prefetchTargetOutcome, ownReason string, requeue time.Duration) (bool, ctrl.Result, error) {
	switch entry.Phase {
	case inferencev1alpha1.PrefetchPhaseSucceeded:
		gen, written := availableObservedGeneration(model)
		if model.Status.Phase == PhaseReady && written && gen == model.Generation {
			// Completed earlier: the cache is warm and status is terminal.
			// Fall through so the ordinary remote-source reconcile keeps
			// owning Ready (revalidation, accelerator checks).
			return false, ctrl.Result{RequeueAfter: requeue}, nil
		}
		r.completePrefetch(ctx, model, plan)
		return true, ctrl.Result{}, nil
	case inferencev1alpha1.PrefetchPhaseFailed:
		model.Status.Phase = PhaseFailed
		setCondition(model, ConditionProgressing, metav1.ConditionFalse, ReasonPrefetchFailed, entry.Message)
		return true, ctrl.Result{}, nil
	case inferencev1alpha1.PrefetchPhasePending:
		model.Status.Phase = "Pending"
		reason := outcome.reason
		if reason == "" {
			reason = ReasonPrefetchRunning
		}
		setCondition(model, ConditionProgressing, metav1.ConditionFalse, reason, entry.Message)
		return true, ctrl.Result{RequeueAfter: requeue}, nil
	default:
		if model.Status.Phase != PhaseDownloading {
			model.Status.Phase = PhaseDownloading
			message := fmt.Sprintf("Started prefetch job downloading model into %q", entry.ClaimName)
			if ownReason != "" {
				message += " (" + ownReason + ")"
			}
			if outcome.reason == ReasonPrefetchRunning {
				message = "Prefetch job downloading model into the model cache"
			}
			setCondition(model, ConditionProgressing, metav1.ConditionTrue, outcome.reason, message)
		}
		return true, ctrl.Result{RequeueAfter: requeue}, nil
	}
}

// reconcilePrefetchTarget brings one target's Job and status entry in line
// with the desired fingerprint.
func (r *ModelReconciler) reconcilePrefetchTarget(ctx context.Context, model, jobModel *inferencev1alpha1.Model, t prefetchTarget, fingerprint string, entry *inferencev1alpha1.PrefetchStatus) (prefetchTargetOutcome, error) {
	logger := logf.FromContext(ctx)

	// Already staged for this spec. The Job may be gone (TTL); the entry is
	// the record. Only the generation moves.
	if entry.Phase == inferencev1alpha1.PrefetchPhaseSucceeded && entry.Fingerprint == fingerprint {
		entry.ObservedGeneration = model.Generation
		return prefetchTargetOutcome{reason: ReasonPrefetchComplete}, nil
	}

	jobName := entry.JobName
	if jobName == "" {
		if t.own {
			jobName = prefetchJobName(model)
		} else {
			jobName = prefetchTargetJobName(model, t.claim)
		}
	}

	// Bounded read. The controller does not watch Jobs, so this is the first
	// touch of the type and controller-runtime starts the informer lazily and
	// waits for it to sync. When that informer can never list -- an RBAC
	// denial like #1593, a partitioned API server -- the reflector retries
	// forever and an unbounded Get holds the worker. The Model controller's
	// workers are a shared resource, so that stalls every other Model, not
	// just this one (#1597). A timeout turns it into a prompt error and an
	// ordinary backoff requeue, which self-heals once the informer can sync.
	getCtx, cancel := context.WithTimeout(ctx, r.prefetchJobReadTimeout())
	defer cancel()

	job := &batchv1.Job{}
	err := r.Get(getCtx, types.NamespacedName{Name: jobName, Namespace: model.Namespace}, job)
	switch {
	case apierrors.IsNotFound(err):
		return r.startPrefetchTarget(ctx, model, jobModel, t, fingerprint, entry, jobName)
	case err != nil:
		return prefetchTargetOutcome{}, fmt.Errorf("checking prefetch job: %w", err)
	}

	// The name is the only key, so a Job this Model does not own is a
	// foreign object: neither adopted as the prefetch nor deleted to make
	// room. Report it and leave it alone.
	if !metav1.IsControlledBy(job, model) {
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhaseFailed,
			fmt.Sprintf("job %q exists but is not owned by this Model; remove or rename it to prefetch into %q", job.Name, t.claim))
		return prefetchTargetOutcome{reason: ReasonPrefetchFailed}, nil
	}

	if job.DeletionTimestamp != nil {
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhasePending,
			fmt.Sprintf("waiting for the replaced prefetch job %q to finish deleting", job.Name))
		return prefetchTargetOutcome{requeue: prefetchRetryInterval, reason: ReasonPrefetchRunning}, nil
	}

	// A Job built for another spec stages the wrong files, and one built for
	// another claim stages them in the wrong place, whether it is still
	// running or already finished: replace it. A Job without the annotations
	// predates them and is adopted as current, so an upgrade never kills a
	// download in flight.
	builtFor, hasFingerprint := job.Annotations[annotationPrefetchFingerprint]
	builtInto, hasClaim := job.Annotations[annotationPrefetchClaim]
	if (hasFingerprint && builtFor != fingerprint) || (hasClaim && builtInto != t.claim) {
		logger.Info("Prefetch job was built for an earlier spec or another claim; replacing it", "job", job.Name, "claim", t.claim, "builtInto", builtInto)
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return prefetchTargetOutcome{}, fmt.Errorf("deleting stale prefetch job: %w", err)
		}
		entry.JobName = job.Name
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhasePending,
			fmt.Sprintf("replacing prefetch job %q, which was built for an earlier spec", job.Name))
		return prefetchTargetOutcome{requeue: prefetchRetryInterval, reason: ReasonPrefetchRunning}, nil
	}

	entry.JobName = job.Name
	entry.Fingerprint = fingerprint
	entry.ObservedGeneration = model.Generation
	switch {
	case jobSucceeded(job):
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhaseSucceeded, "")
		return prefetchTargetOutcome{reason: ReasonPrefetchComplete}, nil
	case jobFailed(job):
		logger.Info("Prefetch job failed", "job", job.Name, "claim", t.claim)
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhaseFailed,
			fmt.Sprintf("prefetch job %q failed; see its pod logs", job.Name))
		return prefetchTargetOutcome{reason: ReasonPrefetchFailed}, nil
	default:
		// Still running: reflect progress and poll. The Job's completion
		// does not generate a Model event, so a modest requeue keeps the
		// status honest without watching Jobs from this controller.
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhaseRunning,
			fmt.Sprintf("prefetch job %q downloading into %q", job.Name, t.claim))
		return prefetchTargetOutcome{requeue: prefetchPollInterval, reason: ReasonPrefetchRunning}, nil
	}
}

// startPrefetchTarget makes sure the target claim exists (the shared cache is
// provisioned; any other claim must already exist) and creates the Job.
func (r *ModelReconciler) startPrefetchTarget(ctx context.Context, model, jobModel *inferencev1alpha1.Model, t prefetchTarget, fingerprint string, entry *inferencev1alpha1.PrefetchStatus, jobName string) (prefetchTargetOutcome, error) {
	logger := logf.FromContext(ctx)

	if t.claim == ModelCachePVCName {
		if err := ensureSharedModelCachePVC(ctx, r.Client, model.Namespace,
			r.ModelCacheSize, r.ModelCacheClass, r.ModelCacheAccessMode); err != nil {
			return prefetchTargetOutcome{}, fmt.Errorf("ensuring shared model cache PVC: %w", err)
		}
	} else {
		// A user-owned claim, or a perService claim the InferenceService
		// controller provisions. A Job pod mounting a claim that does not
		// exist sits Pending forever, neither completing nor failing, so hold
		// the Job and name the claim instead.
		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, types.NamespacedName{Name: t.claim, Namespace: model.Namespace}, pvc); err != nil {
			if !apierrors.IsNotFound(err) {
				return prefetchTargetOutcome{}, fmt.Errorf("checking prefetch cache claim %q: %w", t.claim, err)
			}
			setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhasePending,
				fmt.Sprintf("cache claim %q does not exist in namespace %q; the prefetch job starts once it does", t.claim, model.Namespace))
			return prefetchTargetOutcome{requeue: prefetchClaimWait, reason: ReasonPrefetchClaimNotFound}, nil
		}
	}

	job, err := r.buildPrefetchJob(ctx, jobModel, t.claim, fingerprint)
	if err != nil {
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhaseFailed, err.Error())
		return prefetchTargetOutcome{reason: ReasonPrefetchFailed}, nil
	}
	job.Name = jobName

	// The Job pod mounts the CA ConfigMap from the Model's namespace like the
	// serving pod does. Without it the pod wedges in ContainerCreating, the
	// Job neither completes nor fails, and the Model reads Downloading
	// forever. Hold the Job and name the cause instead.
	required, caMissing, err := caCertConfigMapMissing(ctx, r.Client, r.CACertConfigMap, model.Namespace, &job.Spec.Template.Spec)
	if err != nil {
		return prefetchTargetOutcome{}, err
	}
	transitioned := setCACertConfigMapCondition(&model.Status.Conditions, model.Generation, r.CACertConfigMap, model.Namespace, required, caMissing)
	if caMissing {
		msg := caCertConfigMapMissingMessage(r.CACertConfigMap, model.Namespace)
		if transitioned && r.Recorder != nil {
			r.Recorder.Eventf(model, nil, corev1.EventTypeWarning, ReasonCACertConfigMapMissing, "Prefetch", "%s", msg)
		}
		logger.Info("CA ConfigMap missing in the Model namespace; not creating the prefetch Job",
			"configMap", r.CACertConfigMap, "namespace", model.Namespace)
		setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhasePending, msg)
		return prefetchTargetOutcome{requeue: caCertConfigMapMissingRequeue, reason: ReasonCACertConfigMapMissing}, nil
	}

	if err := controllerutil.SetControllerReference(model, job, r.Scheme); err != nil {
		return prefetchTargetOutcome{}, fmt.Errorf("owner-ref prefetch job: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Raced with a concurrent reconcile; the poll branch takes over.
			return prefetchTargetOutcome{requeue: prefetchRetryInterval, reason: ReasonPrefetchRunning}, nil
		}
		return prefetchTargetOutcome{}, fmt.Errorf("creating prefetch job: %w", err)
	}

	logger.Info("Created prefetch job", "job", job.Name, "claim", t.claim, "requestedBy", t.requestedBy)
	entry.JobName = job.Name
	entry.Fingerprint = fingerprint
	entry.ObservedGeneration = model.Generation
	setPrefetchPhase(entry, inferencev1alpha1.PrefetchPhaseRunning,
		fmt.Sprintf("prefetch job %q downloading into %q", job.Name, t.claim))
	return prefetchTargetOutcome{requeue: prefetchPollInterval, reason: ReasonPrefetchStarted}, nil
}

// pruneStalePrefetch drops entries no target wants any more. A Job still
// running for an abandoned target is deleted: the service that asked moved
// to another claim, or was removed. A Succeeded entry whose fingerprint is
// current is kept as the record of where the files are.
func (r *ModelReconciler) pruneStalePrefetch(ctx context.Context, model *inferencev1alpha1.Model, targets []prefetchTarget, fingerprint string) {
	wanted := map[string]bool{}
	for _, t := range targets {
		wanted[t.claim] = true
	}
	kept := model.Status.Prefetch[:0]
	for _, entry := range model.Status.Prefetch {
		switch {
		case wanted[entry.ClaimName]:
			kept = append(kept, entry)
		case entry.Phase == inferencev1alpha1.PrefetchPhaseSucceeded && entry.Fingerprint == fingerprint:
			kept = append(kept, entry)
		default:
			if entry.JobName != "" && entry.Phase != inferencev1alpha1.PrefetchPhaseSucceeded {
				r.deleteOwnedPrefetchJob(ctx, model, entry.JobName, entry.ClaimName)
			}
		}
	}
	if len(kept) == 0 {
		kept = nil
	}
	model.Status.Prefetch = kept
}

// deleteOwnedPrefetchJob deletes the Job of an abandoned target, after
// checking the object at that name is one this Model owns: the name is
// deterministic, so a user's Job could sit at it.
func (r *ModelReconciler) deleteOwnedPrefetchJob(ctx context.Context, model *inferencev1alpha1.Model, name, claim string) {
	logger := logf.FromContext(ctx)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: model.Namespace}, job); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Info("Could not read the prefetch job of an abandoned target; it finishes on its own", "job", name, "error", err.Error())
		}
		return
	}
	if !metav1.IsControlledBy(job, model) {
		logger.Info("Job at the prefetch name is not owned by this Model; leaving it", "job", name)
		return
	}
	if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		logger.Info("Could not delete the prefetch job of an abandoned target; it finishes on its own", "job", name, "error", err.Error())
		return
	}
	logger.Info("Deleted the prefetch job of an abandoned target", "job", name, "claim", claim)
}

// setPrefetchSkipped records that spec.prefetch is set on a source the
// downloader does not stage, and says so once as an event.
func (r *ModelReconciler) setPrefetchSkipped(model *inferencev1alpha1.Model) {
	msg := fmt.Sprintf("spec.prefetch is set but source %q is not a remote artifact the downloader stages; local paths, pvc://, oci:// and s3:// sources are served without a prefetch", model.Spec.Source)
	changed := meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
		Type:               ConditionPrefetchSkipped,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: model.Generation,
		Reason:             ReasonSourceNotPrefetchable,
		Message:            msg,
	})
	if changed && r.Recorder != nil {
		r.Recorder.Eventf(model, nil, corev1.EventTypeWarning, ConditionPrefetchSkipped, "Prefetch", "%s", msg)
	}
}

// setPrefetchTargetMismatch reports the referencing services whose pods mount
// a claim no prefetch of this Model targets. Only a Model that asked to be
// prefetched is held to it: without spec.prefetch, downloading at serve time
// is the documented behaviour.
func (r *ModelReconciler) setPrefetchTargetMismatch(model *inferencev1alpha1.Model, services []*inferencev1alpha1.InferenceService, targets []prefetchTarget) {
	if !modelWantsPrefetch(model) {
		meta.RemoveStatusCondition(&model.Status.Conditions, ConditionPrefetchTargetMismatch)
		return
	}
	staged := map[string]bool{}
	for _, t := range targets {
		staged[t.claim] = true
	}
	var mismatched []string
	for _, svc := range services {
		if servicePrefetchRequested(svc) || modelCacheIsEphemeral(svc) || isMetalModel(model) {
			continue
		}
		claim := modelCachePVCName(svc, r.ModelCacheMode)
		if !staged[claim] {
			mismatched = append(mismatched, fmt.Sprintf("%s (claim %q)", svc.Name, claim))
		}
	}
	if len(mismatched) == 0 {
		meta.RemoveStatusCondition(&model.Status.Conditions, ConditionPrefetchTargetMismatch)
		return
	}
	meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
		Type:               ConditionPrefetchTargetMismatch,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: model.Generation,
		Reason:             ReasonPrefetchClaimMismatch,
		Message: "InferenceService " + strings.Join(mismatched, ", ") +
			" mounts a cache this Model was not prefetched into and downloads at serve time;" +
			" set modelCache.prefetch on the service, or modelCache.claimName on the Model",
	})
}

// completePrefetch marks the Model Ready with its cache key and the staged
// file set so a referencing InferenceService takes the cache-hit path.
func (r *ModelReconciler) completePrefetch(ctx context.Context, model *inferencev1alpha1.Model, plan *StagingPlan) {
	model.Status.Phase = PhaseReady
	seedPrefetchCacheKey(model)
	if plan != nil {
		model.Status.StagedFiles = plan.Files
	} else {
		model.Status.StagedFiles = nil
	}
	model.Status.AcceleratorReady = r.checkAcceleratorAvailability(ctx, model)
	now := metav1.Now()
	model.Status.LastUpdated = &now
	setCondition(model, ConditionProgressing, metav1.ConditionFalse, ReasonPrefetchComplete, "Prefetch job completed")
	// A Degraded left by an earlier listing or Job failure is cleared, as
	// clearProgressConditions does; none is added where none was.
	if meta.FindStatusCondition(model.Status.Conditions, ConditionDegraded) != nil {
		setCondition(model, ConditionDegraded, metav1.ConditionFalse, ReasonPrefetchComplete, "Prefetch job completed")
	}
	setCondition(model, ConditionAvailable, metav1.ConditionTrue, ReasonModelPrefetched, "Model prefetched into the model cache")
}

// buildPrefetchJob assembles the download Job for one claim. The pod reuses
// the serving path's init containers and volumes verbatim (a synthetic
// InferenceService whose only field is the claim: the other isvc-derived
// inputs are an optional fsGroup override and the ephemeral budget, both
// unset) and adds a no-op completion container, since a Job pod must have at
// least one non-init container.
//
// ctx is needed only to read HF_ENDPOINT from the Model's sourceSecretRef, so
// a prefetch from a mirror carries the bearer token as the serving pod does
// (#1900).
func (r *ModelReconciler) buildPrefetchJob(ctx context.Context, model *inferencev1alpha1.Model, claim, fingerprint string) (*batchv1.Job, error) {
	if effectiveModelCacheKey(model) == "" {
		return nil, fmt.Errorf("prefetch: model has no cache key; refusing to build a Job that would download into an emptyDir")
	}
	var target *inferencev1alpha1.InferenceService
	if claim != "" && claim != ModelCachePVCName {
		target = &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: model.Name, Namespace: model.Namespace},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name, ModelCache: &inferencev1alpha1.ModelCacheSpec{ClaimName: claim}},
		}
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
		podSecurity = &corev1.PodSecurityContext{FSGroup: &fs, FSGroupChangePolicy: fsGroupChangeOnRootMismatch()}
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
			Annotations: map[string]string{
				annotationPrefetchFingerprint: fingerprint,
				annotationPrefetchClaim:       prefetchTargetName(target),
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
					// lands on (#1964). Affinity says what a selector cannot:
					// off the GPU nodes, or a preference (#2136).
					NodeSelector:   model.Spec.PrefetchNodeSelector,
					Affinity:       model.Spec.PrefetchAffinity,
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

// prefetchTargetName returns the PVC name prefetch will stage into, for status
// messages: the resolved per-service claim, or the shared cache PVC.
func prefetchTargetName(target *inferencev1alpha1.InferenceService) string {
	if claim := userModelCacheClaimName(target); claim != "" {
		return claim
	}
	return ModelCachePVCName
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

// findModelForInferenceService maps an InferenceService to the Model it
// references, so a service created or retargeted after its Model reconciles
// the Model: the prefetch target follows the services that will read it
// (#2136). Registered with a generation predicate, so a service's status
// writes do not fan out into Model reconciles.
func (r *ModelReconciler) findModelForInferenceService(_ context.Context, obj client.Object) []reconcile.Request {
	isvc, ok := obj.(*inferencev1alpha1.InferenceService)
	if !ok || isvc.Spec.ModelRef == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: isvc.Spec.ModelRef, Namespace: isvc.Namespace},
	}}
}
