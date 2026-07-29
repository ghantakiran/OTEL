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
                    Names no product, no query language, no model API.
copilot/backend/    the Backend adapter. Knows TraceQL, PromQL, URL paths,
                    label spellings, JSON bodies.
copilot/claude/     the model adapter. Knows the Messages API's message shape,
                    block types, and tool schema. The only package that imports
                    a vendor SDK.
```

There are **two** adapters and one seam apiece — a Backend seam and a model seam.
Both import `copilot`; `copilot` imports neither, and imports nothing outside the
standard library. The import direction is the proof: no query language and no
model API can reach the tool schema or the prompt above it, because the packages
that know them are downstream of both.

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

### At the wire: a `tool_result` travels in a user message

This is the fact the serializer had to be built around, and it is not obvious
from the invariant as stated. **There is no tool-result role in the Messages
API.** A `tool_result` block travels inside a `role: "user"` message, so the
moment a conversation is serialized, telemetry is sitting in a user turn — the
exact surface ADR 0011 is about.

That is not a violation, and the check has to be one level finer than the role:

```
role: user   + text block          ← ours. Must never carry telemetry.
role: user   + tool_result block   ← evidence. This is where it belongs.
role: assistant + text block       ← the model, quoting what it cites (ADR 0009).
```

"No telemetry in a user message" would have been the natural check and it would
have been **wrong** — it fails on correct behaviour, which is how a security
check gets loosened until it guards nothing. The same mistake, one layer up, is
what `PlatformAuthoredText()` exists to avoid.

What keeps the finer check possible is structural rather than careful: **a user
message the serializer emits holds either authored text blocks or `tool_result`
blocks, never both.** Nothing merges them, so a block's type still tells you who
wrote its contents. A test asserts that separation directly.

### Rendering evidence: the one `TraceRef` → string

`copilot/claude/serialize.go` holds the **only** function in the repository that
turns a `TraceRef` into a string. It is unexported, so there cannot be another
outside that file, and its only caller is the code that builds a `tool_result`
block. Until this slice, no such conversion existed anywhere — that absence was
what made the claim structural rather than conventional, so writing it is the
moment the claim stops being free.

Evidence is rendered as a **JSON record set**, not prose:

```json
[{"trace_id":"fe3852be…","service_name":"checkout-api","service_namespace":"payments",
  "service_tier":"tier-1","root_span_name":"POST /checkout","start":"2026-07-28T12:03:22Z",
  "duration_ms":42,"collector_config_version":"sha256:b76e871b…"}]
```

Prose would mean this package writing sentences *about* telemetry, and a sentence
is the shape an instruction takes — rendering is exactly where "data, not
instructions" is easiest to lose. Note what is absent: no preamble, no "here are
the traces", no framing. The framing lives in the system prompt, which is ours.
Putting it here would place authored text in the same block as
attacker-controlled values, and then the two are one string.

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

## Two questions, one tool result

Traces alone cannot tell a broken service from a broken telemetry path. A service
that has gone quiet because it crashed and one whose telemetry is being dropped in
transit look identical: thin trace data, either way.

So `query_traces` returns both:

```json
{
  "traces": [ … ],
  "telemetry_path": {
    "collector_config_version": "sha256:b76e871b…",
    "collectors_reporting": true,
    "telemetry_dropped": true,
    "per_backend": [
      {"backend": "otlp/primary-apm", "queue_size": 20000, "queue_capacity": 20000,
       "telemetry_dropped_count": 417, "send_failed_count": 0}
    ]
  }
}
```

**One tool, not two.** A model that had to remember to ask a second time would
sometimes answer from half the evidence — and the half it would skip is exactly
the half that makes the distinction. `collectors_reporting` is stated rather than
inferred from an empty array, because "no collector has reported" and "every
collector is healthy" are different findings and an empty list reads like the
second.

### The absence that looks like health

Prometheus creates **no series** for a counter that has never incremented, so both
failure counters are missing from a healthy response — not zero, absent. A missing
counter is read as 0, which is correct for a counter and indistinguishable from
what you would see if the metric name were wrong.

What makes that safe is an asymmetry between the two kinds:

| | Behaviour when absent | Failure mode if the name is wrong |
|---|---|---|
| **gauges** (`queue_size`, `queue_capacity`) | the exporter never enters the result at all | the Backend vanishes from the report — **loud** |
| **counters** (`enqueue_failed`, `send_failed`) | genuinely zero | reads as healthy — **silent** |

An exporter only appears when its gauges do, so nothing is reported healthy by
default. The silent case is caught only by `TestHarnessTheTelemetryPathIsQueryable`
running against a real Backend, which is why that test exists.

## Citation provenance — and what it is not

`Citations(summary, conversation)` partitions the trace IDs a summary mentions
into those the tools actually returned and those they did not. `Grounded()` is
true when nothing is fabricated **and at least one trace is cited** — the second
half matters, because "no unknown IDs" is trivially true of a summary that cites
nothing at all, which is the exact failure ADR 0009 is about.

**This is provenance, not support — Partial → #53.** Two different questions:

- *provenance* — was this trace actually fetched? Decidable here, because the
  conversation records what came back.
- *support* — does this trace bear out the claim attached to it? A real trace
  cited for something it does not show is still wrong.

Provenance is the cheaper check and it looks like the expensive one. A summary
whose every ID is real reads as verified; it is only **un-hallucinated**. #18
supplies the mechanism for support and #20 measures it.

## Where #16 stands

| Criterion | Status |
|---|---|
| Tool Runner loop with a `query_traces` typed tool | ✅ |
| Summary citing the traces it used | **Partial → #53** — provenance checked, support not |
| Telemetry as tool-result content, never instructions | ✅ at the wire — **Partial → #54**: structural, never adversarially tested |
| Distinguishes service failure from telemetry-path failure | **Partial → #51** — real Backend queries (#50); one host, not a fleet |
| No vendor query language crosses the tool seam | ✅ |

## What this still does not do

No API key is read and no request leaves the machine in any test — the model
adapter is exercised against `httptest` with recorded response bodies. Running it
for real needs a key and a `Config{Model, MaxTokens}`; both are refused at
construction when missing, because the API's 400 names neither.
