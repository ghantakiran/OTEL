# What a Backend calls the platform's own telemetry

The platform reports on itself in OTLP: a **resource** carrying
`otel.platform.config_version`, and metrics carrying an `exporter` attribute that
names one **Backend**. [platform-self-observation.md](./platform-self-observation.md)
is about what those mean. This page is about what a Backend *calls* them once it
has them, which is not the same question and had never been asked.

Everything here was **discovered by running it**, not derived from a specification.
`bash harness/verify-backend-rendering.sh` stands the platform up in front of a
real Backend and asserts every rule below; if a rule stops being true, that script
fails rather than this page quietly going stale (#50).

> **A Backend's mapping is the Backend's, not the platform's.** What this platform
> guarantees is that `otel.platform.config_version` is **on the resource** and that
> each Backend gets **its own exporter, named after it**. How a product surfaces
> either is that product's business, and it belongs in an adapter — here,
> `harness/backend-real-collector.yaml` and `harness/prometheus.yml`. Nothing
> compiled, no Telemetry Contract, and nothing P1's `query_traces` tool reads knows
> a vendor's name (ADR 0007).

## The Backend these rules were measured against

Prometheus 3.x, ingesting OTLP directly (`--web.enable-otlp-receiver`), pinned in
`harness/backend-images.env`. It fills the metrics half of the `primary-apm` role
for the duration of a harness run. Tempo fills the trace half.

## Rule 1 — a dot becomes an underscore

`otel.platform.config_version` is not a legal Prometheus label name. It arrives as:

```
otel.platform.config_version   →   otel_platform_config_version
```

Same for every other resource attribute: `service.name` → `service_name`,
`deployment.environment` → `deployment_environment`.

## Rule 2 — the metric name gains a unit and a suffix

**This is the one that had been written down wrong.** OTLP metric names are
translated on ingest: the unit is appended, and a monotonic sum gains `_total`.

| What the collector emits | What you query |
| --- | --- |
| `otelcol_process_uptime` | `otelcol_process_uptime_seconds_total` |
| `otelcol_exporter_sent_spans` | `otelcol_exporter_sent_spans_total` |
| `otelcol_exporter_send_failed_spans` | `otelcol_exporter_send_failed_spans_total` |
| `otelcol_exporter_enqueue_failed_spans` | `otelcol_exporter_enqueue_failed_spans_total` |
| `otelcol_exporter_queue_size` | `otelcol_exporter_queue_size` (a gauge — unchanged) |
| `otelcol_exporter_queue_capacity` | `otelcol_exporter_queue_capacity` (a gauge — unchanged) |

The operator queries in `platform-self-observation.md` named the un-suffixed forms.
Run against a real Backend they returned **empty** — and an empty result from a
rollout-confirmation query is the most dangerous possible answer, because it looks
exactly like a fleet that has not rolled out yet. That is now a checked fact rather
than an assumed one.

## Rule 3 — a resource attribute is not on the series unless you ask

By default Prometheus does **not** put OTLP resource attributes on the series. It
writes them once to a synthetic `target_info` metric and leaves the real series
carrying only `job` and `instance`:

```
job       =  <service.namespace>/<service.name>     e.g. observability/otel-gateway
instance  =  <service.instance.id>
```

So `count by (service_name, otel_platform_config_version) (…)` returns nothing —
not because the platform failed to report its version, but because the Backend
filed it elsewhere. There are two ways to get it back, and they have different
costs:

**Promote it** (what `harness/prometheus.yml` does):

```yaml
otlp:
  promote_resource_attributes:
    - service.name
    - service.namespace
    - otel.platform.config_version
```

The attribute then appears on every series and the queries read naturally. The cost
is a Backend-side configuration that must be kept in step with the platform, and
one extra label on every series.

**Or join through `target_info`**, which needs no Backend configuration at all:

```promql
count by (service_name, otel_platform_config_version) (
  otelcol_process_uptime_seconds_total
    * on (job, instance) group_left(otel_platform_config_version, service_name)
  target_info
)
```

The join is the portable form: it is the one to reach for on a Backend whose
configuration you do not own. Promotion is the readable form. This repository's
harness promotes, because the operator queries it publishes should be the ones an
operator can paste.

**Either way it is a Backend-side decision, and a Backend that does neither cannot
confirm a Rollout.** That is a real constraint on adopting a Backend, and it is the
kind of thing worth knowing before an incident rather than during one.

## Rule 4 — the `exporter` label survives, and it is the Backend's name

Per-Backend attribution comes through intact. Measured, on a Gateway fanning out to
three Backends with one of them unreachable:

```
otelcol_exporter_queue_size{service_name="otel-gateway", exporter="otlp/cold-archive"}  = 1
otelcol_exporter_queue_size{service_name="otel-gateway", exporter="otlp/primary-apm"}   = 0
otelcol_exporter_queue_size{service_name="otel-gateway", exporter="otlp/metrics-store"} = 0
otelcol_exporter_queue_size{service_name="checkout-api", exporter="otlp/gateway"}       = 0
```

One Backend holding and the others not, each naming itself — which is what makes
"which Backend is behind?" a query. The last line is an **Agent's** own exporter,
filed under the service's own `service_name`, which is what makes "why is this
service's telemetry thin?" answerable under that service.

## Rule 5 — the declared delivery settings read back unchanged

`queue_size` is declared per Backend in `controlplane/gateway.yaml`, compiled into
the Gateway, held by a running collector, exported as OTLP, and rendered by the
Backend:

```
otelcol_exporter_queue_capacity{exporter="otlp/primary-apm"}   = 20000
otelcol_exporter_queue_capacity{exporter="otlp/metrics-store"} = 10000
otelcol_exporter_queue_capacity{exporter="otlp/cold-archive"}  =  2000
```

Those are the Declaration's numbers, unchanged, at the far end of the whole chain.
It matters because "how close is this Backend to dropping?" is a ratio against this
denominator — if the denominator were someone else's, the answer would be wrong in
a way nothing would flag.

## Rule 6 — a service's span reaches a Backend with no platform namespace on it

Checked against the trace store rather than against a debug log for the first time:

```
resource on the stored span:
  service.name         checkout-api      ← what the Telemetry Contract declared
  service.namespace    payments
  service.version      2.4.1
  deployment.environment production
  service.instance.id  checkout-api-7d9f4b
```

No `otel.platform.*`. The Agent deleted it on the way through, so a service cannot
confirm a Rollout it never received — the claim in
[platform-self-observation.md](./platform-self-observation.md#otelplatform-is-the-platforms-and-services-cannot-borrow-it),
now confirmed where it would actually have to hold.

Note that the span carries the identity the **Contract** stamped, not the one the
service sent: the sample service emits `service.name: harness-sample-service` and
what is stored is `checkout-api`. Searching a real trace store by the sending
process's own idea of its name finds nothing. That is the correct behaviour and it
is a trap worth knowing about — it is the query shape P1's `query_traces` will
front (#16).

## What this does not establish

- **One Backend, one product.** Every rule above is Prometheus's and Tempo's. A
  different product will mangle differently or not at all, and the only honest way
  to know is to run this script against it. What is portable is the *shape* of the
  question, not the answers.
- **One host.** Everything here was measured on a single machine with two
  collectors reporting. Whether these queries stay usable across a real fleet —
  partial rollouts, stragglers, an Agent that never restarted — is #51, and it is
  untouched by this page.
- **Cardinality at fleet scale.** One `otelcol_*` series per exporter per collector
  per signal is a real cost in a real metrics Backend. Two collectors do not
  measure it (#49).
- **Load and hard failure.** `otelcol_exporter_enqueue_failed_*` — the counter that
  means telemetry was *lost*, and the one worth paging on — has still never been
  observed non-zero here, because nothing approaches a 20,000-batch queue (#49).
