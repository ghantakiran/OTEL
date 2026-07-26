# Service Tiers — the 3-tier taxonomy

A **Service Tier** is a criticality classification declared in a service's [Telemetry Contract](./telemetry-contract.md). It decides which Standards apply and which **Signals** are mandatory. There are exactly three tiers. A Contract declaring anything else is a violation, not a silent pass.

Standard **S2** enforces this taxonomy at Preflight (`guardrail/policies/s2_tier_mandatory_signals.rego`).

## The tiers

| Tier | Criticality | Mandatory Signals |
| --- | --- | --- |
| `tier-1` | Customer-facing and revenue-critical. An outage is an incident with external impact; the org pages on it. | traces, metrics, logs |
| `tier-2` | Important internal or supporting service. Degradation is felt by other teams or degrades a tier-1 path, but is not itself customer-visible. | traces, metrics |
| `tier-3` | Everything else — batch jobs, internal tooling, low-traffic services. Failure is absorbed or retried. | traces |

## Why this grading

The taxonomy is deliberately **nested**: `tier-3 ⊂ tier-2 ⊂ tier-1`. A higher tier mandates a superset of the tier below, so raising a service's tier can only ever add obligations, never swap them. That keeps the promotion path mechanical — a service moving from tier-2 to tier-1 adds logs and changes nothing it already declared.

The order in which Signals become mandatory follows what each one buys during an incident:

- **traces** are mandatory at every tier. Without them a service is a black box in a distributed call path, and it degrades the observability of every service that calls it — even a tier-3 batch job sits on someone's critical path. This is the floor.
- **metrics** are added at tier-2. Metrics are what alerting and SLOs are built on; a service whose degradation affects other teams must be continuously measurable, not only inspectable after the fact.
- **logs** are added at tier-1. Logs are the most expensive Signal per unit of value at scale, so they are demanded only where the org needs the full detail of a customer-visible failure. Requiring them everywhere would make the Standard something teams route around rather than meet.

Signals not mandated by a tier are **optional, not forbidden** — a tier-3 service is free to emit metrics and logs. S2 sets a floor, never a ceiling.

## Unknown tiers

S2 blocks a Contract whose `tier` is not one of the three. An unrecognised tier — a typo, or a tier from some other system — has no mandatory Signal set, so a rule keyed on the tier would simply not fire and the service would ship unchecked. Failing closed is the only safe reading: a service the platform cannot classify is a service whose Signals nobody has agreed on.

An empty or absent `tier` is treated the same way, for the same reason.

## Severity

Every S2 violation carries severity `block`. A service emitting less than its criticality demands cannot be operated during an incident, and phasing that in per-service is the job of the Enforcement Epoch and Waivers (ADR 0003), not of the Standard's own severity.

## Where the taxonomy lives

**One file: `guardrail/tiers.yaml`.** It is the source of truth for both consumers.

- Go reads it — `guardrail.CentralTaxonomy()`, then `Tiers()` and `MandatorySignals(tier)`.
- The **S2** policy reads it as the Rego data document `data.otel.taxonomy`. It does **not** declare tiers, and a test fails if any `.rego` file names one.

That split is why it moved out of policy. A tier literal inside S2 was correct while policy was the only consumer, but the Control Plane keys **Pipeline Profiles** off these same identifiers (ADR 0005) — so the taxonomy would have existed twice, in two languages, drifting independently. And drifting silently: a Contract declaring a tier the Guardrail knows but the Control Plane does not would pass Preflight and then compile to the wrong pipeline.

Input is the Contract under test; data is what the org has decided. The taxonomy is the second kind.

## Adding or changing a tier

Edit `guardrail/tiers.yaml` and this document together. Nothing else needs to change — no policy edit, no code generation step — and existing Contracts should be expected to fail until they catch up. That friction is intentional: the tier list is org-wide vocabulary (see [CONTEXT.md](../CONTEXT.md)).

`--taxonomy` is not a CLI flag: unlike the Waiver register and the enforcement schedule, the taxonomy is not optional, and a Guardrail without one would treat every declared tier as unknown and block the whole fleet. Tests supply one directly via `guardrail.WithTaxonomy`.
