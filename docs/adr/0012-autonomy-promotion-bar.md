# Autonomy promotion needs fixed per-rung accuracy plus a zero-harm gate for action rungs

## Context

ADR 0009 made the Eval Harness score the objective gate for climbing the Autonomy Ladder but left the actual bar undefined. Advice rungs and action rungs carry very different risk: a wrong hypothesis at Advisor wastes an engineer's time; a wrong action at Gated/Bounded can cause the outage. Accuracy alone cannot gate an action rung — a Copilot can be highly accurate on average and still take a catastrophic action on the case it gets wrong.

## Decision

Each Autonomy Rung has a published, quantitative promotion bar set before promotion is enabled:

- **Advice rungs (→ Advisor established, → Gated):** at least X% top-1 root-cause accuracy over at least N graded incidents on the Incident Corpus.
- **Action rungs (→ Gated actions, → Bounded Autonomy):** a higher accuracy bar AND zero harmful actions across the entire **Harm Set**, run in shadow (the Copilot proposes actions, nothing executes, outcomes are graded).

The exact X and N per rung are configuration to be set with the observability/SRE org, not fixed here. The harm-gate is absolute: any harmful action on the Harm Set blocks promotion regardless of accuracy.

## Consequences

- The ladder is objective and auditable — promotion is a measured threshold, not a judgment call.
- Action rungs are strictly harder to reach than advice rungs, by construction.
- The Harm Set must be built and maintained as its own artifact (scenarios where the right move is to do nothing or take a strictly safe action), separate from the accuracy corpus.
- A shadow-run mode (propose, don't execute, grade) is required before any action rung goes live.

## Considered alternatives

- **Beat the human baseline** — relative and intuitive, but needs an expensive labeled human baseline and "as good as humans" may be too low a bar for autonomous action.
- **Review-board judgment** — flexible but subjective and slow; re-opens ADR 0008's "decorative ladder" risk unless the criteria are written down, at which point it reduces to the fixed-bar approach anyway.
