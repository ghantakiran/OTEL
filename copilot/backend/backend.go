// Package backend is the one place in Layer 3 that knows a product exists.
//
// It holds the adapters that satisfy copilot.TraceStore. Everything vendor-shaped
// — a query language, a URL path, a label spelling, a JSON body — is here and
// nowhere else. The import direction enforces it: this package imports `copilot`,
// and `copilot` does not import this one, so no query language can reach the
// typed tool surface or the prompt above it (ADR 0007, ADR 0011).
//
// It is the same seam docs/backend-label-mapping.md describes one layer down.
// There, harness/backend-real-collector.yaml is the only file that knows what
// stands behind the `primary-apm` role; here, this package is the only one that
// knows how to ask it a question.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ghantakiran/OTEL/copilot"
)

// TempoPrometheus answers a copilot.TraceQuery from the two products that fill
// the `primary-apm` role in harness/real-backends.yaml: a trace store for the
// traces, and a metrics Backend for which configuration was carrying them.
//
// IT TAKES TWO BACKENDS TO ANSWER ONE QUESTION, and that is not an accident of
// this particular pairing. A trace reaching a Backend carries NO
// otel.platform.config_version — the Agent deletes the namespace from everything
// it forwards, so that a service cannot forge a Rollout confirmation
// (docs/platform-self-observation.md). The configuration a service was running is
// therefore only knowable from the collector's own self-telemetry, which is
// metrics. Joining the two is this adapter's job precisely so that the tool
// surface above never learns there were two.
type TempoPrometheus struct {
	// TraceURL is the trace store's base URL, e.g. http://localhost:3200.
	TraceURL string
	// MetricsURL is the metrics Backend's base URL, e.g. http://localhost:9090.
	MetricsURL string
	// HTTP is the client used for both. A nil client means http.DefaultClient
	// with a timeout, because a Backend that never answers must not hang an
	// incident response.
	HTTP *http.Client
	// Now is the clock, injected so a test can bound a window deterministically.
	// Nil means time.Now.
	Now func() time.Time
	// Tier is what Service Tier to report on a TraceRef.
	//
	// It is CONFIGURED, not queried, and that is a limitation rather than a
	// choice: no Telemetry Contract stamps a tier attribute on a record, so no
	// Backend can be asked for it. The Gateway hits the identical wall for
	// per-tier sampling (#40) and per-tier Pipeline Guardrails (#44). Left empty,
	// a TraceRef simply carries no tier — which is honest, where guessing one
	// would not be.
	Tier string
}

// compile-time proof that this adapter is the seam's implementation and not
// merely a type with a similar method.
var _ copilot.TraceStore = (*TempoPrometheus)(nil)

func (b *TempoPrometheus) client() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (b *TempoPrometheus) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// QueryTraces answers the query, newest first.
func (b *TempoPrometheus) QueryTraces(ctx context.Context, q copilot.TraceQuery) ([]copilot.TraceRef, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	since, until := q.Window(b.now())

	found, err := b.search(ctx, q, since, until)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		// An empty window is a finding, not a failure — and it is one of the more
		// important ones an incident responder can get. Returning an empty slice
		// rather than an error is what lets "this service emitted nothing" be
		// said out loud.
		return []copilot.TraceRef{}, nil
	}

	// The join. A failure here is not fatal: knowing which traces exist while not
	// knowing which configuration produced them is strictly better than knowing
	// neither, and a TraceRef with an empty ConfigVersion says so.
	version := b.configVersion(ctx, q.Service.Name)
	for i := range found {
		found[i].ConfigVersion = version
	}
	return found, nil
}

// tempoSearchResponse is the trace store's answer, in its own shape. Only the
// fields this adapter reads are named; the response carries more (span sets,
// service stats, scan metrics) and none of it crosses the seam.
type tempoSearchResponse struct {
	Traces []struct {
		TraceID string `json:"traceID"`
		// The trace store reports the ROOT service, which for this platform is
		// the identity the Telemetry Contract stamped — never the name the
		// emitting process called itself.
		RootServiceName   string  `json:"rootServiceName"`
		RootTraceName     string  `json:"rootTraceName"`
		StartTimeUnixNano string  `json:"startTimeUnixNano"`
		DurationMs        float64 `json:"durationMs"`
	} `json:"traces"`
}

