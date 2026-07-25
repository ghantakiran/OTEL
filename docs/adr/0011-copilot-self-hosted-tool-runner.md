# Copilot self-hosted via Claude API + Tool Runner, not Managed Agents

## Context

The Copilot is an agentic incident-responder that reads untrusted telemetry (see ADR 0008 — telemetry is a prompt-injection surface). Two harness options exist: Anthropic's Managed Agents (hosted agent loop plus a per-session sandbox with bash/file/code execution), or self-hosting the loop with the Claude API and its Tool Runner helper.

## Decision

The Copilot self-hosts the agent loop using the Claude API Tool Runner, on `claude-opus-4-8` with adaptive thinking (effort `xhigh` for root-cause reasoning). The model is given only the vendor-neutral typed tools we define (`query_traces` / `query_metrics` / `query_logs`, `get_contract`, `get_standards`). Telemetry enters exclusively as tool-result content framed as data, never as instructions. There is no Anthropic-hosted sandbox with reach near production at the Advisor rung.

## Consequences

- The injection boundary and the entire tool surface are owned end-to-end — nothing the Copilot can do exists outside the tools we wrote.
- We own the loop code, retries, and context management (the Tool Runner is a thin SDK helper, so this is small and swappable).
- Managed Agents remains a candidate for the later Gated / Bounded Autonomy rungs, where a caged sandbox to execute reversible actions (and built-in Outcomes / scheduled on-call deployments) becomes valuable — revisit when the Autonomy Ladder reaches those rungs.

## Considered alternatives

- **Managed Agents now** — hosted loop + sandbox, strong for action rungs, rejected for the Advisor rung: it adds a sandbox with reach and cedes some control of the tool surface and injection framing while the Copilot is still read-only.
- **Start self-hosted, switch to Managed Agents at action rungs** — the likely long-term path; deferred as an explicit decision for when those rungs arrive rather than committing to two harnesses now.
