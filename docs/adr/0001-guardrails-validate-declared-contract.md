# Guardrails validate a declared Telemetry Contract, not instrumentation source

## Context

Preflight Guardrails must check services at config-time across a polyglot fleet (Java, Go, Python, Node, .NET). Statically analyzing OTEL SDK init in each language would require a parser and ruleset per language, breaking on every SDK version bump.

## Decision

Each service declares a **Telemetry Contract** (tier, owner, signals, key attributes). Preflight Guardrails validate the Contract plus the collector config against Standards — language-agnostic, no source parsing. Pipeline Guardrails later verify that live telemetry matches the declared Contract.

## Consequences

- The Contract can drift from what code actually emits. This is accepted at Preflight and caught at runtime by the Pipeline Guardrail comparing reality to the Contract.
- Service teams take on authoring/maintaining a Contract file — an explicit, reviewable artifact rather than implicit code behavior.

## Considered alternatives

- **Per-language static analysis of SDK init** — most accurate at config-time, rejected for N-parser cost and SDK-version brittleness.
- **CI smoke-test capture** (run service, capture emitted telemetry) — real data, no drift, but requires a runnable service in CI and is slower/flakier; may be added later as an optional Preflight mode.
