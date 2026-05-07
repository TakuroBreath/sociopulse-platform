# 00. Architecture Overview

This is the entry point for any agent working in `sociopulse-platform`. Read
this before opening an implementation plan. The other documents in this
directory go deeper on individual concerns; this one stitches them together.

The single source of truth for *what* the system does is the system-design
spec at `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md`. The
single source of truth for *how* the codebase is organised is this folder
(`docs/architecture/00..08`). When the spec and an architecture document
disagree, the architecture document wins for code-organisation questions and
the spec wins for behavioural ones.

## Bird's-eye View

СоциоПульс is a **modular monolith** written in Go (1.26.1). The whole
backend lives in a single Go module — `github.com/sociopulse/platform` — and
ships as a small set of independently deployable binaries that share the same
domain code. There is exactly one OLTP database (Postgres 16) and one OLAP
database (ClickHouse 23.x); business state crosses the two via NATS
JetStream. Telephony media (FreeSWITCH 1.10, RTP) lives outside Kubernetes
on dedicated VMs; everything else runs in a single Yandex Managed Kubernetes
cluster in `ru-central-1`.

The shape of the system is:

```
                      ┌─────────────────────────────────────────────┐
                      │                cmd/api                      │  full monolith
   browsers ───WSS────┤  http (gin) + ws + grpc clients + workers   │  (3 replicas)
                      │  imports every internal/<module>            │
                      └────────────┬─────────────┬──────────────────┘
                                   │             │
                       NATS JetStream            Postgres + ClickHouse + Redis
                                   │             │           │           │
                ┌──────────────────┴──┐   ┌──────┴────────┐  │  ┌────────┴───────┐
                │ cmd/telephony-bridge│   │ cmd/migrator  │  │  │ cmd/worker     │
                │ ESL ↔ NATS bridge   │   │ migrate up/dn │  │  │ asynq jobs     │
                │ (sidecar, in-k8s)   │   └───────────────┘  │  │ (3 replicas)   │
                └──────────┬──────────┘                       │  └────────────────┘
                           ESL                                │
                ┌──────────┴──────────┐    ┌──────────────────┴───────┐
                │  FreeSWITCH 1.10    │    │ cmd/recording-uploader   │
                │  on VM (out of k8s) │────│ systemd-unit on FS-VM    │
                │  /var/spool/...wav  │    │ ffmpeg + KMS + S3 PUT    │
                └─────────────────────┘    └──────────────────────────┘
                           ▲                          │
                       SIP-WSS                  gRPC mTLS Commit
                           │                          │
                       operator                       └─→ recording.RecordingService.Commit
                       browser                              (in cmd/api)
```

Two sidecars and a worker pool flank the main binary:

- **`cmd/telephony-bridge`** owns ESL connections to FreeSWITCH and
  bridges them to NATS. It is the only Go process allowed to speak ESL.
- **`cmd/recording-uploader`** runs as a systemd unit on each FS-VM. It
  watches `/var/spool/sociopulse/<tenant>/`, encrypts new `.wav` files
  with a KMS-wrapped DEK, uploads to S3, and finalises via the gRPC
  `RecordingService.Commit` exposed by `cmd/api`.
- **`cmd/worker`** runs background tasks (asynq queues): retry
  rescheduling, recording retention sweeps, audit-archive moves, async
  report generation, quota reconciliation, session cleanup, DNC import.

A few smaller binaries round out the surface:

- **`cmd/migrator`** runs Postgres migrations. Image used as a Helm
  pre-install / pre-upgrade hook. No domain code, no business logic.
- **`cmd/synthetic`** — standalone synthetic-monitoring canary that
  exercises a fixed login → ready → call → hangup path against a test
  tenant. Independent binary because deployment policy is different.
- **`cmd/status-page`** — read-only web page reading Alertmanager API to
  surface incident state. Independent so a sociopulse outage does not
  also take down the status page.

The choice of *modular monolith plus sidecars* (vs microservices) is locked
by **ADR-0004**. The choice of self-hosted FreeSWITCH outside Kubernetes is
locked by **ADR-0007**.

## Module List

Twelve domain modules live under `internal/`. Each owns one bounded context
and exposes its public surface via an `api/` subpackage. Cross-module Go
imports are restricted to those `api/` subpackages — depguard fails the
build otherwise (see `01-package-layout.md`).

