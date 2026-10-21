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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// ReasonHFMirrorNotApplied is the Warning event reason for a runtime that
// downloads its own Model while the Model's sourceSecretRef names a mirror the
// runtime cannot be pointed at without sending a token the Secret does not
// bind to it.
const ReasonHFMirrorNotApplied = "HFMirrorNotApplied"

// operatorStagesModel reports whether the operator stages the Model into the
// serving pod. It stages when the runtime stages by default, or when
// spec.stageModel opts a runtime that does not into the same staging (#1961);
// skipModelInit always opts out. When it does not, the serving container
// fetches the Model itself.
func operatorStagesModel(isvc *inferencev1alpha1.InferenceService, backend RuntimeBackend) bool {
	if isvc.Spec.SkipModelInit != nil && *isvc.Spec.SkipModelInit {
		return false
	}
	return backend.NeedsModelInit() || (isvc.Spec.StageModel != nil && *isvc.Spec.StageModel)
}

// runtimeHFTokenRef returns the runtime's own Hugging Face token reference
// (<runtime>Config.hfTokenSecretRef), and whether the runtime downloads a bare
// repo id with huggingface_hub, which honours HF_ENDPOINT. Runtimes outside
// this list either never fetch a repo id or do not read HF_ENDPOINT.
func runtimeHFTokenRef(backend RuntimeBackend, isvc *inferencev1alpha1.InferenceService) (*corev1.SecretKeySelector, bool) {
	switch backend.(type) {
	case *VLLMBackend:
		if c := isvc.Spec.VLLMConfig; c != nil {
			return c.HFTokenSecretRef, true
		}
		return nil, true
	case *SGLangBackend:
		if c := isvc.Spec.SGLangConfig; c != nil {
			return c.HFTokenSecretRef, true
		}
		return nil, true
	case *TGIBackend:
		if c := isvc.Spec.TGIConfig; c != nil {
			return c.HFTokenSecretRef, true
		}
		return nil, true
	}
	return nil, false
}

// runtimeHFMirrorEnv points a runtime that downloads its own Model at the
// Hugging Face mirror the Model's sourceSecretRef names (HF_ENDPOINT, #1900).
// The operator's downloader already honours that mirror, but a bare repo id
// served with skipModelInit (or by TGI) never passes through it, so without
// this the runtime goes to huggingface.co and the mirror setting is a silent
// no-op.
//
// hfEndpoint is the trimmed value hfEndpointFromSecret read for this Model,
// set as a literal: huggingface_hub strips only a trailing "/", so a Secret
// value with a trailing newline would reach it broken through a secretKeyRef.
//
// The token rule mirrors the operator side, where a token is only ever sent to
// huggingface.co or to the origin HF_ENDPOINT names in the same Secret. The
// runtime sends HF_TOKEN to HF_ENDPOINT, so:
//   - no runtime token: HF_TOKEN comes from the same Secret (optional key);
//   - a runtime token in that same Secret: it is already bound to this mirror;
//   - a runtime token in another Secret: the mirror is not applied, because
//     that token would go to an origin its Secret never named. conflict
//     reports this case so the caller can say so.
func runtimeHFMirrorEnv(backend RuntimeBackend, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, hfEndpoint string) (env []corev1.EnvVar, conflict bool) {
	if hfEndpoint == "" || model == nil || model.Spec.SourceSecretRef == nil ||
		!isHFRepoSource(model.Spec.Source) || operatorStagesModel(isvc, backend) {
		return nil, false
	}
	tokenRef, downloads := runtimeHFTokenRef(backend, isvc)
	if !downloads {
		return nil, false
	}
	secret := model.Spec.SourceSecretRef.Name
	if tokenRef != nil && tokenRef.Name != secret {
		return nil, true
	}
	env = []corev1.EnvVar{{Name: "HF_ENDPOINT", Value: hfEndpoint}}
	if tokenRef == nil {
		env = append(env, corev1.EnvVar{
			Name: "HF_TOKEN",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret},
				Key:                  "HF_TOKEN",
				Optional:             ptr.To(true),
			}},
		})
	}
	return env, false
}

// hfMirrorNotAppliedMessage names the conflict and both ways out.
func hfMirrorNotAppliedMessage(model *inferencev1alpha1.Model) string {
	return "Model " + model.Name + "'s sourceSecretRef (" + model.Spec.SourceSecretRef.Name +
		") names an HF_ENDPOINT mirror, but this runtime's hfTokenSecretRef names a different Secret; " +
		"the runtime downloads from huggingface.co so that token is not sent to the mirror. " +
		"Point hfTokenSecretRef at " + model.Spec.SourceSecretRef.Name + ", or unset it to use that Secret's HF_TOKEN"
}
