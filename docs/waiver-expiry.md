# The Waiver expiry cron

A **Waiver** is the escape hatch that makes phased Guardrail enforcement politically survivable, and it is the thing most likely to become a permanent hole. ADR 0003 is explicit about the cost of not watching it:

> The system must track waiver expiry and surface soon-to-expire and expired waivers, or the escape hatch becomes a permanent hole.

This is that watch. There is no backend and no database to run it from (ADR 0004), so CI's own scheduler is the entire runtime: a daily GitHub Actions job scans the register and keeps **one** tracking issue in step with what it finds.

## What runs, and when

`.github/workflows/waiver-expiry.yml` runs at **06:17 UTC every day**, and on demand via **Run workflow** (`workflow_dispatch`), which takes a `within` input so you can ask "what does the next quarter look like?" without editing anything.

It does three things:

1. Builds `otel-guardrail` and runs `otel-guardrail waivers --within 30 --format json` over the register embedded in the binary — `guardrail/waivers.yaml`.
2. Classifies the exit code. `0` and `1` are both answers; `2` is a tooling failure, which fails the run and files nothing.
3. Hands the JSON to `.github/scripts/waiver-expiry-issue.sh`, which opens, updates or closes the tracking issue.

## The report

The command is usable on its own — it is the same thing the cron runs.

```
$ otel-guardrail waivers --within 30
1 Waiver to review as of 2026-07-25: 1 already expired, 0 expiring within 30 days

Already expired — the Standard is blocking these services again:
  legacy-payments-batch: Standard S1 expired 2026-01-15, 191 days ago, approved by obs-team
```

| Flag | Meaning |
| --- | --- |
| `--within N` | How many days ahead to look. Default `30`. |
| `--as-of YYYY-MM-DD` | The day to judge expiry on. Default today. Same seam as `otel-guardrail check --as-of`. |
| `--register PATH` | Register to scan. Defaults to the org register built into the binary. Point it at a file to review a change to the register itself. |
| `--format text\|json` | `text` for a person, `json` for a program. |

### Expired and expiring are different problems

The report separates them, and so does the issue, because the actions differ:

- **Expiring within N days** is a deadline. The Waiver still holds; someone has until the date to fix the Telemetry Contract or file a fresh Waiver.
- **Already expired** is a fire. Enforcement reverted on its own the morning after the expiry, so that service's build is failing on that Standard *right now*. Nobody revoked anything — the date simply passed.

Within each group the most urgent comes first: soonest to lapse, longest since lapsed.

### Exit codes

| Code | Meaning |
| --- | --- |
| `0` | No Waiver has expired and none expires within the window. |
| `1` | At least one Waiver needs attention. |
| `2` | The report could not run — a malformed register, a bad flag, an unknown `--format`. |

`1` is not a tooling failure, and it is deliberately not `2`. It carries the same meaning it does for `otel-guardrail check`: *a finding, and somebody must act*. That is exactly the branch the workflow needs — exit `1` files the issue, exit `0` closes it. Exit `2` stays reserved for "the Guardrail could not run", so a register that fails to parse can never be mistaken for an all-clear. A `--within -1` and a `--format yaml` are both rejected rather than silently matching nothing, for the same reason: a report of zero expiring Waivers is indistinguishable from a report that never ran.

There is no separate code for "expired" versus "expiring". The caller's action is identical — file or update the issue — and the distinction belongs in what the report *says*, not in how the shell branches.

## The issue it files

One issue, labelled **`waiver-expiry`**, titled e.g. `Waiver expiry: 1 expired, 0 expiring within 30 days`. The body is a short framing plus one table per group:

| Service | Standard | Expiry | Approved by | Reason it was filed |
| --- | --- | --- | --- | --- |
| `legacy-payments-batch` | S1 | 2026-01-15 (191 days ago) | @obs-team | Batch runner read its environment from a decommissioned config service… |