| Module | Owns | Depends on (api/) |
|---|---|---|
| `auth` | Sessions, JWT issuance/validation, refresh-token rotation, TOTP, RBAC matrix, login-rate-limit, account lockout, password CRUD | `tenancy`, `audit` |
| `tenancy` | `tenants` + `tenant_settings`, per-tenant KEK lifecycle (Yandex KMS), envelope encryption, phone hashing (HMAC-SHA256+pepper), settings cache, S3 bucket provisioning | `audit` |
| `crm` | Projects, respondents, quotas, DNC, async CSV/XLSX import, deletion right (152-ФЗ §13.3) | `tenancy`, `audit` |
| `surveys` | Survey definitions and immutable versions, JSON-Schema validation, graph validation, DSL evaluator, runtime (next-node + answer validation), versioning | `tenancy`, `audit` |
| `telephony` | ESL wire protocol, NATS bridge, originate / hangup / mixmonitor commands, channel events, idempotency, FreeSWITCH directory provider | `tenancy` |
| `dialer` | OperatorFSM, CallQueue (Redis ZSET), RDDGenerator, Router (NATS abstraction), LineCapacityTracker, WorkingHoursChecker, RetryOrchestrator, retry orchestration | `crm`, `surveys`, `telephony`, `tenancy`, `audit` |
| `realtime` | WebSocket Hub, presence (Redis), subscriptions, listen-in (silent/whisper/barge), force commands, NATS dispatcher | `auth`, `tenancy`, `dialer`, `audit` |
| `recording` | Recording metadata CRUD, gRPC Commit, Search, integrity verification, S3 streaming with envelope decrypt, presigned URLs, retention plan | `tenancy`, `audit` |
| `analytics` | NATS → ClickHouse ingest pipeline (durable, batched, dedup), MetricsQuery surface (calls, operator state, region progress, hourly, comparisons) | `tenancy` |
| `reports` | Six preset reports + custom reports, async asynq jobs, XLSX/CSV/PDF rendering, presigned downloads | `tenancy`, `analytics`, `audit` |
| `billing` | CostCalculator (telecom + wages + storage), tariff store, monthly breakdowns, project margins, finance dashboard | `tenancy`, `analytics` |
| `audit` | Append-only audit log: `Write(ctx, action, payload)` + retention pass | (no internal deps) |

The `audit` module is the **leaf**: every other module depends on it (or
imports a tiny adapter), but it imports no domain modules itself. The
`tenancy` module is the **trunk**: every cross-tenant guarantee bottoms out
in it (KMS, settings, phone hashing). These two facts shape the dependency
graph in the next section.

There is also a small non-domain module `internal/gateway/` (not in the
table above) hosting cross-cutting HTTP middleware: request ID, recovery,
zap logger bridge, JWT extraction, RLS context binding, idempotency, rate
limit. It exposes no business surface — only middleware and a route
registry — so it does not appear in `02-module-contracts.md`. Plan 02
builds it.

## Dependency Graph

Arrows go from importer to importee. Every cross-module edge crosses an
`api/` subpackage; no edge bypasses one. There are no cycles.

```
                ┌──────────────────────────────────────────┐
                │                cmd/api                   │
                └──┬──┬──┬──┬──┬──┬──┬──┬──┬──┬──┬──┬──────┘
                   │  │  │  │  │  │  │  │  │  │  │  │
                   ▼  ▼  ▼  ▼  ▼  ▼  ▼  ▼  ▼  ▼  ▼  ▼
        gateway  reports  realtime  dialer  surveys  recording  billing  analytics  crm  auth  tenancy  audit
              │      │       │        │       │         │         │         │       │     │      │       ▲
              │      │       │        │       │         │         │         │       │     │      │       │
              │      │       │        ├───────┼─────────┼─────────┼─────────┤       │     │      │       │
              │      │       │        │       │         │         │         │       │     │      │       │
              │      └──> analytics ──┘       │         │         │         │       │     │      │       │
              │      └──> tenancy             │         │         │         │       │     │      │       │
              │      └──> audit ──────────────┴─────────┴─────────┴─────────┴───────┴─────┴──────┴───────┘
              │
              └──> auth ──> tenancy
              └──> all module HTTP routers (no cycles)

   realtime  ──> auth ──> tenancy ──> audit
   realtime  ──> dialer ──> {crm, surveys, telephony, tenancy, audit}
   recording ──> tenancy ──> audit
   crm       ──> tenancy ──> audit
   surveys   ──> tenancy ──> audit
   billing   ──> tenancy, analytics
   reports   ──> tenancy, analytics, audit
   analytics ──> tenancy
   telephony ──> tenancy
```

