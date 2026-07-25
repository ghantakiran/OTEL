# Git-native source of truth, no backend service in v1

## Context

Standards, Telemetry Contracts, and Waivers need a home with governance and an audit trail. The obvious enterprise instinct is a registry service with a database, API, and UI — but that is a whole service to build, host, and secure before a single Guardrail runs.

## Decision

Git is the source of truth. Standards (Rego) and Waivers live in a central policy repo; each Telemetry Contract lives in its own service repo. Governance is PR review plus CODEOWNERS. Waiver expiry is a date field checked in CI. There is no backend service or database in v1.

## Consequences

- Fastest path to value; audit trail and change history come free from git.
- Cross-fleet queries and live waiver-expiry alerting are not available until a later layer indexes git into a read model (deferred, see the hybrid option).
- No "server" exists — this is deliberate; do not build one to answer questions git history already answers.

## Considered alternatives

- **Central registry service (backend + DB + API/UI)** — better querying and alerting, rejected for v1 as too heavy before any Guardrail ships.
- **Hybrid (git source indexed to a read model)** — the intended evolution once dashboards/alerts are needed; deferred past v1.
