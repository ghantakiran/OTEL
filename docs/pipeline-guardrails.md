# Pipeline Guardrails

A **Pipeline Guardrail** is a Standard enforced against **live telemetry**, at run time, in the Gateway. A **Preflight Guardrail** is the same Standard enforced against a **declared Telemetry Contract**, before deploy, in CI.

Same catalog. Same Severity. Different evidence, different moment.

|  | Preflight Guardrail | Pipeline Guardrail |
| --- | --- | --- |
| Asks | does the *declaration* comply? | does *reality* match the declaration? |
| Reads | `guardrail/examples/*.yaml` — a Telemetry Contract | OTLP records crossing the Gateway |
| Runs | `otel-guardrail check`, in CI, before deploy | the Gateway's collector processors, continuously |
| Written as | Rego (ADR 0002) | collector processors, compiled |
| On violation | exit 1, the build stops | the record is **tagged** and keeps going |

Neither replaces the other. A Contract can declare an attribute the service never emits; a service can emit for months against a Contract nobody checked. Preflight catches the first, the Pipeline Guardrail catches the second.

## Where a Pipeline Guardrail is defined

In `guardrail/standards.yaml`, the **Standard catalog** — the same file the Preflight Guardrail reads. There is no second place.

```yaml
- standard: S1
  title: Every service must declare the org's required resource attributes.
  severity: block
  enforced_at: [preflight, pipeline]
  requires:
    resource_attributes:
      - service.name
      - service.version
      - deployment.environment
```

`enforced_at` is what selects it. A Standard declares `preflight`, `pipeline`, or both; a Standard that declares neither, or something else, is a hard error against the catalog — exit 2, the same treatment an absent Severity gets.

## Why the catalog exists at all

**A collector cannot evaluate Rego.** So the Rego body of a Standard cannot run inside the Gateway, and "the same Standard, at a second enforcement point" had to mean something other than "the same code".

