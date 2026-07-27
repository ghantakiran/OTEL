# The Agent and Gateway topology

Telemetry takes two hops on this platform, and a service is only aware of the first one.

```
                                                  ┌─OTLP─▶ primary-apm    (all Signals)
  service ──OTLP──▶ Agent ──OTLP──▶ Gateway ──────┼─OTLP─▶ metrics-store  (metrics only)
   (one per          (one per        (one,        └─OTLP─▶ cold-archive   (traces, logs)
    service)          service)        shared)              Backends (three, today)
```

## The three pieces

**Agent** — a lightweight collector running next to one service. It receives that service's OTLP, stamps the resource attributes its **Telemetry Contract** declares, batches, and forwards to the Gateway under a memory ceiling. It does no enforcement, and it names no Backend.

**Gateway** — the central collector tier. Every Agent in the fleet forwards here. It rebatches across all of them and **fans out to every Backend**. It is also where **Pipeline Guardrails** will run and where back-pressure is handled, per Backend (ADR 0010).

**Backend** — a destination system where telemetry lands: an APM, a metrics store, a cold archive. Only the Gateway names one, and it names it by the **role** it fills rather than the product filling it — `primary-apm`, not a vendor. Services are Backend-agnostic (ADR 0007), and so is this repository: a vendor name here would end up in every compiled Gateway config, in git history, and in the exporter IDs the platform's own metrics are labelled by, so a migration would become a rename across all of it instead of one edit to one file.

The reason a service never names a Backend is ADR 0007: if each service targeted a Backend directly, telemetry would fragment across vendors, cross-service correlation would break, and adding or swapping a Backend would become a fleet-wide change. Instead it is one edit to one file.

## Where each shape is declared

| | Declared in | How many | Compiled by |
| --- | --- | --- | --- |
| What a service emits | its **Telemetry Contract** | one per service | `otel-guardrail compile` |
| How a tier ships it | `controlplane/profiles.yaml` — the **Pipeline Profiles** | one per Service Tier | `otel-guardrail compile` |
| The Gateway | `controlplane/gateway.yaml` — the **Gateway Declaration** | exactly one | `otel-guardrail gateway` |

The Gateway's shape is *not* in a Pipeline Profile, and that is the load-bearing decision here (ADR 0013). A Profile is per Service Tier; the Gateway is one shared piece of infrastructure that no tier selects. Three tier Profiles each naming a Gateway shape would describe three Gateways where the org runs one, and merging them would leave the Gateway's shape with no owner.

What stays in the Profile is what is genuinely per tier. `sampling.traces_percent` is the standing example: it is the Gateway's tail-sampling budget *for that tier*, a per-tier cost decision. **Nothing reads it yet** — see [How it interacts with `sampling.traces_percent`](#how-it-interacts-with-samplingtraces_percent) below.

## Fanning out to several Backends

The Gateway exports to every Backend the Declaration names. A service is unaware of all of it: adding, removing or swapping one is an edit to `controlplane/gateway.yaml` and a rollout, never a fleet change (ADR 0007).

### Per Signal

A Backend declares which **Signals** it receives:

```yaml
- backend: metrics-store
  endpoint: metrics-otlp.observability.svc.cluster.local:4317
  signals: [metrics]
```

Omitting `signals:` means every Signal — a Backend takes the whole stream unless it says otherwise, which is what an APM does. A Backend that declares a subset appears **only in those pipelines**. This is not cosmetic: a metrics store has nowhere to put a span, so exporting traces to it produces export failures indistinguishable from that Backend being down.

Compiling refuses a Signal no Backend receives at all — that is a pipeline taking the fleet's telemetry and exporting it nowhere, which is the no-Backend failure one Signal at a time.

### Each Backend is isolated from the others

This is the point of fanning out from one place, and it is the whole of the mechanism:

| Each Backend gets its own… | Declared as | Because a shared one would mean |
| --- | --- | --- |
| exporter | `backend:` (compiles to `otlp/<name>`) | one endpoint for several destinations |
| sending queue | `delivery.queue_size` | one slow Backend filling everyone's queue — the outage this exists to prevent |
| retry | `delivery.retry` | one Backend's back-pressure policy applied to all |
| spill storage + directory | `delivery.spill`, derived from `spill_root` and the Backend's name | one file lock, one disk budget, one corruption blast radius — the Backends re-coupled a layer down |

