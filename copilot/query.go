// Package copilot is Layer 3: the GenAI layer that reads standardized telemetry
// and assists incident response.
//
// This package defines the TYPED TOOL SURFACE and nothing else. It knows what a
// question about telemetry looks like and what an answer looks like; it does not
// know what a Backend is, what query language one speaks, or that Tempo and
// Prometheus exist. That knowledge lives in exactly one adapter
// (copilot/backend), and the import direction is the proof: `backend` imports
// `copilot`, never the reverse.
//
// The reason is ADR 0007 read one layer up. Services are Backend-agnostic because
// a Backend is named in one place; the Copilot is Backend-agnostic for the same
// reason and with a sharper edge — a vendor's query language reaching the model
// would mean swapping a Backend rewrites a prompt, and a prompt is not a thing
// that can be swapped safely.
package copilot

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ServiceIdentity is who a record says it is — the fields a Telemetry Contract
// stamps, and therefore the only identity the Copilot is allowed to reason about.
//
// It is NOT the identity of the process that emitted the telemetry. A service can
// call itself anything; what reaches a Backend is what its Contract declared,
// because the Agent upserts these attributes on the way through. That is a real
// trap for a query tool: searching a trace store for the sending process's own
// idea of its name finds nothing (docs/backend-label-mapping.md, rule 6).
type ServiceIdentity struct {
	// Name is service.name, as declared in the Telemetry Contract.
	Name string
	// Namespace is service.namespace. Empty is legal — not every Contract
	// declares one — and an empty Namespace means "do not filter on it" rather
	// than "match the empty string".
	Namespace string
	// Tier is the Service Tier the Contract declares. It is carried because what
	// a tier-1 incident means differs from what a tier-3 one means, and because
	// the Standards that applied to this telemetry are a function of it.
	//
	// It is NOT queryable at a Backend today: no Contract stamps a tier attribute
	// on a record, which is the same wall #40 and #44 hit from two other
	// directions. An adapter fills it from the Contract, or leaves it empty.
	Tier string
}

// TraceQuery is the whole of what the Copilot may ask for. Deliberately small:
// every field an incident-responder needs and nothing that would let a query
// language leak in through the parameters.
//
// There is no free-text field, and there will not be one. A `filter string` here
// would be a vendor query language crossing the seam by the back door, and every
// prompt would end up carrying TraceQL or PromQL in it.
type TraceQuery struct {
	// Service is which service's telemetry to look at. Name is required.
	Service ServiceIdentity
	// Since and Until bound the window. Until may be zero, meaning "now".
	Since time.Time
	Until time.Time
	// Limit caps how many traces come back. Zero means the adapter's default.
	// It exists because a tool result becomes model context, and an unbounded
	// result set is an unbounded prompt.
	Limit int
}

// DefaultLimit is what an adapter uses when a TraceQuery does not set one.
const DefaultLimit = 20

// ErrNoService is returned for a query that names no service. It is an error
// rather than a fleet-wide search on purpose: "show me every trace" is not a
// question an incident-responder asks, and answering it would put the whole
// fleet's telemetry into one prompt.
var ErrNoService = errors.New("copilot: a TraceQuery must name a service")

// Validate reports whether the query is answerable. Adapters call it first, so
// that every adapter refuses the same queries for the same reasons rather than
// each inventing its own idea of a bad one.
func (q TraceQuery) Validate() error {
	if q.Service.Name == "" {
		return ErrNoService
	}
	if !q.Until.IsZero() && q.Until.Before(q.Since) {
		return fmt.Errorf("copilot: window ends (%s) before it starts (%s)",
			q.Until.Format(time.RFC3339), q.Since.Format(time.RFC3339))
	}
	if q.Limit < 0 {
		return fmt.Errorf("copilot: negative limit %d", q.Limit)
	}
	return nil
}

// Window returns the query's bounds with the open end resolved against now, so
// every adapter resolves "until now" identically.
func (q TraceQuery) Window(now time.Time) (since, until time.Time) {
	until = q.Until
	if until.IsZero() {
		until = now
	}
	return q.Since, until
}

// EffectiveLimit is the query's limit with the default applied.
func (q TraceQuery) EffectiveLimit() int {
	if q.Limit <= 0 {
		return DefaultLimit
	}
	return q.Limit
}

