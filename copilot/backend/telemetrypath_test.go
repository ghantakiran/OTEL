package backend_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ghantakiran/OTEL/copilot"
	"github.com/ghantakiran/OTEL/copilot/backend"
)

// RECORDED, not invented — captured from the metrics Backend that
// harness/real-backends.yaml stands up, answering the exact query this adapter
// sends, with a compiled Gateway fanning out to three Backends.
//
// NOTE WHAT IS NOT IN IT. The two failure counters are ABSENT, not zero.
// Prometheus creates no series for a counter that has never incremented, and
// nothing in the harness has ever driven a queue to capacity (#49). That absence
// is the whole reason for the test below it.
const recordedTelemetryPath = `{
  "status": "success",
  "data": {
    "resultType": "vector",
    "result": [
      {"metric": {"__name__": "otelcol_exporter_queue_size", "exporter": "otlp/cold-archive"},
       "value": [1785299345.588, "1"]},
      {"metric": {"__name__": "otelcol_exporter_queue_capacity", "exporter": "otlp/cold-archive"},
       "value": [1785299345.588, "2000"]},
      {"metric": {"__name__": "otelcol_exporter_queue_size", "exporter": "otlp/primary-apm"},
       "value": [1785299345.588, "0"]},
      {"metric": {"__name__": "otelcol_exporter_queue_capacity", "exporter": "otlp/primary-apm"},
       "value": [1785299345.588, "20000"]},
      {"metric": {"__name__": "otelcol_exporter_queue_size", "exporter": "otlp/metrics-store"},
       "value": [1785299345.588, "0"]},
      {"metric": {"__name__": "otelcol_exporter_queue_capacity", "exporter": "otlp/metrics-store"},
       "value": [1785299345.588, "10000"]}
    ]
  }
}`

// The same shape with a Backend that HAS dropped telemetry. Constructed rather
// than recorded, and said so: nothing in the harness has ever driven a queue to
// capacity, so this series has never been observed here (#49). It is the shape
// Prometheus would return, not a reading anyone has taken.
const constructedDroppingPath = `{
  "status": "success",
  "data": {
    "resultType": "vector",
    "result": [
      {"metric": {"__name__": "otelcol_exporter_queue_size", "exporter": "otlp/primary-apm"},
       "value": [1785299345.588, "20000"]},
      {"metric": {"__name__": "otelcol_exporter_queue_capacity", "exporter": "otlp/primary-apm"},
       "value": [1785299345.588, "20000"]},
      {"metric": {"__name__": "otelcol_exporter_enqueue_failed_spans_total", "exporter": "otlp/primary-apm"},
       "value": [1785299345.588, "417"]}
    ]
  }
}`

// THE TRACER BULLET for the second seam. Per-Backend delivery health comes back
// keyed by the exporter, which is the Backend's own name.
func TestTheTelemetryPathReportsEachBackendByName(t *testing.T) {
	b, _ := backends(t, emptyTempoSearch, recordedTelemetryPath)

	path, err := b.QueryTelemetryPath(context.Background(),
		copilot.ServiceIdentity{Name: "otel-gateway"})
	if err != nil {
		t.Fatalf("QueryTelemetryPath: %v", err)
	}

	if len(path.PerExporter) != 3 {
		t.Fatalf("got %d Backends, want 3", len(path.PerExporter))
	}

	// Sorted, so a summary does not differ run to run for no reason.
	want := []string{"otlp/cold-archive", "otlp/metrics-store", "otlp/primary-apm"}
	for i, name := range want {
		if path.PerExporter[i].Name != name {
			t.Errorf("Backend %d = %q, want %q (sorted)", i, path.PerExporter[i].Name, name)
		}
	}
}

// The declared delivery settings round-trip: `queue_size` from the Gateway
// Declaration, through compile and a running collector, back out as the capacity
// a ratio divides by. Same numbers docs/backend-label-mapping.md measured.
func TestTheDeclaredQueueCapacitiesSurviveIntoThePath(t *testing.T) {
	b, _ := backends(t, emptyTempoSearch, recordedTelemetryPath)

	path, err := b.QueryTelemetryPath(context.Background(),
		copilot.ServiceIdentity{Name: "otel-gateway"})
	if err != nil {
		t.Fatalf("QueryTelemetryPath: %v", err)
	}

	capacities := map[string]float64{}
	for _, e := range path.PerExporter {
		capacities[e.Name] = e.QueueCapacity
	}
	for name, want := range map[string]float64{
		"otlp/primary-apm":   20000,
		"otlp/metrics-store": 10000,
		"otlp/cold-archive":  2000,
	} {
		if capacities[name] != want {
			t.Errorf("%s capacity = %v, want the declared %v", name, capacities[name], want)
		}
	}
}