The same picture, simplified to the trunk and leaf:

```
                       audit  (leaf — every module writes here, no imports)
                         ▲
       ┌─────────┬───────┴──────┬──────────┬─────────┬─────────┐
       │         │              │          │         │         │
     auth      crm           surveys    recording  realtime  reports …
       │         │              │          │         │
       └─────────┴───────┬──────┴──────────┴─────────┘
                         ▼
                      tenancy  (trunk — every module reads tenant + settings + KMS)
                         │
                         ▼
                      Yandex KMS
```

If you find yourself wanting to add an import that creates a cycle (e.g.
`tenancy → auth`), STOP. Either refactor the call into a shared
`pkg/`-level utility, or pass the dependency in via composition root
(`cmd/<binary>/main.go`) using the smaller-of-the-two interface.

## Binary Mapping

Modules are wired into binaries from each binary's `main.go` composition
root. The same `internal/<module>/service/...` constructors are reused
across binaries — only the set of modules differs.

| Binary | Modules wired | Notes |
|---|---|---|
| `cmd/api` | All twelve domain modules | The full monolith. Serves HTTP (gin), WS, gRPC servers (recording, telephony control). Runs the NATS subscribers for realtime, analytics ingest, billing hooks. |
| `cmd/worker` | `auth`, `tenancy`, `crm`, `recording`, `billing`, `audit` | asynq consumers + scheduled jobs (retry rescheduling, retention pass, deletion-right purge, archive pass). No HTTP listener except `/healthz` + `/metrics`. |
| `cmd/migrator` | None | Reads `migrations/` and applies them via `golang-migrate`. No domain code. Used as Helm pre-install / pre-upgrade hook. |
| `cmd/telephony-bridge` | `telephony` only | Owns the ESL pool. Subscribes to `tenant.<t>.telephony.cmd.>` and publishes `tenant.<t>.telephony.event.>`. mTLS to FS, mTLS to NATS. |
| `cmd/recording-uploader` | The gRPC client of `recording` (no service code) | Deployed as systemd unit on each FS-VM. Reads filesystem, encrypts, PUTs to S3, calls `RecordingService.Commit` over gRPC mTLS. |
| `cmd/synthetic` | None — standalone client | Hits the public REST API as a synthetic test tenant on a schedule. |
| `cmd/status-page` | None — standalone Alertmanager-API reader | Renders a small HTML page. Decoupled from the rest so a platform outage does not silence the status page. |

The `internal/modules` package (built in Plan 02) defines a `Module`
interface that every domain module satisfies. The composition root then
calls `module.Register(deps)` once for each module that participates in
that binary. This keeps `cmd/api/main.go` flat and testable.

## External Dependencies

Each module talks to a small, well-defined set of out-of-process services.
The matrix below is the source of truth for network policies, IAM scopes
and chaos-test expectations.

