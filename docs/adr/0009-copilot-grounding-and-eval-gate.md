# Grounded output + eval-harness score gates Autonomy Ladder promotion

## Context

A hallucinated root cause is the Copilot's core failure mode — dangerous when a human acts on it, worse when a Gated or Bounded Autonomy rung acts on it. ADR 0008 established the Autonomy Ladder but left the rung-promotion criterion undefined ("otherwise the ladder is decorative").

## Decision

Trust is built in two parts:

- **Grounding (inference-time).** Every claim the Copilot makes must cite the specific telemetry evidence behind it — a trace, metric query, or log query fetched through the vendor-neutral query tools. Ungrounded hypotheses are suppressed or flagged low-confidence.
- **Eval Harness (offline).** A harness replays past incidents with a known root cause and scores the Copilot's root-cause accuracy over time. This score is the objective gate for promoting the Copilot to the next Autonomy Rung.

## Consequences

- Discharges ADR 0008's open promotion criterion: a rung advance requires a measured accuracy threshold on the Eval Harness, not a judgment call.
- Requires a labeled incident corpus (past incidents with confirmed root cause) to exist and be maintained — a real, ongoing data investment, and the harness is only as good as that corpus.
- Grounding depends on the query tools returning evidence handles (IDs/queries), not just prose — a constraint on the data-access tool design.

## Considered alternatives

- **Grounding only** — gives live trust but leaves promotion subjective; no objective number to earn the next rung.
- **Human-review loop only** — real-world signal but sparse, slow, inconsistent; weak promotion gate and no inference-time grounding.
