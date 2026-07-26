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

**Amended (G5, #7)**: the Enforcement Epoch is built, closing the "new vs legacy needs a definition" gap below. One published date in `guardrail/enforcement.yaml` classifies every service; a legacy service is held back from a blocking Standard until that Standard's published graduation deadline, then blocks unattended. Three supporting decisions:

- **A service is dated by the first git commit that added its Telemetry Contract, never by a field it declares.** Git is the source of truth (ADR 0004), so the date is neither the service team's to write down nor theirs to backdate, and deleting and re-declaring a Contract does not reset the clock. A `first_declared:` field in the Contract was rejected: the entire value of the Epoch is that no team has to be trusted about its own age, and a self-declared date is a one-line way to buy warn-only treatment.
- **Undeterminable history stops the run (exit 2); it does not fall back to a default.** `actions/checkout` clones shallow by default, which leaves nothing to read. Guessing *new* would fail every legacy service the moment somebody shallow-clones — the political death this ADR exists to avoid; guessing *legacy* would hand every service a trivial escape from blocking — the permanent hole it also exists to avoid. Consumers set `fetch-depth: 0`; the action cannot do it for them, because the Contract lives in the *caller's* checkout.
- **A blocking Standard with no published graduation deadline is an error, not a default**, for the same symmetry: one default makes the rollout permanent for that Standard, the other springs it on every legacy service at once. The platform team publishes the day. This bites only when a legacy service actually violates that Standard, so authoring a new Standard does not stop the fleet.

The Epoch and a Waiver compose without either masking the other: both can hold one violation back, they lapse on different days, and the report names both — a service told about only one is told the wrong date.

**Amended (G7, #8)**: the expiry report is built, closing the "must surface soon-to-expire and expired Waivers" obligation below. `otel-guardrail waivers [--within N]` lists Waivers lapsing within a window and reports already-expired ones **separately** — an expiring Waiver needs attention before a build breaks, an expired one means a service is already blocked, and the two call for different actions. A daily scheduled workflow runs it against the register embedded in the binary and opens or **updates in place** a single labelled tracking issue, writing nothing at all when the report is unchanged: a cron that files a fresh issue every morning is spam, spam gets muted, and a muted alert is indistinguishable from no alert. Exit 1 means "somebody must act" — the same meaning it carries for `check` — which is the branch the workflow keys on; a register that will not parse stays on exit 2, so a broken register can never read as an all-clear.

**Amended (correctness pass)**: the illustrative Waivers were moved out of `guardrail/waivers.yaml` into `guardrail/examples/demo-waivers.yaml`. Demonstrating the honoured and lapsed paths needs Waivers on fixed dates, and pinning those in the live register made this file — which ADR 0004 designates the source of truth — untouchable: filing, extending or retiring a real Waiver broke tests. Tests now assert only *invariants* of the live register (every Waiver well-formed, no service+Standard covered twice), which hold whether it holds fifty Waivers or none. An empty register is a valid, expected state.

Nothing above changes the rule that only `block` fails a build.

## Consequences

- Adoption is gradual and political friction is bounded; compliance still trends up because warn has a deadline and waivers expire.
- The system must track waiver expiry and surface soon-to-expire and expired waivers, or the escape hatch becomes a permanent hole. This is now in place end to end: expiry is enforced automatically, every waived violation is printed on every run with its expiry date, and a daily scheduled job surfaces what is about to lapse and what already has (G7, #8) — all three reading the same `Waiver.DaysUntilExpiry` seam, so there is one definition of "expiring".
- Embedding the register in the binary means an approved Waiver takes effect in a service repo only when that repo's pinned `guardrail-ref` moves forward — the same latency a Standards change already has. Accepted: it is one distribution mechanism instead of two, and a service pinning an old ref gets an old Waiver *and* old Standards together, which is at least coherent.
- "New vs legacy service" is now defined: the first git commit that added the Telemetry Contract, against a published Enforcement Epoch. The cost is that the Preflight Guardrail needs real git history at check time, so every consumer's checkout must be unshallow — a requirement that is invisible until it bites, which is why it fails loudly with an actionable message rather than defaulting.
- Every Standard that can block now carries an obligation the catalog cannot satisfy alone: a graduation deadline published in the enforcement schedule. Authoring a Standard is therefore a two-file change, and forgetting the second file is caught the first time a legacy service violates it.

## Considered alternatives

- **Hard block everything immediately** — cleanest, politically fatal.
- **Warn-only forever** — zero friction, toothless; standards get ignored.
