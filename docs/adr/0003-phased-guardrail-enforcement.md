# Phased Guardrail enforcement with severity tiers and waivers

## Context

Turning on blocking Preflight Guardrails across an existing polyglot fleet would fail CI for every non-compliant service on day one. That breaks teams' pipelines simultaneously and gets the whole guardrail program disabled — the common death of org-wide observability standards.

## Decision

Each Standard carries a Severity (`info` / `warn` / `block`); only `block` fails a build. New services get `block` immediately; existing services start at `warn` with a published deadline to graduate to `block`. A Waiver — time-boxed and owner-approved — lets a specific service defer a specific Standard past its deadline.

## Consequences

- Adoption is gradual and political friction is bounded; compliance still trends up because warn has a deadline and waivers expire.
- The system must track waiver expiry and surface soon-to-expire and expired waivers, or the escape hatch becomes a permanent hole.
- "New vs legacy service" needs a definition (e.g. first-seen date) so the engine knows which Severity to apply.

## Considered alternatives

- **Hard block everything immediately** — cleanest, politically fatal.
- **Warn-only forever** — zero friction, toothless; standards get ignored.
