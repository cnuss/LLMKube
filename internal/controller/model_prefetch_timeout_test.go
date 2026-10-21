package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// hangingGetClient models the #1593 failure: controller-runtime starts an
// informer lazily on first Get of a type, and when that informer can never
// list (RBAC denial, missing CRD, partitioned API server) the reflector
// retries forever and the Get blocks instead of failing. Get here blocks
// until its context is done, which is exactly what the real client does.
type hangingGetClient struct {
	client.Client
}

func (c *hangingGetClient) Get(ctx context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	<-ctx.Done()
	return ctx.Err()
}

// The InferenceService listing that resolves prefetch targets is served by
// an informer the InferenceService controller already runs; only the Job
// read is the first touch of a type, so only it hangs here.
func (c *hangingGetClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return nil
}

// A prefetch-eligible Model: remote source, prefetch on, not already complete.
func prefetchModelForTimeout() *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:   "https://example.invalid/model.gguf",
			Format:   "gguf",
			Prefetch: true,
		},
	}
}

// The reconcile worker must not be held by a dependency that cannot be read.
// The Model controller runs a single worker, so a blocked Get stalls every
// other Model on the cluster, not just this one (#1597).
func TestReconcilePrefetch_DoesNotBlockOnUnreadableJob(t *testing.T) {
	const timeout = 50 * time.Millisecond
	r := &ModelReconciler{Client: &hangingGetClient{}, PrefetchJobReadTimeout: timeout}

	// Generous budget: the point is bounded-vs-unbounded, not the exact value.
	budget := timeout + 10*time.Second
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, _, err := r.reconcilePrefetch(context.Background(), prefetchModelForTimeout())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error when the Job cannot be read, got nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want a deadline-exceeded error, got %v", err)
		}
		if elapsed := time.Since(start); elapsed < timeout {
			t.Fatalf("returned after %v, before the %v timeout could have elapsed",
				elapsed, timeout)
		}
	case <-time.After(budget):
		t.Fatalf("reconcilePrefetch still blocked after %v: the worker is held "+
			"indefinitely by an unreadable Job", budget)
	}
}
