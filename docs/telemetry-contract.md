# Telemetry Contract — schema v1

A **Telemetry Contract** is a per-service declaration of what telemetry a service intends to emit. It is declared intent, not observed reality: Preflight Guardrails check the declaration before deploy; Pipeline Guardrails later check reality against it (ADR 0001).

One file per service, committed to the service repo, PR-reviewed (ADR 0004).

```yaml
apiVersion: guardrail.otel/v1
kind: TelemetryContract
service_name: checkout-api
owner: team-payments
tier: tier-1
signals:
  - traces
  - metrics
  - logs
resource_attributes:
  service.name: checkout-api
  service.version: "2.4.1"
  deployment.environment: production
  service.namespace: payments
  service.instance.id: checkout-api-7d9f4b
```

| Field | Type | Meaning |
| --- | --- | --- |
| `apiVersion` | string | Schema version. `guardrail.otel/v1`. |
| `kind` | string | Always `TelemetryContract`. |
| `service_name` | string | The service this Contract governs. |
| `owner` | string | The team accountable for the service. |
| `tier` | string | **Service Tier** — criticality, decides which Standards apply and which Signals are mandatory. |
| `signals` | list of string | The **Signals** the service emits: `traces`, `metrics`, `logs`. |
| `resource_attributes` | map of string to string | The resource attributes the service declares it sets. |

The field set is deliberately lean. It grows only when a Standard needs a field to be checkable.

## How Standards see it

The Contract is handed to Rego as the `input` document, addressed by these exact field names:

```rego
input.tier
input.signals
input.resource_attributes["deployment.environment"]
```

## Checking a Contract

```
otel-guardrail check path/to/telemetry-contract.yaml
```

Every violated Standard is reported with its **Severity**, blocking ones first:

```
legacy-reporting: 1 blocking Standard violation(s), 2 non-blocking
  [block] S1: required resource attribute "deployment.environment" is not declared
  [warn] S3: recommended resource attribute "service.instance.id" is not declared
  [warn] S3: recommended resource attribute "service.namespace" is not declared
```

Only a `block` Severity fails the build (ADR 0003); `warn` and `info` violations are reported and the command still exits 0, so a Standard can roll out before it bites.

Exit codes: `0` no blocking Standard was violated, `1` a blocking Standard was violated, `2` the Guardrail could not run.

To author or read the Standards themselves, see [standards.md](./standards.md).
