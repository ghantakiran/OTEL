# The Copilot's tool surface

Layer 3 begins here. This is the seam between a model and the platform's
telemetry: what the Copilot may ask, what it gets back, and — the part that
matters most — what can and cannot reach a prompt.

Nothing in this page calls a model. The first P1 slice is the **data path and the
tool seam**; the loop that actually talks to Claude comes next, on top of exactly
these types.

## The one question, and its shape

```go
type TraceStore interface {
    QueryTraces(ctx context.Context, q TraceQuery) ([]TraceRef, error)
}
```

One method, because P1 is a tracer bullet. The rest of the typed tool surface —
`query_metrics`, `query_logs`, `get_contract`, `get_standards` — arrives with P2
(#17).

A **TraceQuery** is a service identity and a window. There is no free-text field,
and there will not be one: a `filter string` here would be a Backend's query
language crossing the seam by the back door, and every prompt would end up
carrying it.

A **TraceRef** is a citation, not a summary — a trace ID, the identity the
Telemetry Contract stamped, the root span name, timing, and the Config Version
that was running. Grounding (ADR 0009) means a claim points at a handle an
operator can follow; prose about telemetry the model saw once is what this shape
exists to prevent.

## Two facts that shaped the types

**A trace does not carry its Config Version.** The Agent deletes the
`otel.platform.` namespace from everything it forwards, so that a service cannot
forge a Rollout confirmation. So `TraceRef.ConfigVersion` is **joined** from the
collector's own Self-Telemetry, keyed by service — the adapter asks two Backends
one question. A span that appeared to carry one would be a bug worth failing on,
and `harness/verify-backend-rendering.sh` asserts it never does.

**A service's name is not its own.** What reaches a Backend is what the Telemetry
Contract declared, because the Agent upserts it. The harness's sample service
emits `service.name: harness-sample-service` and what is stored is `checkout-api`.
Searching a trace store for the sending process's own idea of its name finds
nothing — a real trap for a query tool, and asserted in both the adapter's tests.

`ServiceIdentity.Tier` is carried but **not queryable**: no Contract stamps a tier
attribute on a record, so no Backend can be asked for it. An adapter fills it from
configuration or leaves it empty. This is the same wall #40 and #44 hit from two
other directions.

## Where the vendor stops

```
copilot/            the typed tool surface. Knows what a question is.
                    Names no product, no query language, no Backend.
copilot/backend/    the adapters. Knows TraceQL, PromQL, URL paths, label
                    spellings, JSON bodies.
```

The import direction is the proof: `backend` imports `copilot`, and `copilot`
does not import `backend`. A query language cannot reach the tool schema or the
prompt above it, because the package that knows one is downstream of both.

Every label spelling in the adapter is a **measured** fact from
[backend-label-mapping.md](./backend-label-mapping.md), not a guess:
`otelcol_process_uptime_seconds_total` (the metric name after ingest appends the
unit and `_total`) and `otel_platform_config_version` (the attribute after dots
become underscores, and only because the Backend was told to promote it). Getting
either wrong returns an empty result that reads exactly like a service which never
reported — the most dangerous possible answer.

## The injection boundary, as types

ADR 0011 says telemetry enters "exclusively as tool-result content, framed as
data, never as instructions". A comment saying so is worth nothing. What makes it
true here is that **there is no code path from telemetry to an authored message**:

- Everything the platform authors is a string, and each of those strings is a
  package constant or a field an operator filled in. `SystemPrompt` is a `const` —
  a prompt built with `Sprintf` is one substitution away from carrying a span name.
- Everything telemetry-derived is a `[]TraceRef` — a **typed value, never a
  string** — and it lives only inside a `ToolResult`.
- **Nothing turns a `TraceRef` into a string.** No `String()` method, no `Render`
  function, no `fmt` verb. That function does not exist yet; when it arrives it
  belongs to the API serializer, which writes `tool_result` blocks and nothing
  else.

So a span name cannot reach the system prompt by accident, because reaching it
would require writing a conversion that is not there.

A Backend's **error text** is treated the same way. It can quote a service name, a
label value, a URL, so it is never passed through — a tool that fails reports a
constant, not `err.Error()`.

`Conversation.AuthoredText()` exists so this is checkable rather than asserted:
it returns every string the platform or the model wrote, and the tests walk it
looking for a hostile span name.

### What that is, and what it is not

It is **structural prevention**. It is **not** evidence of resilience. A span name
is written by the instrumented service — by anyone who can deploy one — and
nothing here has ever been run against an adversary trying variants. That is #54,
and the Incident Corpus (#20) is where those variants belong.

## Running the tests

```bash
go test ./copilot/...                       # hermetic: recorded fixtures, no Docker
```

The adapter's fixtures are **recorded**, not invented — captured from the products
`harness/real-backends.yaml` stands up, answering the same queries this adapter
sends, with the platform carrying a real span end to end.

Recorded fixtures rot silently, so the same assertions also run live:

```bash
bash harness/verify-backend-rendering.sh --keep
COPILOT_TRACE_URL=http://localhost:3200 \
COPILOT_METRICS_URL=http://localhost:9090 \
  go test ./copilot/... -run Harness -v
```

Those tests **skip** when the two variables are unset, so `go test ./...` stays
hermetic and runnable without Docker — the same line the Distribution Check draws
between its stubbed test and its real one. A trace store that renamed a field, or
a metrics Backend upgraded past the label promotion, leaves every hermetic test
green and turns these red, which is the correct place for that failure to appear.

## What this does not do yet

No model is called. There is no vendor SDK, no API key path, and **no model ID** —
ADR 0011 names one that has since been superseded, which is #55, and committing to
it here would put a stale identifier in the loop as well as in the ADR.

`Run` drives the loop against a `Model` interface; the only implementation today
is a test double. What is missing to make it real is small and listed in the
second P1 slice: an adapter that serializes a `Conversation` to the API's message
shape, renders `[]TraceRef` into `tool_result` blocks, and returns the model's
turn.
