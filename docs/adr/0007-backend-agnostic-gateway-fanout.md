# Services are Backend-agnostic; Gateway fans out to multiple Backends via Profile

## Context

The org already sends telemetry to Splunk and other APM tools. If services target a Backend directly (Splunk-specific exporters in each service, or a Backend field per Contract), telemetry fragments across vendors, cross-service correlation breaks, and switching or adding a Backend becomes a fleet-wide change.

## Decision

Services emit OTLP to their Agent and never name a Backend. The Gateway fans out to one or more Backends (e.g. Splunk + a metrics store + a cold archive), with exporters and routing declared in the tier's Pipeline Profile. Adding, swapping, or splitting Backends is a Profile change, not a fleet change.

## Consequences

- Vendor lock-in stays low and centralized; a Splunk migration or hot/cold routing split is a Profile edit plus a GitOps rollout.
- The Gateway becomes a critical fan-out point — its reliability and export back-pressure handling matter for every Backend.
- Per-service Backend needs (a team wanting its own tool) have no first-class path by design; such a case forces an explicit decision (new tier/Profile) rather than silent fragmentation.

## Considered alternatives

- **Single primary Backend** — simplest ops/billing, rejected for single-vendor lock and no hot/cold routing.
- **Per-service Backend choice in the Contract** — flexible for teams, rejected for fragmenting telemetry and breaking correlation.
