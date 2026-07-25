# GitOps distribution of compiled config, not OpAMP (for now)

## Context

The Control Plane must deliver Compiled collector config to Agents and the Gateway. OpAMP is the OTEL-native answer — a server pushes config to collectors live and receives health/status back — and will be suggested repeatedly. But OpAMP is a stateful backend to build, host, and secure, reversing the server-less line set in ADR 0004.

## Decision

The Control Plane compiles config and commits it to a config repo; existing GitOps/k8s tooling (Argo/Flux, ConfigMaps) rolls it out to Agents and the Gateway. No OpAMP server in this phase. Reconfiguration is a commit plus a normal rollout.

## Consequences

- Stays server-less and consistent with ADR 0004; config changes get a full git audit trail and reuse existing deploy infra.
- No live push and no status/health back-channel from collectors — rollout latency is whatever the GitOps loop is, and fleet health must come from the telemetry itself, not a control protocol.
- Revisit OpAMP when live reconfiguration or a collector status back-channel becomes a real need; at that point a backend is justified.

## Considered alternatives

- **OpAMP** — OTEL-native, live reconfig + status back-channel, rejected now for the stateful-backend cost.
- **Bake config into deploy artifacts** — immutable and simple, but every config change needs a rebuild+redeploy; too slow to remediate a bad pipeline in prod.
