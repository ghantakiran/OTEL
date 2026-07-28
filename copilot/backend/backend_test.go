package backend_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ghantakiran/OTEL/copilot"
	"github.com/ghantakiran/OTEL/copilot/backend"
)

// The response bodies below are RECORDED, not invented. They were captured from
// the trace store and metrics Backend that harness/real-backends.yaml stands up,
// answering the same queries this adapter sends, with the platform carrying a
// real span end to end. A fixture written from memory would test this adapter
// against a shape nothing produces — which is the failure #50 exists to have
// stopped this repository making.
//
// backend_harness_test.go runs the same assertions against those products live.
const (
	// GET /api/search?q={resource.service.name="checkout-api"}&…
	recordedTempoSearch = `{
      "traces": [
        {
          "traceID": "fe3852be4562dca17922b0b2758ff910",
          "rootServiceName": "checkout-api",
          "rootTraceName": "POST /checkout",
          "startTimeUnixNano": "1785261802000000000",
          "durationMs": 42,
          "spanSet": {"spans": [{"spanID": "e3bbf04c3176623b"}], "matched": 1},
          "serviceStats": {"checkout-api": {"spanCount": 1}}
        }
      ],
      "metrics": {"inspectedBytes": "17470", "completedJobs": 1, "totalJobs": 1}
    }`

	// GET /api/v1/query?query=max by (otel_platform_config_version) (…)
	recordedPromConfigVersion = `{
      "status": "success",
      "data": {
        "resultType": "vector",
        "result": [
          {
            "metric": {
              "otel_platform_config_version": "sha256:b76e871b3d59cd9421b2483c799b89e87045a49f8bea330452d0103cecaa9d4a",
              "service_name": "checkout-api"
            },
            "value": [1785261825.802, "30.008396916"]
          }
        ]
      }
    }`

	emptyTempoSearch = `{"traces": [], "metrics": {"inspectedBytes": "0"}}`
	emptyPromVector  = `{"status":"success","data":{"resultType":"vector","result":[]}}`

	contractVersion = "sha256:b76e871b3d59cd9421b2483c799b89e87045a49f8bea330452d0103cecaa9d4a"
)

// backends stands up a stub for each product and returns an adapter pointed at
// them, plus the queries each was asked. Recording the queries is the point:
// what this adapter SENDS is as much of its behaviour as what it returns, and it
// is the only place a query language is written.
type recorded struct {
	traceQuery   string
	metricsQuery string
}

func backends(t *testing.T, searchBody, metricsBody string) (*backend.TempoPrometheus, *recorded) {
	t.Helper()
	got := &recorded{}

	traces := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.traceQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(searchBody))
	}))
	t.Cleanup(traces.Close)

	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.metricsQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(metricsBody))
	}))
	t.Cleanup(metrics.Close)

	return &backend.TempoPrometheus{
		TraceURL:   traces.URL,
		MetricsURL: metrics.URL,
		Now:        func() time.Time { return time.Unix(1785261900, 0).UTC() },
		Tier:       "tier-1",
	}, got
}

// THE TRACER BULLET. A service-name and time-window query comes back as a
// citable reference carrying the identity the Telemetry Contract stamped.
func TestAServiceAndWindowQueryReturnsCitableTraces(t *testing.T) {
	b, _ := backends(t, recordedTempoSearch, recordedPromConfigVersion)

	refs, err := b.QueryTraces(context.Background(), copilot.TraceQuery{
		Service: copilot.ServiceIdentity{Name: "checkout-api"},
		Since:   time.Unix(1785258300, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1", len(refs))
	}

	ref := refs[0]
	if ref.TraceID != "fe3852be4562dca17922b0b2758ff910" {
		t.Errorf("TraceID = %q — without it a claim cannot be cited", ref.TraceID)
	}
	if ref.RootSpanName != "POST /checkout" {
		t.Errorf("RootSpanName = %q", ref.RootSpanName)
	}
	if got, want := ref.Duration, 42*time.Millisecond; got != want {
		t.Errorf("Duration = %v, want %v", got, want)
	}
	if ref.Start.IsZero() {
		t.Error("a citable trace needs a time")
	}
}

// THE TRAP, ASSERTED. A service can call itself anything; what reaches a Backend
// is what its Telemetry Contract declared, because the Agent upserts it. A tool
// that returned the sender's own name would cite telemetry under an identity no
// operator could look up.
func TestATraceCarriesTheContractsIdentityNotTheSendersOwn(t *testing.T) {
	b, _ := backends(t, recordedTempoSearch, recordedPromConfigVersion)

	refs, err := b.QueryTraces(context.Background(), copilot.TraceQuery{
		Service: copilot.ServiceIdentity{Name: "checkout-api"},
	})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}

	// The harness's sample service emits `service.name: harness-sample-service`
	// and its Contract declares `checkout-api`. The recorded body is what the
	// store actually returned for that span.
	if refs[0].Service.Name != "checkout-api" {
		t.Errorf("Service.Name = %q, want the Contract's checkout-api", refs[0].Service.Name)
	}
	if refs[0].Service.Name == "harness-sample-service" {
		t.Error("the ref carries the sending process's name, which no operator can look up")
	}
}

// The configuration a service was running is JOINED from the collector's own
// self-telemetry, because the Agent strips otel.platform. from every span it
// forwards. This asserts the join happened and that it asked for the label
// spelling a real Backend actually uses.
func TestConfigVersionIsJoinedFromSelfTelemetryBecauseNoSpanCarriesIt(t *testing.T) {
	b, got := backends(t, recordedTempoSearch, recordedPromConfigVersion)

	refs, err := b.QueryTraces(context.Background(), copilot.TraceQuery{
		Service: copilot.ServiceIdentity{Name: "checkout-api"},
	})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}

	if refs[0].ConfigVersion != contractVersion {
		t.Errorf("ConfigVersion = %q, want the joined %q", refs[0].ConfigVersion, contractVersion)
	}

	// The measured spellings from docs/backend-label-mapping.md. Getting either
	// wrong returns an empty result that reads exactly like a service which never
	// reported — the most dangerous possible answer.
	if !strings.Contains(got.metricsQuery, "otelcol_process_uptime_seconds_total") {
		t.Errorf("the metrics query does not use the ingested metric name: %s", got.metricsQuery)
	}
	if !strings.Contains(got.metricsQuery, "otel_platform_config_version") {
		t.Errorf("the metrics query does not use the promoted label name: %s", got.metricsQuery)
	}
	if strings.Contains(got.metricsQuery, "otel.platform.config_version") {
		t.Errorf("the metrics query uses the dotted attribute name, which no Backend answers to: %s", got.metricsQuery)
	}
}

