# Per-Backend spill ends core-only; the Gateway runs the contrib collector distribution

## Context

ADR 0010 decided Gateway back-pressure is handled per Backend — "sending queue + retry + spill/dead-letter" — so that one slow or down Backend cannot block exports to the others. Building the fan-out (C5, #13) forced the third of those three to be made concrete, and it is the one with a cost.

Everything this platform has compiled so far has been in the OpenTelemetry Collector's **core** distribution, deliberately and without ever being written down as a decision. The Agent uses `otlp`, `memory_limiter`, `resource` and `batch`; the Gateway added nothing beyond them; even the harness's Backend stand-in uses `debug` rather than the tidier `file` exporter, on that ground. Core-only is worth something concrete: a smaller image with a smaller attack surface, a build every distribution vendor ships, and no dependency on components with per-component stability levels that move independently of the collector's.

A `sending_queue` is in core and gives isolation: each exporter owns its queue, so a Backend that stops answering fills its own and drops on its own, while the others keep exporting. What core cannot give is **durability**. An in-memory queue is lost when the Gateway process restarts — a rollout, an OOM kill, a node drain — and the Gateway is the one place in this topology holding the whole fleet's telemetry. A Backend that is down for twenty minutes across a Gateway rollout loses everything queued for it, and nothing about that is visible from any service: every Agent's export succeeded.

Making a queue persistent requires the exporter to name a `storage` extension, and the only such extension is `file_storage`, which lives in **opentelemetry-collector-contrib**, not core.

## Decision

Spill is required, so core-only ends here, at the Gateway.

- A Backend may declare `delivery.spill: true` in the Gateway Declaration. It compiles to a `file_storage/<backend>` extension and a `sending_queue` that names it. Each spilling Backend gets **its own storage instance on its own directory**, derived from its name under the Gateway's single `spill_root`.
- **The Gateway therefore runs `otel/opentelemetry-collector-contrib`, pinned.** The harness runs the same image at the same tag, so what is tested is what a deployment runs.
- **The Agent stays core-only.** An Agent runs beside a service, on whatever disk that service has, and its next hop is the Gateway — which is inside the cluster and, unlike a Backend, something this platform operates. `Delivery.Spill` exists on the shared type but no Pipeline Profile sets it, and nothing compiles it into an Agent config.

The Gateway needs a mounted volume at `spill_root`. That is a deployment fact this repository does not own, and it is now load-bearing: a Gateway whose `spill_root` is not a real volume has queues that read as durable and are not.

## Consequences

- **The collector distribution is now part of the platform's contract, not an implementation detail.** Deploying this Gateway on a core build fails at load — the config names a component that build does not have. That is the right failure (loud, immediate, at start-up rather than during an outage) but it is a real constraint on anyone running this, and on any org with a vendor-supplied collector distribution.
- A larger image with more components in it, and therefore more attack surface and more CVE traffic, for the Gateway only. Agents — of which there is one per service, so the overwhelming majority of collector processes on the platform — stay core.
- `file_storage` carries its own stability level, separate from the collector's, and can change independently of the core components around it. The pin is what keeps that from arriving unannounced; upgrading it is now a decision rather than a bump.
- Spill costs disk, per Backend, and the sizing is real: a Backend with a 20,000-batch queue that is down for an hour writes an hour of the fleet's telemetry to that volume. Giving each Backend its own directory means one Backend cannot consume another's; it also means the volume must be sized for all of them.
- Having taken contrib for the Gateway, the argument against other contrib components there is weaker than it was — `tail_sampling` is the immediate example. That is a consequence to be aware of, not a licence: each one is still its own decision, and the Agent's core-only line is unchanged.
- C7's self-observation (#15) gains something concrete to read. Spill depth per Backend is a real signal about a real Backend, and because the storage instances are separate, "which Backend is behind" is answerable rather than inferred.

## Considered alternatives

- **A bounded in-memory `sending_queue` per Backend, dropping when full — core only.** This is the closest call, and it delivers most of what #13 asks: each Backend has its own queue, so one stalling does not block the others. What it does not deliver is durability across a restart, which is exactly the window ADR 0010 named when it wrote "spill/dead-letter". Rejected because the Gateway is a single shared choke point holding every service's telemetry, and "we lose everything queued whenever we roll the Gateway out" is a worse property than a bigger image.
- **One shared `file_storage` extension for all Backends.** Fewer components, one directory, one volume. Rejected outright: a shared storage instance is a shared file lock, a shared disk budget and a shared corruption blast radius, so a Backend filling the disk would take the others down with it — re-coupling the Backends through the mechanism that exists to decouple them, and defeating the point of fanning out from one Gateway.
- **Building a custom collector distribution** with core plus `file_storage` only, via the OpenTelemetry Collector Builder. This is genuinely the better long-term answer — it takes contrib's one useful component without contrib's surface area. Rejected for now because it makes this repository responsible for building, signing, scanning and publishing a collector image, which is a larger commitment than the component it would add. Worth revisiting if the contrib surface becomes a problem.
- **Spill declared per Backend as a full directory path** rather than derived from `spill_root` plus the Backend's name. More flexible — an archive's spill could sit on cheaper disk — but two Backends can then be given the same path by a typo, silently sharing a storage instance and reintroducing the coupling above. Deriving makes that unrepresentable; two Backends cannot share a name.
