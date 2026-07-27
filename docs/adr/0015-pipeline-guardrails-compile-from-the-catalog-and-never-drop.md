# Pipeline Guardrails compile from the one Standard catalog, and Severity never drops telemetry

## Context

ADR 0002 makes a Standard a Rego policy. CONTEXT.md says a Standard is "authored once in a single catalog" and "declares its enforcement point(s) — `preflight`, `pipeline`, or both". Building the Pipeline Guardrail (C6, #14) collides those two sentences with a fact neither anticipated: **a collector cannot evaluate Rego.**

So "the same Standard, selected via `enforced_at: [pipeline]`" cannot mean "the same Rego body, run in the Gateway". Something else has to run there, and there were only two shapes for it:

1. **Author the pipeline check separately** from the Rego — a `processors:` block per Standard, or a second file. This is one Standard with two definitions, in two languages, failing independently. It is the exact failure this repository has already removed three times: the Service Tier taxonomy defined in both Go and Rego (#28), the Waiver register pinned in two places (#32), the summary line computed twice (#33). A service would pass Preflight against one list of required attributes while the Gateway tagged its telemetry against another, and nothing would show it.
2. **Derive both enforcement points from one source** where the requirement is declaratively expressible, and refuse to compile a Standard that claims `pipeline` and cannot deliver it.

Separately, once something *does* run at the Gateway, CONTEXT.md says a Pipeline Guardrail acts by "drop, tag, or alert" — and the obvious reading of Severity is `block` → drop. That reading needs examining, not assuming.

## Decision

### One catalog, two consumers

The requirement a Standard checks, its Severity, and its enforcement points move out of the Rego body into **`guardrail/standards.yaml`** (`kind: StandardCatalog`). Both enforcement points read it and neither defines it:

- The **Preflight Guardrail** hands it to Rego as the data document `data.otel.standards`. A policy reads `data.otel.standards.S1.severity` and `.requires.resource_attributes`. Tests assert that no `.rego` file names a resource attribute or a Severity — the same guard #28 put on the taxonomy.
- The **Control Plane** reads the same entries, selects those with `pipeline` in `enforced_at`, and compiles them into collector processors in the Gateway's config.

This is `guardrail/tiers.yaml` applied a second time: a document the platform owns, that policy consumes and never declares.

### A Standard that cannot be enforced where it claims does not load

A requirement has a **kind**, and the kind decides expressibility:

| Kind | Preflight | Pipeline | Why |
| --- | --- | --- | --- |
| `resource_attributes` | yes | yes | A property of one record. Preflight asks whether the Contract declares them; the Gateway asks whether the live record carries them. |
| `tier_mandatory_signals` | yes | **no** | A property of a service's stream over time. A collector sees one record and cannot tell "emits no metrics" from "no metric in the last second". |

A Standard declaring `enforced_at: [pipeline]` with a requirement of the second kind is **refused when the catalog is read** — exit 2, "the Guardrail could not run", naming the Standard. Refusal happens at the document, not at the Gateway compiler, because a catalog that lies about where it is enforced is one broken fact and should have one error, reached by every reader.

`enforced_at` binds in both directions: the Preflight Guardrail also refuses a violation reported by a Standard the catalog does not enforce at preflight. The Rego aggregator picks up every policy in the catalog directory with no registration step, so a Standard moved to `[pipeline]` while its `.rego` stayed behind would otherwise go on failing builds at a point the catalog says it is not enforced at — a setting one layer honours and the other ignores.

### Severity decides how loudly, never whether the telemetry survives

Every Severity **tags**. No Severity **drops**.

| Severity | What the Gateway does to a violating record |
| --- | --- |
| `info` | sets `otel.guardrail.violation.<id>` = `"info"` |
| `warn` | sets `otel.guardrail.violation.<id>` = `"warn"` |
| `block` | sets `otel.guardrail.violation.<id>` = `"block"`, **and** `otel.guardrail.blocking` = `true` |

`block` → drop is rejected, for three reasons:

- **It destroys the evidence of the violation at the moment it is most wanted.** Telemetry goes malformed during exactly the incidents where somebody needs it — a rushed hotfix, a service started with half its environment, a sidecar that lost its config. Dropping is irreversible and it fires hardest when the cost is highest.
- **A dropped stream is indistinguishable from a service that is down.** That is the failure mode this repository refuses everywhere else: an Agent exporting into nothing (ADR 0013), a Signal no Backend receives (ADR 0007), a queue that reads as durable and is not (ADR 0014). Silently deleting a team's telemetry recreates it, centrally, for the whole fleet.
- **Preflight's `block` stops the violator, not the observation.** It fails a *build* — the deploy of the thing that is wrong, with a human reading the CI output and a cheap fix in front of them. The runtime analogue of "stop the deploy" is not "delete the record"; the record is not the violator, the service is. Dropping punishes the observer.

`otel.guardrail.blocking` is the roll-up an operator counts and alerts on: one low-cardinality boolean, rather than a query that has to know every Standard id. It is written with the same value by every blocking Standard, so no statement order can change what an operator sees, and each Standard writes its own `violation.<id>` key, so no violation can mask another.

**The `otel.guardrail.` namespace is cleared before the verdict is written.** Nothing stops a service emitting `otel.guardrail.blocking: false` itself, and the Agent forwards whatever it is given — so a Guardrail that only ever `set`s on violation would leave that forgery in place, and the single field an operator alerts on would be the easiest thing on the platform to suppress. The first statement in every context deletes the namespace unconditionally; everything after it is the Gateway's.

The verdict is written on the **resource** and nowhere else — one record, one verdict, in the place an operator groups by service. The **clearing** goes further, into every context that carries attributes (`scope`, and `span`/`spanevent`, `datapoint`, `log`), because a service can put the key on a span as easily as on its resource and most Backends flatten the two into one queryable field. Sweeping only the resource would leave the claim true only where somebody happened to look.

### Absence is silence, so the catalog cannot be allowed to go quiet

A Rego policy reads its catalog entry as a data document, and in Rego an **absent document is not an error** — the rule body simply fails and the Standard stops enforcing, reporting nothing. Every way the catalog can go quiet therefore has to be refused in Go, because no consumer downstream can tell it from a fleet that complies:

- **A catalog with no entries is refused**, and unknown keys are refused rather than ignored. `standards:` misspelt decodes cleanly into a catalog with nothing in it — which disarms *both* enforcement points at once, the Gateway compiling no Guardrail processor and every policy reporting nothing. A nil-check on the loaded value cannot catch this, because the file cannot express the difference between "the org enforces nothing" and "this document is broken". The Service Tier taxonomy already refuses an empty document for the same reason.
- **The policies and the catalog are checked against each other when the Guardrail is constructed**, not in a test over the shipped pair — both are loadable from a path, so a caller can assemble any pair and only `NewPreflight` sees the one they assembled. A policy with no entry, an entry whose id its policy cannot reach (Rego keys carry case, package names do not), an entry of a requirement kind its policy does not implement, an entry its policy implements but which is not enforced at preflight, and a preflight Standard with no policy at all: each is refused by name. Every one of them otherwise ends as no violation and no error on a Contract that violates the Standard.
- **A Standard's id is narrower than either use alone would need.** It is an attribute-name segment, a case-sensitive Rego data key, *and* the last segment of a Rego package name — which cannot carry case or a hyphen. `S-9` would be a Standard the Gateway tags and no policy can be written for.

Dropping is not abolished, but it is **no longer derivable from Severity**. A Standard that genuinely should discard telemetry would have to declare that separately and explicitly, and that is a decision to take on its own evidence, not one that arrives as a side effect of an author writing `severity: block`.

### It runs in the Gateway only

The processors are emitted by `assembleGateway` and by nothing else. An Agent's compiled config is unchanged, byte for byte. Agents do no enforcement (ADR 0007), and a Standard enforced in a thousand Agents is a Standard with a thousand places to be out of date.

## Consequences

- **A Standard is one entry in one file.** Authoring one is now: the catalog entry, the `.rego` if it is enforced at preflight, and a graduation deadline if it can block (ADR 0003). The requirement itself is written once and read twice.
- **The Guardrail refuses to start on a catalog and a policy set that do not match**, which is a stricter constructor than before and will reject pairs that used to build. That is the point — every pair it rejects would otherwise have enforced nothing and said nothing — but it means editing the catalog and the policies is now one change, not two that can be landed apart.
- **The catalog's expressiveness is now a real constraint on what a Standard can be.** Only requirements the compiler knows how to turn into processors can be enforced at the pipeline. Adding a kind is a code change here, not a Rego change — which is a genuine cost, and the price of not having two definitions. The cardinality-class Standards this platform will eventually want (#43) need statistics over time and are not expressible at all yet.
- **One Severity per Standard.** ADR 0003's G3 amendment let one Standard emit `block` on a hard condition and `warn` on a softer one, by writing a literal per violation. The catalog declares Severity once per Standard, so that flexibility is gone. It is the right trade: the Gateway compiler needs a Standard's Severity, and a Standard whose Severity depends on the condition is really two requirements sharing an id — which the Gateway could not tag apart anyway, since it tags by id. A Standard that needs two Severities is now written as two Standards.
- **Every record that violates a Standard is bigger.** Two extra resource attributes on a non-compliant stream is not free at fleet scale, and it lands in every Backend the Signal fans out to. It is bounded (one key per pipeline-enforced Standard, plus one roll-up) and it only touches records that violate something.
- **C7 (#15) gains its first Guardrail signal, and it is in the telemetry rather than in a side channel.** ADR 0010 says the platform watches its own telemetry and builds no status back-channel; a tag on the record is exactly that. Counting violations, alerting on `otel.guardrail.blocking`, and attributing them per service is C7's to build on top — deliberately not built here, and deliberately not designed out.
- **Nothing about `otel-guardrail check` changes.** Layer 1 output and exit codes over every example are byte-identical to before this change, and so are both compiled Agent configs. The boundary is intact: Layer 1 checks a *declared* Contract before deploy, Layer 2 checks *live* telemetry at run time — same catalog, same Severity, different evidence, different moment.
- **A per-tier Pipeline Guardrail is still impossible**, and is not attempted. The Gateway cannot tell Service Tiers apart at run time because nothing stamps a tier attribute — the gap #40 already records for tail sampling. Every Standard enforced here today applies to every service regardless of tier, so C6 needed no answer; #44 records what it would take.

## Considered alternatives

- **Author the pipeline check beside the Rego** (option 1 above). Rejected on the drift criterion: one Standard, two definitions, failing independently and silently. The three prior removals of exactly this shape (#28, #32, #33) are the evidence.
- **Generate collector processors *from* the Rego**, by analysing the policy's AST. This would keep the Rego as the single source and need no catalog document. Rejected: it makes an arbitrary Turing-complete policy the input to a compiler that can only emit a tiny declarative subset, so whether a Standard compiles would depend on how it happened to be written rather than on what it requires. The failure would be obscure ("your `some ... in` was not in the recognised shape") where the catalog's is plain ("this requirement kind is not expressible at the pipeline").
- **Run OPA beside the Gateway**, as an external processor or an OTTL extension calling out. This would let the actual Rego run against live telemetry, which is the honest version of "the same Standard". Rejected for now: it puts a policy evaluation in the hot path of the whole fleet's telemetry, needs a component this platform would own and operate, and re-opens the per-record latency question that `batch` and `memory_limiter` exist to bound. Worth revisiting if the expressible set proves too narrow.
- **`block` → drop.** Rejected above. The strongest argument for it is symmetry with Preflight — a `block` Standard should feel the same at both points. The counter is that it *is* the same at both points once the analogy is drawn correctly: Preflight stops the violator from shipping, and the runtime equivalent is making the violation impossible to ignore, not deleting the evidence.
- **`block` → route to a quarantine Backend.** Real isolation, and available (`routing` is in contrib per ADR 0014). Rejected: it invents a Backend nobody declared, splits a service's telemetry across two destinations so correlation breaks, and makes the fan-out depend on the record's content — all to solve a problem tagging already solves.
- **Count violations into a metric with the `count` connector**, so the roll-up is a real number rather than an attribute. Genuinely better for alerting, and available. Deferred rather than rejected: it adds connectors to the compiled-config model, and counting is what C7 (#15) exists to build on top of the evidence C6 emits. #45 records it.
- **A Standard declaring its own action** (`pipeline_action: tag | drop`) instead of deriving it from Severity. Rejected for C6 as premature: no Standard wants to drop, and the field would exist unset on every entry, inviting the first author who wants a stronger word to reach for it. It is the right shape *if* a dropping Standard is ever justified, and this ADR is what that decision would amend.
