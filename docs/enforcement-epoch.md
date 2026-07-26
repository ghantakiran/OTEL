# The Enforcement Epoch

Turning on blocking Standards across an existing fleet would fail CI for every non-compliant service on day one. That breaks every team's pipeline at once and gets the guardrail programme switched off — the usual death of an org-wide observability effort (ADR 0003).

The **Enforcement Epoch** is the one published date that avoids it. It classifies every service, once:

- A service whose Telemetry Contract first appeared **on or after** the Epoch is **new**. Every Standard enforces at its declared Severity immediately — a `block` Standard blocks.
- A service whose Contract first appeared **before** the Epoch is **legacy**. A `block` Standard is still reported, but held back until that Standard's **graduation deadline**, after which it blocks by itself.

The Epoch is inclusive: a service arriving on the published day is new. There is no day on which a service is neither.

## Where it is published

`guardrail/enforcement.yaml`, alongside the Standard catalog and the Waiver register, embedded in the binary and changed by pull request:

```yaml
apiVersion: guardrail.otel/v1
kind: EnforcementSchedule

epoch: 2026-01-01

graduations:
  - standard: S1
    graduates: 2027-01-01
  - standard: S2
    graduates: 2027-04-01
```

`--enforcement <path>` points at a different schedule — that is how a change to this file gets reviewed against real Contracts before it merges.

## How a service is dated

**By the first git commit that added its Telemetry Contract**, not by anything the service declares:

```
git log --diff-filter=A --format=%aI --reverse -- telemetry-contract.yaml
```

Git is the source of truth (ADR 0004), so the date is not the service team's to write down and not theirs to backdate. Deleting the Contract and re-declaring it does not reset the clock — the first commit that ever added it is the one that counts.

The alternative, a `first_declared:` field in the Contract, was rejected: it hands every service a one-line way to buy itself warn-only treatment, and the whole point of the Epoch is that nobody has to be trusted about their own age.

### This requires full git history

`actions/checkout` clones with `fetch-depth: 1` by default, which leaves no history to read. When the first-appearance day cannot be determined, `otel-guardrail check` **stops with exit 2** — "the Guardrail could not run" — and says how to fix it.

A shallow clone is detected explicitly, with `git rev-parse --is-shallow-repository`, rather than inferred from a lookup that came back empty. It has to be: a shallow clone grafts its tip commit as parentless, so `git log --diff-filter=A` reports **every** file in it as added in that tip commit. The lookup therefore *succeeds* and returns today's date. Left to infer, the Guardrail would classify every legacy service as new and block the whole fleet — silently, and in exactly the scenario this check exists for.

It does not guess, because both guesses are bad in opposite directions. Guessing *new* fails every legacy service the moment somebody shallow-clones. Guessing *legacy* hands every service a trivial escape from blocking. Neither is a decision a tool should make quietly on the platform team's behalf.

Set `fetch-depth: 0` on your checkout — see [ci-action.md](./ci-action.md).

## Graduation deadlines

Every Standard that can block needs a graduation deadline in the schedule. A blocking Standard with none is an **error**, not a default — the run stops with exit 2 naming the Standard.

The two available defaults are both worse than stopping. Defaulting to "keep deferring" makes the phased rollout permanent for that Standard, which is exactly the hole ADR 0003 exists to close. Defaulting to "block now" springs the Standard on every legacy service the moment it is authored, which is the political failure the same ADR exists to avoid. The platform team publishes the day.

Note this only bites when a legacy service actually violates the Standard — authoring a new Standard does not stop the world.

## What a legacy service sees

Held-back findings are reported, never hidden, with the day they start failing:

```
legacy-inventory: nothing fails the build — 1 blocking Standard violation held back by the Enforcement Epoch
  [block, legacy service, blocks from 2027-01-01] S1: required resource attribute "deployment.environment" is not declared
```

A finding that quietly stops mattering is how a phased rollout becomes a permanent exemption.

## Together with a Waiver

Both can hold the same violation back, and they lapse on different days, so both are named:

```
  [block, waived by obs-team until 2026-12-01; legacy service, blocks from 2027-01-01] S1: ...
```

A service told about only one of them is told the wrong date. The two are independent: a [Waiver](./waivers.md) is an approved, argued exemption for one service; the Epoch is an automatic grace every legacy service gets for free.

## Relationship to Severity

The Epoch never changes a Standard's declared [Severity](./standards.md) — `warn` stays `warn`. It changes only whether a `block` Standard fails *this* service's build *today*. `Result.FailsTheBuild()` remains the single question CI asks.
