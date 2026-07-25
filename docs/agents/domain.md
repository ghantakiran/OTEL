# Domain Docs

How the engineering skills should consume this repo's domain documentation when exploring the codebase.

This repo is **single-context**: one `CONTEXT.md` + `docs/adr/` at the root.

## Before exploring, read these

- **`CONTEXT.md`** at the repo root (the glossary).
- **`docs/adr/`** — read ADRs that touch the area you're about to work in. There are currently 12 (0001–0012) spanning Guardrails, Control Plane, and Copilot.

If any of these files don't exist, **proceed silently**. Don't flag their absence; don't suggest creating them upfront. The producer skill (`/grill-with-docs`) creates them lazily when terms or decisions actually get resolved.

## File structure

Single-context repo:

```
/
├── CONTEXT.md
├── docs/
│   ├── PRD.md
│   └── adr/
│       ├── 0001-guardrails-validate-declared-contract.md
│       └── … (through 0012)
└── docs/agents/            ← this config
```

## Use the glossary's vocabulary

When your output names a domain concept (in an issue title, a refactor proposal, a hypothesis, a test name), use the term as defined in `CONTEXT.md`. Don't drift to synonyms the glossary explicitly avoids — e.g. use **Telemetry Contract** (not manifest/spec), **Preflight/Pipeline Guardrail** (not linter/processor), **Pipeline Profile** (not preset), **Backend** (not APM/sink).

If the concept you need isn't in the glossary yet, that's a signal — either you're inventing language the project doesn't use (reconsider) or there's a real gap (note it for `/grill-with-docs`).

## Flag ADR conflicts

If your output contradicts an existing ADR, surface it explicitly rather than silently overriding:

> _Contradicts ADR-0006 (GitOps, not OpAMP) — but worth reopening because…_
