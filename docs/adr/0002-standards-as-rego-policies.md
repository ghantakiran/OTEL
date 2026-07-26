# Standards are authored as Rego policies, evaluated via OPA/Conftest

## Context

Preflight Guardrails must evaluate Standards against a Telemetry Contract and collector config. Standards include conditional logic (e.g. "tier-1 services require a `deployment.environment` attribute and a metrics signal"), not just structural shape checks. The observability team — not only platform engineers — should be able to author Standards.

## Decision

Standards are authored as Rego policies. Preflight Guardrails are a thin CLI that feeds the Contract YAML and collector YAML to OPA as input documents. No custom policy language is built.

**Amended (G1, #2)**: the CLI embeds the OPA Go SDK and evaluates Rego in-process rather than shelling out to the `conftest` binary. Rationale: `otel-guardrail` ships as one static binary with the Standard catalog embedded, so CI needs no second tool installed and no version skew between the two; policy evaluation is directly testable from Go tests. Standards remain plain Rego and stay evaluable with `opa test` / `conftest` outside the CLI.

## Consequences

- Standards are versioned, testable policy files; the OPA ecosystem (test harness, editor tooling) comes for free.
- Rego is a learning curve for authors; mitigated by a starter policy library and examples.
- The CLI owns packaging, result formatting, and CI integration — not policy evaluation itself.
- Embedding OPA couples the CLI's release cadence to the OPA library version; a Rego-language change requires rebuilding the CLI, not just updating a pinned tool.

## Considered alternatives

- **JSON Schema / CUE** — clean for required-field shape, too weak for conditional "tier → required signal" logic.
- **Custom rule functions in code** — flexible but non-engineers can't author Standards, and the check library becomes ours to maintain.
- **Custom YAML DSL** — reinvents OPA; we'd own a policy language.
