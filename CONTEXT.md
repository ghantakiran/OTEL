# OTEL Platform

An internal platform for managing OpenTelemetry across an engineering org. Delivered in three layers, built in order: **Guardrails** (standards enforcement), then a **Control Plane** (collector/pipeline management), then a **Copilot** (GenAI-assisted observability). This glossary defines the shared language for that platform.

## Language

### Copilot

**Copilot**:
The GenAI (Claude-driven) layer that reads standardized telemetry and assists incident response — summarizing, ranking root-cause hypotheses, and drafting or executing remediation depending on its current Autonomy Rung. Built after the Control Plane.
_Avoid_: Assistant, bot, AI, agent (bare)

**Autonomy Ladder**:
The staged progression that governs how much the Copilot may do on its own: **Advisor** (read + suggest, human executes) → **Gated** (executes an allowlist of actions behind human approval + audit) → **Bounded Autonomy** (acts alone inside a blast-radius cage). A rung is earned by measured accuracy before the next is enabled.
_Avoid_: Autonomy level, permission tier

**Autonomy Rung**:
The Copilot's current position on the Autonomy Ladder — Advisor, Gated, or Bounded Autonomy — which fixes what actions it may take.
_Avoid_: Mode, level, state

**Grounding**:
The requirement that every claim the Copilot makes cites the specific telemetry evidence behind it (a trace, metric query, or log query); an ungrounded hypothesis is suppressed or flagged low-confidence.
_Avoid_: Citation, sourcing, attribution

**Eval Harness**:
An offline system that replays past incidents with a known root cause and scores the Copilot's root-cause accuracy. Its score is the objective gate for promoting the Copilot to the next Autonomy Rung.
_Avoid_: Test suite, benchmark, evaluation

**Incident Corpus**:
The labeled dataset the Eval Harness replays — past incidents with confirmed root cause, seeded from postmortems and augmented with fault-injection experiments.
_Avoid_: Dataset, test set, training data

**Harm Set**:
A collection of scenarios where the correct Copilot behavior is to take no action (or a strictly safe one); a candidate action rung must produce zero harmful actions across it, in shadow, before promotion. Distinct from the Incident Corpus, which measures root-cause accuracy.
_Avoid_: Safety test, red-team set, adversarial set

### Control Plane

**Control Plane**:
The layer that turns Telemetry Contracts into running collector configuration and distributes it to the fleet. Built after Guardrails.
_Avoid_: Config manager, orchestrator, agent manager

**Pipeline Profile**:
A named, central, org-owned template that defines *how* telemetry ships — exporters, sampling, batching — as opposed to *what* a service emits (the Contract). A service's Service Tier selects its default Profile; a rare, platform-approved override may assign another.
_Avoid_: Config template, preset, pipeline config

**Compile**:
The Control Plane action of combining a Telemetry Contract (what to emit) with its tier's Pipeline Profile (how to ship it) to produce a service's collector configuration.
_Avoid_: Generate, render, build

**Fleet**:
The set of services whose Telemetry Contracts the Control Plane Compiles together — one Contract per service, gathered in the config repo and filed under the service's own name. A Fleet is the unit a Rollout covers.
_Avoid_: Estate, inventory, all services, cluster

**Rollout**:
One fleet-wide Compile, committed to git and applied to the Agents and the Gateway by existing GitOps tooling. Reconfiguring the fleet is a commit plus a normal rollout, never a push from a server (ADR 0006) — so a Rollout is something a human reviews as a diff before it reaches anything.
_Avoid_: Deploy, push, sync, apply

**Rollout Manifest**:
The committed index of one Rollout: every service in the Fleet, whether its collector configuration Compiled, and the reason it did not. It exists because a diff shows what changed and never what is absent — without it, a Rollout covering only part of the Fleet would be invisible to the reviewer.
_Avoid_: Index, lockfile, status file, report

**Agent**:
A lightweight collector running next to a service (sidecar or node daemon) that collects telemetry and forwards it to the Gateway. Agents do no enforcement.
_Avoid_: Collector (bare), sidecar, node collector

**Gateway**:
The central collector tier that receives from Agents, batches, tail-samples, and exports to backends. The single place where Pipeline Guardrails run.
_Avoid_: Central collector, aggregator, proxy

**Backend**:
A destination system where telemetry lands. The Gateway fans out to one or more Backends per the Gateway Declaration; services never target a Backend directly. A Backend is named for the **role** it fills — `primary-apm`, `metrics-store`, `cold-archive` — never the product filling it, so swapping the product stays one edit to one file. It declares which Signals it receives and its own durability, and compiles to its own exporter, isolated from every other Backend's.
_Avoid_: APM, sink, store, destination; and any vendor's name

