# Plan 20 — Observability Foundation

> **Scope reality.** Plan 20 has 7 tasks; **only Task 1 is in scope for
> `sociopulse-platform`** (this repo). Tasks 2-7 (Prometheus alert rules,
> Grafana dashboards as code, `cmd/synthetic` canary, `cmd/status-page`,
> Alertmanager routing, Helm chart) are explicitly deferred to Phase 2
> and belong to `sociopulse-infra` (Yandex Cloud / MKS / kube-prometheus-stack).
> See the plan's opening note at
> [`docs/superpowers/plans/2026-05-06-20-observability-foundation.md`](../superpowers/plans/2026-05-06-20-observability-foundation.md).

## 1. Canonical specs

- **Master plan:** [`docs/superpowers/plans/2026-05-06-20-observability-foundation.md`](../superpowers/plans/2026-05-06-20-observability-foundation.md)
  - § "Task 1: Severity matrix + on-call basics in `docs/runbooks/README.md`" — verbatim content for the deliverable.
  - § Architecture — eventual shape (Grafonnet / PrometheusRule CR / Alertmanager routing / `cmd/synthetic` / `cmd/status-page`) lives in Phase 2 (`sociopulse-infra`).
- **System-design spec coverage:** [`docs/superpowers/specs/2026-05-06-sociopulse-system-design.md`](../superpowers/specs/2026-05-06-sociopulse-system-design.md)
  - §15 (observability stack — full)
  - §13.7 (incident response)
  - §NFR-2 (availability monitoring)
  - §NFR-10 (logging / observability principles)
- **Cross-cutting:** [`docs/references/COMMON.md`](COMMON.md) — compliance posture (152-ФЗ pragmatic stance — status page is in-house, no third-party SaaS).
- **Domain glossary:** [`CONTEXT.md`](../../CONTEXT.md) — incident terminology (operator, FSM, tenant, recording, audit log) is canonical; use it in runbook prose.

## 2. Phase-1 deliverable (this repo)

A single markdown file: `docs/runbooks/README.md`. Contents per the plan:

| Section | Purpose |
|---|---|
| Severity matrix (P1/P2/P3) | Defines what an incident is — without this nothing else hangs together. |
| Принципы | Incident-command basics (one IC, one channel, rollback-first, rest-rule). |
| Post-mortem | Pointer to template; blameless culture statement. |
| Список runbooks | Pre-declared filenames for Phase-2 runbooks (10 items) — links resolve to 404 until Phase 2 lands; intentional. |

**No code involved.** The deliverable is a documentation artefact only; no Go files, no migrations, no CI changes beyond an additional file under `docs/`. The plan itself spec's the content verbatim in markdown.

## 3. Phase-2 deferrals (NOT in this repo)

Tasks 2-7 of Plan 20 stay deferred until `sociopulse-infra` has the kube-prometheus-stack Helm chart deployed:

| Task | Repo | Notes |
|---|---|---|
| Task 2 — Top-10 runbooks (one per file) | `sociopulse-infra` | Each runbook references real `kubectl`/`fs_cli`/SQL commands — only meaningful after the production environment is real. |
| Task 3 — Prometheus alert rules (YAML → `PrometheusRule` CR) | `sociopulse-infra` | Needs kube-prometheus-stack + Alertmanager routing. |
| Task 4 — Grafana dashboards as code (Grafonnet) | `sociopulse-infra` | `make dashboards` JSONNet pipeline; Helm `dashboard-sidecar`. |
| Task 5 — `cmd/synthetic` (canary monitor) | could land in platform | Implemented as new `cmd/synthetic/` Go binary; deferred because there's no prod endpoint to canary yet. |
| Task 6 — `cmd/status-page` (in-house static page) | could land in platform | Implemented as new `cmd/status-page/` Go binary; deferred for same reason. |
| Task 7 — Alertmanager routing config | `sociopulse-infra` | Helm-templated `alertmanager-config.yaml`. |

`cmd/synthetic` and `cmd/status-page` are Go binaries that could technically land here, but they consume production endpoints and Alertmanager routing — without Phase-2 infra they would be dead code.

## 4. Production lessons (post-execution 2026-05-17)

- **Pragmatic close-out for partial plans.** Plan 20 is a 7-task plan where 6 tasks are infra-side. Trying to "complete Plan 20" in the platform repo is wrong-shape. The plan's own opening note carved out Task 1 as the platform-repo deliverable explicitly — read the plan's prologue before estimating scope.
- **Docs-only plans don't need the full subagent ceremony.** The pipeline skill prescribes implementer-subagent + 2-stage review for code-heavy tasks. For a single markdown file with verbatim spec content, that's overkill; write directly, commit, close out properly (PROJECT_STATUS + tag + CI watch). The skill's own anti-pattern list says "doc-edit → handle directly" — Plan 20 Task 1 is the in-ledger version of that rule.
- **Stub runbook links are intentional, not bugs.** The README lists 10 runbook filenames (`api-down.md`, `fs-vm-down.md`, etc.) that don't exist yet. Future Phase-2 work in `sociopulse-infra` will create them. Linking-but-not-creating is the right shape: lets reviewers see the planned navigation, and Phase-2 PRs just fill in the leaves.
- **No tag for Phase-2 portion.** We tag this Phase-1 close-out (`v0.0.29-runbook-readme`) but DO NOT tag the plan as "Plan 20 done". Future Phase-2 work in `sociopulse-infra` may produce its own tags there; this repo's contribution to Plan 20 is closed.

## 5. Open questions

- **Slack channel names** (`#incidents`, `#alerts`) — assumed; teams may not exist yet. Phase-2 update should reconcile with actual workspace.
- **Post-mortem template (`docs/postmortems/template.md`)** — pointer added; the file itself is a Phase-2 deliverable (no incidents yet to post-mortem). Not blocking.
- **On-call rotation roster** — out of scope per the plan's "What's intentionally out of scope" list (operational decision, not architecture).