Nothing is shared between two Backends, and the shared cases are unrepresentable rather than merely discouraged: the storage instance and its directory are **derived** from the Backend's name, and two Backends cannot share a name.

`delivery` sits per Backend and always has (ADR 0010) — C3 put the field in the right place before there was more than one Backend to distinguish, so fan-out needed no migration.

### Spill, and the collector distribution

`delivery.spill: true` makes a Backend's queue persistent, so it survives the Gateway restarting while that Backend is down. It compiles to a `file_storage/<backend>` extension on `<spill_root>/<backend>`.

**This is the one place the platform leaves the collector's core distribution.** `file_storage` is in opentelemetry-collector-contrib, so **the Gateway must run a contrib build** — `otel/opentelemetry-collector-contrib`, pinned. On a core build this config fails at load. ADR 0014 records what spill buys, what contrib costs, and what was considered instead.

**Agents stay core-only.** An Agent needs no spill: it runs beside a service on that service's disk, and its next hop is the Gateway, which this platform operates. No Pipeline Profile sets `spill`, and nothing compiles it into an Agent config.

The Gateway needs a real volume mounted at `spill_root`. That is a deployment fact this repository does not own and it is load-bearing — a `spill_root` that is not a real volume gives queues that read as durable and are not.

### How it interacts with `sampling.traces_percent`

It does not, yet, and that is now tracked in **#40** rather than deferred a fourth time.

`sampling.traces_percent` has been in every Pipeline Profile since C2, labelled as the Gateway's tail-sampling budget. C5 built the fan-out and deliberately did **not** consume it, because the blocker is not the `tail_sampling` processor — the Gateway already runs contrib, so that is available. The blocker is that **the Gateway cannot tell Service Tiers apart at run time**: no Telemetry Contract stamps a tier attribute, so a per-tier policy has nothing to key on. ADR 0013 flagged exactly this. #40 either resolves it or deletes the field.

Where tail sampling will sit when it lands is already fixed by the shape above: it is a **processor**, upstream of the fan-out, so it drops traces once for every Backend rather than per destination. A Backend wanting an unsampled stream would be a different decision.

### What C7 will read from this