**Backend Isolation**:
The property that no two Backends share anything that would let one stall the others: each has its own exporter, its own sending queue, its own retry, and — when it Spills — its own storage on its own directory. It is the reason to fan out from one Gateway rather than several, and it is enforced by construction: the storage and directory are derived from the Backend's name, and two Backends cannot share a name.
_Avoid_: Independence, decoupling, per-backend config

**Spill**:
Keeping a Backend's sending queue on disk rather than in memory, so telemetry queued for a Backend that is not answering survives the Gateway restarting. Declared per Backend; each Spilling Backend writes to its own directory under the Gateway's one spill root. It is the single reason the Gateway runs the collector's contrib distribution rather than core (ADR 0014).
_Avoid_: Disk buffer, persistence, dead-letter queue, overflow

**Self-Telemetry**:
The OTEL an Agent or the Gateway emits about *itself* — its Config Version, its queue depths, its export failures and its drops — as opposed to the fleet's telemetry it is carrying. It leaves by its own OTLP client with no queue, no retry and no batching, deliberately not through the pipeline it reports on, because the outage it exists to describe is exactly what would hold it up (ADR 0010, ADR 0016). It is why there is no health API, no heartbeat and no status back-channel on this platform, and none is to be built.
_Avoid_: Health check, heartbeat, metrics endpoint, internal metrics, status

**Config Version**:
The identity of one compiled collector configuration, as the collector running it reports it in its own Self-Telemetry: the sha256 of that configuration with the Config Version attribute itself excluded. It identifies *what is running*, never what it was compiled from — an input that does not reach the compiled config does not change it. A Rollout is confirmed when every service's Agent, and the Gateway, report the Config Version the Rollout Manifest recorded for them. Distinct from the Manifest's **digest**, which hashes the compiled *file*, header and stamp included, and answers whether that file is still the one the Rollout wrote; the two are never equal and only the Config Version can be compared against telemetry.
_Avoid_: Config hash, revision, generation, version (bare), digest

**Back-Pressure**:
What a collector reports when the next hop is not keeping up: a sending queue filling, exports failing, records dropped. At the Gateway it is per Backend and attributed by that Backend's exporter, which is named after it — so "which Backend is behind?" is answered rather than inferred, and one Backend's back-pressure is visibly not another's (ADR 0014, ADR 0016).
_Avoid_: Lag, congestion, throttling, overload

**Gateway Declaration**:
The org's single description of the shared Gateway — the address Agents reach it on, how it rebatches, where it Spills to, and which Backends it exports to. There is exactly one, because there is exactly one Gateway tier: unlike a Pipeline Profile it is neither per service nor per tier, and nothing selects it. Facts that genuinely vary by Service Tier (the tail-sampling budget) stay in the Profile.
_Avoid_: Gateway Profile, gateway config, gateway manifest

### Guardrails

**Guardrail**:
An umbrella concept: an automated check that enforces the org's observability standards. Always specialize to one of the two children below in real usage.
_Avoid_: Rule, policy, check (when used bare)

**Preflight Guardrail**:
A static Guardrail that runs before deploy (in CI/CD), reading a service's OTEL configuration and instrumentation setup, and blocking the pipeline on violation.
_Avoid_: Static check, linter, gate

**Pipeline Guardrail**:
A runtime Guardrail that runs inside the Gateway, inspecting real telemetry and acting (drop, tag, or alert) on violation. It is compiled from the Standard Catalog into collector processors, because a collector cannot evaluate Rego. Today it acts by Guardrail Tag alone: Severity decides how loudly a violation is marked, never whether the telemetry survives (ADR 0015).
_Avoid_: Processor rule, runtime check

**Standard**:
A single org-defined requirement that Guardrails enforce (e.g. required resource attribute, mandatory signal per service tier, cardinality limit, semantic-convention conformance). Authored once in a single catalog; each Standard declares its enforcement point(s) — `preflight`, `pipeline`, or both — since not every requirement is checkable at both.
_Avoid_: Rule, policy, convention

**Standard Catalog**:
The org's single document of every Standard — what each requires, at what Severity, and at which Enforcement Point(s). It is the source of truth for *both* Guardrails: a Preflight Guardrail's Rego reads it as data, and the Control Plane compiles the pipeline-enforced entries into the Gateway. A Standard's Rego says how to detect a violation in a declared Contract; it never says what the Standard requires or how severely, because a collector cannot run Rego and the second enforcement point would then be a second definition.
_Avoid_: Policy library, ruleset, standards list