The tempting answer is to write the pipeline check separately — a processor block per Standard, beside the Rego. That gives one Standard **two definitions, in two languages, failing independently**. A service would pass Preflight against one list of required attributes while the Gateway tagged its telemetry against another, and nothing would show it. This repository has removed that exact shape three times already: the Service Tier taxonomy defined in both Go and Rego (#28), the Waiver register pinned in two places (#32), the summary line computed twice (#33).

So the *requirement* moves out of the Rego body into the catalog, and both enforcement points read it:

```
                 guardrail/standards.yaml
                    (one definition)
                   /                 \
      data.otel.standards        PipelineEnforced()
             |                            |
        the .rego                the Gateway compiler
             |                            |
   otel-guardrail check        transform/guardrail in the
   (declared Contract)         compiled Gateway config
                               (live telemetry)
```

It is `guardrail/tiers.yaml` applied a second time — a document the platform owns, that policy consumes and never declares. Tests assert that no `.rego` file names a required attribute or a Severity.

## Not every Standard can be enforced at the pipeline

A requirement has a **kind**, and the kind decides where it can run:

| Kind | preflight | pipeline | |
| --- | --- | --- | --- |
| `resource_attributes` | yes | yes | A property of one record. Both points can answer it. |
| `tier_mandatory_signals` | yes | **no** | A property of a stream over time. A collector sees one record and cannot tell "this service emits no metrics" from "no metric has arrived in the last second". |

A Standard that declares `enforced_at: [pipeline]` with a requirement of the second kind **does not load**:

```
otel-guardrail: Standard catalog guardrail/standards.yaml: Standard S2 declares
`enforced_at: [pipeline]`, but its tier_mandatory_signals requirement is not
expressible as collector processors — a Gateway inspects one record at a time and
cannot answer it, so there is nothing to compile (drop "pipeline" from `enforced_at:`)
```

Refusing is the point. The alternative is a Gateway that reads as enforcing a Standard it is not enforcing, which is worse than a Gateway that says it cannot. Cardinality-class Standards are the important case this rules out today — see **#43**.

`enforced_at` binds in both directions. A Standard the catalog does **not** enforce at preflight cannot report a violation there either: the Rego aggregator picks up every policy in `guardrail/policies/` with no registration step, so a Standard moved to `[pipeline]` while its `.rego` stayed behind would go on failing builds at a point the catalog says it is not enforced at. That is a broken catalog, and it stops the run.

## What a Pipeline Guardrail does on violation

CONTEXT.md says a Pipeline Guardrail acts by **drop, tag, or alert**.

**Severity decides how loudly the violation is marked. It never decides whether the telemetry survives.**

| Severity | What the Gateway does to the record |
| --- | --- |
| `info` | sets `otel.guardrail.violation.<id>` = `"info"` |
| `warn` | sets `otel.guardrail.violation.<id>` = `"warn"` |
| `block` | sets `otel.guardrail.violation.<id>` = `"block"`, **and** `otel.guardrail.blocking` = `true` |

Nothing is dropped, at any Severity. `block` → drop is the obvious reading and it is wrong, for three reasons argued in full in [ADR 0015](./adr/0015-pipeline-guardrails-compile-from-the-catalog-and-never-drop.md):

- it destroys the evidence of the violation at the moment it is most wanted — telemetry goes malformed during exactly the incidents where somebody needs it;
- a dropped stream is indistinguishable from a service that is down, which is the failure mode this platform refuses everywhere else;
- Preflight's `block` stops the *violator* (the deploy), and the runtime analogue of that is not deleting the observation. The record is not the violator; the service is.

## How an operator reads a Guardrail event

There is no Guardrail event stream, no status endpoint and no side channel — deliberately (ADR 0010). **The evidence is on the telemetry itself**, in whatever Backend that Signal fans out to.

A non-compliant record arrives looking like this:

```
Resource attributes:
     -> service.name: Str(drifted-checkout-worker)
     -> otel.guardrail.violation.S1: Str(block)
     -> otel.guardrail.blocking: Bool(true)
     -> otel.guardrail.violation.S3: Str(warn)
```

Read it as:

- **`otel.guardrail.violation.<id>`** — *this record violated Standard `<id>`, and that Standard's Severity is the value.* One key per Standard, so a record violating two names both and neither can overwrite the other. To find the Standard, look it up in `guardrail/standards.yaml`: the entry says what it requires and why.
- **`otel.guardrail.blocking: true`** — *at least one Standard the org blocks builds for was violated.* This is the roll-up to count and alert on: one low-cardinality boolean, rather than a query that has to know every Standard id in advance. Every blocking Standard writes the same value, so no statement order changes what you see.
- **No `otel.guardrail.*` attribute at all** — the record met every Standard enforced at the pipeline. Compliant telemetry is untouched.

**The whole `otel.guardrail.` namespace belongs to the Gateway.** It is cleared unconditionally before the verdict is written, so nothing a service sends under it survives. Without that, a service could emit `otel.guardrail.blocking: false` on its own resource and the single field an operator alerts on would be the easiest thing on the platform to suppress. Read anything in that namespace as the Gateway's verdict, never as something a service said about itself.

Three questions it answers directly:

| Question | How |
| --- | --- |
| Which services are non-compliant right now? | group by `service.name` where `otel.guardrail.blocking = true` |
| Which Standard is each failing? | the `otel.guardrail.violation.*` key present on the record |
| Is it getting better? | the same query over time — the tag is on ordinary telemetry, so the usual rate/trend tooling applies |

And one it does **not**: *how many* violations. The tag is on records, not a counter, so counting is whatever your Backend can do with an attribute. A real counted signal is C7's (#15), with the concrete mechanism recorded in **#45**.

### The Layer 1 answer is different, and both are right

A service tagged `otel.guardrail.violation.S1: block` at the Gateway is **not** necessarily failing CI. Severity is the same at both points, but what happens to it is not:

- At Preflight, a `block` Standard's **Effective Enforcement** may be held back by a **Waiver** or by the **Enforcement Epoch** — the build passes, with the Hold named on the line.
- At the pipeline, no Hold applies. Holds are about failing a *build*, and there is no build here; the record is tagged either way.

So the tag says "reality violates this Standard", not "this team is in trouble". The two are answering different questions on purpose. A waived service still gets tagged, which is what makes a Waiver's real-world cost visible while it is in force.

**This has a sharp edge, and it is deliberate.** `otel.guardrail.blocking` is described above as the field to alert on, and a waived service raises it like any other — with nothing on the record saying it is waived. An alert built on it alone will page for a violation the platform team has already approved and dated. That is the honest reading: a Waiver holds back a *build*, not the fact that telemetry is non-compliant, and an operator triaging an incident wants to know the telemetry is thin whether or not somebody signed a form. Joining the tag to `guardrail/waivers.yaml` to separate "unapproved" from "approved and expiring" is a real piece of work and belongs with the alerting itself — see **#45**.

## What C7 reads from this

C7 (#15) builds the platform's self-observation on ADR 0010's decision that the platform watches its own telemetry rather than a status back-channel. Pipeline Guardrails fit that exactly, and are deliberately not extended into it here:

- **The violation is in the telemetry**, not in a Gateway-only log or metric. It reaches every Backend the Signal fans out to, so it is available to whatever C7 chooses to read.
- **`otel.guardrail.blocking` is the one field to alert on.** It is low-cardinality and stable, so a threshold on it does not have to change when a Standard is added.
- **The Standard id is on the record**, so "which Standard, which service" is read off the data rather than inferred.
- **Counting is C7's**, not C6's. The `count` connector is available (the Gateway already runs contrib, ADR 0014) and would turn the roll-up into a real metric labelled by Standard and Severity — counting the same tag the transform writes, so there is still one source. **#45** records it.

## What runs in the Gateway

`otel-guardrail gateway` compiles the pipeline-enforced Standards into one `transform` processor:

```yaml
transform/guardrail:
    error_mode: ignore
    trace_statements:
        - context: resource
          statements:
            - delete_matching_keys(attributes, "^otel\\.guardrail\\.")
            - set(attributes["otel.guardrail.violation.S1"], "block") where attributes["service.name"] == nil or attributes["service.version"] == nil or attributes["deployment.environment"] == nil
            - set(attributes["otel.guardrail.blocking"], true) where attributes["service.name"] == nil or attributes["service.version"] == nil or attributes["deployment.environment"] == nil
            - set(attributes["otel.guardrail.violation.S3"], "warn") where attributes["service.namespace"] == nil or attributes["service.instance.id"] == nil
    metric_statements: …
    log_statements: …
```

Notes on the shape, each load-bearing:

- **One processor, not one per Standard.** It is a single pass over each record, and a pipeline listing eight processors named after Standards would make their order look meaningful when it is not.
- **The namespace is cleared first**, unconditionally, so a service cannot forge its own verdict. Everything after that statement is the Gateway's, and their order carries no meaning — each Standard writes its own key, and the roll-up is written with the same value by every blocking Standard.
- **The same statements for all three Signals.** The requirement is about the record's resource, and a resource is a resource whether it carries spans, metrics or logs. A Standard enforced on traces but not on logs would mean something different depending on what a service happens to emit.
- **`context: resource`.** Resource attributes are what a Standard of this kind is about.
- **`error_mode: ignore`.** A Guardrail that takes the Gateway down is worse than the violation it was watching for. A record whose evaluation fails is logged and passed through unmodified. That branch is untested — **#45**.
- **Before `batch`, after `memory_limiter`, upstream of the fan-out.** A record is judged once rather than once per Backend, so no two Backends can receive a different verdict; and batching stays last, because it is how telemetry leaves rather than something that decides anything about it.
- **`transform` is a contrib processor.** The Gateway already runs the contrib distribution for spill (ADR 0014), so this adds no new constraint. **Agents stay core-only**, and nothing compiles a Guardrail into one.

### It is centralised, and that is the point

The processors are emitted for the Gateway and for nothing else. Agents do no enforcement (ADR 0007). Beyond that being the declared topology, a Standard enforced in a thousand Agents is a Standard with a thousand places to be out of date — and the Gateway is the only place that sees the whole fleet.

Both reference Agent compiles are byte-identical to before Pipeline Guardrails existed, and a test asserts an Agent's compiled config never mentions `otel.guardrail`.

## Authoring one

1. Add or extend an entry in `guardrail/standards.yaml`. Give it a `severity`, an `enforced_at` including `pipeline`, and a `requires:` of a kind that can be expressed at the pipeline.
2. If it is also `preflight`, write the `.rego` in `guardrail/policies/` — reading `data.otel.standards.<id>` for its requirement and its Severity, never a literal. See [standards.md](./standards.md).
3. If it can `block` at preflight, publish a graduation deadline in `guardrail/enforcement.yaml` (ADR 0003).
4. `otel-guardrail gateway` and read the `transform/guardrail` block. If the Standard cannot be compiled, it says so and exits 2.
5. The Gateway picks it up on the next Rollout — a commit and a normal GitOps rollout (ADR 0006), like any other config change.

## Seeing it work

`harness/run.sh` stands up the whole topology on compiled configs and observes it, on real collectors, over the wire:

- Everything the **compliant** Agent sends carries **no** `otel.guardrail` attribute. Asserted first, before any non-compliant telemetry exists — afterwards the Backend's log would match either way.
- A **second Agent**, compiled from `harness/drifted-contract.yaml` (which declares one of the three resource attributes S1 requires), emits a span. It **arrives** at the Backend by trace ID — nothing was dropped — carrying `otel.guardrail.violation.S1`, `otel.guardrail.violation.S3` and `otel.guardrail.blocking`.
- **Neither Agent's compiled config mentions any of it**, so the Gateway is the only thing that could have written those attributes.

That contract is one `otel-guardrail check` exits 1 on, deliberately. It stands in for the two ways reality drifts from a declaration: a service that predates its Standards, and telemetry that never went through Preflight. Layer 1 would have stopped it at the build; Layer 2 is what sees it when Layer 1 did not run.

What the harness does **not** prove — load, real Backends, `error_mode: ignore`, alerting as a number — is recorded in full in **#45**.
