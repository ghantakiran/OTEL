# The platform observes itself

There is **no health API, no heartbeat and no status back-channel** on this
platform. There is deliberately not one, and there is not going to be one
(ADR 0006, ADR 0010). Everything an operator needs to know about the Agents and
the Gateway arrives the same way a service's telemetry does: as OTEL, through the
Gateway, into a Backend.

That means two things you can watch, and this page is about how.

| Question | What answers it |
| --- | --- |
| Did the Rollout I merged actually reach the fleet? | `otel.platform.config_version` on the platform's own metrics |
| Is anything backing up, and where? | the collector's own queue, export-failure and drop counters, **per Backend** |

## Confirming a Rollout

### 1. Merge the rollout and read the Manifest

`otel-guardrail compile-fleet fleet` writes `fleet/rollout-manifest.yaml`. Every
service that compiled has two hashes, and they are **not** interchangeable:

```yaml
    - service_name: orders-api
      tier: tier-1
      pipeline_profile: tier-1-critical
      collector_config: compiled/orders-api.yaml
      digest: sha256:c239ced9…
      config_version: sha256:df1d11f6…
```

- **`digest`** hashes the *file*, header and stamp included. It answers "is
  `compiled/orders-api.yaml` in this repo still the one this Rollout wrote?" —
  which is what catches a hand-edited `config_version`.
- **`config_version`** is what that service's Agent will **report about itself**
  once the Rollout reaches it. It hashes the collector configuration with its own
  stamp excluded, so it identifies the running pipeline rather than the file.

Only `config_version` can be compared against telemetry. Comparing a `digest` to
something you saw in a Backend is a category error — they hash different things and
are never equal.

The Gateway is not in the Fleet, so its expected version comes from the compiler:

```
otel-guardrail gateway | grep otel.platform.config_version
```

### 2. Watch for it fleet-wide

The platform's own metrics all carry the `otelcol_` prefix and a resource whose
identity is the service's own Telemetry Contract (for an Agent) or the Gateway
Declaration's `self_telemetry.resource_attributes` (for the Gateway). Any of them
carries the version, so pick a metric every collector always emits:

```promql
# every collector currently reporting, and which configuration it is running
count by (service_name, otel_platform_config_version) (otelcol_process_uptime_seconds_total)
```

```promql
# the services NOT yet on the version this Rollout compiled for them
count by (service_name) (
  otelcol_process_uptime_seconds_total{otel_platform_config_version != "sha256:df1d11f6…"}
)
```

