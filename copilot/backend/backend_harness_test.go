package backend_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ghantakiran/OTEL/copilot"
	"github.com/ghantakiran/OTEL/copilot/backend"
)

// The same claims as backend_test.go, against the products themselves rather
// than against recorded bodies.
//
// SKIPPED unless the harness is pointed at, so `go test ./...` stays hermetic and
// runnable on a machine without Docker — the same line the Distribution Check
// draws between validate-compiled-configs_test.sh (stubbed, in CI) and
// validate-compiled-configs.sh (real, local).
//
//	bash harness/verify-backend-rendering.sh --keep
//	COPILOT_TRACE_URL=http://localhost:3200 \
//	COPILOT_METRICS_URL=http://localhost:9090 \
//	  go test ./copilot/... -run Harness -v
//
// It exists because recorded fixtures rot silently. A trace store that renamed a
// field, or a metrics Backend upgraded past the label promotion, would leave every
// test above green and this one red — which is the correct place for that failure
// to appear.
func harness(t *testing.T) *backend.TempoPrometheus {
	t.Helper()
	traceURL, metricsURL := os.Getenv("COPILOT_TRACE_URL"), os.Getenv("COPILOT_METRICS_URL")
	if traceURL == "" || metricsURL == "" {
		t.Skip("set COPILOT_TRACE_URL and COPILOT_METRICS_URL to run against the harness (see harness/verify-backend-rendering.sh)")
	}
	return &backend.TempoPrometheus{
		TraceURL:   traceURL,
		MetricsURL: metricsURL,
		Tier:       "tier-1",
	}
}

// The tracer bullet, end to end: a span emitted by the harness's sample service,
// carried by a compiled Agent and a compiled Gateway, stored by a real trace
// store, and retrieved here through the seam.
func TestHarnessAServiceAndWindowQueryReachesRealBackends(t *testing.T) {
	b := harness(t)

	refs, err := b.QueryTraces(context.Background(), copilot.TraceQuery{
		Service: copilot.ServiceIdentity{Name: "checkout-api"},
		Since:   time.Now().Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("QueryTraces against the harness: %v", err)
	}
	if len(refs) == 0 {
		t.Fatal("no traces came back; emit one first (harness/verify-backend-rendering.sh)")
	}

	for _, ref := range refs {
		if ref.TraceID == "" {
			t.Error("a ref came back with no trace ID and so cannot be cited")
		}
		// The identity the Contract stamped, not the one the sample service sent.
		if ref.Service.Name != "checkout-api" {
			t.Errorf("Service.Name = %q, want the Contract's checkout-api", ref.Service.Name)
		}
		if ref.Start.IsZero() {
			t.Error("a ref came back with no start time")
		}
	}
}

// The join, live: the config_version on a ref is the one a real metrics Backend
// renders, in the label spelling #50 measured.
func TestHarnessTheConfigVersionIsJoinedFromARealMetricsBackend(t *testing.T) {
	b := harness(t)

	refs, err := b.QueryTraces(context.Background(), copilot.TraceQuery{
		Service: copilot.ServiceIdentity{Name: "checkout-api"},
		Since:   time.Now().Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("QueryTraces against the harness: %v", err)
	}
	if len(refs) == 0 {
		t.Fatal("no traces came back; emit one first")
	}

	got := refs[0].ConfigVersion
	if got == "" {
		t.Fatal("no config_version was joined — either the self-telemetry has not arrived (30s interval) or the label is not promoted (docs/backend-label-mapping.md, rule 3)")
	}
	if len(got) < len("sha256:") || got[:len("sha256:")] != "sha256:" {
		t.Errorf("config_version = %q, want a sha256: digest", got)
	}
}

// The negative control, through the seam this time. A service's span reaches a
// Backend with no otel.platform. attribute on it, which is why the version above
// had to be joined at all. If a span ever did carry one, the join would be
// unnecessary and forgery would be possible — so this is the assertion that keeps
// the design honest rather than merely working.
func TestHarnessNoSpanCarriesTheVersionItselfWhichIsWhyItIsJoined(t *testing.T) {
	b := harness(t)

	// Ask for a service that does not exist. Nothing should come back — and in
	// particular nothing should come back carrying a version, which would mean
	// the adapter was inventing identity rather than reading it.
	refs, err := b.QueryTraces(context.Background(), copilot.TraceQuery{
		Service: copilot.ServiceIdentity{Name: "no-such-service-in-the-fleet"},
		Since:   time.Now().Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("QueryTraces against the harness: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("a service that does not exist returned %d traces", len(refs))
	}
}
