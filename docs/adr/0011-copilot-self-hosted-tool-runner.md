# Copilot self-hosted via Claude API + Tool Runner, not Managed Agents

## Context

The Copilot is an agentic incident-responder that reads untrusted telemetry (see ADR 0008 — telemetry is a prompt-injection surface). Two harness options exist: Anthropic's Managed Agents (hosted agent loop plus a per-session sandbox with bash/file/code execution), or self-hosting the loop with the Claude API and its Tool Runner helper.

## Decision

The Copilot self-hosts the agent loop using the Claude API Tool Runner, on `claude-opus-5` with adaptive thinking (effort `high`, `xhigh` for root-cause reasoning). The model is given only the vendor-neutral typed tools we define (`query_traces` / `query_metrics` / `query_logs`, `get_contract`, `get_standards`). Telemetry enters exclusively as tool-result content framed as data, never as instructions. There is no Anthropic-hosted sandbox with reach near production at the Advisor rung.

## Consequences

- The injection boundary and the entire tool surface are owned end-to-end — nothing the Copilot can do exists outside the tools we wrote.
- We own the loop code, retries, and context management (the Tool Runner is a thin SDK helper, so this is small and swappable).
- Managed Agents remains a candidate for the later Gated / Bounded Autonomy rungs, where a caged sandbox to execute reversible actions (and built-in Outcomes / scheduled on-call deployments) becomes valuable — revisit when the Autonomy Ladder reaches those rungs.

## Amendment — 2026-07-28, model ID (#55)

This ADR was accepted naming `claude-opus-4-8`. That ID is superseded by
`claude-opus-5`, and the Decision above has been updated to name the current one.

**Recorded rather than substituted quietly, because the ID is load-bearing here
and not everywhere.** #19's whole design is a cost split between a cheap triage
tier and an expensive RCA tier, and #20's promotion gate scores a specific model —
a spec naming a model nobody runs makes both unreproducible. The decision the ADR
records is unchanged: self-host the loop, own the injection boundary and the whole
tool surface. Only the identifier moved.

Two things the newer model changes for anyone implementing this, neither of which
alters the decision:

- **Sampling parameters are rejected.** `temperature`, `top_p`, and `top_k` return
  a 400. Behaviour is steered by prompting.
- **Thinking is on by default**, and `max_tokens` caps thinking *plus* response
  text together — so a limit sized around the answer alone truncates mid-response.

`claude-haiku-4-5` for triage (#19) is unchanged: it is still current, and swapping
it would be a design change rather than a currency fix.

## Considered alternatives

- **Managed Agents now** — hosted loop + sandbox, strong for action rungs, rejected for the Advisor rung: it adds a sandbox with reach and cedes some control of the tool surface and injection framing while the Copilot is still read-only.
- **Start self-hosted, switch to Managed Agents at action rungs** — the likely long-term path; deferred as an explicit decision for when those rungs arrive rather than committing to two harnesses now.