// The unreachable Backend is the one holding, and it names itself. This is what
// makes "which Backend is behind?" answerable rather than a guess.
func TestOnlyTheUnreachableBackendIsHolding(t *testing.T) {
	b, _ := backends(t, emptyTempoSearch, recordedTelemetryPath)

	path, err := b.QueryTelemetryPath(context.Background(),
		copilot.ServiceIdentity{Name: "otel-gateway"})
	if err != nil {
		t.Fatalf("QueryTelemetryPath: %v", err)
	}

	for _, e := range path.PerExporter {
		holding := e.QueueSize > 0
		if e.Name == "otlp/cold-archive" && !holding {
			t.Error("the unreachable Backend's queue is not holding")
		}
		if e.Name != "otlp/cold-archive" && holding {
			t.Errorf("%s is also holding; isolation is not attributable", e.Name)
		}
	}
}

// THE ABSENCE THAT LOOKS LIKE HEALTH.
//
// Prometheus creates no series for a counter that has never incremented, so both
// failure counters are missing from a healthy response entirely — and a missing
// counter is reported as 0, which is CORRECT for a counter but indistinguishable
// from the reading you would get if the metric name were wrong.
//
// What makes it safe is the asymmetry between the two kinds:
//
//	gauges (queue_size, queue_capacity)  always present while the collector
//	                                     reports. Absent ⇒ that exporter never
//	                                     enters the result at all, so nothing is
//	                                     reported as healthy-by-default.
//	counters (enqueue_failed, …)         absent until first increment. Absent ⇒
//	                                     genuinely zero.
//
// So an exporter only appears when its gauges do, and only its counters may be
// inferred. Getting a gauge name wrong drops the Backend from the report — loud.
// Getting a counter name wrong reads as zero — silent, and caught only by the
// live test below, which is why that test exists.
func TestAbsentFailureCountersAreZeroRatherThanMissingBackends(t *testing.T) {
	b, _ := backends(t, emptyTempoSearch, recordedTelemetryPath)

	path, err := b.QueryTelemetryPath(context.Background(),
		copilot.ServiceIdentity{Name: "otel-gateway"})
	if err != nil {
		t.Fatalf("QueryTelemetryPath: %v", err)
	}

	if len(path.PerExporter) != 3 {
		t.Fatalf("an exporter was dropped because its counters were absent: got %d", len(path.PerExporter))
	}
	for _, e := range path.PerExporter {
		if e.EnqueueFailed != 0 {
			t.Errorf("%s reports %v drops from a response that carried no drop series", e.Name, e.EnqueueFailed)
		}
	}
	if path.Dropping() {
		t.Error("a healthy path reports as dropping")
	}
}

// A Backend that HAS dropped telemetry is distinguishable from one that has not.
// This is the reading a summary needs to say "telemetry-path" rather than
// "service".
func TestADroppingBackendIsDistinguishableFromAHealthyOne(t *testing.T) {
	b, _ := backends(t, emptyTempoSearch, constructedDroppingPath)

	path, err := b.QueryTelemetryPath(context.Background(),
		copilot.ServiceIdentity{Name: "otel-gateway"})
	if err != nil {
		t.Fatalf("QueryTelemetryPath: %v", err)
	}

	if !path.Dropping() {
		t.Fatal("a path with a non-zero enqueue-failed counter does not report as dropping")
	}
	if got := path.PerExporter[0]; got.QueueSize != got.QueueCapacity {
		t.Errorf("queue %v of %v — the fixture should show a full queue", got.QueueSize, got.QueueCapacity)
	}
}

// Every metric name is a measured fact, not a guess. Getting one wrong returns an
// empty vector, which reads exactly like a healthy path.
func TestThePathQueryUsesTheMeasuredMetricNames(t *testing.T) {
	b, got := backends(t, emptyTempoSearch, recordedTelemetryPath)

	if _, err := b.QueryTelemetryPath(context.Background(),
		copilot.ServiceIdentity{Name: "otel-gateway"}); err != nil {
		t.Fatalf("QueryTelemetryPath: %v", err)
	}

	// The path query is the FIRST of two — the second joins the config_version.
	if len(got.metricsQueries) < 2 {
		t.Fatalf("expected a path query and a config_version join, got %d", len(got.metricsQueries))
	}
	pathQuery := got.metricsQueries[0]

	// The gauges keep their names; the counters gain `_total` on ingest.
	for _, name := range []string{
		"otelcol_exporter_queue_size",
		"otelcol_exporter_queue_capacity",
		"otelcol_exporter_enqueue_failed_spans_total",
		"otelcol_exporter_send_failed_spans_total",
	} {
		if !strings.Contains(pathQuery, name) {
			t.Errorf("the path query does not ask for %s: %s", name, pathQuery)
		}
	}
	// One round trip, so the depth and the capacity are read at one instant.
	if !strings.Contains(pathQuery, "__name__=~") {
		t.Errorf("the path is not queried in one request: %s", pathQuery)
	}
}

// The same refusal the trace seam makes, for the same reason: a fleet-wide
// telemetry-path query is not a question an incident responder asks.
func TestThePathQueryRefusesToNameNoService(t *testing.T) {
	b, _ := backends(t, emptyTempoSearch, recordedTelemetryPath)

	if _, err := b.QueryTelemetryPath(context.Background(), copilot.ServiceIdentity{}); err == nil {
		t.Fatal("a path query naming no service should be refused")
	}
}

// compile-time proof the adapter is the second seam's implementation.
var _ copilot.TelemetryPathStore = (*backend.TempoPrometheus)(nil)