Everything needed to act is in the row, so nobody has to open the register to triage. The approvers recorded on the Waivers are **mentioned**, not assigned: `approved_by` is a free-text approver in the register (`obs-team`), not guaranteed to be a GitHub login, and assigning a non-user either fails the job or gets swallowed silently. If the register ever grows a GitHub handle per approver, switching the mention to an assignment is a one-line change in `build_body`.

## How it avoids becoming spam

A cron that opens a fresh issue every morning gets muted, and a muted alert is the same as no alert. So the job keeps exactly one issue alive:

| Today's report | Open tracking issue? | What happens |
| --- | --- | --- |
| Waivers need attention | none | Opens one, labelled `waiver-expiry`. |
| Waivers need attention | yes, body differs | Edits it **in place**. No new issue, no daily comment. |
| Waivers need attention | yes, body identical | **Nothing at all.** No edit, no notification. |
| Nothing needs attention | yes | Comments why, then closes it. |
| Nothing needs attention | none | Nothing. |

Two keys make that work:

- **The `waiver-expiry` label is how the job finds its issue.** `gh issue list --label waiver-expiry --state open` is the lookup. Remove that label from the issue and tomorrow's run will open a duplicate.
- **A hidden marker, `<!-- otel-guardrail:waiver-expiry -->`, at the top of the body proves the issue is the job's own** before anything overwrites it. If a human labels an unrelated issue `waiver-expiry`, the job refuses to touch it and fails loudly rather than replacing their text.

The report is rendered deterministically from the JSON — sorted, with stable tie-breaks — so an unchanged register produces a byte-identical body, which is what makes the "do nothing" row above possible. If two open issues carry the label, the job stops and says so rather than guessing which to update and leaving the other to rot.

Once closed, an issue is never reopened; a later problem opens a fresh one. That keeps a resolved thread resolved instead of resurrecting an ancient one months later.

## Changing the notice period

The default window, **30 days**, is one constant in two places, and both should move together:

- `defaultWithinDays` in `guardrail/cli/waivers.go` — what a bare `otel-guardrail waivers` reports.
- `WITHIN` in the `Scan the Waiver register` step of `.github/workflows/waiver-expiry.yml`, and the `workflow_dispatch` input default beside it — what the cron reports.

Thirty days is chosen to be long enough to fix a Telemetry Contract or get a fresh Waiver reviewed and merged, and short enough that the issue is not permanently full of things nobody can act on yet. Widening it past the longest expiry in the register makes the issue permanent, which is the same as having no alert.

## Running it by hand

```sh
go run ./guardrail/cmd/otel-guardrail waivers                      # today, 30-day window
go run ./guardrail/cmd/otel-guardrail waivers --within 90          # a quarter ahead
go run ./guardrail/cmd/otel-guardrail waivers --as-of 2027-03-15   # what March's report will say
go run ./guardrail/cmd/otel-guardrail waivers --register ./guardrail/waivers.yaml   # review a proposed register
go run ./guardrail/cmd/otel-guardrail waivers --format json | jq '.expiring[].service_name'
```

To exercise the issue logic without touching the repository:

```sh
.github/scripts/waiver-expiry-issue_test.sh
```

That builds the real binary, captures real reports, stubs `gh`, and asserts every row of the table above — open, update, no-op, close, do-nothing, plus the two refusal paths. It creates no issues and needs no authentication.

## Caveat

GitHub Actions is disabled on this account, so **this workflow has never run**. The command, the branching script and the workflow's exit-code classification are all verified locally; what remains unproven is the parts only a real runner exercises — that `schedule:` fires, that `${{ github.token }}` with `issues: write` is sufficient for `gh label create` / `issue create` / `issue edit` / `issue close`, and that a body round-tripped through the GitHub API compares byte-identical on the next day's run (the script strips `\r` defensively for exactly that reason). Watch the first two runs: the first should open the issue, the second should report `already says exactly this` and write nothing.
