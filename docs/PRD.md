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

A self-hosted Claude-driven assistant (Claude API + Tool Runner, `claude-opus-4-8`) that reads standardized telemetry through vendor-neutral typed tools and assists incident response (ADR 0011). Telemetry always enters as data, never as instructions. Output is **Grounded** in evidence; an **Eval Harness** over an **Incident Corpus** scores accuracy and gates promotion up the **Autonomy Ladder** (Advisor → Gated → Bounded), with a **Harm Set** zero-harm gate for action rungs (ADR 0008, 0009, 0012). Cost: haiku-4-5 triage → opus-4-8 deep RCA.

**User stories**
- As on-call, I get an incident summary with ranked root-cause hypotheses, each citing the trace/metric/log it rests on.
- As the platform team, I promote the Copilot a rung only when it clears a published accuracy bar and (for action rungs) takes zero harmful actions on the Harm Set.
- As on-call, cheap triage filters known-noise alerts before expensive RCA runs.

## Milestones

1. **M1 — Guardrails spine**: tracer bullet (Contract → Rego → conftest → CI block), then S2, severity, waivers, epoch, CI action, waiver-expiry cron.
2. **M2 — Control Plane**: Contract×Profile compile, Agent+Gateway topology, GitOps distribution, multi-backend fan-out, self-observation, Pipeline Guardrails.
3. **M3 — Copilot**: typed tool surface + Tool Runner loop, grounding, triage tiering, Eval Harness + Incident Corpus, Autonomy Ladder Advisor rung, Harm Set + promotion gate.

## Open questions

- Concrete promotion numbers (X% accuracy, N incidents) per rung — set with SRE org (ADR 0012).
- Telemetry Contract JSON schema finalization; Rego starter policy library.
- Gateway self-telemetry bootstrap path (ADR 0010).
