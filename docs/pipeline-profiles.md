# Pipeline Profiles, and compiling a Contract

A **Telemetry Contract** says *what* a service emits. A **Pipeline Profile** says *how* it ships. **Compiling** combines them into collector configuration (ADR 0005).

Those are three separate facts with three separate homes, and the split is strict:

| Fact | Lives in | Owner |
| --- | --- | --- |
| What this service emits | the service's Telemetry Contract | the service team |
| What its tier *must* emit | `guardrail/tiers.yaml` — the **Service Tier Taxonomy** | the platform team |
| How it ships | `controlplane/profiles.yaml` — the **Pipeline Profiles** | the platform team |

Compile reads all three. None of them restates another — notably, a Profile does **not** list mandatory Signals, because the Taxonomy already does and Standard S2 already enforces it. Two copies of that fact would drift, and the drift would be silent.

## Compiling

```
otel-guardrail compile path/to/telemetry-contract.yaml
```

Writes collector configuration for that service's **Agent** to stdout. Exit `0` compiled, `1` this Contract cannot be compiled, `2` the compiler could not run.

Compiling is **not** checking. `otel-guardrail check` tells you whether a Contract is *allowed*; `compile` tells you what would *ship*. A Contract that violates a `warn` Standard still compiles.

## What gets compiled

An Agent receives OTLP, stamps the Contract's resource attributes, batches, and forwards to the Gateway. It names no **Backend** and does no enforcement — fan-out to Backends is the Gateway's job (ADR 0007), landing with C5.

Stamping the resource attributes *from the Contract* is what makes **declared equals deployed** true by construction: the same file Preflight checked produces the running config, so the two cannot drift into disagreement.

## A Profile

```yaml
apiVersion: guardrail.otel/v1
kind: PipelineProfileSet
profiles:
  - profile: tier-1-critical
    tiers: [tier-1]
    description: Customer-facing services. Everything is kept.
    gateway_endpoint: otel-gateway.observability.svc.cluster.local:4317
    batch:
      timeout: 5s
      send_batch_size: 8192
    sampling:
      traces_percent: 100
```

A Service Tier selects exactly one Profile. Two Profiles claiming one tier is refused: pipeline shape would depend on file order.

## What refuses to compile

Compile stops rather than guessing, because each of these would otherwise put a quietly wrong config on the fleet:

| Situation | Why it is not a default |
| --- | --- |
| The Contract omits a Signal its tier mandates | The fleet would silently not collect telemetry the org requires, while the Contract still read as compliant. |
| The Contract names something that is not a Signal | A typo like `metricks` would become a pipeline for telemetry that does not exist. |
| The tier is outside the Taxonomy | There is no telemetry floor to compile against. |
| The tier has no published Profile | How its telemetry ships is undecided, and a default would be a pipeline nobody chose. |

## Which tiers can be compiled today

**`tier-1` only.** C1 (#9) is the tracer bullet: one Profile, one tier, end to end. `tier-2` and `tier-3` are real Service Tiers with no Profile yet, so they fail to compile with a message saying exactly that — which is the intended state, not a gap that silently produces something wrong. C2 (#11) adds them with real per-tier sampling and batching.

## Validation

A compiled config is checked for **referential integrity**: every component a pipeline names is defined, and every defined component is used by some pipeline. Dangling references are what this class of generator actually gets wrong, and an unused component means a Profile setting silently did nothing.

This is *not* the same as proving a given `otelcol` build would accept the file — that needs the collector binary in CI, which is a follow-up. The CLI test does assert the output parses as collector-shaped YAML with every pipeline wired end to end.

## For the Control Plane

```go
config, err := controlplane.Compile(declared, taxonomy, profiles)  // one entry point
config.Collects("traces")   // does it route this Signal?
config.Signals()            // everything it routes
config.Validate()           // internally coherent?
config.YAML()               // one rendering, not the thing itself
```

`CollectorConfig` is a typed value rather than a string of YAML so that a fleet-wide rollout can ask questions of it — and diff two rollouts by structure rather than formatting — before anything reaches a file.
