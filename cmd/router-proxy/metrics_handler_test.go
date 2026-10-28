package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	clientmetrics "k8s.io/client-go/tools/metrics"

	prommetrics "github.com/defilantech/llmkube/internal/metrics"
)

// The metrics endpoint must serve the registry the llmkube collectors are
// actually registered in.
//
// This is a regression test for a silent failure that shipped once already.
// promhttp.Handler() serves prometheus.DefaultGatherer, while internal/metrics
// registers the router's collectors into RouterRegistry. Wiring the
// wrong one still returns HTTP 200, with roughly 38 families of Go runtime and
// process metrics, so the scrape succeeds, the PodMonitor reports a healthy
// target, and the dashboards are just empty. Nothing logs an error, which is
// why it survived review and a merge.
//
// Asserting on a real series rather than a synthetic collector is deliberate:
// it pins the contract (this endpoint exposes the llmkube collectors) rather
// than the mechanism, so the test still means something if registration is
// refactored.
func TestMetricsHandlerServesLLMKubeCollectors(t *testing.T) {
	// Load-bearing: a labelled collector emits no family until it has at least
	// one child series, so an idle scrape is byte-identical under either
	// registry. Without touching a counter first, this test passes on the bug.
	prommetrics.RouterRequestsTotal.WithLabelValues(
		"test-router", "test-rule", "test-backend", "test-class", "success").Inc()

	rec := httptest.NewRecorder()
	newMetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "llmkube_router_requests_total") {
		t.Errorf("metrics endpoint does not expose llmkube_router_requests_total; "+
			"it is serving the wrong registry. Got %d bytes beginning: %.200s",
			len(body), body)
	}
}

// The proxy runs none of the controller's loops, so a controller collector on
// this endpoint is a zero the alerts read as real: llmkube_watch_canary_up 0
// fires LLMKubeControllerWatchCanaryDown for the router's namespace (#2294).
func TestMetricsHandlerServesOnlyProxyFamilies(t *testing.T) {
	// client-go's hook, as a real API-server call records it; the family has
	// no series until then.
	clientmetrics.RequestResult.Increment(context.Background(), "403", http.MethodGet, "kubernetes.default.svc")

	rec := httptest.NewRecorder()
	newMetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(rec.Body)
	if err != nil {
		t.Fatalf("parse scrape: %v", err)
	}

	served := []string{"llmkube_router_", "llmkube_modelpool_", "llmkube_relay_", "rest_client_", "go_", "process_"}
	for name := range families {
		if !slices.ContainsFunc(served, func(p string) bool { return strings.HasPrefix(name, p) }) {
			t.Errorf("metrics endpoint serves %s, which the router-proxy does not record", name)
		}
	}
	// The relay's and the runtime collectors are unlabelled, so present from
	// registration on.
	for _, name := range []string{
		"llmkube_relay_pin_mismatch_total", "rest_client_requests_total", "go_goroutines", "process_cpu_seconds_total",
	} {
		if _, ok := families[name]; !ok {
			t.Errorf("metrics endpoint does not serve %s", name)
		}
	}
}
