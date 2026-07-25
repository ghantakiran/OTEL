# Platform observes itself via its own telemetry; no separate control-plane health channel

## Context

Skipping OpAMP (ADR 0006) left two open edges: without a status back-channel, how do we confirm config rolled out fleet-wide (0006), and how do we watch Gateway health and back-pressure when it is the critical multi-Backend fan-out point (0007)? Adding a heartbeat/status service would re-introduce the mini control-plane ADR 0006 deliberately avoided.

## Decision

The platform is its own first telemetry citizen. Agents and the Gateway emit their own OTEL — queue depth, export failures, dropped spans, and the applied `config_version`. Dashboards and Pipeline Guardrails watch these signals. Config rollout is confirmed when the expected `config_version` appears fleet-wide in telemetry, not via a control protocol. Gateway back-pressure is handled per Backend (sending queue + retry + spill/dead-letter) so one slow or down Backend cannot block exports to the others.

## Consequences

- No separate health API or status back-channel exists — deliberately; do not build one, watch the platform's own telemetry instead.
- There is a bootstrap dependency: if the Gateway's own export path is fully broken, its self-telemetry may not escape. Mitigate with a minimal independent export path for the platform's own signals (e.g. a direct Backend route not gated on the same failing exporter).
- Config-rollout confirmation is eventually-consistent (as fast as the metric pipeline), not transactional — acceptable given GitOps rollout is already asynchronous.

## Considered alternatives

- **Lightweight status endpoint/heartbeat** — direct confirmation, rejected as a mini back-channel that re-opens ADR 0006.
- **Trust GitOps sync status only** — confirms the file was delivered, not that the collector loaded it or is exporting; misses runtime failures.