// TraceRef is one trace, in the form a citation needs: enough to identify it, to
// say what it was, and to fetch it again.
//
// It is a REFERENCE, not a trace. The distinction is the whole grounding story
// (ADR 0009): a claim cites a handle an operator can follow, rather than prose
// the model wrote about telemetry it saw once. Nothing here is a summary.
type TraceRef struct {
	// TraceID is the citation. It is what makes a claim checkable by a human, and
	// what a later Grounding check (#18) will verify a claim against.
	TraceID string
	// Service is the identity the Telemetry Contract stamped on this trace.
	Service ServiceIdentity
	// RootSpanName is what the trace was, in the trace's own words. It is
	// attacker-controllable — a span name is written by the instrumented service —
	// and it is exactly the string this package refuses to let near a prompt as
	// anything but tool-result content (ADR 0011, and #54 for what is still
	// untested about that).
	RootSpanName string
	// Start is when the root span began.
	Start time.Time
	// Duration is how long the root span took.
	Duration time.Duration
	// ConfigVersion is the otel.platform.config_version of the collector that
	// carried this service's telemetry at query time.
	//
	// IT IS JOINED, NOT READ OFF THE SPAN, and that is not an implementation
	// detail. The Agent deletes the otel.platform. namespace from everything it
	// forwards, so a span reaching a Backend carries no config_version at all —
	// if it did, any service could forge a Rollout confirmation. So an adapter
	// gets this from the collector's own self-telemetry, keyed by service, and a
	// span that appeared to carry one would be a bug worth failing on.
	//
	// Empty means the adapter could not establish which configuration was
	// running, which is itself worth saying out loud rather than guessing.
	ConfigVersion string
}

// ExporterHealth is one Backend's delivery health, as the collector reports it.
//
// The name is the Backend's: an exporter is named after the Backend it writes to
// (`otlp/primary-apm`), which is what makes "which Backend is behind?" a question
// with an answer rather than a guess. That naming is a compile-time property —
// two Backends cannot share an exporter — so a name here identifies one Backend
// and no other.
type ExporterHealth struct {
	// Name is the exporter, and therefore the Backend: `otlp/primary-apm`.
	Name string
	// QueueSize is how many batches are waiting to be sent, right now.
	QueueSize float64
	// QueueCapacity is what the Gateway Declaration's `queue_size` compiled to.
	// The ratio of the two is how close this Backend is to dropping.
	QueueCapacity float64
	// EnqueueFailed counts telemetry DROPPED because the queue was full. It is
	// the one number here that means data was lost rather than delayed, and the
	// one worth paging on.
	EnqueueFailed float64
	// SendFailed counts export attempts that failed for good.
	SendFailed float64
}

// TelemetryPath is the state of the road the telemetry travelled, as opposed to
// the state of the service that emitted it.
//
// It exists to make one distinction available to a summary: a service that has
// gone quiet because it is broken looks identical, in traces alone, to a service
// whose telemetry is being dropped on the way. Thin trace data plus a healthy
// path means the service; thin trace data plus a filling queue means the path.
// Without this, both read as "the service is fine" or "the service is down" with
// no way to tell which.
type TelemetryPath struct {
	// ConfigVersion is which compiled configuration the collector is running.
	// Same value as TraceRef.ConfigVersion and joined the same way.
	ConfigVersion string
	// PerExporter is one entry per Backend the collector writes to, newest
	// reading. Empty means the collector's own telemetry has not arrived — which
	// is itself a finding, and not the same as "everything is healthy".
	PerExporter []ExporterHealth
}

// Dropping reports whether any Backend has lost telemetry outright. It is a
// method rather than a field so the judgement lives with the data instead of
// being re-derived by every caller — the deletion test on a one-line helper:
// delete it and this loop reappears in the summariser, the tool layer, and every
// future consumer.
func (p TelemetryPath) Dropping() bool {
	for _, e := range p.PerExporter {
		if e.EnqueueFailed > 0 {
			return true
		}
	}
	return false
}

// TelemetryPathStore answers the second question an incident needs: not "what did
// this service emit" but "did what it emitted actually get here".
//
// A separate seam from TraceStore rather than a fifth method on it. The two are
// answered by different systems — traces by a trace store, this by the metrics
// Backend carrying the collector's self-telemetry — and an adapter that can do
// one may not be able to do the other. One interface per question keeps a
// Backend that only has traces implementable.
type TelemetryPathStore interface {
	// QueryTelemetryPath reports on the collectors carrying this service's
	// telemetry.
	//
	// It returns a zero TelemetryPath and no error when nothing has reported:
	// "the collector's own telemetry has not arrived" is a finding, not a
	// failure, and it is one an incident responder needs to hear.
	QueryTelemetryPath(ctx context.Context, service ServiceIdentity) (TelemetryPath, error)
}

// TraceStore is the seam. One method, because the tracer bullet needs one
// question answered; the rest of the typed tool surface (query_metrics,
// query_logs, get_contract, get_standards) arrives with P2 (#17).
//
// Everything vendor-specific in this system sits behind this interface. The
// deletion test is the argument for it: delete TraceStore and TraceQL appears in
// the Tool Runner, in every test fixture, and eventually in a prompt — the
// complexity does not vanish, it spreads to every caller and to the one place it
// must never reach.
type TraceStore interface {
	// QueryTraces answers a TraceQuery, newest first.
	//
	// It returns an empty slice and no error when the window holds nothing: "this
	// service emitted no traces in the last hour" is a finding an incident
	// responder needs, not a failure.
	QueryTraces(ctx context.Context, q TraceQuery) ([]TraceRef, error)
}
