# References — internal curated reading list

> **Goal**: snap each plan to authoritative external sources, so subagents (and humans) don't re-derive what's already known. Per-plan reference files are loaded into implementer subagent prompts at dispatch time.

## Format

Each file follows this structure:

```markdown
# Plan NN — <topic> references

## Canonical specs (must-read)
- [Title](URL) — one-line value statement.
  Note: what to take, what to skip, gotchas.

## Reference implementations
- [Project name](URL) — what they do well.
  Files of interest: <path>, <path>.

## Production lessons (blog posts, talks)
- [Title](URL) — what's the lesson.

## Russian-specific (152-ФЗ, Yandex Cloud, RU integrations)
- [Title](URL) — RU-only context.

## Gotchas (do-not-do list)
- Specific anti-patterns we've decided to avoid + why.

## Open questions
- What we still don't know and need to learn empirically.
```

## Workflow

1. **Before starting a plan**: read its `plan-NN-<topic>.md`. Skim "Canonical specs" + "Gotchas" sections at minimum.
2. **When dispatching an implementer subagent**: include the file path in the prompt. Subagents read it before writing code.
3. **After completing a plan**: update the file with what we actually learned (especially "Production lessons" and "Gotchas" sections). This is the institutional memory.
4. **Cross-cutting**: things that apply to >1 plan (152-ФЗ, Yandex KMS quirks, Go skill discipline) live in `COMMON.md`.

## Coverage status

| Plan | Status | File |
|---|---|---|
| Plan 00, 00a, 00b | retroactive — TBD | — |
| Plan 02 — cmd/api skeleton | retroactive — TBD | — |
| Plan 03 — database | retroactive — TBD | — |
| Plan 04 — tenancy | retroactive — TBD | `plan-04-tenancy.md` |
| Plan 05 — auth | **shipped (v0.0.7)** | [`plan-05-auth.md`](plan-05-auth.md) |
| Plan 06 — CRM | **shipped (v0.0.8)** | [`plan-06-crm.md`](plan-06-crm.md) |
| Plan 07 — surveys | **shipped (v0.0.9)** | [`plan-07-surveys.md`](plan-07-surveys.md) |
| Plan 08 — FreeSWITCH cluster | TBD (sociopulse-infra repo) | — |
| Plan 09 — telephony-bridge | **shipped (v0.0.10)** | [`plan-09-telephony-bridge.md`](plan-09-telephony-bridge.md) |
| Plan 10 — dialer | **shipped (v0.0.11)** | [`plan-10-dialer.md`](plan-10-dialer.md) |
| Plan 11 — realtime | TBD | — |
| Plan 12 — recording | TBD | — |
| Plan 13 — analytics+reports | TBD | — |
| Plan 14 — billing | TBD | — |
| Plan 20 — observability | TBD | — |

Cross-cutting: [`COMMON.md`](COMMON.md).

## Caveats

- **Links rot**. When you find a 404, replace with the closest current equivalent and note the original was lost.
- **My (Claude's) curation bias**: I weight authoritative specs (RFC, OWASP) higher than blog posts. For production-realism, prioritize blog posts + ClueCon talks.
- **WebFetch'd at curation time**: when I curate a file, I verify URLs are live + note training-data version vs current. If a library API has changed since my training, I'll flag it.

## Use the runtime tools first

For **library APIs** — don't guess from training. Use `context7` MCP:
1. `mcp__plugin_context7_context7__resolve-library-id` to find the lib.
2. `mcp__plugin_context7_context7__query-docs` for the current doc.

For **specific errors / unknown territory** — `WebSearch`. Stack Overflow / Habr / GitHub issues are usually more current than my training.

For **specific URLs** — `WebFetch`.

The links in per-plan files are **starting points and rationales**, not the source of truth. Source of truth = current docs, fetched at use-time.

## 152-ФЗ stance

**Functional security, not compliance theater.** No external audit planned in v1 scope. We do good crypto and isolation because they're good engineering, not because of regulators. See `COMMON.md` § Compliance posture.
