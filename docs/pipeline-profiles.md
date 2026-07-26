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
    description: Customer-facing services. Telemetry must appear fast and must not be lost.
    gateway_endpoint: otel-gateway.observability.svc.cluster.local:4317
    memory_limit_mib: 512
    batch:
      timeout: 5s
      send_batch_size: 8192
    delivery:
      queue_size: 10000
      retry: true
    sampling:
      traces_percent: 100
```

A Service Tier selects exactly one Profile. Two Profiles claiming one tier is refused: pipeline shape would depend on file order.

## What differs per tier, and why

Every tier has a default Profile, and they are not three copies of one pipeline. Criticality decides four things:

| | `tier-1` | `tier-2` | `tier-3` |
| --- | --- | --- | --- |
| `batch.timeout` | 5s | 15s | 30s |
| `memory_limit_mib` | 512 | 256 | 128 |
| `delivery.queue_size` | 10000 | 5000 | 1000 |
| `delivery.retry` | yes | yes | **no** |

- **Batch timeout** trades latency against cost. Tier-1 telemetry needs to be visible while an incident is happening; a batch job's does not.
- **Memory limit** — an Agent runs *beside* the service it collects from. A tier-1 Agent must never become the reason a latency-sensitive service degrades, so the ceiling is a per-tier decision rather than a global default.
- **Delivery** is the sharpest difference. Retrying protects telemetry the org cannot lose. **Not** retrying protects the service: an Agent applying back-pressure to a batch job, over telemetry nobody is waiting on, is a worse outcome than losing the telemetry. That is why `tier-3` sets `retry: false`.

A setting that would do nothing is not emitted. A Profile with no `delivery` and no `memory_limit_mib` compiles a config with no `sending_queue`, no `retry_on_failure` and no `memory_limiter` — rather than a disabled block that invites the next reader to think it is doing something.

## Head sampling is deliberately absent

No Profile does head sampling at the Agent. The Gateway tail-samples with the whole trace in hand (ADR 0007), and an Agent dropping spans first would hand it broken traces.

`sampling.traces_percent` is therefore the **Gateway's** tail-sampling budget for that tier — a per-tier cost decision the Profile owns, consumed when the Gateway config compiles (C5, #13). Nothing in an Agent config is derived from it today.

## What refuses to compile

Compile stops rather than guessing, because each of these would otherwise put a quietly wrong config on the fleet:

| Situation | Why it is not a default |
| --- | --- |
| The Contract omits a Signal its tier mandates | The fleet would silently not collect telemetry the org requires, while the Contract still read as compliant. |
| The Contract names something that is not a Signal | A typo like `metricks` would become a pipeline for telemetry that does not exist. |
| The tier is outside the Taxonomy | There is no telemetry floor to compile against. |
| The tier has no published Profile | How its telemetry ships is undecided, and a default would be a pipeline nobody chose. |

## Which tiers can be compiled today

**All of them.** Every Service Tier in the taxonomy selects a default Profile, so `compile` behaves uniformly across tiers — a test walks the taxonomy and asserts exactly that.

A tier that exists in the taxonomy but has no Profile — the state a tier is in between being added and having its pipeline shape decided — still fails to compile, naming itself. That is the intended behaviour, not a gap: a default there would be a pipeline nobody chose.

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

## Compiling the whole Fleet

`compile` handles one Contract to stdout. `otel-guardrail compile-fleet fleet` compiles every Contract in a **Fleet** into a committed tree of collector configuration, which is how config reaches the fleet at all — see [`docs/gitops-distribution.md`](gitops-distribution.md) for the layout, what happens when a Contract does not compile, and the scheduled job that proposes each **Rollout** as a pull request.