| Module | Postgres | Redis (Valkey) | NATS JetStream | ClickHouse | S3 | Yandex KMS | FreeSWITCH (ESL) |
|---|---|---|---|---|---|---|---|
| `auth` | RW (`users`, `user_sessions`) | RW (refresh-token rotation, lockout, sliding-window rate limit) | (publishes `audit.event` only) | — | — | (via tenancy) | — |
| `tenancy` | RW (`tenants`, `tenant_settings`, `tenancy_admin` BYPASSRLS) | RW (settings cache) | pub `tenant.<id>.settings.updated`, sub `tenant.<id>.settings.updated` | — | RW (per-tenant bucket provisioning) | RW (KEK lifecycle, GenerateDataKey, Encrypt/Decrypt) | — |
| `crm` | RW (`projects`, `project_quotas`, `respondents`, `project_dnc`) | RW (quota counters, import progress) | pub `crm.project.*`, `crm.respondent.*` | — | — | (via tenancy phone hashing) | — |
| `surveys` | RW (`surveys`, `survey_versions`) | RW (active-version cache) | pub `surveys.version.*` | — | — | — | — |
| `telephony` | RO (`trunks`, `fs_nodes`, `tenants`) | RW (idempotency, ESL connection pool stats) | pub `tenant.<t>.telephony.event.>`; sub `tenant.<t>.telephony.cmd.>` | — | — | — | RW (ESL TLS) |
| `dialer` | RW (`calls`, `call_answers`, `respondents` retry windows) | RW (call queue ZSET, FSM state hash, line capacity, RDD throttle) | pub `tenant.<t>.dialer.op.*.state`, `dialer.call.*.lifecycle`; sub `telephony.event.*` | — | — | — | (via telephony) |
| `realtime` | RO (presence reconciliation) | RW (presence, sub-counts) | sub `tenant.>` (everything tenant-scoped) | — | — | — | — |
| `recording` | RW (`call_recordings`, `recording_retention`) | — | pub `tenant.<t>.recording.uploaded` | — | RW (audio + DEK objects, presigned URLs, retention transitions) | (via tenancy decrypt) | — |
| `analytics` | RO (small joins for region progress vs `project_quotas`) | RW (query cache, dedup LRU mirror) | sub `analytics.event.*`, `dialer.call.finalized`, `operator.state.changed`, `recording.uploaded` | RW (event tables, materialised views) | — | — | — |
| `reports` | RW (`reports_jobs`) | RW (job state cache) | pub `reports.report.ready` | RO (query MVs) | RW (rendered file objects, presigned URLs) | — | — |
| `billing` | RW (`call_costs`, `tariff_audit`) | — | sub `dialer.call.finalized` | RO (long-window rollups) | — | — | — |
| `audit` | RW (`audit_log`) | — | sub `tenant.<t>.audit.event` (collector) | (no direct — analytics duplicates from this stream) | RW (cold-tier archive after 1 year) | — | — |

A few invariants flow out of this matrix:

- The **only** module touching FreeSWITCH ESL is `telephony`. Everything
  else routes through NATS subjects.
- The **only** module talking to Yandex KMS is `tenancy`. Other modules
  call `tenancy.api.KMSResolver.Encrypt/Decrypt` and never see the SDK.
- The **only** module bypassing RLS is `tenancy` (via the
  `tenancy_admin` BYPASSRLS Postgres role) and `audit` for the
  Service-Owner cross-tenant feed. All other Postgres access goes
  through the RLS-bound `app` role and `SET LOCAL app.tenant_id` per
  transaction (ADR-0006).
- The `recording-uploader` sidecar is the **only** writer to
  `sociopulse-recordings-<tenant>` S3 buckets. `cmd/api` reads but
  never writes recording objects.

## Where to Read Next

| You want to know… | Read |
|---|---|
| Where a particular `.go` file should live, and why | `01-package-layout.md` |
| The exact public interface of module X | `02-module-contracts.md` § X |
| How a sentinel error is named, wrapped, surfaced over HTTP/gRPC | `03-error-handling.md` |
| What "good test" means here, and what `goleak`, `paralleltest` enforce | `04-testing-strategy.md` |
| The order in which YAML, env, Lockbox layers resolve | `05-configuration.md` |
| The shape of zap fields, Prometheus metrics, OTel span names | `06-observability.md` |
| The full distilled Go standard the codebase follows | `07-go-coding-standards.md` |
| The Red-Green-Refactor playbook | `08-tdd-discipline.md` |
| The behaviour the system must implement | `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md` |

## Cross-references

- Spec §4–§5 — high-level architecture and module decomposition source.
- Spec §10.2 — canonical NATS subject naming. `02-module-contracts.md`
  must match this exactly.
- Spec §17 — test pyramid, coverage targets, security testing surface.
- Spec §22 — ADR registry. ADR-0004 (modular monolith), ADR-0007 (FS
  outside k8s), ADR-0011 (NATS JetStream), ADR-0010 (PG + CH split),
  ADR-0014 (gin), ADR-0015 (TDD).
