# config_version identifies the compiled configuration, not its inputs, and the platform's own signals bypass the pipeline they report on

## Context

ADR 0010 decided the platform observes itself: Agents and the Gateway emit their
own OTEL — queue depth, export failures, drops, and the applied `config_version`
— and a Rollout is confirmed when the expected version appears fleet-wide rather
than through a status protocol. Building it (C7, #15) forced three sentences of
that ADR to be made concrete, and each of them had a trap in it.

**`config_version` is circular as stated.** The Rollout Manifest already records
a `digest: sha256:…` per compiled config (ADR 0006's committed index). The obvious
reading of "the applied config_version" is that digest. But stamping it into the
config so the collector can emit it changes the config, which changes the digest.
A naive implementation either loops, or emits a version derived from something
other than what it claims to identify — which is worse than emitting nothing,
because an operator would trust it and confirm rollouts that had not landed.

**"No separate health channel" removes the fallback.** ADR 0010 is explicit: no
health API, no heartbeat, no back-channel, and none is to be built. So every
failure of self-observation has to be caught where the configuration is written,
because nothing downstream can report it — the thing that would have reported it
is the thing that is broken.

**The bootstrap dependency is named and not solved.** ADR 0010: *"if the Gateway's
own export path is fully broken, its self-telemetry may not escape. Mitigate with
a minimal independent export path."* A queue-depth metric that queues behind the
queue it is reporting on is self-observation that works when things are fine,
which is the only time it does not matter.

Two further facts constrain the answer. C5 (#13) gave each Backend its own
exporter, named after it; C6 (#14) put a `transform/guardrail` in the Gateway that
writes a verdict onto the resource of everything passing through — and a
collector's own telemetry passes through.

## Decision

### config_version is the hash of the compiled configuration, with its own stamp excluded

`otel.platform.config_version` is `sha256:<hex>` over the rendered collector
configuration with the `config_version` attribute removed. Everything that decides
what the collector does is inside the hash; the single field that cannot be
covered is the field carrying the answer, and nothing else is left out.

The alternative was hashing the compile **inputs** — Contract, Profile, taxonomy,
Standard catalog. Rejected, and the reason is not tidiness:

- **An input change that does not reach the artefact would demand a confirmation
  that can never arrive.** Editing a `description:` in `controlplane/profiles.yaml`
  would change every Agent's expected version fleet-wide, while no compiled file
  changed, so no GitOps sync fires, so no collector restarts, so no collector ever
  reports the new value. The platform would be permanently mid-rollout.
- **The version could not be checked from the repository.** Hashing the artefact
  makes the expected value derivable from the committed file itself: strip the
  stamp, hash, compare. Hashing inputs makes it derivable only by trusting the
  compiler that produced it.

The mirror-image cost is real and is accepted: an input that does **not** reach the
compiled config does not move the version. `sampling.traces_percent` is the
standing example — nothing reads it yet (#40), so changing it changes no Agent's
version. That is correct rather than a gap. The version means "this is the pipeline
I am running", and nothing about the running pipeline changed.

Two tests pin it, and they are the two halves that make it trustworthy:
`TestConfigVersionChangesWhenTheProfileChanges` and
`TestConfigVersionDoesNotChangeWhenTheInputsDoNot`.

### The Manifest's digest and the config_version are different values, deliberately

They sit next to each other in `fleet/rollout-manifest.yaml` and answer different
questions:

| | hashes | answers |
| --- | --- | --- |
| `digest` | the compiled **file**: generated header, config, and the stamp | "Is the file in this repo still the one this Rollout wrote?" |
| `config_version` | the **configuration**, with the stamp excluded and no header | "Is the pipeline running out on the fleet the one this Rollout compiled?" |

Only `config_version` can be compared against telemetry; comparing a digest to a
version seen in a Backend is a category error. They cannot coincide, because the
digest hashes a strict superset of the version's input — and the digest is what
catches a hand-edited stamp, which the version by construction cannot. A test
asserts the two are never equal rather than a comment claiming it.

They were not merged into one value. Making the digest *be* the version would
give up the file-integrity property: a forged stamp, or a corrupted header naming
the wrong Contract, would then pass unnoticed.

### The platform's own signals travel a path that is not the pipeline they report on

`service.telemetry.metrics.readers` — a periodic OTLP client with **no sending
queue, no retry, no batch processor and no memory limiter** in front of it. None of
the machinery whose failure it exists to report.

- **The Agent** sends its own metrics to the Gateway, at the address its Pipeline
  Profile already names. Same address, different client: a full or blocked
  `otlp/gateway` sending queue cannot hold up the metric that says the queue is
  full.
- **The Gateway** sends its own straight to one declared Backend, named in
  `self_telemetry.backend`, never through its own pipelines. A Backend that stops
  answering fills its own queue; the metric that says so leaves by a route that
  touches no exporter.

An interval this reader misses is one interval lost. A queue it blocked on would
be the platform going quiet at the only moment anybody was looking.

**The residual is written down rather than glossed.** An Agent's only next hop is
the Gateway (ADR 0007 — an Agent names no Backend), so an Agent cannot report
through a Gateway that is down. Giving every Agent a direct Backend route was
rejected: it would put a Backend's address in a thousand compiled Agent configs
and end Backend-agnosticism for the whole fleet, to buy a signal the Gateway's own
independent path already carries. A collector that is not running reports nothing
at all, and absence is what says so. #47 records what would close it.

`service.telemetry.metrics.level: normal` is written out rather than left implicit.
It is the collector's current default, but it is exactly what gates the
queue-depth, export-failure and drop counters this ADR exists to route — so a
default that moves in a later release would take the platform's back-pressure
signals away without changing a line of this repository.

### The Agent stays core-only

A periodic OTLP reader under `service.telemetry` is in the collector's **core**
distribution. ADR 0014's line holds: the Gateway runs contrib for `file_storage`,
and every Agent — one per service, so the overwhelming majority of collector
processes on this platform — stays core. A test enumerates the components an
Agent's compiled config may name.

The Prometheus **pull** reader was rejected on a different ground: a scrape
endpoint is a status channel by another name, and needs a scraper this platform
does not operate.

### Per-Backend attribution is the naming, and the naming is now load-bearing

The collector labels `otelcol_exporter_queue_size`,
`otelcol_exporter_send_failed_*` and `otelcol_exporter_enqueue_failed_*` with the
exporter's component ID and nothing else. C5 named each Backend's exporter
`otlp/<backend>` for readability; that is now the mechanism by which "which
Backend is behind?" is answerable rather than inferred. One exporter per Backend,
named for it, and no exporter belonging to no Backend — asserted, because an
exporter shared by two Backends would leave an operator with a number and nobody
to attribute it to.

### Self-telemetry is not exempt from a Pipeline Guardrail — it satisfies it

An Agent's own signals reach the Gateway through the ordinary pipeline, so C6's
`transform/guardrail` judges them. It judges them **exactly as it judges the
service**, and no exemption is added:

- An exemption would have to be a resource attribute, and a resource attribute is
  precisely the thing a service can forge. C6 cleared the whole verdict namespace
  because of that; re-opening it here for a "this is platform telemetry" flag
  would undo it.
- An Agent's own telemetry carries the identity its **Telemetry Contract**
  declares. So a compliant service's Agent is compliant for the same reason the
  service is, and a drifted service's Agent is tagged for the same reason. The
  tagging is correct rather than spurious: that Agent *is* that service's Agent,
  and an operator asking why this service's telemetry is thin wants its queue
  depth filed under the service.

The **Gateway's** own telemetry never enters a pipeline, so it is never tagged.
Instead the Gateway is held to the Standards at compile time: a Gateway whose
declared `self_telemetry.resource_attributes` omit an attribute a `block` Standard
requires at the pipeline **does not compile**. The platform does not tag the fleet
for a rule its own telemetry breaks. `block` only, for the same reason only `block`
fails a build at Preflight (ADR 0003) — refusing to compile over `warn`-severity
advice would make Severity mean something different here than everywhere else.

### The platform namespace is the platform's, and it is closed at three points

`otel.platform.config_version` is the single field a Rollout is confirmed by, so a
service answering it would confirm rollouts it never received. Nothing about the
namespace is decorative:

1. **A Telemetry Contract declaring anything under `otel.platform.` does not
   compile**, and neither does a Gateway Declaration. Refused where it was
   written, rather than stripped later where the declaration would still read as
   honoured.
2. **The Agent deletes the namespace from everything it forwards** — a final
   `delete` action on the `resource` processor, after the upserts so nothing can
   put it back. The Agent is the only component that can do this: its own signals
   do not travel its own pipelines, so it can strip the namespace from the
   service's telemetry without stripping its own answer. By the time telemetry
   reaches the Gateway, a collector's legitimate stamp and a service's forgery are
   the same attribute on the same kind of record.
3. **The Gateway sweeps the namespace from every context except the resource.** A
   collector's stamp is on the resource; on a span, a datapoint, a log record or a
   scope the namespace can only have been put there by a service, and most Backends
   flatten record and resource attributes into one queryable field — so a forged
   record-level key would answer an operator's rollout query just as well as a real
   one. The asymmetry is the decision: sweeping the resource too would delete the
   answer the platform is waiting for.

The two namespaces now share the same records, and a test asserts no single
clearing statement reaches both — a Guardrail sweep widened to `^otel\.` would
take every `config_version` on the fleet with it, and rollouts would silently stop
confirming.

What this does **not** close, and it is worth naming rather than implying: a party
that can reach the Gateway's OTLP receiver directly, bypassing any Agent, can set
`otel.platform.config_version` on a resource and the Gateway cannot tell it from an
Agent's. That is not a new exposure and it is not a smaller one than it sounds —
the same party can already claim any `service.name` and impersonate any service on
the platform outright. The boundary is the network: the Gateway's receiver is for
Agents. Nothing about a version stamp makes that boundary weaker than it was.

## Consequences

- **Agent compile output changes, for the first time in seven slices, and only
  here.** Two additions: one `delete` action on the `resource` processor, and the
  `service.telemetry` block. Nothing else about an Agent moves. `fleet/compiled/*`
  and `fleet/rollout-manifest.yaml` are recompiled in the same commit.
- **Layer 1 is untouched.** `otel-guardrail check` over every example in
  `guardrail/examples/`, and `waivers`, produce byte-identical output and exit
  codes. The boundary holds: Layer 1 checks a declared Contract before deploy,
  Layer 2 compiles and observes what runs.
- **The Gateway Declaration gained a required block.** A Gateway that declares no
  `self_telemetry:` does not compile. That is a stricter document than before and
  will reject declarations that used to build — which is the point: a Gateway
  nobody can see looks exactly like a Gateway that is fine, and there is no status
  endpoint to notice the difference from.
- **The Gateway now declares its own `service.version`**, and it is the pinned
  collector distribution's (ADR 0014). It has to move when that pin moves, and a
  stale one is visible in exactly the way any service's stale `service.version`
  is — which is to say, only to somebody looking. It is the price of holding the
  Gateway to S1 rather than exempting it.
- **The self-telemetry Backend is a single point of blindness.** The Gateway's own
  signals go to one declared Backend; if that Backend is the one that fails, the
  Gateway goes dark. The declaration is where that choice is made, and the comment
  there says to pick the most available Backend. Fanning self-telemetry out to
  every metrics Backend was rejected as multiplying the platform's own volume by
  the size of the fan-out for a case the fleet's own telemetry already reveals.
- **Rollout confirmation is per service, not one value fleet-wide.** Each Agent
  runs its own configuration and so announces its own version; the Rollout Manifest
  lists them. A single fleet-wide version was considered and rejected outright — it
  would change every service's version whenever any service's Contract changed,
  and services whose compiled file did not change would never restart to report it,
  so it could never converge.
- **Self-telemetry is fleet volume.** One OTLP export per collector per 30s,
  through the Gateway for every Agent, fanned out to every Backend that takes
  metrics. It is bounded and small per process and it is not free at fleet scale.
  The interval is a fleet-wide constant rather than a per-tier Profile field: what
  varies per Service Tier is how a *service's* telemetry ships, and this is the
  platform reporting on itself at a cadence the platform owns.
- **The harness cannot run the compiled self-telemetry reader as compiled.** In
  the pinned collector, a periodic OTLP reader over gRPC has no plaintext mode —
  `insecure: true` is accepted and ignored — and `readers:` is a list, which
  confmap replaces rather than merges. So the harness restates the reader over
  OTLP/HTTP. Two things close the gap: the compiled files are additionally started
  with **no overlay at all** on their pinned images (a malformed reader is a
  start-up failure — this caught `protocol: grpc/protobuf`, which passes
  `otelcol validate` and then refuses to run), and the `resource` the harness
  asserts on is still the compiled one. #48 records what would close it properly.
- **The harness's own assertions were weaker than they read, and self-telemetry is
  what exposed it.** `compose logs X | grep -q needle` looks obviously correct and
  is not: `grep -q` exits at the first match, `compose logs` takes SIGPIPE and
  exits 141, and `set -o pipefail` reports the pipeline as failed — so a needle
  that *is* present reads as absent. It only bites once the log is large enough for
  `grep` to win the race, which no previous slice's traffic achieved. Every
  polling helper now reads a captured file instead. Two consequences worth
  stating: some C5/C6 assertions had a latent way to fail spuriously, and every
  *negative* assertion in the harness ("no `otel.guardrail` attribute reached the
  Backend") was weaker than it appeared for the same reason and is now real.
- **What is unproven is unproven.** Sustained load, real Backends, and genuine
  fleet-wide confirmation across many hosts are not in reach of a five-container
  harness: #49, #50 and #51.

## Considered alternatives

- **Hash the compile inputs** (Contract + Profile + taxonomy + catalog). Rejected
  above: an input change that does not reach the artefact demands a confirmation
  that can never arrive, and the expected value stops being checkable from the
  repository.
- **Make `config_version` the Rollout Manifest's digest.** The circular one. It
  cannot be stamped into the file it hashes; and defining the digest to *be* the
  version instead gives up the file-integrity property that catches a forged stamp.
- **Truncate the version to something typeable.** Friendlier in a query bar, and
  rejected: this is the value an operator decides a rollout on, and a shorter hash
  buys convenience with exactly the property that makes it worth trusting.
- **A lightweight status endpoint, or the collector's default Prometheus scrape
  endpoint.** Direct confirmation, and both are the back-channel ADR 0006 and ADR
  0010 refused — the second by accident rather than design, which makes it worth
  naming: configuring any reader replaces that default, so the platform pushes
  rather than being scraped.
- **Route the Gateway's self-telemetry into its own OTLP receiver**, so it flows
  through the Gateway's pipelines and fans out to every Backend. Uniform, and it
  would be tagged by the Guardrail like everything else. Rejected: it is the
  bootstrap dependency reintroduced deliberately — the signals would queue behind
  the exporters they report on, and the memory limiter refusing data would refuse
  the telemetry saying so.
- **Give every Agent a direct Backend route** for its own telemetry, so an Agent
  can report through a dead Gateway. Rejected: it puts a Backend's address into
  every compiled Agent config on the fleet and ends Backend-agnosticism (ADR 0007)
  to buy a signal the Gateway's own independent path already carries.
- **Exempt platform self-telemetry from the Pipeline Guardrail**, by a marker
  attribute the Gateway skips. Rejected: the marker is a resource attribute, so
  every service could set it and opt out of enforcement — the exact hole C6's
  namespace clearing exists to close.
- **Have the Gateway clear `otel.platform.` from resources too**, symmetrically
  with the verdict namespace. Rejected: that namespace on a resource is a
  collector's own stamp arriving, and the Gateway cannot tell it from a forgery —
  so the sweep has to happen at the Agent, where the distinction still exists.
- **A per-Service-Tier self-telemetry interval**, in the Pipeline Profile. Rejected
  for now as a field nobody has asked for — the `sampling.traces_percent` mistake
  (#40) in advance rather than in retrospect.
