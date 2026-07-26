# Authoring a Standard

A **Standard** is a single org-defined requirement that Guardrails enforce. Standards are authored as Rego policies (ADR 0002) and live in `guardrail/policies/`, embedded in the `otel-guardrail` binary.

## The shape of a Standard

One Standard per package under `otel.guardrail.standards.<id>`. The aggregator in `guardrail/policies/guardrail.rego` collects every such package automatically — there is no registration step, adding the file is enough.

```rego
# S9 — one sentence saying what the Standard requires, and why.
package otel.guardrail.standards.s9

violation contains v if {
	not input.resource_attributes["service.namespace"]
	v := {
		"standard": "S9",
		"severity": "warn",
		"message": "recommended resource attribute \"service.namespace\" is not declared",
	}
}
```

Rego v1 syntax (OPA 1.x): rules take `if`, partial sets take `contains`.

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

| Severity | Effect | Choose it when |
| --- | --- | --- |
| `block` | Reported; `otel-guardrail check` exits 1 and CI stops. | The requirement is non-negotiable and the fix is cheap and unambiguous. |
| `warn` | Reported; exit code unaffected. | The requirement is genuinely valuable but a service is still operable without it, or the Standard is rolling out and teams need time. |
| `info` | Reported; exit code unaffected. | Advisory: worth surfacing where the author happens to be looking, not worth chasing. |

A Standard declares its Severity on each violation it emits, so a single Standard may block on one condition and warn on a softer one.

**A missing or unrecognised Severity is a hard error.** The Preflight Guardrail rejects the whole run — `otel-guardrail check` exits 2, "the Guardrail could not run", and names the Standard at fault. The alternative (defaulting to something) means a Standard that forgets to declare its Severity quietly stops blocking, and nobody notices until an incident. A broken catalog is the platform team's problem, and exit 2 says so rather than charging it to the service team as a violation.

## The catalog today

| Standard | Severity | Requires |
| --- | --- | --- |
| S1 | `block` | The required resource attributes: `service.name`, `service.version`, `deployment.environment`. |
| S3 | `warn` | The recommended resource attributes: `service.namespace`, `service.instance.id`. Not needed to ship; they tell same-named services and individual replicas apart during triage. |

## Trying a Standard out

```
otel-guardrail check guardrail/examples/compliant-contract.yaml                        # exit 0, no violations
otel-guardrail check guardrail/examples/missing-recommended-attributes-contract.yaml   # exit 0, warn violations reported
otel-guardrail check guardrail/examples/missing-attributes-contract.yaml               # exit 1, a blocking Standard was violated
```

Exit codes: `0` no blocking Standard was violated, `1` a blocking Standard was violated, `2` the Guardrail could not run.
