# Domain Docs

How the engineering skills should consume this repo's domain documentation when exploring the codebase.

## Layout: single-context

This repo follows the single-context layout — one `CONTEXT.md` at the repo root, one `docs/adr/` directory:

```
/
├── CONTEXT.md
├── docs/
│   ├── adr/
│   │   ├── 0001-...md
│   │   └── 0002-...md
│   └── agents/
└── ...source/
```

Each of the three СоциоПульс repos (`sociopulse-platform`, `sociopulse-web`, `sociopulse-infra`) has its own single-context layout — they describe different domain languages (backend services, UI components, infrastructure resources) and don't share a glossary.

## Before exploring, read these

- **`CONTEXT.md`** at the repo root.
- **`docs/adr/`** — read ADRs that touch the area you're about to work in.

If any of these files don't exist yet, **proceed silently**. Don't flag their absence; don't suggest creating them upfront. The producer skill (`/grill-with-docs`) creates them lazily when terms or decisions actually get resolved during conversation.

## Use the glossary's vocabulary

When your output names a domain concept (in an issue title, a refactor proposal, a hypothesis, a test name), use the term as defined in `CONTEXT.md`. Don't drift to synonyms the glossary explicitly avoids.

If the concept you need isn't in the glossary yet, that's a signal — either you're inventing language the project doesn't use (reconsider) or there's a real gap (note it for `/grill-with-docs`).

## Flag ADR conflicts

If your output contradicts an existing ADR, surface it explicitly rather than silently overriding:

> _Contradicts ADR-0007 (...) — but worth reopening because…_

## Cross-repo references

When working in this repo and needing context that lives in a sibling repo:

- Read the sibling's `CONTEXT.md` if relevant (e.g. backend reading `sociopulse-web/CONTEXT.md` to understand a UI flow that defines an API contract).
- Don't copy glossary entries between repos — link instead. Each repo owns its own vocabulary; cross-cutting concepts get a brief reference in each repo's `CONTEXT.md` pointing to the canonical definition.

## Existing artefacts

Project documentation lives in this repo at [`docs/superpowers/`](../superpowers/):

- `specs/2026-05-06-sociopulse-system-design.md` — the system design spec (157 KB, 22 sections + 13 ADRs + 2 appendices).
- `plans/` — 22 implementation plans (00, 00a, 01-20).
- `reviews/2026-05-06-architecture-and-plans-review.md` — architecture & plans review with Phase 1 / Phase 2 execution order.

`CONTEXT.md` and `docs/adr/` will be seeded during execution of **Plan 00a — Architecture Foundation** (Tasks 1-3). Until then, this directory and the spec are the source of truth.

Sibling repos (`sociopulse-web`, `sociopulse-infra`) consume these documents via GitHub URLs (https://github.com/TakuroBreath/sociopulse-platform/blob/main/docs/superpowers/) — they don't carry copies.