**Enforcement Point**:
Where a Standard is enforced — `preflight` or `pipeline`. It is a property of the Standard, declared in the Standard Catalog, and both Guardrails honour it: a Standard not enforced at `preflight` must not fail a build, and one not enforced at `pipeline` is not compiled into the Gateway. An absent or unrecognised Enforcement Point is a hard error, the same treatment an absent Severity gets.
_Avoid_: Stage, phase, where-it-runs, scope

**Requirement Kind**:
What sort of thing a Standard demands, which is what decides whether it can be enforced at the pipeline at all. A requirement about one record (a resource attribute being present) can be compiled into collector processors; one about a stream over time (whether a service emits a Signal at all, or how many distinct values an attribute takes) cannot, because a Gateway inspects one record at a time. A Standard declaring an Enforcement Point its Requirement Kind cannot deliver is refused, never compiled into something weaker.
_Avoid_: Check type, rule type, predicate

**Guardrail Tag**:
The resource attributes a Pipeline Guardrail writes onto a violating record: one per violated Standard, named for it and valued at that Standard's Severity, plus a single `blocking` roll-up when any violated Standard is `block`. It is how a violation is reported — there is no separate event stream, in keeping with the platform observing itself through its own telemetry (ADR 0010). Compliant telemetry carries none, and the Gateway clears the whole namespace before writing, so a Tag is always the Gateway's verdict and never something a service said about itself.
_Avoid_: Annotation, label, marker, violation event

**Telemetry Contract**:
A per-service declaration of what telemetry a service intends to emit — its tier, owner, signals, and key attributes — authored by the service team and checked by Guardrails. It is a declared intent, not observed reality.
_Avoid_: Manifest, spec, schema, config

**Service Tier**:
A criticality classification of a service, declared in its Telemetry Contract, that determines which Standards apply and which signals are mandatory (a higher tier requires more).
_Avoid_: Level, class, rank, priority

**Signal**:
One of the three OTEL telemetry kinds a service emits — traces, metrics, or logs. A Standard may require specific Signals per Service Tier.
_Avoid_: Data type, stream

**Severity**:
The enforcement weight of a Standard when violated: `info`, `warn`, or `block`. Only `block` fails the build. Severity can differ by service age (new vs legacy) during phased rollout.
_Avoid_: Level, priority

**Waiver**:
A time-boxed, owner-approved exemption that lets a specific service skip a specific Standard until an expiry date, downgrading its Effective Enforcement from `block` to non-failing.
_Avoid_: Exception, override, suppression, ignore

**Effective Enforcement**:
What actually happens to a violated Standard on the day it is checked: the Severity the Standard declared, adjusted by anything holding it back. One of three — it **fails the build**, it is **held back**, or it is **advisory**. Distinct from Severity: a `block` Standard held back by a Waiver is still a `block` Standard.
_Avoid_: Effective severity, outcome, status, verdict

**Hold**:
One thing keeping a `block` Standard from failing a build, carrying the day it lapses — either a Waiver or the Enforcement Epoch's grace for a legacy service. Several can apply to one violation at once, and they lapse on different days. A Hold never applies to a Standard that was not going to fail the build anyway.
_Avoid_: Suppression, exemption, grace (bare), deferral

**Service Tier Taxonomy**:
The org's single definition of which Service Tiers exist and which Signals each mandates. It is a document the platform owns, read both by Guardrails and by the Control Plane when it selects a Pipeline Profile; a Standard consumes it and never defines it.
_Avoid_: Tier list, tier config, tier map

