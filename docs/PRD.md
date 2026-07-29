# PRD — OTEL Platform

## Overview

An internal platform for managing OpenTelemetry across an engineering org, delivered in three layers built in strict order: **Guardrails** (standards enforcement) → **Control Plane** (collector/pipeline management) → **Copilot** (GenAI-assisted incident response). The dependency runs one way: guardrails define the standards the control plane enforces and the copilot reasons over. Terminology is defined in [CONTEXT.md](../CONTEXT.md); architecture decisions are in [docs/adr/](./adr/).

## Goals

- Make observability standards enforceable, not aspirational — required attributes, per-tier signals, semantic conventions checked automatically.
- Drive standardized telemetry to backends without per-service config drift.
- Give on-call an assistant that reasons over the resulting clean telemetry, earning autonomy by measured accuracy rather than by default.

## Non-goals (v1)

- No backend service / database in v1 — git is the source of truth (ADR 0004, 0006).
- No OpAMP control protocol yet (ADR 0006).
- No autonomous Copilot actions on day one — advice only, staged behind an Autonomy Ladder (ADR 0008).

## Layer 1 — Guardrails

A **Guardrail** enforces **Standards** at two points: **Preflight** (static, CI/CD) and **Pipeline** (runtime, in the Gateway). Preflight validates a declared **Telemetry Contract** + collector config, not instrumentation source (ADR 0001). Standards are authored as Rego, evaluated with OPA/conftest (ADR 0002). Enforcement is phased via **Severity** + **Waivers** + an **Enforcement Epoch** (ADR 0003). Everything lives in git, PR-reviewed (ADR 0004).

**User stories**
- As a service owner, I declare a Telemetry Contract and CI tells me before merge whether it meets org Standards.
- As the observability team, I author a Standard once (Rego) and choose where it enforces (`preflight` / `pipeline`).
- As a service owner on a legacy service, I get `warn` (not `block`) until a published deadline, and can file a time-boxed Waiver.
- As the platform team, I get alerted before a Waiver expires (scheduled CI in the policy repo).

**v1 slice**: S1 (required resource attributes present) + S2 (tier → mandatory signals), 3-tier taxonomy.

## Layer 2 — Control Plane

The Control Plane **Compiles** a Telemetry Contract with its tier's **Pipeline Profile** into collector config and distributes it (ADR 0005). Topology is **Agent** + **Gateway**, with Pipeline Guardrails running centrally in the Gateway. Distribution is GitOps, not OpAMP (ADR 0006). Services are **Backend**-agnostic; the Gateway fans out to multiple Backends per Profile (ADR 0007). The platform observes itself via its own telemetry — no separate health channel (ADR 0010).

**User stories**
- As a service owner, I emit OTLP to a local Agent and never name a Backend; my pipeline shape comes from my tier's Profile.
- As the platform team, I change exporters/sampling for a whole tier by editing one Profile, not touching the fleet.
- As the platform team, I confirm a config rollout by seeing the expected `config_version` metric appear fleet-wide.
- As a service with an unusual need, I get a new named Profile or a platform-approved override — no silent fork (ADR 0005).

## Layer 3 — Copilot

A self-hosted Claude-driven assistant (Claude API + Tool Runner, `claude-opus-5`) that reads standardized telemetry through vendor-neutral typed tools and assists incident response (ADR 0011). Telemetry always enters as data, never as instructions. Output is **Grounded** in evidence; an **Eval Harness** over an **Incident Corpus** scores accuracy and gates promotion up the **Autonomy Ladder** (Advisor → Gated → Bounded), with a **Harm Set** zero-harm gate for action rungs (ADR 0008, 0009, 0012). Cost: `claude-haiku-4-5` triage → `claude-opus-5` deep RCA.

**User stories**
- As on-call, I get an incident summary with ranked root-cause hypotheses, each citing the trace/metric/log it rests on.
- As the platform team, I promote the Copilot a rung only when it clears a published accuracy bar and (for action rungs) takes zero harmful actions on the Harm Set.
- As on-call, cheap triage filters known-noise alerts before expensive RCA runs.

## Milestones

1. **M1 — Guardrails spine**: tracer bullet (Contract → Rego → conftest → CI block), then S2, severity, waivers, epoch, CI action, waiver-expiry cron.
2. **M2 — Control Plane**: Contract×Profile compile, Agent+Gateway topology, GitOps distribution, multi-backend fan-out, self-observation, Pipeline Guardrails.
3. **M3 — Copilot**: typed tool surface + Tool Runner loop, grounding, triage tiering, Eval Harness + Incident Corpus, Autonomy Ladder Advisor rung, Harm Set + promotion gate.

### M3 progress

**First slice delivered** (#16, #18): the typed tool surface and the Tool Runner
loop run on `claude-opus-5` with a single vendor-neutral tool, `query_traces`,
fronting a real Backend. Telemetry-path evidence rides on the same tool result, so
a summary can tell a failing service from a failing telemetry path. Every claim in
a summary is checked for **provenance** (was the cited trace fetched?) and
**support** (does it bear the claim out?); claims that fail either, or that cite
nothing, reach the operator marked rather than deleted.

**Three partials, stated plainly**, because the mechanisms exist and the evidence
that they work does not:

| Partial | What is missing | Tracked by |
|---|---|---|
| Grounding is unmeasured | Support is judged by a mechanism nobody has scored against labelled incidents. A rule that has never been scored is a rule, not a property. | #53 → **#20** |
| Injection resistance is structural, not behavioural | Hostile telemetry provably cannot reach the system prompt or a platform-authored user turn, and twelve adversarial fixtures now drive that through the real loop. Nothing measures whether the model's *answer* changes when hostile text arrives as legitimate evidence. | #54 → **#20** |
| "Fleet-wide" means one host | The service-vs-telemetry-path distinction works against a real Backend, on one host and one Backend. | #51 |

Two of the three are closed by the same thing: the **Eval Harness over an Incident
Corpus** (#20), which is what turns grounding and injection resistance from
mechanisms into scored properties — and it is the promotion gate for the Autonomy
Ladder besides (ADR 0012). It is the next thing worth building. The remaining M3
items — triage tiering (#19), the Advisor rung (#21), the Harm Set — are untouched.

**Not yet built**: `query_metrics`, `query_logs`, `get_contract`, `get_standards`
(P2, #17). The grounding mechanism does not need to change when they arrive — a
Judge is handed evidence rather than going to find it.

## Open questions

- Concrete promotion numbers (X% accuracy, N incidents) per rung — set with SRE org (ADR 0012).
- Telemetry Contract JSON schema finalization; Rego starter policy library.
- Gateway self-telemetry bootstrap path (ADR 0010).
