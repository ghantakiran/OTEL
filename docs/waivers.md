# Filing a Waiver

A **Waiver** is a time-boxed, owner-approved exemption that lets **one service** skip **one Standard** until an expiry date. Until that date the Standard is still reported for that service, it just stops failing the build. On the day after the expiry, enforcement reverts on its own — nobody revokes anything, nobody runs anything, the date simply passes (ADR 0003).

A Waiver is the escape hatch that makes phased enforcement politically survivable. It is also the thing most likely to become a permanent hole, so everything below is designed to make a stale Waiver visible rather than silent.

## Where Waivers live

In this repository, in one file: **`guardrail/waivers.yaml`**, next to the Standard catalog.

Not in service repositories, deliberately:

- **A service must not be able to waive itself.** Filing a Waiver is a pull request against this repository, reviewed and approved by the platform team.
- **Expiry has to be scannable from one place.** The register is the single list to walk when asking "what lapses in the next two weeks?" — nobody has to crawl every service repo to find out.

The register is compiled into the `otel-guardrail` binary the same way the Standard catalog is. Git is the source of truth and there is no service to call at run time (ADR 0004), so a merged Waiver reaches a service repo the same way a Standards change does: by the pin in that repo's `guardrail-ref` moving forward.

## The file format

```yaml
apiVersion: guardrail.otel/v1
kind: WaiverRegister
waivers:
  - service_name: legacy-inventory
    standard: S1
    reason: >-
      deployment.environment is stamped by the platform sidecar, not by the
      service; the sidecar rollout for supply-chain lands 2027-03.
    approved_by: obs-team
    expires: 2027-04-01
```

| Field | Required | Meaning |
| --- | --- | --- |
| `service_name` | yes | The one service this Waiver covers. Must match the `service_name` in that service's Telemetry Contract exactly. |
| `standard` | yes | The one Standard it waives, e.g. `S1`. |
| `reason` | yes | Why the Standard cannot be met yet, and what changes that. Written for the reviewer who reads this in six months. |
| `approved_by` | yes | Who approved it. |
| `expires` | yes | `YYYY-MM-DD`. The Waiver holds for the whole of that day and is gone the next morning. |

**All five fields are required, and an incomplete Waiver is rejected loudly.** `otel-guardrail check` exits `2` — "the Guardrail could not run" — naming the Waiver and the missing field. That is deliberate: an unexplained, unapproved or unbounded Waiver is exactly the permanent hole ADR 0003 warns about, and a broken register is the platform team's bug, never a violation charged to a service team.

A Waiver is scoped to exactly one service and one Standard. It does not reach another service, and it does not reach another Standard of the same service — file a second Waiver, with its own reason and its own expiry.

## What a Waiver changes, and what it does not

It changes **effective enforcement** and nothing else:

```
legacy-inventory: nothing fails the build, but 1 blocking Standard violation(s) are only held back by a Waiver; 0 other non-blocking finding(s) to address
  [block, waived by obs-team until 2027-04-01] S1: required resource attribute "deployment.environment" is not declared
```

The violation is still found, still printed, still attributed to `block` Severity — with the approver and the expiry date on the same line. **A waived Standard never disappears from the output.** A Waiver whose violation vanishes is a Waiver nobody notices, and one nobody notices is one nobody ever retires.

Once the expiry passes, the same Contract on the same register produces:

```
legacy-inventory: 1 blocking Standard violation(s), 0 non-blocking
  [block] S1: required resource attribute "deployment.environment" is not declared
```

Exit `1`. Nothing was edited to make that happen.

## Filing one

1. Fix the Contract instead, if you can. A Waiver is for a Standard you genuinely cannot meet yet — a platform dependency that has not shipped, a migration in flight — not for one that is inconvenient.
2. Open a PR adding an entry to `guardrail/waivers.yaml`. Pick an expiry that matches the real unblocking date, not a round year out.
3. The platform team reviews the reason and the date, and approves by merging.
4. Before the expiry, either the Contract is fixed or a fresh Waiver is filed with a fresh reason. Silence is not a renewal.

## Seeing ahead

`--as-of` judges Waiver expiry on a day you choose, so you can ask what a build will do before it does it:

```
otel-guardrail check --as-of 2026-08-01 guardrail/examples/waived-contract.yaml   # exit 0, S1 reported and waived
otel-guardrail check --as-of 2028-01-01 guardrail/examples/waived-contract.yaml   # exit 1, the Waiver has lapsed
otel-guardrail check guardrail/examples/expired-waiver-contract.yaml              # exit 1, this one lapsed already
```

`--waivers <path>` points the run at a register other than the one built into the binary — useful when reviewing a change to the register itself.

You do not have to remember to look. `otel-guardrail waivers` reports every Waiver that has already expired or expires within the next 30 days, and a daily scheduled job runs it and keeps a single tracking issue in step with the answer, mentioning each Waiver's approver:

```
otel-guardrail waivers                     # today, 30-day window
otel-guardrail waivers --within 90         # a quarter ahead
otel-guardrail waivers --as-of 2027-03-15  # what March's report will say
```

See [docs/waiver-expiry.md](./waiver-expiry.md).

## Worked examples

`guardrail/waivers.yaml` ships two Waivers, one on each path:

| Service | Standard | Expires | Example Contract | Result |
| --- | --- | --- | --- | --- |
| `legacy-inventory` | S1 | 2027-04-01 | `guardrail/examples/waived-contract.yaml` | Honoured — violation reported, exit `0`. |
| `legacy-payments-batch` | S1 | 2026-01-15 | `guardrail/examples/expired-waiver-contract.yaml` | Lapsed — S1 blocks again, exit `1`. |
