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
	"fmt"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/cachekey"
)

type deleteOptions struct {
	name       string
	namespace  string
	purgeCache bool
}

func NewDeleteCommand() *cobra.Command {
	opts := &deleteOptions{}

	cmd := &cobra.Command{
		Use:     "delete [NAME]",
		Aliases: []string{"del", "rm"},
		Short:   "Delete an LLM deployment",
		Long:    `Delete both the Model and InferenceService resources for a deployment.`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.name = args[0]
			return runDelete(opts)
		},
	}

	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().BoolVar(&opts.purgeCache, "purge-cache", false,
		"Also purge the model from the persistent cache when deleting the deployment")

	return cmd
}

// Test seams so runDelete can be driven against a fake client and clientset.
var (
	deleteKubeConfig = config.GetConfig
	deleteClientset  = func(cfg *rest.Config) (kubernetes.Interface, error) { return kubernetes.NewForConfig(cfg) }
	newDeleteClient  = func(cfg *rest.Config) (deleteCRClient, error) {
		return client.New(cfg, client.Options{Scheme: scheme.Scheme})
	}
)

// deleteCRClient is the subset of the controller-runtime client delete uses.
type deleteCRClient = client.Client

func runDelete(opts *deleteOptions) error {
	ctx := context.Background()

	cfg, err := deleteKubeConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add scheme: %w", err)
	}

	k8sClient, err := newDeleteClient(cfg)
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}

	var cacheKey string
	var cacheTargets []purgeTarget
	var cacheSvcs []inferencev1alpha1.InferenceService
	var cacheResolveErr error
	var modelClaim string
	var stagedClaims []string
	if opts.purgeCache {
		// Resolve the key, the Model's own claim and the claims before
		// deleting: the Model has no finalizer, so nothing of it can be read
		// after, and in perService mode the claim is owner-ref'd to the
		// InferenceService and is collected with it.
		purgeModel := &inferencev1alpha1.Model{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: opts.name, Namespace: opts.namespace}, purgeModel); err != nil {
			fmt.Printf("Warning: failed to get Model for cache purge: %v\n", err)
		} else if cacheKey = cachekey.EffectiveKey(purgeModel); cacheKey != "" {
			modelClaim, stagedClaims = modelCacheClaimName(purgeModel), prefetchedClaims(purgeModel)
			cacheTargets, cacheSvcs, cacheResolveErr = resolvePurgeTargets(ctx, k8sClient, opts.namespace)
		}
	}

	fmt.Printf("Deleting InferenceService '%s' in namespace '%s'...\n", opts.name, opts.namespace)
	inferenceService := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.name,
			Namespace: opts.namespace,
		},
	}
	if err := k8sClient.Delete(ctx, inferenceService); err != nil {
		fmt.Printf("Warning: failed to delete InferenceService: %v\n", err)
	} else {
		fmt.Printf("✓ InferenceService '%s' deleted\n", opts.name)
	}

	fmt.Printf("Deleting Model '%s'...\n", opts.name)
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.name,
			Namespace: opts.namespace,
		},
	}

	if err := k8sClient.Delete(ctx, model); err != nil {
		fmt.Printf("Warning: failed to delete Model: %v\n", err)
	} else {
		fmt.Printf("✓ Model '%s' deleted\n", opts.name)
	}

	if opts.purgeCache && cacheKey == "" {
		fmt.Printf("⚠️  Model has no cache key; nothing to purge\n")
	}

	// The InferenceService stays mounted on its claim until it is gone, so its
	// deletion (issued above) is what frees a strict RWO claim for the purge.
	if opts.purgeCache && cacheKey != "" {
		if cacheResolveErr != nil {
			return fmt.Errorf("deployment deleted, but purging the cache failed: %w; "+
				"run 'llmkube cache clear -n %s' to clear what remains", cacheResolveErr, opts.namespace)
		}
		targets := targetedClaims(cacheTargets, cacheSvcs, opts.name, modelClaim, stagedClaims)
		var owned, purged []purgeTarget
		for _, target := range targets {
			// The operator garbage-collects a claim owner-ref'd to the service
			// just deleted, and a pod mounting a terminating claim never
			// schedules.
			if target.InferenceService == opts.name {
				owned = append(owned, target)
				continue
			}
			purged = append(purged, target)
		}
		for _, target := range owned {
			fmt.Printf("Cache PVC '%s' is removed with InferenceService '%s'; not purging it separately\n",
				target.Name, opts.name)
		}
		if len(purged) == 0 {
			if len(owned) == 0 {
				fmt.Printf("⚠️  No model cache PVC found in namespace '%s'; nothing to purge\n", opts.namespace)
			}
			fmt.Printf("\n✓ Deployment '%s' deleted successfully\n", opts.name)
			return nil
		}
		clientset, err := deleteClientset(cfg)
		if err != nil {
			return fmt.Errorf("deployment deleted, but purging the cache failed: %w", err)
		}
		fmt.Printf("🗑️  Purging model cache (cache key: %s)...\n", cacheKey)
		if err := purgeDeleteTargets(ctx, cfg, clientset, opts.namespace, cacheKey, purged); err != nil {
			return fmt.Errorf("deployment deleted, but purging the cache failed: %w; "+
				"run 'llmkube cache clear -n %s' to clear what remains", err, opts.namespace)
		}
	}

	fmt.Printf("\n✓ Deployment '%s' deleted successfully\n", opts.name)
	return nil
}

// purgeDeleteTargets removes cacheKey from every resolved claim, purging each
// claim on its own so one failure is reported with the claim it belongs to.
func purgeDeleteTargets(
	ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface,
	namespace, cacheKey string, targets []purgeTarget,
) error {
	if len(targets) == 0 {
		return fmt.Errorf("no model cache PVC found")
	}

	var errs []error
	var outcomes []purgeOutcome
	for _, target := range targets {
		outcome, err := purgeCacheKeys(ctx, cfg, clientset, namespace, target.Name, []string{cacheKey})
		if err != nil {
			errs = append(errs, fmt.Errorf("cache PVC %s: %w", target.Name, err))
			continue
		}
		outcomes = append(outcomes, outcome)
	}
	reportPurgeOutcomes(outcomes)
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if keysAbsentEverywhere([]string{cacheKey}, outcomes) {
		fmt.Printf("%s is not on any cache claim in namespace '%s'.\n", cacheKey, namespace)
	}
	return nil
}
