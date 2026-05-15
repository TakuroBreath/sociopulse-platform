# Module Graph — sociopulse-platform

> **Living document.** Created 2026-05-15 at Plan 13.3 close-out. Future plans append rows as they ship new publishers / consumers.

Cross-module communication in this repo flows almost exclusively through NATS subjects published via [`pkg/outbox`](../../pkg/outbox/) (durable; same-Tx with the source row) and consumed via NATS JetStream subscribers. Direct in-process function calls between modules are mediated by narrow interfaces declared in `internal/<module>/api/` (see [`02-module-contracts.md`](02-module-contracts.md) for the per-module surface).

This file is the **canonical events catalog** — one table row per `(subject, publisher, consumer)` triple. Use it to answer:

- "Who emits `tenant.<t>.crm.project.status_changed`?"
- "Who consumes `analytics.event.calls`?"
- "When was `tenant.<t>.audit.event` introduced?"

Subject constants live in `internal/<module>/api/events.go`. The wildcard form (`tenant.*.<module>.<event>`) is what subscribers register; the per-tenant form (`tenant.<t>.<module>.<event>`) is what publishers emit at runtime.

---

## Per-tenant subjects (form: `tenant.<t>.<module>.<event>`)

| Subject | Publisher | Consumer | Plan |
|---|---|---|---|
| `tenant.<t>.auth.user.deleted` | `internal/auth/service` (Plan 11.4) | `internal/realtime` (cache invalidator) | 11.4 |
| `tenant.<t>.crm.project.created` | `internal/crm/service` | `internal/realtime` | 06 |
| `tenant.<t>.crm.project.updated` | `internal/crm/service` | `internal/realtime` | 06 |
| `tenant.<t>.crm.project.status_changed` | `internal/crm/service` | `internal/realtime` (cache invalidator), `internal/dialer` (pause-on-status-change) | 06 / 11 |
| `tenant.<t>.crm.respondents.import.started` | `internal/crm/import` | `internal/realtime` (user-scoped notify) | 06 |
| `tenant.<t>.crm.respondents.import.progress` | `internal/crm/import` | `internal/realtime` | 06 |
| `tenant.<t>.crm.respondents.import.finished` | `internal/crm/import` | `internal/realtime` | 06 |
| `tenant.<t>.crm.respondents.import.failed` | `internal/crm/import` | `internal/realtime` | 06 |
| `tenant.<t>.crm.respondent.deletion_requested` | `internal/crm/service` | `internal/dialer` (purge from active dialing) | 06 |
| `tenant.<t>.crm.quota.incremented` | `internal/crm/service` | `internal/realtime` | 06 |
| `tenant.<t>.crm.dnc.added` | `internal/crm/service` | `internal/dialer` (DNC reload) | 06 |
| `tenant.<t>.dialer.op.<op_id>.state` | `internal/dialer/fsm` | `internal/realtime`, `internal/analytics/ingest` (via wildcard) | 10 |
| `tenant.<t>.dialer.call.finalized` | `internal/dialer/fsm` | `internal/analytics/ingest` (via wildcard), `internal/realtime` | 13.2 |
| `tenant.<t>.dialer.call.<call_id>.lifecycle` | `internal/dialer/fsm` | `internal/realtime` (per-call subscribers) | 10 |
| `tenant.<t>.telephony.event.<call_id>.<verb>` | `cmd/telephony-bridge` (Plan 09) | `internal/dialer/transport/nats.CallEventSubscriber` (Plan 13.2.5 Task 2) | 09 / 13.2.5 |
| `tenant.<t>.telephony.bridge.health` | `cmd/telephony-bridge` | `internal/realtime` | 09 |
| `tenant.<t>.recording.uploaded` | `internal/recording/service` (Plan 12.1) | `internal/analytics/ingest` (via wildcard); future: `internal/notify` | 12.1 / 13.2 |
| `tenant.<t>.recording.call.deleted` | `internal/recording/worker.retention_pass` (Plan 12.4) | `internal/realtime` (cache invalidator) | 12.4 |
| `tenant.<t>.surveys.version.saved` | `internal/surveys/service` | `internal/realtime` | 07 |
| `tenant.<t>.surveys.version.activated` | `internal/surveys/service` | `internal/dialer` (reload survey), `internal/realtime` | 07 |
| `tenant.<t>.settings.updated` | `internal/tenancy/service` | `pkg/tenant_settings/cache` (invalidator) | 04 |
| `tenant.<t>.notify.user.<user_id>` | various (user-scoped notifications) | `internal/realtime` (WS gateway) | 11 |
| **`tenant.<t>.reports.report.ready`** | **`internal/reports/service` (Plan 13.3 Task 5/6)** | **`internal/realtime`/notify (Plan 11 territory; subscriber may or may not exist yet)** | **13.3** |
| **`tenant.<t>.audit.event`** | **`internal/reports/service.AuditEmitter` (Plan 13.3 Task 3); future: any module needing auditing today** | **Future audit Service (Plan 03 Task 7) — currently `internal/audit/module.go` is a no-op stub; nothing subscribes yet** | **13.3 (publisher); 03 Task 7 (consumer)** |

## Cross-tenant subjects (no tenant in path)

| Subject | Publisher | Consumer | Plan |
|---|---|---|---|
| `tenant.<t>.created` | `internal/tenancy/service` | platform-ops (audit only) | 04 |
| `tenant.<t>.suspended` | `internal/tenancy/service` | platform-ops | 04 |
| `tenant.<t>.resumed` | `internal/tenancy/service` | platform-ops | 04 |
| `tenant.<t>.archived` | `internal/tenancy/service` | platform-ops | 04 |
| `analytics.event.calls` | `internal/dialer/fsm` (dual-publish at `EventStatusSubmitted`) | `internal/analytics/service.IngestPipeline` | 13.2 |
| `analytics.event.operator_state` | `internal/dialer/fsm` (dual-publish at `appendStateLogAndOutbox`) | `internal/analytics/service.IngestPipeline` | 13.2 |

---

## Direct in-process consumption (non-NATS)

Reports + analytics + realtime consume read-side aggregates directly (declared in `internal/<module>/api/interfaces.go`). These are NOT events; they're synchronous reads.

| Consumer | Port | Provider |
|---|---|---|
| `internal/reports/service` | `analytics.ServiceRO` (= `MetricsQuery + Overview`) | `internal/analytics/service` |
| `internal/analytics/service` | `crm.ProjectService.GetProgress` | `internal/crm/service` |
| `internal/realtime/cache.ProjectResolver` | `crm.ProjectService.ResolveTenant` (Plan 13.2.5 Task 1) | `internal/crm/service` |
| `internal/recording/transport/grpc` | `tenancy.KMSResolver`, `tenancy.PhoneHasher` | `internal/tenancy/service` |
| `internal/dialer/service` | `surveys.SurveyService.GetActive` | `internal/surveys/service` |

---

## How to extend this file

When a plan introduces a new subject:

1. Add the constant under `internal/<module>/api/events.go` with a `Subject<Verb>` name.
2. Add a row to the appropriate table above with publisher / consumer / plan number.
3. Subjects intended for cross-module wildcard subscription (`tenant.*.<module>.<event>`) should also be exposed as `Subject<Verb>Wildcard` for the subscriber side (mirrors `SubjectRecordingUploadedWildcard`).

The depguard rules in `.golangci.yml` keep cross-module imports honest; this file keeps cross-module **events** honest.
