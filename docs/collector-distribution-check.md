# The Distribution Check

Every collector configuration this repository ships is loaded and started by the **Collector Distribution** it is compiled for, on every pull request.

Before this, nothing in CI had ever handed a compiled configuration to a collector. `fleet/compiled/` was YAML that a Go test believed in.

## What it runs

```
GUARDRAIL=<path to otel-guardrail> bash .github/scripts/validate-compiled-configs.sh
```

| Configuration | Where it comes from | Distribution |
|---|---|---|
| `fleet/compiled/*.yaml` — one per service in the Fleet | committed by `compile-fleet` | **core** |
| the shared Gateway | compiled at check time by `otel-guardrail gateway` | **contrib** |

The Gateway is compiled rather than read because it is not a committed artefact: there is one Gateway and its configuration is produced on demand from the Gateway Declaration.

Both distributions are pinned in [`harness/collector-images.env`](../harness/collector-images.env), which `harness/docker-compose.yaml` reads too. One pin, two consumers — a harness proving a configuration runs on one version while CI checked it against another would be two answers to one question, and the wrong one would be the one nobody ran.

## Two steps, and the second is not redundant

**`otelcol validate`** resolves the configuration against the distribution's component set. It catches the whole unknown-component class, and it is cheap.

**Starting the collector** on the file, and requiring it to announce `Everything is ready`, catches what `validate` does not. This is not a precaution: `protocol: grpc/protobuf` in a self-telemetry reader passes `otelcol validate` and then refuses to start, because the telemetry SDK is built after configuration resolution and before the first pipeline byte moves. C7 hit exactly that. A check that stopped at `validate` would have shipped it.

## Why the distribution split *is* the check

Agents go to core and the Gateway to contrib because that is what they run on (ADR 0014): the Gateway needs `file_storage` to **Spill** and `transform` for the **Pipeline Guardrail**, and neither is in the core distribution. An Agent needs neither and stays core-only.

Checking every file on contrib would be easier and would prove less. It would pass a compiled Agent that had quietly acquired a contrib-only component — a Pipeline Profile edit is all it would take — and the first anyone would know is a crash loop next to a production service. The split here is ADR 0014's line, mechanised.

The negative control is real, not hypothetical. An Agent configuration given a `transform` processor is refused by core with the distribution's own words:

```
error decoding 'processors': unknown type: "transform" for id: "transform/sneaky"
  (valid values: [attributes resource span probabilistic_sampler filter batch memory_limiter])
```

## What the output looks like

```
== The Fleet's compiled Agent configuration, on the core distribution
  ok   orders-api.yaml starts on otel/opentelemetry-collector:0.127.0
  ok   payments-api.yaml starts on otel/opentelemetry-collector:0.127.0
  ok   reporting-worker.yaml starts on otel/opentelemetry-collector:0.127.0

== The shared Gateway, on the contrib distribution
  ok   gateway.yaml starts on otel/opentelemetry-collector-contrib:0.127.0
```

A failure names the file, names the distribution, and prints the collector's own output rather than a summary of it — the whole value of the check is that the collector says something `Validate()` cannot, so paraphrasing it would throw away the thing being bought. One failure does not stop the run; a broken Rollout reports every broken file at once.

A run that finds no compiled configuration **fails**. A moved directory, or a Fleet that did not compile, would otherwise turn CI green on the strength of an empty loop — the same vacuous success the Rollout Manifest exists to prevent.

## What it proves that nothing else did

- Every compiled Agent configuration is one a **core** `otelcol` accepts and starts. Nothing had established that for the committed fleet: the harness compiles its own configurations from harness Contracts, not from `fleet/contracts/`.
- The compiled Gateway configuration is one a **contrib** `otelcol` accepts and starts, including two `file_storage` extensions each opening their own spill directory.
- The compiled **self-telemetry** reader is constructed and dialled exactly as compiled, with no overlay. The collector logs `Setting up own telemetry...` carrying the compiled resource — `service.name`, the Telemetry Contract's attributes, and the `otel.platform.config_version` the Rollout Manifest predicted — and then dials the compiled endpoint over gRPC with the compiled TLS setting. That is the part of #48 this closes for free, and it closes it on the *shipped* artefact rather than a harness-generated one.

## What it does not prove

- **A started collector is not a working one.** Nothing here sends telemetry through these configurations. `harness/run.sh` does that, on the topology, over about eight minutes; this is deliberately the cheap check that can run on every pull request.
- **The self-telemetry reader never delivers.** It dials the compiled endpoint and fails, because a compiled configuration names cluster DNS that does not resolve in a bare container. So the reader is proven *buildable and startable as compiled* — the failure mode C7 actually hit — and not proven to complete a TLS handshake or land a metric in a Backend. That residual stays on #48, which is now narrower than it was but not closed.
- **Exporter endpoints are not reachable, on purpose.** The DNS failures in the logs are expected and are not what is being asserted. A check that required them to resolve would be the harness, and would not fit in a pull request.
- **One version.** The check says these configurations run on the pinned distribution. It says nothing about the next one; ADR 0014 made the pin a decision rather than a bump, and this is one more thing to re-run when it moves.

## Its own test

`.github/scripts/validate-compiled-configs_test.sh` stubs `docker` and asserts the branching: which distribution each file is handed to, whether a refusal came from `validate` or from a collector that would not start, that one failure does not end the run, and that a run which checked nothing exits non-zero. It creates no containers and pulls no images.

Same precedent as `gitops-rollout-pr_test.sh` and `waiver-expiry-issue_test.sh`: the parts of this platform that live in bash still get tested, because the branching is where the failures are.
