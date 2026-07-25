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

**Agent**:
A lightweight collector running next to a service (sidecar or node daemon) that collects telemetry and forwards it to the Gateway. Agents do no enforcement.
_Avoid_: Collector (bare), sidecar, node collector

**Gateway**:
The central collector tier that receives from Agents, batches, tail-samples, and exports to backends. The single place where Pipeline Guardrails run.
_Avoid_: Central collector, aggregator, proxy

**Backend**:
A destination system where telemetry lands (e.g. Splunk, a metrics store, a cold archive). The Gateway fans out to one or more Backends per the tier's Pipeline Profile; services never target a Backend directly.
_Avoid_: APM, sink, store, destination

### Guardrails

**Guardrail**:
An umbrella concept: an automated check that enforces the org's observability standards. Always specialize to one of the two children below in real usage.
_Avoid_: Rule, policy, check (when used bare)

**Preflight Guardrail**:
A static Guardrail that runs before deploy (in CI/CD), reading a service's OTEL configuration and instrumentation setup, and blocking the pipeline on violation.
_Avoid_: Static check, linter, gate

**Pipeline Guardrail**:
A runtime Guardrail that runs inside the Gateway, inspecting real telemetry and acting (drop, tag, or alert) on violation.
_Avoid_: Processor rule, runtime check

**Standard**:
A single org-defined requirement that Guardrails enforce (e.g. required resource attribute, mandatory signal per service tier, cardinality limit, semantic-convention conformance). Authored once in a single catalog; each Standard declares its enforcement point(s) — `preflight`, `pipeline`, or both — since not every requirement is checkable at both.
_Avoid_: Rule, policy, convention

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
A time-boxed, owner-approved exemption that lets a specific service skip a specific Standard until an expiry date, downgrading its effective enforcement from `block` to non-failing.
_Avoid_: Exception, override, suppression, ignore

**Enforcement Epoch**:
A single published cutoff date that classifies a service as new or legacy: a service whose Telemetry Contract first appears on or after the Epoch is **new** (Standards `block` immediately); one appearing before is **legacy** (Standards `warn` until each Standard's own graduation deadline).
_Avoid_: Cutoff, go-live, launch date

## Relationships

- A **Guardrail** is either a **Preflight Guardrail** or a **Pipeline Guardrail** — never both at once in a given sentence.
- A **Guardrail** enforces one or more **Standards**.
- A **Standard** declares its enforcement point(s); the matching **Guardrail** (Preflight and/or Pipeline) runs it.
- The **Copilot** must **Ground** every claim in telemetry evidence; the **Eval Harness** score gates promotion up the **Autonomy Ladder**.
- Advice rungs are gated on **Incident Corpus** accuracy; action rungs are additionally gated on zero harmful actions across the **Harm Set**.
- Each service has one **Telemetry Contract**.
- A **Preflight Guardrail** checks a **Telemetry Contract** and collector config against **Standards** (does the declaration comply?).
- A **Pipeline Guardrail** checks live telemetry against the **Telemetry Contract** and **Standards** (does reality match the declaration?).
- Each **Standard** carries a **Severity**; only `block` fails a build.
- The **Enforcement Epoch** decides whether a service is new (`block` now) or legacy (`warn` until deadline).
- A **Waiver** exempts one service from one **Standard** until it expires.
- The **Control Plane** **Compiles** a **Telemetry Contract** with its tier's **Pipeline Profile** into collector configuration.
- Each **Service Tier** selects a default **Pipeline Profile**; a platform-approved override may assign a different one.
- An **Agent** forwards to a **Gateway**; the **Gateway** hosts **Pipeline Guardrails**.
- A **Gateway** fans out to one or more **Backends**, defined in the **Pipeline Profile**; a service never targets a **Backend** directly.

## Example dialogue

> **Dev:** "Does the cardinality **Standard** get enforced by a **Preflight Guardrail**?"
> **Domain expert:** "Partly. Preflight can flag a config that's *likely* to blow up cardinality, but only a **Pipeline Guardrail** sees the real attribute values at runtime, so both enforce the same **Standard** at different points."

## Flagged ambiguities

- "Guardrail" was used to mean both static (CI/CD) and runtime (collector) enforcement — resolved: **Guardrail** is an umbrella term with two distinct children, **Preflight Guardrail** and **Pipeline Guardrail**.