**Two things about those queries are the Backend's doing, not the platform's**, and
both were assumed here until a real one was put behind the Gateway (#50):

- The metric is `otelcol_process_uptime_seconds_total`, not `otelcol_process_uptime`.
  Prometheus appends the unit and `_total` on OTLP ingest. The un-suffixed name
  returns **empty**, which reads exactly like a fleet that has not rolled out.
- `otel_platform_config_version` is on the series only because the Backend was told
  to put it there. By default Prometheus files resource attributes on a separate
  `target_info` metric and these queries find nothing. Promote it, or join through
  `target_info` — a Backend that does neither cannot confirm a Rollout at all.

Both rules, the measured evidence, and the portable `target_info` join are in
[backend-label-mapping.md](./backend-label-mapping.md).

The Rollout is confirmed when, for every service in the Manifest, the version
reported equals the version recorded — and the Gateway reports its own. Nothing was
polled and nothing was asked.

**It is eventually consistent, and that is by design.** The floor is the
self-telemetry interval (30s) on top of however long the GitOps loop takes to
deliver the file and restart the collector. GitOps rollout is already asynchronous
(ADR 0006), so a transactional confirmation would be a promise the delivery
mechanism cannot keep.

### What a version does and does not move on

`config_version` identifies **what is running**, not what it was compiled from.

- Changing a Pipeline Profile's `batch.timeout`, a Contract's resource
  attributes, the Gateway's Backends — all reach the compiled config, so all
  change the version.
- Recompiling unchanged inputs changes nothing: the file is byte-identical, the
  version is identical, and `compile-fleet` reports the rollout as a no-op.
- An input that does **not** reach the compiled config does not move the version.
  `sampling.traces_percent` is the standing example — nothing reads it yet (#40).

ADR 0016 has the full argument for why it is derived from the artefact rather than
from the inputs.

## Detecting back-pressure

The collector's own instrumentation is routed, not invented. At
`service.telemetry.metrics.level: normal` the ones that matter here are:

| Metric | Where | Means |
| --- | --- | --- |
| `otelcol_exporter_queue_size` | Agent and Gateway | batches waiting to be sent, **now** |
| `otelcol_exporter_queue_capacity` | Agent and Gateway | what `delivery.queue_size` compiled to |
| `otelcol_exporter_send_failed_spans` / `_metric_points` / `_log_records` | Agent and Gateway | export attempts that failed for good |
| `otelcol_exporter_enqueue_failed_*` | Agent and Gateway | telemetry **dropped** because the queue was full |
| `otelcol_receiver_refused_*` | Agent and Gateway | telemetry rejected on the way in, usually the memory limiter |

Every exporter metric carries an `exporter` label, and that label **is the
Backend's name**: `otlp/primary-apm`, `otlp/metrics-store`, `otlp/cold-archive`.
That is why Backends are role-named and why each gets its own exporter — it is what
makes "which Backend is behind?" a query rather than a guess.

```promql
# which Backend is backing up, right now
max by (exporter) (otelcol_exporter_queue_size{service_name="otel-gateway"})
```

```promql
# how close each Backend is to dropping
max by (exporter) (
    otelcol_exporter_queue_size{service_name="otel-gateway"}
  / otelcol_exporter_queue_capacity{service_name="otel-gateway"}
)
```

```promql
# telemetry actually lost, per Backend — the alert worth paging on
sum by (exporter) (rate(otelcol_exporter_enqueue_failed_spans_total[5m])) > 0
```

The two queue metrics are gauges and keep their names; the failure counters are
sums and gain `_total`, like every counter above. The `exporter` label and the
declared `queue_capacity` were both measured surviving into a real Backend
unchanged — see [backend-label-mapping.md](./backend-label-mapping.md) rules 4
and 5. `otelcol_exporter_enqueue_failed_*` is the exception: nothing here has ever
driven a queue to capacity, so that counter has still never been seen non-zero
(#49).

An Agent's numbers use the same metrics with `exporter="otlp/gateway"`, filed under
the service's own `service.name` — so "why is this service's telemetry thin?" is
answered under that service rather than in a pool of anonymous collectors.

### What a healthy fan-out looks like when one Backend fails

One Backend stalling shows up as **its own** numbers moving and nobody else's:

```
otelcol_exporter_queue_size{exporter="otlp/cold-archive"}   1  ← holding
otelcol_exporter_queue_size{exporter="otlp/primary-apm"}    0  ← unaffected
otelcol_exporter_sent_spans{exporter="otlp/primary-apm"}    rising
```

If two Backends' queues rise together, either both destinations are genuinely down
or the isolation has been broken — the second would mean an exporter shared between
Backends, which does not compile (ADR 0014, ADR 0016).

## The bootstrap dependency, stated plainly

**The platform's own signals do not travel the pipeline they report on.**
`service.telemetry.metrics.readers` is a separate OTLP client with no sending
queue, no retry, no batch and no memory limiter in front of it:

```
   Agent ──pipeline── otlp/gateway ─(queue, retry)──▶ Gateway
     └────self-telemetry, own client, no queue ─────▶ Gateway

 Gateway ──pipeline── otlp/primary-apm ─(queue, retry, spill)──▶ primary-apm
     │             ── otlp/metrics-store ────────────────────▶ metrics-store
     │             ── otlp/cold-archive ─────────────────────▶ cold-archive
     └────self-telemetry, own client, no queue ──────────────▶ primary-apm
```

So a full `otlp/gateway` queue cannot hold up the metric that says the queue is
full, and a Backend that has stopped answering cannot hold up the Gateway's report
that it stopped answering. That is the difference between self-observation that
works when things are fine and self-observation that works when they are broken,
which is the only time it matters.

**What is still not covered, and it is not pretended otherwise:**

- **An Agent cannot report through a Gateway that is down.** An Agent's only next
  hop is the Gateway; it names no Backend (ADR 0007), and giving every Agent a
  direct Backend route would end Backend-agnosticism for the whole fleet. What
  reports a failing Gateway is the Gateway's own path, which goes straight to a
  Backend — and a Gateway that is not running reports nothing at all. Absence is
  the signal there. (#47)
- **The Gateway's self-telemetry goes to one Backend.** If that is the Backend
  that fails, the Gateway goes dark. `self_telemetry.backend` in
  `controlplane/gateway.yaml` is where that choice is made: pick the most
  available one.

## How it fits with the rest of Layer 2

**With the fan-out (C5).** Per-Backend isolation is what makes back-pressure
attributable at all: each Backend has its own exporter, named after it, so the
`exporter` label on every queue and failure metric names one Backend and no other.
Compiling refuses two Backends sharing a name or an exporter, so the label cannot
become ambiguous.

**With the Pipeline Guardrails (C6).** An Agent's own telemetry reaches the Gateway
through the ordinary pipeline, so a Guardrail judges it — and judges it **exactly
as it judges that service**, because the Agent's own resource is the identity its
Telemetry Contract declares. A compliant service's Agent is untagged; a drifted
service's Agent is tagged `otel.guardrail.violation.S1=block` like the service's own
records. There is no exemption, because an exemption would be a resource attribute
and any service could write one.

The **Gateway's** own telemetry never enters a pipeline, so it is never tagged.
Instead the Gateway is held to the same Standards when it compiles: a Gateway whose
`self_telemetry.resource_attributes` omit an attribute a `block` Standard requires
at the pipeline **does not compile**. The platform does not tag the fleet for a rule
its own telemetry breaks.

The two namespaces sit side by side on the same records and stay out of each
other's way:

| Namespace | Written by | Cleared on |
| --- | --- | --- |
| `otel.guardrail.` | the Gateway, as its verdict about a record | every context, before the verdict is written |
| `otel.platform.` | a collector, about itself | every context **except the resource** — the resource is where a collector's own stamp legitimately arrives |

## `otel.platform.` is the platform's, and services cannot borrow it

`otel.platform.config_version` is the single field a Rollout is confirmed by, so a
service able to write it could confirm rollouts it never received. Three things
close that:

1. A **Telemetry Contract** declaring anything under `otel.platform.` does not
   compile, and neither does a Gateway Declaration.
2. The **Agent** deletes the namespace from everything it forwards — the last
   action on its `resource` processor, after the upserts. The Agent is the only
   component that can: its own signals do not travel its own pipelines, so it can
   strip the namespace from the service's telemetry without stripping its own
   answer.
3. The **Gateway** sweeps the namespace from spans, datapoints, log records and
   scopes — everywhere it can only be a forgery. Most Backends flatten record and
   resource attributes into one queryable field, so a record-level key would
   otherwise answer a rollout query just as well as a real one.

## Seeing it run

`bash harness/run.sh` drives the whole thing end to end on real collectors: both
compiled configs started with no overlay at all, both config versions arriving in a
Backend, a Pipeline Profile changed and the new version appearing after the Agent is
replaced, and one Backend's queue filling while another's stays empty. What it does
and does not prove is in
[agent-gateway-topology.md](./agent-gateway-topology.md#what-the-harness-proves-and-what-it-does-not).

Every Backend in that run is a collector with a `debug` exporter, so what it shows
is telemetry crossing a wire and being printed. **Whether a Backend can render any
of it as a query** is a different claim, and it has its own run:

```
bash harness/verify-backend-rendering.sh
```

That stands a real metrics Backend and a real trace store behind the same
unmodified compiled configs and asks for the platform's own telemetry back, in the
form the queries on this page use. It is where the two corrections above came from.
See [backend-label-mapping.md](./backend-label-mapping.md).
