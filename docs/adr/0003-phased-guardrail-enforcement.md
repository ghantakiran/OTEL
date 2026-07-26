# Phased Guardrail enforcement with severity tiers and waivers

## Context

Turning on blocking Preflight Guardrails across an existing polyglot fleet would fail CI for every non-compliant service on day one. That breaks teams' pipelines simultaneously and gets the whole guardrail program disabled — the common death of org-wide observability standards.

## Decision

Each Standard carries a Severity (`info` / `warn` / `block`); only `block` fails a build. New services get `block` immediately; existing services start at `warn` with a published deadline to graduate to `block`. A Waiver — time-boxed and owner-approved — lets a specific service defer a specific Standard past its deadline.

**Amended (G3, #3)**: a Standard declares its Severity on each violation it emits, not once for the Standard, so one Standard can block on a hard condition and warn on a softer one. Two supporting decisions:

- **An absent or unrecognised Severity is an error, not a default.** The Preflight Guardrail validates Severity where policy data becomes domain data (decoding the Rego result) and fails the whole run, exit 2 — "the Guardrail could not run" — naming the Standard. Any default would let a Standard that forgets to declare its Severity silently stop blocking; and a broken catalog is the platform team's fault, so it must not surface as a violation charged to the service team.
- **`Check` returns a `Result`, not a `[]Violation`.** The Result owns the question "does this fail the build?" (`FailsTheBuild`, `Blocking`, `NonBlocking`). Returning a bare slice would make every caller — CLI today, the Control Plane and CI reporters later — re-implement "any violation whose Severity is block", and each one would be free to get phased enforcement wrong.

**Amended (G4, #6)**: Waivers are built. A Waiver downgrades one service's effective enforcement of one Standard from `block` to non-failing until an expiry date, after which enforcement reverts with nobody taking an action. Four supporting decisions:

- **Waivers live centrally, in this repository, at `guardrail/waivers.yaml`** — not in service repositories. A service must not be able to waive itself, so filing one is a PR the platform team approves; and the expiry report (which Waivers lapse within N days) needs one place to scan rather than every service repo. The register is embedded in the binary alongside the Standard catalog, since git is the source of truth and there is nothing to fetch from at run time (ADR 0004). `--waivers` overrides it for reviewing a change to the register itself.
- **A Waiver is Go, not Rego.** ADR 0002 makes *Standards* Rego, and a Waiver is not a Standard: it states nothing about what a service must emit. It is an administrative record about how hard a finding lands. Keeping it out of Rego keeps the catalog answering one question ("does this Contract meet this requirement?"), keeps date arithmetic against an injectable clock out of policy input, keeps a malformed register on the exit-2 "the Guardrail could not run" path rather than turning it into a policy result, and — decisively — means a Waiver can never delete a violation, only annotate one. A Rego implementation would have had to suppress or rewrite the violation, and a suppressed violation is an invisible Waiver.
- **A waived violation is still reported.** It keeps its declared `block` Severity in the output and gains the approver and expiry date on the same line; only `Result.FailsTheBuild` changes its answer. A Waiver whose violation vanishes from CI output is one nobody retires — the permanent hole this ADR exists to avoid.
- **"Now" is injected, never read from the wall clock inside the expiry logic.** The Guardrail takes a `Clock` (`WithClock`); `WaiverRegister.InForce` and `Waiver.DaysUntilExpiry` take the day to judge as a parameter. Tests fix the day, `otel-guardrail check --as-of` lets an operator ask what a build will do on a future day, and the expiry report reuses the same seam.

Still not built: new-vs-legacy classification by Enforcement Epoch, and the report of soon-to-expire Waivers. Neither changes the rule that only `block` fails a build.

## Consequences

- Adoption is gradual and political friction is bounded; compliance still trends up because warn has a deadline and waivers expire.
- The system must track waiver expiry and surface soon-to-expire and expired waivers, or the escape hatch becomes a permanent hole. Half of this is now in place: expiry is enforced automatically and every waived violation is printed on every run with its expiry date. The *soon-to-expire* report is still outstanding, and builds on `Waiver.DaysUntilExpiry`.
- Embedding the register in the binary means an approved Waiver takes effect in a service repo only when that repo's pinned `guardrail-ref` moves forward — the same latency a Standards change already has. Accepted: it is one distribution mechanism instead of two, and a service pinning an old ref gets an old Waiver *and* old Standards together, which is at least coherent.
- "New vs legacy service" needs a definition (e.g. first-seen date) so the engine knows which Severity to apply.

## Considered alternatives

- **Hard block everything immediately** — cleanest, politically fatal.
- **Warn-only forever** — zero friction, toothless; standards get ignored.