// A Backend that cannot say which configuration was running must not stop the
// traces coming back. Knowing which traces exist is strictly better than knowing
// nothing, and an empty ConfigVersion says the rest out loud.
func TestTracesStillComeBackWhenTheConfigVersionCannotBeEstablished(t *testing.T) {
	b, _ := backends(t, recordedTempoSearch, emptyPromVector)

	refs, err := b.QueryTraces(context.Background(), copilot.TraceQuery{
		Service: copilot.ServiceIdentity{Name: "checkout-api"},
	})
	if err != nil {
		t.Fatalf("a missing config_version should not fail the query: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1", len(refs))
	}
	if refs[0].ConfigVersion != "" {
		t.Errorf("ConfigVersion = %q, want empty rather than guessed", refs[0].ConfigVersion)
	}
}

// "This service emitted nothing in the window" is a finding an incident
// responder needs, not an error to swallow.
func TestAnEmptyWindowIsAFindingRatherThanAFailure(t *testing.T) {
	b, _ := backends(t, emptyTempoSearch, recordedPromConfigVersion)

	refs, err := b.QueryTraces(context.Background(), copilot.TraceQuery{
		Service: copilot.ServiceIdentity{Name: "checkout-api"},
	})
	if err != nil {
		t.Fatalf("an empty window should not be an error: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("got %d refs, want none", len(refs))
	}
}

// The window is sent to the store rather than applied afterwards — otherwise the
// limit truncates before the window filters, and the answer is a lie about a
// different period.
func TestTheWindowIsAskedForRatherThanFilteredAfterwards(t *testing.T) {
	var start, end string
	traces := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end = r.URL.Query().Get("start"), r.URL.Query().Get("end")
		_, _ = w.Write([]byte(emptyTempoSearch))
	}))
	defer traces.Close()

	b := &backend.TempoPrometheus{
		TraceURL:   traces.URL,
		MetricsURL: traces.URL,
		Now:        func() time.Time { return time.Unix(1785261900, 0).UTC() },
	}
	_, err := b.QueryTraces(context.Background(), copilot.TraceQuery{
		Service: copilot.ServiceIdentity{Name: "checkout-api"},
		Since:   time.Unix(1785258300, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}

	if start != "1785258300" {
		t.Errorf("start = %q, want the query's Since", start)
	}
	if end != "1785261900" {
		t.Errorf("end = %q, want the injected now", end)
	}
}

// A namespace narrows the search at the store. Without this, two services with
// the same name in different namespaces are one service to the Copilot.
func TestANamespaceNarrowsTheSearchRatherThanBeingIgnored(t *testing.T) {
	b, got := backends(t, recordedTempoSearch, recordedPromConfigVersion)

	_, err := b.QueryTraces(context.Background(), copilot.TraceQuery{
		Service: copilot.ServiceIdentity{Name: "checkout-api", Namespace: "payments"},
	})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}

	if !strings.Contains(got.traceQuery, "payments") {
		t.Errorf("the namespace did not reach the store: %s", got.traceQuery)
	}
}

// A service name is not attacker-controlled today, but that is a property of this
// adapter's callers rather than of the adapter. Escaping is what keeps it true
// when the callers change.
func TestAQuoteInAServiceNameCannotBreakOutOfTheSelector(t *testing.T) {
	b, got := backends(t, emptyTempoSearch, emptyPromVector)

	_, err := b.QueryTraces(context.Background(), copilot.TraceQuery{
		Service: copilot.ServiceIdentity{Name: `checkout" || true || "`},
	})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}

	if strings.Contains(got.traceQuery, `checkout" || true`) {
		t.Errorf("an unescaped quote reached the selector: %s", got.traceQuery)
	}
	if !strings.Contains(got.traceQuery, `\"`) {
		t.Errorf("the quote was not escaped: %s", got.traceQuery)
	}
}

// The seam refuses the same queries for the same reasons whichever adapter is
// behind it, so validation lives on the query rather than in each adapter.
func TestTheAdapterRefusesAQueryThatNamesNoService(t *testing.T) {
	b, _ := backends(t, recordedTempoSearch, recordedPromConfigVersion)

	if _, err := b.QueryTraces(context.Background(), copilot.TraceQuery{}); err == nil {
		t.Fatal("a query naming no service should be refused by the adapter too")
	}
}
