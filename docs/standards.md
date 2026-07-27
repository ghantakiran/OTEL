# Authoring a Standard

A **Standard** is a single org-defined requirement that Guardrails enforce. A Standard is authored **once**, in two files that say different things:

| File | Says |
| --- | --- |
| `guardrail/standards.yaml` — the **Standard catalog** | *what* it requires, at what **Severity**, and *where* it is enforced |
| `guardrail/policies/<id>_*.rego` | *how* to detect it in a declared Telemetry Contract (ADR 0002) |

Both are embedded in the `otel-guardrail` binary. The catalog is the source of truth for both enforcement points: the Rego reads it as a data document, and the Control Plane compiles the same entries into **Pipeline Guardrails** in the Gateway. Nothing is written twice.

## The catalog entry

```yaml
- standard: S9
  title: One line saying what the Standard requires.
  severity: warn
  enforced_at: [preflight]
  requires:
    resource_attributes: [service.namespace]
```

| Field | Meaning |
| --- | --- |
| `standard` | The Standard's id. Not just a label: it is what a violation is reported under, and the segment the Gateway tags telemetry with (`otel.guardrail.violation.S9`). Letters, digits, hyphens and underscores. |
| `title` | One line, for humans reading the catalog. |
| `severity` | The **Severity**: `info`, `warn` or `block`. See below. |
| `enforced_at` | `preflight`, `pipeline`, or both. See below. |
| `requires` | The requirement, by kind. Exactly one kind per Standard. |

