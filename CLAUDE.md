# OTEL Platform

Internal platform for managing OpenTelemetry across the org, built in three layers in order: **Guardrails** → **Control Plane** → **Copilot**. Domain language is in [CONTEXT.md](./CONTEXT.md); architecture decisions in [docs/adr/](./docs/adr/); the plan in [docs/PRD.md](./docs/PRD.md).

## Agent skills

### Issue tracker

Issues live in GitHub Issues on `ghantakiran/OTEL` (use the `gh` CLI). See `docs/agents/issue-tracker.md`.

### Triage labels

Five canonical triage roles map 1:1 to same-named labels (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`); plus repo-specific `hitl` and `layer:*` labels. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `CONTEXT.md` + `docs/adr/` at the root. See `docs/agents/domain.md`.
