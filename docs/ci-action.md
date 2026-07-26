# Preflight Guardrail in your PR checks

A composite GitHub Action that runs the **Preflight Guardrail** over your service's **Telemetry Contract** and tells you, before merge, whether it meets org **Standards**.

The action is a thin wrapper around `otel-guardrail check`. All the policy lives in the Standards (Rego) inside the Guardrail; the action decides nothing.

## Wiring it into a service repo

Commit your Telemetry Contract (see [telemetry-contract.md](./telemetry-contract.md)) as `telemetry-contract.yaml` at the root of your service repo, then add:

```yaml
# .github/workflows/preflight.yml
name: Preflight Guardrail

on:
  pull_request:

jobs:
  preflight:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: ghantakiran/OTEL/.github/actions/preflight@v0.1.0
        with:
          contract: telemetry-contract.yaml
          guardrail-ref: v0.1.0
```

Make that job a required check on your default branch and a violated Standard can no longer reach `main`.

## What each outcome means

| Guardrail exit code | Step | What you should do |
| --- | --- | --- |
| `0` | passes | Nothing. Your Contract violates no blocking Standard. Non-blocking (`info` / `warn`) findings, if any, are printed in the step summary. |
| `1` | **fails** | Your Telemetry Contract violates a Standard. Fix the Contract, or file a **Waiver** ([waivers.md](./waivers.md)) if the Standard genuinely cannot be met yet. The violations appear as an annotation on the Contract file and in the job summary. |
| `2` (or anything other than `0` / `1`) | **fails** | The Guardrail itself could not run — bad ref, unreachable repo, missing Contract file, broken policy load, binary that never got built. **This is not a finding about your Contract**; your Contract has not been judged. Escalate to the platform team rather than editing your Contract. |

The action branches purely on the exit code and echoes the Guardrail's output verbatim. When the **Severity** model lands, `info` and `warn` violations will print but exit `0`; only `block` exits `1`. The action needs no change for that.

## Inputs

| Input | Default | Meaning |
| --- | --- | --- |
| `contract` | `telemetry-contract.yaml` | Path to your Telemetry Contract, relative to the workspace. |
| `guardrail-repository` | `ghantakiran/OTEL` | Repository holding the Guardrail source. |
| `guardrail-ref` | `master` | Git ref of that repository to build the Guardrail from. **Pin this.** See below. |
| `guardrail-token` | `${{ github.token }}` | Token used to check the Guardrail out. Only sufficient if that repository is public; otherwise pass a token with read access. |
| `guardrail-source` | *(empty)* | Path to an already-checked-out Guardrail source tree. Skips the checkout. Used by the Guardrail's own repo to test itself; service repos leave this empty. |
| `fail-on-violation` | `true` | Set to `'false'` for a report-only rollout: violations are still annotated and summarised, but the step passes. A Guardrail that could not run (exit `2`) fails the step regardless. |

## Outputs

| Output | Meaning |
| --- | --- |
| `exit-code` | `0`, `1` or `2` — the Guardrail's exit code. |
| `outcome` | `compliant`, `violation` or `error`. |

Outputs are most useful with `fail-on-violation: 'false'`, or with `continue-on-error: true` on the step, when you want to react to the result rather than just fail.

## Pinning

Two things need pinning, and they are separate on purpose:

- **The action** — the `@ref` on the `uses:` line pins the CI plumbing.
- **The Guardrail** — `guardrail-ref` pins the binary, and therefore the Standards you are checked against.

Pin both to the same tag (or commit SHA) unless you have a reason not to. Leaving `guardrail-ref` at its `master` default means an org-wide Standards change can turn a green PR red without anything in your repo changing — which is sometimes exactly what the platform team wants, and sometimes a very unwelcome surprise.

## How the action gets the binary, and why

The action **builds `otel-guardrail` from a pinned source checkout** of `guardrail-repository` at `guardrail-ref`: `actions/checkout` into `.otel-guardrail-src/` in your workspace, `actions/setup-go` from that tree's `go.mod`, then `go build`.

That is the only option that works today — there is no release pipeline in this repo, so there are no release assets and no published container image to pull. Building from source needs zero new machinery.

The trade-off is real: every PR check pays a Go toolchain setup and a compile (tens of seconds, mostly absorbed by `setup-go`'s module and build cache), the service repo needs read access to the Guardrail repo, and `.otel-guardrail-src/` appears in the workspace — add it to `.gitignore` if a later job runs `git status`. In exchange there is nothing to publish, sign, or host.

The seam is deliberately narrow. Checkout + setup-go + build sit between two marked comments in `action.yml`; everything after them consumes `$RUNNER_TEMP/otel-guardrail` and nothing else. When a release pipeline exists, those three steps collapse into one `curl` of a pinned release asset (or a container image), `guardrail-ref` becomes a version tag, and the exit-code handling below the seam is untouched.

## Testing the action itself

`.github/workflows/preflight-action.yml` in this repository runs the action against the sample Contracts in `guardrail/examples/`, asserting all three outcomes: the compliant Contract passes, the violating Contract both reports `exit-code=1` and fails the step, and a missing Contract file fails the step as a tooling failure even with `fail-on-violation: 'false'`.