func (b *TempoPrometheus) search(ctx context.Context, q copilot.TraceQuery, since, until time.Time) ([]copilot.TraceRef, error) {
	// THE ONLY PLACE A QUERY LANGUAGE IS WRITTEN. A TraceQuery has no free-text
	// field, so there is no path by which a caller's string reaches this
	// expression — the selector is built from typed fields and nothing else.
	selector := fmt.Sprintf(`{resource.service.name=%s}`, quote(q.Service.Name))
	if q.Service.Namespace != "" {
		selector = fmt.Sprintf(`{resource.service.name=%s && resource.service.namespace=%s}`,
			quote(q.Service.Name), quote(q.Service.Namespace))
	}

	params := url.Values{}
	params.Set("q", selector)
	params.Set("limit", strconv.Itoa(q.EffectiveLimit()))
	params.Set("start", strconv.FormatInt(since.Unix(), 10))
	params.Set("end", strconv.FormatInt(until.Unix(), 10))

	var body tempoSearchResponse
	if err := b.getJSON(ctx, b.TraceURL+"/api/search?"+params.Encode(), &body); err != nil {
		return nil, fmt.Errorf("copilot/backend: searching traces: %w", err)
	}

	refs := make([]copilot.TraceRef, 0, len(body.Traces))
	for _, t := range body.Traces {
		startNanos, _ := strconv.ParseInt(t.StartTimeUnixNano, 10, 64)
		refs = append(refs, copilot.TraceRef{
			TraceID: t.TraceID,
			Service: copilot.ServiceIdentity{
				// Taken from what the store returned rather than echoed back from
				// the query: a ref has to say whose telemetry it actually is, and
				// echoing the request would make a wrong answer unfalsifiable.
				Name:      t.RootServiceName,
				Namespace: q.Service.Namespace,
				Tier:      b.Tier,
			},
			RootSpanName: t.RootTraceName,
			Start:        time.Unix(0, startNanos).UTC(),
			Duration:     time.Duration(t.DurationMs * float64(time.Millisecond)),
		})
	}
	return refs, nil
}

// promVectorResponse is the metrics Backend's instant-query answer.
type promVectorResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
		} `json:"result"`
	} `json:"data"`
}

// configVersion asks which configuration this service's collector is running.
//
// Every spelling here is a measured fact rather than a guess, and all of them are
// this Backend's rather than the platform's (docs/backend-label-mapping.md):
//
//	otelcol_process_uptime_seconds_total   the metric name AFTER ingest appends
//	                                       the unit and _total. The un-suffixed
//	                                       name returns empty, which reads exactly
//	                                       like a service that never reported.
//	otel_platform_config_version           the resource attribute after dots
//	                                       become underscores, and only because
//	                                       this Backend was told to promote it.
//
// A failure returns "" rather than an error: see QueryTraces.
func (b *TempoPrometheus) configVersion(ctx context.Context, service string) string {
	q := fmt.Sprintf(
		`max by (otel_platform_config_version) (otelcol_process_uptime_seconds_total{service_name=%s})`,
		quote(service))

	params := url.Values{}
	params.Set("query", q)

	var body promVectorResponse
	if err := b.getJSON(ctx, b.MetricsURL+"/api/v1/query?"+params.Encode(), &body); err != nil {
		return ""
	}
	if body.Status != "success" {
		return ""
	}
	for _, r := range body.Data.Result {
		if v := r.Metric["otel_platform_config_version"]; v != "" {
			return v
		}
	}
	return ""
}

func (b *TempoPrometheus) getJSON(ctx context.Context, endpoint string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := b.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", endpoint, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// quote renders a string as a query-language literal.
//
// A service name reaching here came from an operator or from a Telemetry
// Contract, not from telemetry — but "not attacker-controlled today" is a
// property of the callers rather than of this function, and callers change.
// Escaping is what keeps that true.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
