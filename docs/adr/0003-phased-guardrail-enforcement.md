# Phased Guardrail enforcement with severity tiers and waivers

## Context

Turning on blocking Preflight Guardrails across an existing polyglot fleet would fail CI for every non-compliant service on day one. That breaks teams' pipelines simultaneously and gets the whole guardrail program disabled — the common death of org-wide observability standards.

## Decision

Each Standard carries a Severity (`info` / `warn` / `block`); only `block` fails a build. New services get `block` immediately; existing services start at `warn` with a published deadline to graduate to `block`. A Waiver — time-boxed and owner-approved — lets a specific service defer a specific Standard past its deadline.

**Amended (G3, #3)**: a Standard declares its Severity on each violation it emits, not once for the Standard, so one Standard can block on a hard condition and warn on a softer one. Two supporting decisions:

- **An absent or unrecognised Severity is an error, not a default.** The Preflight Guardrail validates Severity where policy data becomes domain data (decoding the Rego result) and fails the whole run, exit 2 — "the Guardrail could not run" — naming the Standard. Any default would let a Standard that forgets to declare its Severity silently stop blocking; and a broken catalog is the platform team's fault, so it must not surface as a violation charged to the service team.
- **`Check` returns a `Result`, not a `[]Violation`.** The Result owns the question "does this fail the build?" (`FailsTheBuild`, `Blocking`, `NonBlocking`). Returning a bare slice would make every caller — CLI today, the Control Plane and CI reporters later — re-implement "any violation whose Severity is block", and each one would be free to get phased enforcement wrong.

Not yet built: new-vs-legacy classification by Enforcement Epoch, and Waivers. Both change what Severity applies to a given service; neither changes the rule that only `block` fails a build, which is what this slice implements.

## Consequences

- Adoption is gradual and political friction is bounded; compliance still trends up because warn has a deadline and waivers expire.
- The system must track waiver expiry and surface soon-to-expire and expired waivers, or the escape hatch becomes a permanent hole.
- "New vs legacy service" needs a definition (e.g. first-seen date) so the engine knows which Severity to apply.

## Considered alternatives

- **Hard block everything immediately** — cleanest, politically fatal.
- **Warn-only forever** — zero friction, toothless; standards get ignored.