Two requirement kinds exist today: `resource_attributes` (a list of keys that must be present) and `tier_mandatory_signals` (the Signals the service's Service Tier makes mandatory, read from the Service Tier Taxonomy).

## The shape of a policy

One Standard per package under `otel.guardrail.standards.<id>`. The aggregator in `guardrail/policies/guardrail.rego` collects every such package automatically — there is no registration step, adding the file is enough.

```rego
# S9 — one sentence saying what the Standard requires, and why.
#
# What it requires and how severely come from `data.otel.standards.S9` — the
# catalog. Naming either here would create a second definition that drifts.
package otel.guardrail.standards.s9

standard := data.otel.standards.S9

violation contains v if {
	some attribute in standard.requires.resource_attributes
	not input.resource_attributes[attribute]
	v := {
		"standard": "S9",
		"severity": standard.severity,
		"message": sprintf("recommended resource attribute %q is not declared", [attribute]),
	}
}
```

Rego v1 syntax (OPA 1.x): rules take `if`, partial sets take `contains`.

**A policy never names a required attribute or a Severity.** It reads both from `data.otel.standards.<id>`, which the Guardrail builds from the catalog — the same entries the Gateway compiles into Pipeline Guardrails. Writing either here would give one Standard two definitions in two languages, failing independently; tests assert no `.rego` file does. It is the same rule that already applies to Service Tiers, which come from `data.otel.taxonomy` (#28).

Every policy must have a catalog entry that enforces it at `preflight`, and every `preflight` Standard must have a policy. A policy with no entry reads an absent data document and quietly enforces nothing; an entry with no policy is a Standard the catalog advertises and nothing runs. Both are caught by tests, and the second is caught again at run time.

The link is by **exact id** — `S1`'s policy reads `data.otel.standards.S1`, and Rego document keys carry case while package names do not — and by **requirement kind**: a policy that reads the Service Tier Taxonomy must belong to a `tier_mandatory_signals` entry, and one that reads `.requires.resource_attributes` to a `resource_attributes` entry. The kind is what decides pipeline eligibility, so a mislabelled one would let a Standard compile a Gateway check its Rego does not perform. Tests assert both directions.

## The violation object

Every Standard emits violations in one fixed shape. All three keys are mandatory:

| Key | Meaning |
| --- | --- |
| `standard` | The Standard's id, e.g. `"S1"`. Identifies who is complaining. |
| `severity` | The **Severity**: `"info"`, `"warn"` or `"block"`. |
| `message` | What is wrong, in the service team's terms. Name the offending thing. |

Emit one violation per offending thing, not one per Standard — a Contract missing two required attributes should produce two violations.

## Declaring a Severity

**Severity** is the enforcement weight of the Standard when violated. Only `block` fails a build (ADR 0003).

| Severity | Effect at Preflight | Effect at the pipeline | Choose it when |
| --- | --- | --- | --- |
| `block` | Reported; `otel-guardrail check` exits 1 and CI stops. | The record is tagged, and `otel.guardrail.blocking` is raised. | The requirement is non-negotiable and the fix is cheap and unambiguous. |
| `warn` | Reported; exit code unaffected. | The record is tagged. | The requirement is genuinely valuable but a service is still operable without it, or the Standard is rolling out and teams need time. |
| `info` | Reported; exit code unaffected. | The record is tagged. | Advisory: worth surfacing where the author happens to be looking, not worth chasing. |

**No Severity drops telemetry at run time.** `block` fails a *build*; the runtime analogue of stopping a deploy is making the violation impossible to ignore, not deleting the evidence of it. See [ADR 0015](./adr/0015-pipeline-guardrails-compile-from-the-catalog-and-never-drop.md).

A Standard declares one Severity, in the catalog. A Standard that would want two — blocking on a hard condition and warning on a softer one — is really two requirements sharing an id, and should be two Standards: the Gateway tags by id and could not tell them apart.

**A missing or unrecognised Severity is a hard error.** The catalog is rejected — `otel-guardrail` exits 2, "the Guardrail could not run", and names the Standard at fault. The alternative (defaulting to something) means a Standard that forgets to declare its Severity quietly stops blocking, and nobody notices until an incident. A broken catalog is the platform team's problem, and exit 2 says so rather than charging it to the service team as a violation.

## Declaring where it is enforced

`enforced_at` says which Guardrail runs the Standard: `preflight`, `pipeline`, or both. Not every requirement is checkable at both.

| | Checks | Runs |
| --- | --- | --- |
| `preflight` | the *declared* Telemetry Contract, before deploy | `otel-guardrail check`, in CI |
| `pipeline` | *live* telemetry, at run time | collector processors in the Gateway |

**A missing or unrecognised enforcement point is a hard error**, exactly as an absent Severity is, and for the same reason.

**A Standard cannot claim `pipeline` for a requirement that cannot run there.** A collector cannot evaluate Rego, so a Pipeline Guardrail is collector processors compiled from the catalog entry — and only some requirement kinds compile. `resource_attributes` is a property of one record and works at both points; `tier_mandatory_signals` is a property of a stream over time and works only at preflight, because a collector sees one record and cannot tell "this service emits no metrics" from "no metric has arrived in the last second". Declaring `pipeline` on one of those does not load, and says so.

The binding works the other way too: a Standard the catalog does **not** enforce at `preflight` must not have a policy in `guardrail/policies/`, because the aggregator would pick it up and it would go on failing builds at a point the catalog says it is not enforced at.

See [pipeline-guardrails.md](./pipeline-guardrails.md) for what a Pipeline Guardrail does on violation and how an operator reads it.

## The catalog today

| Standard | Severity | Enforced at | Requires |
| --- | --- | --- | --- |
| S1 | `block` | preflight, pipeline | The required resource attributes: `service.name`, `service.version`, `deployment.environment`. |
| S2 | `block` | preflight | The Signals a service's **Service Tier** makes mandatory — `tier-1` all three, `tier-2` traces and metrics, `tier-3` traces. Also flags a tier outside the taxonomy. See [service-tiers.md](./service-tiers.md). Preflight only: whether a service emits a Signal at all is a statement about a stream, not about a record. |
| S3 | `warn` | preflight, pipeline | The recommended resource attributes: `service.namespace`, `service.instance.id`. Not needed to ship; they tell same-named services and individual replicas apart during triage. |

Every Standard that can `block` also needs a graduation deadline in `guardrail/enforcement.yaml`, or a legacy service violating it can be neither blocked nor deferred — see [enforcement-epoch.md](./enforcement-epoch.md). That obligation is about failing a *build*, so it is a Preflight concern; a Waiver and the Enforcement Epoch do not apply at the pipeline, where there is no build to hold back.

## When a service cannot meet a Standard yet

A **Waiver** lets one service skip one Standard until an expiry date, downgrading its effective enforcement from `block` to non-failing. It does not silence the Standard: the violation is still reported, with the approver and the expiry date, and the Standard blocks again by itself once the date passes.

Waivers are not authored in Rego — a Waiver is not a Standard, it says nothing about what a service must emit. They live in one central register, `guardrail/waivers.yaml`, and are approved by the platform team. See [waivers.md](./waivers.md).

Services that predate the org's standards programme get a grace nobody has to file for: the **Enforcement Epoch** holds every blocking Standard back for them until that Standard's published graduation deadline. That is why authoring a blocking Standard is a two-file change — the Rego, plus a graduation deadline in `guardrail/enforcement.yaml`. See [enforcement-epoch.md](./enforcement-epoch.md).

## Trying a Standard out

```
otel-guardrail check guardrail/examples/compliant-contract.yaml                        # exit 0, no violations
otel-guardrail check guardrail/examples/missing-recommended-attributes-contract.yaml   # exit 0, warn violations reported
otel-guardrail check guardrail/examples/missing-attributes-contract.yaml               # exit 1, a blocking Standard was violated
otel-guardrail check --as-of 2026-08-01 guardrail/examples/waived-contract.yaml        # exit 0, S1 reported but held back by a Waiver
otel-guardrail check guardrail/examples/expired-waiver-contract.yaml                   # exit 1, the Waiver expired and S1 blocks again
```

Exit codes: `0` no blocking Standard was violated, `1` a blocking Standard was violated, `2` the Guardrail could not run.