**Enforcement Epoch**:
A single published cutoff date that classifies a service as new or legacy: a service whose Telemetry Contract first appears on or after the Epoch is **new** (Standards `block` immediately); one appearing before is **legacy** (Standards `warn` until each Standard's own graduation deadline).
_Avoid_: Cutoff, go-live, launch date

## Relationships

- A **Guardrail** is either a **Preflight Guardrail** or a **Pipeline Guardrail** — never both at once in a given sentence.
- A **Guardrail** enforces one or more **Standards**.
- A **Standard** declares its **Enforcement Point**(s); the matching **Guardrail** (Preflight and/or Pipeline) runs it, and the other one does not.
- The **Standard Catalog** defines every **Standard**; both Guardrails read it rather than restating it, exactly as both a **Standard** and a **Pipeline Profile** read the **Service Tier Taxonomy**.
- A **Standard**'s **Requirement Kind** decides which **Enforcement Point**s it can declare; one it cannot deliver is refused rather than compiled.
- A **Pipeline Guardrail** reports a violation as a **Guardrail Tag** on the telemetry itself, so an operator reads it in a **Backend** rather than from a status channel the platform does not have.
- The **Copilot** must **Ground** every claim in telemetry evidence; the **Eval Harness** score gates promotion up the **Autonomy Ladder**.
- Advice rungs are gated on **Incident Corpus** accuracy; action rungs are additionally gated on zero harmful actions across the **Harm Set**.
- Each service has one **Telemetry Contract**.
- A **Preflight Guardrail** checks a **Telemetry Contract** and collector config against **Standards** (does the declaration comply?).
- A **Pipeline Guardrail** checks live telemetry against the **Telemetry Contract** and **Standards** (does reality match the declaration?).
- Each **Standard** carries a **Severity**; only `block` fails a build.
- A violated Standard's **Effective Enforcement** is its **Severity** adjusted by its **Holds**; a **Waiver** and the **Enforcement Epoch** are both **Holds**.
- The **Service Tier Taxonomy** defines the **Service Tiers**; both a **Standard** and a **Pipeline Profile** read it rather than restating it.
- The **Enforcement Epoch** decides whether a service is new (`block` now) or legacy (`warn` until deadline).
- A **Waiver** exempts one service from one **Standard** until it expires.
- The **Control Plane** **Compiles** a **Telemetry Contract** with its tier's **Pipeline Profile** into collector configuration.
- Each **Service Tier** selects a default **Pipeline Profile**; a platform-approved override may assign a different one.
- The **Control Plane** **Compiles** a whole **Fleet** into a **Rollout**, indexed by a **Rollout Manifest**; merging that commit *is* the rollout.
- A **Telemetry Contract** that does not **Compile** is recorded in the **Rollout Manifest** and keeps whatever collector configuration it last compiled to — it is skipped, never guessed at and never silently dropped.
- An **Agent** forwards to a **Gateway**; the **Gateway** hosts **Pipeline Guardrails**, and an **Agent** hosts none.
- A **Gateway** fans out to one or more **Backends**, defined in the **Gateway Declaration**; a service never targets a **Backend** directly.
- A **Backend** receives only the **Signals** it declares, so a metrics store is absent from the traces pipeline; a **Signal** no **Backend** receives does not **Compile**.
- **Backend Isolation** is what makes fanning out from one **Gateway** safe: a **Backend** that stops answering fills its own queue and **Spills** to its own directory, and the others keep exporting.
- **Spill** is the one thing on this platform that is not in the collector's core distribution, so the **Gateway** runs a contrib build while every **Agent** stays core.
- The **Control Plane** **Compiles** the **Gateway Declaration** into the **Gateway**'s configuration, cross-checked against every **Pipeline Profile**: the address the Gateway answers on must be where the Profiles forward **Agents**.
- Every **Agent** and the **Gateway** emit **Self-Telemetry**; a **Rollout** is confirmed when each reports the **Config Version** the **Rollout Manifest** recorded for it, and by nothing else — there is no status channel to ask.
- **Self-Telemetry** leaves by a path that is not the pipeline it reports on: an **Agent**'s goes to the **Gateway** on its own client, and the **Gateway**'s goes straight to one **Backend**, never through the **Gateway**'s own pipelines.
- An **Agent**'s **Self-Telemetry** carries the identity its **Telemetry Contract** declares, so a **Pipeline Guardrail** judges it exactly as it judges that service — no exemption, because an exemption would be a resource attribute and a service can write one.
- The **Gateway**'s own identity is declared in the **Gateway Declaration** and checked against every `block` **Standard** the **Gateway** enforces at the pipeline: it does not **Compile** if the **Gateway** would tag the fleet for something its own telemetry omits.
- **Back-Pressure** at the **Gateway** is attributed per **Backend**, which is what **Backend Isolation** looks like from outside: one **Backend**'s queue filling while the others' stay empty.

## Example dialogue

> **Dev:** "Does the cardinality **Standard** get enforced by a **Preflight Guardrail**?"
> **Domain expert:** "Partly. Preflight can flag a config that's *likely* to blow up cardinality, but only a **Pipeline Guardrail** sees the real attribute values at runtime, so both enforce the same **Standard** at different points."

## Flagged ambiguities

- "Guardrail" was used to mean both static (CI/CD) and runtime (collector) enforcement — resolved: **Guardrail** is an umbrella term with two distinct children, **Preflight Guardrail** and **Pipeline Guardrail**.
