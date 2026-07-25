# Copilot autonomy is staged via an Autonomy Ladder; no full autonomy day one

## Context

The end goal for the Copilot is autonomous incident response. But an LLM with autonomous write access to production observability infrastructure carries severe, specific risks:

- **Prompt injection from telemetry.** Logs, span attributes, and error messages contain arbitrary user- and attacker-controlled strings. An agent that reads telemetry and then acts treats that untrusted input as potential instructions; this cannot be fully sanitized because it is the agent's core input.
- **Self-inflicted outage.** Autonomous silence/sampling-down/reroute on a wrong hypothesis can blind the org to a real outage, destroy the evidence needed to debug, or directly cause a new outage.
- **Corrupting the ground truth.** Observability is what the org trusts during an incident; an agent that damages it removes the footing needed to recover.

## Decision

Autonomy remains the north star, but is reached in stages via an **Autonomy Ladder**: **Advisor** → **Gated** → **Bounded Autonomy**. The Copilot starts at Advisor (read + suggest, human executes). Promotion to the next rung is earned by measured accuracy, not shipped by default. Telemetry is always treated as untrusted input and never as instructions.

## Consequences

- First value ships fast and safely at the Advisor rung, which also benefits most from the clean data the guardrails produce.
- The org must define the promotion criteria (what accuracy, over what sample, judged how) before any rung advance — otherwise the ladder is decorative.
- Gated and Bounded Autonomy rungs require an action-authorization + audit model and a blast-radius cage (reversible-only actions, no alert-silencing, auto-reverting GitOps PRs) — designed when those rungs are approached, not at Advisor.

## Considered alternatives

- **Full autonomy from day one** — highest leverage, rejected: prompt-injection and self-outage risk on prod observability infra is unacceptable without earned confidence and a cage.
- **Permanent read-only advisor** — safest, rejected as it forecloses the autonomy goal; the ladder keeps the goal while bounding risk.