C7 (#15) builds the platform's self-observation, and per-Backend isolation is what makes it answerable:

- **Exporter IDs are Backend names.** `otlp/primary-apm`, not `otlp/1`. Queue depth, send failures and dropped batches come out of the collector labelled by exporter, so "which Backend is behind" is read off the metric rather than inferred.
- **Storage instances are separate.** Spill depth per Backend is a real per-Backend signal, because the directories are not shared.
- **The fan-out is per Signal**, so a Backend's failure rate is comparable only against the Signals it actually receives.

## The two halves cannot drift

Every Profile forwards its tier's Agents to a `gateway_endpoint`. The Gateway Declaration says what `address` the Gateway answers on. If those disagree, every Agent on the affected tier exports into nothing — successfully, as far as the service can tell, for as long as nobody looks.

So `otel-guardrail gateway` reads the Profiles too, and refuses:

```
otel-guardrail: cannot compile the Gateway: Pipeline Profile "tier-1-critical" forwards
Service Tier tier-1 to otel-gateway.observability.svc.cluster.local:4319, but the Gateway
answers on otel-gateway.observability.svc.cluster.local:4317 — telemetry sent there would
be dropped, so fix one side or the other
```

## Compiling the Gateway

```
otel-guardrail gateway
```

Writes the shared Gateway's collector configuration to stdout. It takes no Telemetry Contract — there is one Gateway and it is nobody's service.

<!-- verify: gateway -->
```
# Compiled for the shared Gateway, which answers on otel-gateway.observability.svc.cluster.local:4317.
# Generated by otel-guardrail gateway — do not edit; change the Gateway Declaration.
receivers:
    otlp:
        protocols:
            grpc:
                endpoint: 0.0.0.0:4317
processors:
    batch:
        send_batch_size: 8192
        timeout: 5s
    memory_limiter:
        check_interval: 1s
        limit_mib: 1024
exporters:
    otlp/cold-archive:
        endpoint: archive-otlp.observability.svc.cluster.local:4317
        retry_on_failure:
            enabled: true
        sending_queue:
            enabled: true
            queue_size: 2000
    otlp/metrics-store:
        endpoint: metrics-otlp.observability.svc.cluster.local:4317
        retry_on_failure:
            enabled: true
        sending_queue:
            enabled: true
            queue_size: 10000
            storage: file_storage/metrics-store
    otlp/primary-apm:
        endpoint: apm-otlp.observability.svc.cluster.local:4317
        retry_on_failure:
            enabled: true
        sending_queue:
            enabled: true
            queue_size: 20000
            storage: file_storage/primary-apm
extensions:
    file_storage/metrics-store:
        create_directory: true
        directory: /var/lib/otelcol/spill/metrics-store
    file_storage/primary-apm:
        create_directory: true
        directory: /var/lib/otelcol/spill/primary-apm
service:
    extensions:
        - file_storage/primary-apm
        - file_storage/metrics-store
    pipelines:
        logs:
            receivers:
                - otlp
            processors:
                - memory_limiter
                - batch
            exporters:
                - otlp/primary-apm
                - otlp/cold-archive
        metrics:
            receivers:
                - otlp
            processors:
                - memory_limiter
                - batch
            exporters:
                - otlp/primary-apm
                - otlp/metrics-store
        traces:
            receivers:
                - otlp
            processors:
                - memory_limiter
                - batch
            exporters:
                - otlp/primary-apm
                - otlp/cold-archive
```

Exit `0` compiled, `1` the Gateway cannot be compiled as declared, `2` the compiler could not run — the same split `check` and `compile` make.

  `--declaration` — a Gateway Declaration to compile instead of the org one built into the binary.
  `--profiles` — Pipeline Profiles to cross-check against instead of the org ones.

Note what differs from an Agent's config, and why:

- **Every Signal, always.** An Agent's pipelines are the Signals *one* Contract declares. The Gateway is shared, so it must relay anything any Contract could declare. Which *Backends* each of those pipelines exports to is per Signal, though.
- **No `resource` processor.** Resource attributes are the Contract's, stamped at the Agent. The Gateway restamping them would mean two places could decide what a service is called.
- **`delivery` is per Backend**, not per Gateway. One slow or unreachable Backend must not block exports to the others (ADR 0010).
- **`extensions`.** The spill storage, one instance per spilling Backend. An Agent's config has none, and the field is omitted entirely rather than emitted empty.

## What refuses to compile

| Situation | Why it is not a default |
| --- | --- |
| A Profile forwards Agents somewhere the Gateway does not answer | Every Agent on that tier exports into nothing, successfully. |
| The Gateway names no Backend | The fleet's telemetry arrives and stops there, invisibly from any service. |
| No Backend receives some Signal | That pipeline takes the fleet's telemetry and exports it nowhere — the same failure, one Signal at a time. |
| A Backend declares a `signals:` entry that is not a Signal | It matches no pipeline, so it silently receives nothing while every other export succeeds. |
| Two Backends share a name | Their exporters and spill directories collapse into one; the second replaces the first, and nothing shows the first was ever declared. |
| A Backend asks to `spill` with no `spill_root` | Its storage directory would be relative to wherever the process started — unmounted, gone on restart, and reading as durable the whole time. |
| A Backend asks to `spill` with no `queue_size` | Spill is where the sending queue is kept. With no queue there is nothing to keep, and the volume would be mounted for nothing. |
| The address is not `host:port` | The receiver is derived from its port; the fleet finds out, not the editor. |
| A Backend has no name or no endpoint | An exporter that is unnameable or unreachable. |

## Running the harness

```
bash harness/run.sh          # add --keep to leave it running
```

Needs Docker, `docker compose`, and Go. Takes about two minutes. It stands up an Agent, a Gateway, **three Backend stand-ins** and a sample service, and asserts telemetry crosses every hop — with one of the three Backends deliberately unreachable throughout.

**Both collector configs are compiled by the binary under test**, into `harness/generated/`, and mounted verbatim. A harness proving a hand-written config works would prove nothing about the compiler.

The Gateway runs `otel/opentelemetry-collector-contrib` and the Agent runs the core `otel/opentelemetry-collector`, at the same pin. That split is not incidental: it is ADR 0014 made executable, and the compiled Agent config is still proven to run on a core build.

The cold archive's container is simply never started, so `archive-otlp...` does not resolve. Its Backend is genuinely absent rather than simulated.

It asserts, in order:

1. **A negative control.** With the Gateway not running, a span emitted to the Agent does *not* reach the Backend. Without this, arrival later would not prove the Gateway was in the path.
2. **The Gateway starts with one Backend unreachable**, and each spilling Backend's own `file_storage` extension starts with it.
3. **Arrival by trace ID at the healthy Backend**, while the archive is down.
4. **The Contract's `service.name`, not the sample service's.** The sample service sends `service.name: harness-sample-service`; what reaches the Backend says `checkout-api`, which is what `guardrail/examples/compliant-contract.yaml` declares. Only the compiled Agent config could have made that substitution, so it ran — this is *declared equals deployed* (ADR 0005) observed rather than asserted.
5. **Fan-out per Signal, both directions.** The span does *not* reach the metrics-only Backend, which is running and would have printed it. A metric emitted next *does* reach it — and also reaches the primary APM, which takes every Signal.
6. **The archive had no container at all** while 3–5 ran, so those assertions really were made against a Gateway holding a down Backend.
7. **The archive's own queue drains when it comes up.** A span emitted while the archive had no container reaches it once it is started, which it cannot have had before: the Gateway held that span in *that Backend's* queue while serving the other two normally.

## What the harness proves, and what it does not

**Proves:**

- OTLP flows service → Agent → Gateway → several Backends over the wire, on compiled configs.
- Two real `otelcol` binaries **accept and start on** the compiled configs — including the contrib Gateway with two `file_storage` extensions and per-Backend persistent queues. That is stronger than `Validate()`, which only checks referential integrity, and it narrows the gap #34 records for these configs on this collector version.
- The Gateway is genuinely in the path (the negative control).
- The compiled Agent's `resource` processor stamps the Contract's attributes over whatever the service sent.
- **Fan-out is per Signal**, observed on running Backends in both directions.
- **One Backend being unreachable does not stop the others receiving**, and what that Backend missed was held in its own queue rather than dropped — for as long as its own `retry_on_failure` budget allows, which is the collector's 300s default since no Backend declares otherwise.

**Does not prove:**

- **That a real Backend ingests any of this.** All three stand-ins are collectors with a `debug` exporter. They receive OTLP exactly as a Backend would and print what arrived — so this proves telemetry crossed the wire out of the Gateway, and nothing about how a real APM, metrics store or archive would ingest, index, or render it.
- **That the compiled configs work under TLS.** The harness adds exactly one thing to each compiled config: `tls: insecure: true`, in `harness/insecure/*.yaml`. The compiled configs are written for a cluster with certificates; the harness runs on a docker bridge with none. Nothing here exercises the TLS path a real deployment uses. The concession is kept to one visible file per collector precisely so it cannot be mistaken for something the compiler produces.
- **What happens when a Backend is SLOW rather than ABSENT.** This is the important gap. An unreachable Backend fails fast; one that accepts a connection and then takes 30 seconds per batch holds the exporter's workers, and how that interacts with the `batch` processor upstream of all three exporters is untested. That case is C7 (#15).
- **Anything under sustained load.** One span and one metric do not fill a 20,000-batch queue or approach a 1024 MiB `memory_limiter`, and never reach the point where a queue is full and drops — which is where isolation is actually tested.
- **That spill survives a Gateway restart.** The extensions load, start, and each open their own directory; persistent queues are initialised per Backend per Signal. But the harness never restarts the Gateway, and it mounts `spill_root` as **tmpfs** — so the one property spill exists for is the one property this cannot show.
- **That it runs anywhere but a developer's machine.** GitHub Actions is disabled on this repository, so this has never run in CI.

Those gaps are recorded in full, with what it would take to close each, in **#38** (the C3 harness) and **#41** (this fan-out specifically).

## Reading the compiled configs by hand

```
bash harness/run.sh --keep
cat harness/generated/agent.yaml
cat harness/generated/gateway.yaml
docker compose --project-directory harness -f harness/docker-compose.yaml logs backend
docker compose --project-directory harness -f harness/docker-compose.yaml logs backend-metrics
```
