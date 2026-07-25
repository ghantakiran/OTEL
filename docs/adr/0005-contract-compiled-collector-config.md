# Control Plane compiles collector config from Contract × tier Profile

## Context

The Control Plane (layer 2) must get collector configuration onto the fleet. If collector config is authored separately from the Telemetry Contract, the two artifacts drift — the exact problem Pipeline Guardrails exist to catch. But the lean v1 Contract deliberately omits pipeline details (exporters, sampling, batching), so the Contract alone cannot produce a full config.

## Decision

The Contract remains the single source of *what* a service emits. The Control Plane keeps a small set of **Pipeline Profiles** keyed by Service Tier that define *how* telemetry ships. It **Compiles** `Contract × Profile[tier] → collector config` and distributes the result. The Contract stays lean; pipeline shape is owned centrally per tier, not per service.

## Consequences

- Declared equals deployed by construction — the same Contract drives Preflight Guardrails and runtime config, shrinking drift.
- Pipeline decisions (exporters, sampling) are centralized in Profiles; teams cannot hand-tune per service without an escape hatch, which must be designed if needed.
- A service needing a genuinely bespoke pipeline is an unresolved edge — likely a new tier or an explicit Profile override, to be decided when it arises.

## Considered alternatives

- **Distribute hand-written config (OpAMP-style)** — simpler, but Contract and config stay separate and drift.
- **Extend the Contract with pipeline fields** — pushes pipeline choices to every team, causing config sprawl and drift.
- **Per-service override files** — reintroduces the two-artifact drift the compile approach removes.
