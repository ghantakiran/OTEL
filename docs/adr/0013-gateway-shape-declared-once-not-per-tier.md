# The Gateway's shape is declared once, not in a per-tier Pipeline Profile

## Context

ADR 0007 decided services are Backend-agnostic and the Gateway fans out, "with exporters and routing declared in the tier's Pipeline Profile". Building the Gateway (C3, #10) forced that sentence to be made concrete, and it does not survive contact: a Pipeline Profile is per Service Tier and there are three of them, while the org runs **one** Gateway. Three Profiles each naming a Gateway shape would describe three Gateways. Merging them into one would leave the Gateway's shape with no single owner and make it depend on file order — the silent drift ADR 0005 exists to remove.

The pressure in the other direction is real too: some Gateway behaviour genuinely *is* per tier. Tail sampling is the standing example — how much of a tier's traces to keep is a tier-by-tier cost decision, and `sampling.traces_percent` already lives in each Profile.

## Decision

Split by what the fact is *about*, not by which component consumes it.

- The **Gateway Declaration** (`controlplane/gateway.yaml`, `kind: GatewayDeclaration`) declares the one shared Gateway: the address Agents reach it on, its memory ceiling, how it rebatches, and which Backend(s) it exports to. There is exactly one, and nothing selects it.
- A **Pipeline Profile** keeps facts that are per Service Tier, including ones the Gateway will consume — the tail-sampling budget stays there.

`CompileGateway` reads both and cross-checks them: every Profile's `gateway_endpoint` must be the Declaration's `address`. A tier whose Agents forward where the Gateway does not answer does not compile.

This refines ADR 0007 rather than reversing it. Services remain Backend-agnostic, and Backends are still declared centrally in one file that a fleet-wide change never touches — that file is now the Gateway Declaration rather than each tier's Profile.

## Consequences

- Adding, swapping or splitting a Backend is one edit to one file, as 0007 intended — and it is unambiguously one file rather than three that must agree.
- The two halves of the topology live in separate documents and cannot silently disagree, because compiling refuses when they do. That failure is otherwise invisible from every service: an Agent exporting into nothing reports success.
- Two documents now describe "how telemetry ships", and which one a new setting belongs in is a judgement call every time. The test is whether the fact varies per Service Tier. Getting it wrong is recoverable — moving a field between two org-owned files is not a fleet change.
- Gateway-side per-tier behaviour needs the Gateway to distinguish tiers at run time, from the resource attributes an Agent stamped. Tail sampling (C5, #13) is the first thing to need this and will decide how.
- `delivery` sits per Backend rather than per Gateway, so back-pressure handling can be per Backend when fan-out lands (ADR 0010) without moving the field.

## Considered alternatives

- **Gateway shape in each tier's Pipeline Profile**, as 0007's wording implies — rejected: three descriptions of one deployment, with no owner for the merge.
- **A `gateway:` block inside `controlplane/profiles.yaml`** — the endpoint agreement would be visible on one screen, but a document whose `kind` is `PipelineProfileSet` would then hold a singular shared-infrastructure fact, and the two have different editors, different change cadence and very different blast radius. The cross-check gives the same protection without the muddle.
- **Deriving the Gateway entirely from the Profiles** (union of every tier's needs) — no new document, but nothing in a Profile says where telemetry finally lands, so the Backend would have had to go somewhere anyway.
