# Plan 13.2 — Analytics Ingest + Queries — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **Plan ID:** 13.2
> **Predecessor:** Plan 13.1 (`v0.0.21-analytics-clickhouse-schema`) — schema foundation
> **Successor:** Plan 13.3 — reports module (XLSX/CSV/PDF + asynq)
> **References (read FIRST):** `docs/references/plan-13-analytics.md`
> **Architecture:** `docs/architecture/00-overview.md` § analytics, `docs/architecture/02-module-contracts.md` § analytics, `docs/architecture/04-testing-strategy.md`, `docs/architecture/analytics-mv.md` (Plan 13.1 Task 4)
> **ADRs in scope:** ADR-0010 (Postgres + ClickHouse), ADR-0011 (NATS over Kafka), ADR-0013 (Viper config), ADR-0015 (TDD mandatory)

**Goal:** Wire the durable JetStream → ClickHouse ingest pipeline, the typed `MetricsQuery` read surface with Redis cache, and 5 HTTP admin endpoints under `/api/analytics/*`. Closes the read+write halves of Plan 13's analytics block; reports (Plan 13.3) consume `MetricsQuery` next.

**Architecture (2-3 sentences):** Dialer FSM commits + recording.Commit produce additional outbox rows on three subjects (`analytics.event.calls` + `analytics.event.operator_state` cross-tenant, `tenant.<t>.recording.uploaded` per-tenant). A new long-running `analytics.IngestPipeline` daemon in `cmd/worker` subscribes to all three, accumulates per-subject batches (10 000 rows or 5 s — whichever comes first), dedups by `event_id` via a per-subject LRU, and flushes via `clickhouse-go/v2`'s native `PrepareBatch → Append → Send`. `cmd/api` mounts `analytics.MetricsQuery` (5 typed methods reading the AggregatingMergeTree MVs via `sumMerge`/`uniqMerge`) behind a Redis read-through cache keyed by `analytics:{tenant}:{q_hash}` with 30 s short-window / 5 min long-window TTLs, exposed as 5 `GET /api/analytics/*` admin endpoints.

**Tech Stack:**
- ClickHouse client: `github.com/ClickHouse/clickhouse-go/v2` (native protocol, NOT `database/sql`). Connection via `clickhouse.Open(&clickhouse.Options{Addr, Auth, Settings, Compression: lz4})`.
- NATS: existing `pkg/eventbus.NATSSubscriber` (JetStream push consumer, `AckExplicit`, `DeliverNew`, `MaxAckPending=1024`).
- Outbox: existing `pkg/outbox.PostgresWriter` + relay (Plan 03 Task 7).
- Redis: existing `redis.UniversalClient` from `Deps.Redis` (auth/dialer already use it).
- HTTP: existing `*gin.Engine` from `Deps.HTTPRouter`; query params bound via `c.ShouldBindQuery`.
- Tests: `testcontainers-go/modules/clickhouse:24.8` (already wired in Plan 13.1) + embedded NATS via `pkg/eventbus/helpers_test.go::startEmbeddedJetStream`. `testify` for assertions, `goleak.VerifyTestMain` per package.

---

## System design framing (per `engineering:system-design`)

### 1. Requirements

**Functional:**
- Ingest 3 NATS subjects → 3 CH source tables: `events_calls`, `events_operator_state`, `events_recording_uploaded`.
- Expose 5 typed `MetricsQuery` methods: `Calls`, `OperatorState`, `RegionProgress`, `Hourly`, `OperatorComparisons` + `Overview` aggregate.
- Read-through Redis cache with `analytics:{tenant_id}:{q_hash}` key.
- 5 HTTP `GET /api/analytics/*` endpoints, JWT-auth, tenant scoped.

**Non-functional (load model from master spec):**
- 30 tenants × 50k calls/day → 1.5M call events/day → ~17/s steady, ~100/s peak.
- Plus operator state transitions ~5× → ~85/s peak.
- CH ingest target: sustain 200/s combined (2× peak headroom) without backpressure.
- Dashboard p99 latency: < 500 ms with warm cache, < 2 s on cold cache.
- Drain SLA: ≤ 5 s after `ctx.Done` (all buffers flushed).

**Constraints:**
- MUST NOT block dialer FSM (publish via outbox row, NOT direct NATS publish).
- MUST dedup on `event_id` — outbox relay may double-publish on transient failure (at-least-once semantics).
- depguard: `clickhouse-go/v2` imports allowed in `internal/analytics/*` + `cmd/migrator` (already allow-listed); NOT in any other module.

### 2. High-level architecture

```
  ┌────────────┐                                          ┌──────────────────┐
  │  dialer    │ Tx { calls.update + audit + outbox×N }   │   analytics      │
  │  FSM       ├──────────────────────────────┐           │  IngestPipeline  │
  │  (audit.go)│                              │           │ (cmd/worker)     │
  └────────────┘                              ▼           │                  │
                                  ┌────────────────────┐  │   ┌─────────┐    │
  ┌────────────┐ Tx { rec.commit  │   event_outbox     │  │   │ batch   │    │
  │ recording  │   + audit        │   (Postgres)       │  │   │ buffers │    │
  │ service    ├──────────────────┤                    │  │   │ × 3     │    │
  └────────────┘                  └─────────┬──────────┘  │   └────┬────┘    │
                                            │             │        │         │
                                            ▼             │        ▼         │
                                  ┌────────────────────┐  │   ┌─────────┐    │
                                  │  outbox.Relay      │  │   │ dedup   │    │
                                  │  (cmd/api)         │  │   │ LRU×3   │    │
                                  └─────────┬──────────┘  │   └────┬────┘    │
                                            │             │        │         │
                                            ▼             │        ▼         │
                                  ┌────────────────────┐  │   ┌──────────┐   │
                                  │  NATS JetStream    │──┼──▶│ CH batch │   │
                                  │  (durable subs)    │  │   │ Send×N/s │   │
                                  └────────────────────┘  │   └────┬─────┘   │
                                                          └────────┼─────────┘
                                                                   ▼
                                                          ┌────────────────┐
                                                          │  ClickHouse    │
                                                          │ events_calls   │
                                                          │ events_op_st   │
                                                          │ events_rec_upl │
                                                          └────────┬───────┘
                                                                   │
                                       MV (Plan 13.1):             │
                                       ┌───────────────────────────┴─┐
                                       ▼                             ▼
                            mv_calls_hourly_state          mv_operator_kpi_daily_state
                            mv_quotas_progress_state       (read via canonical VIEW)
                                       │                             │
                                       └──────────────┬──────────────┘
                                                      │
                                                      ▼
                                          ┌─────────────────────┐
                                          │  MetricsQuery       │
                                          │  (5 methods + cache)│
                                          └──────────┬──────────┘
                                                     ▼
                                          ┌─────────────────────┐
                                          │ Redis (read-thru)   │
                                          │ analytics:{t}:{h}   │
                                          └──────────┬──────────┘
                                                     ▼
                                          ┌─────────────────────┐
                                          │ 5× GET /api/analytics│
                                          │ (cmd/api gin engine)│
                                          └─────────────────────┘
```

**Producer subjects (post-13.2):**

| Subject | Producer module | Stream | Tokens | Consumer queue group |
|---|---|---|---|---|
| `analytics.event.calls` | dialer (NEW) | ANALYTICS | 3 | `analytics-ingest` |
| `analytics.event.operator_state` | dialer (NEW) | ANALYTICS | 3 | `analytics-ingest` |
| `tenant.*.recording.uploaded` | recording (EXTENDED payload) | RECORDING | 3 | `analytics-ingest` |
| `tenant.<t>.dialer.call.finalized` | dialer (NEW) | DIALER | 4 | (billing, future) |
| `tenant.<t>.dialer.op.<op>.state` | dialer (EXISTING) | DIALER | 6 | (realtime, existing) |

The two NEW dialer subjects (`tenant.<t>.dialer.call.finalized` + `analytics.event.calls`) are written as TWO outbox rows in the same `EventStatusSubmitted` FSM transition Tx. The `analytics.event.operator_state` row is appended ALONGSIDE the existing `tenant.<t>.dialer.op.<op>.state` row in `appendStateLogAndOutbox`.

### 3. Deep dive

- **Data model:** event payloads match the CH column tuple exactly. No type mismatch surface — payload struct embeds every column with `json:` tag = column name. Marshalling drift is caught by a schema-shape integration test (compare payload JSON keys vs `system.columns` for the target CH table).
- **API design:** REST GET, query params bound via `c.ShouldBindQuery` into typed `*Query` DTOs. Tenant scoping derived from JWT claim (`req.JWT.TenantID`), NOT from query string (defence in depth — caller cannot read another tenant's data even with a tampered query).
- **Caching:** read-through. Key = `analytics:{tenant_id}:{q_hash}` where `q_hash` = first 16 bytes of `sha256(canonical-JSON(query))`. TTL policy: window ≤ 24 h → 30 s; window > 24 h → 5 min. Cache value = gzip-deflated JSON of the DTO (Redis MEMORY savings for long lists).
- **Event flow:** `clickhouse-go/v2 PrepareBatch(ctx, "INSERT INTO …") → Append(c1, c2, …) per row → Send()` per batch. Fresh batch object per flush (NOT reused with `Flush()`); deferred `Close()` for resource cleanup.
- **Error handling:** poison payload (json.Unmarshal error, missing required fields) → ack + increment `analytics_ingest_dead_letter_total` (terminal: redelivery would loop). Transient CH error (network, timeout, server unavailable) → nak with default 250 ms delay; eventbus's exponential redelivery kicks in if the error persists.

### 4. Scale & reliability

- **Throughput:** batch every 10k OR 5 s. At 200/s peak, batches flush by time (5 s × 200 = 1 000 rows/batch) NOT count — well below the 10k cap. Headroom: 50× peak before the count threshold trips.
- **Backpressure:** maxAckPending=1024 (eventbus default). Each batch buffer caps at 10 000 (config). On overflow (CH down), buffer overflow triggers per-event nak → broker redelivers later. No silent drop.
- **Failover:** NATS durable consumer name = `analytics-ingest-<subject-hash>` (derived from `pkg/eventbus.durableNameFor`). Survives cmd/worker restart; resumes from last-ack'd seq. Dedup LRU is per-process — a restart re-processes the last batch's events if they were already in CH (idempotency check: CH `events_*` tables include `event_id UUID` — Plan 13.1 reserved this column for future dedup via `ReplacingMergeTree` once the volume justifies it; v1 accepts rare double-counted rows under restart-then-redeliver).
- **Drain:** on `ctx.Done`, pipeline flushes all 3 buffers BEFORE returning. `ctx.WithTimeout(5 * time.Second)` caps the drain.

### 5. Trade-offs

| Decision | Chosen | Alternative | Rationale |
|---|---|---|---|
| Batch API | `PrepareBatch + Send` (v2) | `WithAsync(true)` server-side buffering | Deterministic flush boundaries; safer drain semantics. |
| Cross-tenant subjects | `analytics.event.calls` (no tenant prefix) | `tenant.<t>.analytics.calls` | Spec §1228; ingester is stateless; tenant lives in payload, not subject. |
| Recording subject | `tenant.*.recording.uploaded` wildcard | New global subject | Avoids double-publish; recording already emits per-tenant. |
| HTTP location | `cmd/api` (existing) | New `cmd/analytics-api` binary | Single API surface; modules wire into shared gin engine. |
| Ingest location | `cmd/worker` (existing) | New `cmd/analytics-worker` binary | Shared lifecycle with retry/recording workers; one fewer process to operate. |
| Cache invalidation | TTL only (30 s / 5 min) | Subject-based active invalidation | Project rename / delete is rare; TTL is simpler. |
| RegionProgress.Plan source | `crm.api.ProjectService.GetProgress` via locator | Denormalise plan into CH | CH cannot join PG; locator lookup is the canonical cross-module port pattern. |

**What I would revisit as the system grows:**
- If CH ingest rate exceeds 1 000/s steady, switch to `WithAsync` + server-side buffering and accept "lose up-to-flush-window on broker disconnect" trade-off.
- If `events_calls.event_id` double-counting becomes measurable, migrate to `ReplacingMergeTree(event_id)` and accept slower MV materialisation.
- If `analytics:*` Redis key budget bloats, introduce a per-tenant LRU (`analytics:{tenant}` HASH with per-query field; trim with `HSCAN + HDEL`).

---

## File structure

### Created

| Path | Responsibility |
|---|---|
| `internal/analytics/api/ingest_payloads.go` | NEW — `AnalyticsCallEvent`, `AnalyticsOperatorStateEvent` payload structs (column-exact for `events_calls` + `events_operator_state`). Stays in `analytics/api/` so producers (dialer) import the type cleanly. |
| `internal/analytics/store/clickhouse.go` | NEW — `Conn` wrapper around `clickhouse-go/v2`'s `driver.Conn` with `Open(Config) (*Conn, error)` + `Close()` + `Ping(ctx)` + `Healthy()`. |
| `internal/analytics/store/clickhouse_test.go` | NEW — unit tests for `Config.Validate` (DSN, batch sizes), constructor errors. |
| `internal/analytics/store/clickhouse_integration_test.go` | NEW — testcontainer CH 24.8; `Open + Ping + Close` lifecycle. `//go:build integration`. |
| `internal/analytics/store/batch.go` | NEW — typed batch helpers: `InsertCalls(ctx, []AnalyticsCallEvent) error`, `InsertOperatorStates(...)`, `InsertRecordingsUploaded(...)`. Each calls `conn.PrepareBatch + Append per row + Send`. |
| `internal/analytics/store/batch_integration_test.go` | NEW — testcontainer CH; insert N rows, read back via `count() + sum(*)`, assert byte-for-byte parity. |
| `internal/analytics/store/queries.go` | NEW — typed CH SELECT helpers (one func per `MetricsQuery` method). Parameterized via `clickhouse-go/v2`'s named args; no string concatenation. |
| `internal/analytics/store/queries_integration_test.go` | NEW — testcontainer CH; insert fixture → run query → assert DTO shape. |
| `internal/analytics/service/ingest.go` | NEW — `IngestPipeline` implementation: 3 subject subscribers, 3 batch buffers, per-subject dedup LRU, periodic flush ticker, ctx-aware drain. |
| `internal/analytics/service/ingest_test.go` | NEW — unit tests for buffer overflow, dedup hit, malformed payload classification (poison vs transient). |
| `internal/analytics/service/ingest_integration_test.go` | NEW — embedded NATS + testcontainer CH; publish N events, await ingest, assert CH row count + dedup. `//go:build integration`. |
| `internal/analytics/service/dedup_lru.go` | NEW — `Cache[uuid.UUID]` LRU with size cap; uses `container/list` (per `golang-data-structures` skill). |
| `internal/analytics/service/dedup_lru_test.go` | NEW — table-driven: capacity, eviction order, concurrent Add+Has. |
| `internal/analytics/service/query.go` | NEW — `MetricsQuery` impl wrapping `store/queries.go` + Redis cache + crm.ProjectService port (for `RegionProgress.Plan`). |
| `internal/analytics/service/query_test.go` | NEW — table-driven HTTP-shape tests with a `fakeStore` and a `fakeCache`. |
| `internal/analytics/service/cache.go` | NEW — `RedisCache` wrapper: `Get(ctx, key) (cachedJSON, bool, error)`, `Set(ctx, key, json, ttl) error`. JSON+gzip codec. |
| `internal/analytics/service/cache_test.go` | NEW — miniredis-backed; encoding round-trip, TTL plumbing, miss path. |
| `internal/analytics/service/http_handlers.go` | NEW — 5 gin handlers: `getCalls`, `getOperatorState`, `getRegionProgress`, `getHourly`, `getOperatorComparisons` + `getOverview` aggregate. JWT-tenant binding, gin.ShouldBindQuery validation. |
| `internal/analytics/service/http_handlers_test.go` | NEW — table-driven httptest; gin.TestMode; per-endpoint happy path + validation errors + cache-hit short-circuit. |
| `internal/analytics/wire/ingest.go` | NEW — `BuildIngestPipeline(BuildDeps) (*service.IngestPipeline, error)` factory — keeps cmd/worker thin (mirrors `internal/recording/wire/local.go`'s `LocalPorts`). |
| `internal/analytics/wire/ingest_test.go` | NEW — unit-test for BuildDeps validation. |
| `internal/analytics/metrics/metrics.go` | NEW — Prometheus collectors: `sociopulse_analytics_ingest_received_total{subject}`, `_inserted_total{subject}`, `_failed_total{subject,reason}`, `_dead_letter_total{subject}`, `_lag_seconds{subject}`, `_batch_size{subject}` (histogram), `_query_duration_seconds{method}` (histogram), `_cache_hits_total{method}`, `_cache_misses_total{method}`. |
| `internal/analytics/metrics/metrics_test.go` | NEW — counter increment + label cardinality bounded. |
| `migrations/000012_event_outbox_dialer_finalize.up.sql` | (Conditional) NEW — only if implementer determines an INDEX on `event_outbox(subject)` is needed for relay perf with the new finalize subject. Plan baseline: skip; revisit at close-out if relay metrics flag it. |

### Modified

| Path | Change |
|---|---|
| `internal/analytics/api/events.go` | Replace placeholder `Subject*` constants with canonical names: `SubjectCallsAnalytics = "analytics.event.calls"`, `SubjectOperatorStateAnalytics = "analytics.event.operator_state"`, `SubjectRecordingUploadedWildcard = "tenant.*.recording.uploaded"`. Mirror dialer + recording producer-side declarations. |
| `internal/analytics/api/dto.go` | No changes (DTOs already complete from earlier api-stub work). Add `// import alignment` comment. |
| `internal/analytics/module.go` | Wire HTTP routes only — `mountAnalyticsRoutes(d.HTTPRouter, queryService)` under `/api/analytics/*`. Locator-register `analytics.MetricsQuery` for downstream Plan 13.3 (reports). NO ingest construction here (cmd/worker owns that). |
| `internal/dialer/api/events.go` | Add `SubjectAnalyticsCalls = "analytics.event.calls"` + `SubjectAnalyticsOperatorState = "analytics.event.operator_state"` (existing values stay). Add `AnalyticsCallEventPayload` + `AnalyticsOperatorStateEventPayload` structs (column-exact for CH). |
| `internal/dialer/fsm/audit.go` | `appendStateLogAndOutbox` — APPEND second `outbox.Append` for `analytics.event.operator_state` row alongside the existing per-tenant row. Compute `duration_in_state_sec` from previous-state log row (`sessions.LastStateLog(ctx, tx, sessionID)` lookup, add this method if missing). |
| `internal/dialer/fsm/transitions.go` (or wherever EventStatusSubmitted is wired) | New `appendCallFinalizedOutbox(tx, …)` helper — emits BOTH `tenant.<t>.dialer.call.finalized` + `analytics.event.calls` outbox rows. Wired into `EventStatusSubmitted` transition path. |
| `internal/dialer/fsm/store.go` | If `sessions.LastStateLog` doesn't exist, add it: `LastStateLog(ctx, tx, sessionID) (operatorStateLog, error)`. Returns `ErrNotFound` for first state (no prior log). |
| `internal/recording/api/events.go` | Extend `RecordingUploadedEvent` payload with missing CH columns: `ProjectID uuid.UUID`, `FSNode string`, `S3Key string`, `EncryptionKeyAlias string`, `EventID uuid.UUID`, `DurationSec int32` (seconds, not millis). |
| `internal/recording/service/service.go` (Commit hot path) | Populate the new payload fields from `RecordingMetadata` + boot-time config. `EventID` is `uuid.Must(uuid.NewV7())` per outbox row. |
| `pkg/config/analytics.go` | NEW — `AnalyticsConfig { Enabled bool, BatchSize int, FlushInterval time.Duration, DedupLRUSize int, CacheShortTTL time.Duration, CacheLongTTL time.Duration, LongWindowThreshold time.Duration }`. Wired into `Config` via mapstructure tag `analytics`. Defaults in `DefaultDev()`. |
| `configs/development/config.yaml` | Add `analytics:` block with sensible dev defaults (`enabled: true`, `batch_size: 10000`, `flush_interval: 5s`, `dedup_lru_size: 10000`, `cache_short_ttl: 30s`, `cache_long_ttl: 5m`, `long_window_threshold: 24h`). |
| `cmd/worker/main.go` | Add `openNATS(ctx, cfg, logger)` mirroring `cmd/api`'s helper. Add `buildAnalyticsIngest(...)` factory invoked when `cfg.Analytics.Enabled && natsSub != nil`. Append `analytics.IngestPipeline.Run` to the errgroup. |
| `cmd/api/main.go` | Wire `analytics.Module{}` into the providers walk AFTER `crm.Module{}` (so `crm.api.ProjectService` is resolvable from `Deps.Locator` at analytics.Register-time for `RegionProgress.Plan`). Add `openClickHouseConn` helper if MetricsQuery requires a live CH conn (best-effort; degrade to 503 on missing). |
| `docs/architecture/analytics-mv.md` | Append § "Operational caveats v1" listing the Q8/Q9 fidelity caveats (empty hangup_cause, partial region_code) and the Plan 13.3 follow-up needed to enrich. |

### depguard verification

- `clickhouse-go/v2` imports: ONLY in `internal/analytics/*` + `cmd/migrator` (latter already allow-listed Plan 13.1). Plan 13.2 adds `internal/analytics/store/*` to the existing rule — verify `.golangci.yml::depguard.rules.clickhouse-go-isolation` allows the path. If the rule does not exist yet (analytics is the first non-migrator user), ADD the rule.
- `redis` imports: already allow-listed in modules using `Deps.Redis`; nothing new.
- `eventbus.Subscriber` imports: already allow-listed.
- No new banned-stdlib violations expected (uses `crypto/sha256` for query hash, `compress/gzip` for cache codec — both allowed).

### Vocabulary check (per `CONTEXT.md`)

| Term used | Glossary entry | Notes |
|---|---|---|
| FSM | ✓ "FSM (operator)" | Used in dialer audit.go discussion |
| AHT, Abandonment | (info only — not used as identifiers in this plan) |
| AggregatingMergeTree, MV | (CH terms — not in glossary, project-tech) | Acceptable — ClickHouse-native vocabulary |
| Tenant, Respondent | ✓ | |
| KMS, DEK, KEK | ✓ | Indirect — only in recording's encryption_key_alias column |
| Outbox pattern | ✓ | Central to producer-side change |
| JetStream | ✓ | Existing infrastructure |
| Audit log | ✓ | Indirect — analytics does NOT write audit_log; it's a sink |
| RLS | ✓ | Indirect — analytics is read-only from CH (no PG RLS) |

No vocabulary drift. No new domain terms introduced (analytics ingest/query is project-tech, not domain).

---

## Pre-task — bootstrap commit

- [ ] **Step 0.1: Bootstrap commit — plan file + references update**

This plan file + the references update (`docs/references/plan-13-analytics.md` § Plan 13.2 resolved) are committed BEFORE Task 1 dispatch so the implementer subagent has the canonical source-of-truth on disk.

```bash
git add docs/superpowers/plans/2026-05-14-13-2-analytics-ingest-queries.md docs/references/plan-13-analytics.md
git commit -m "$(cat <<'EOF'
docs: Plan 13.2 bootstrap (analytics ingest + queries)

- Add docs/superpowers/plans/2026-05-14-13-2-analytics-ingest-queries.md (6 tasks).
- Update docs/references/plan-13-analytics.md § 13.2 (resolve Q4-Q12 with on-disk decisions).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

Expected: clean commit, no test runs (docs-only).

---

## Task 1: Producer side — subjects, payloads, dual-publish in dialer FSM + recording payload extensions

**Goal:** dialer's `EventStatusSubmitted` writes two new outbox rows in the same Tx (`tenant.<t>.dialer.call.finalized` + `analytics.event.calls`). dialer's `appendStateLogAndOutbox` writes one additional row (`analytics.event.operator_state`). recording's `Commit` populates the extended `RecordingUploadedEvent` payload fields.

**Files:**
- Create: `internal/analytics/api/ingest_payloads.go`
- Modify: `internal/analytics/api/events.go:9-19`
- Modify: `internal/dialer/api/events.go:9-89`
- Modify: `internal/dialer/fsm/audit.go:183-218`
- Modify: `internal/dialer/fsm/transitions.go` (find `EventStatusSubmitted` → wire `appendCallFinalizedOutbox`)
- Modify: `internal/dialer/fsm/store.go` (add `LastStateLog` if missing)
- Modify: `internal/recording/api/events.go:9-78`
- Modify: `internal/recording/service/service.go` (Commit path — populate new fields)
- Test: `internal/dialer/fsm/audit_test.go` (existing — extend)
- Test: `internal/dialer/fsm/audit_pg_test.go` (existing — extend with dual-row assertion)
- Test: `internal/recording/service/service_test.go` (existing — extend with full-payload assertion)

### Step 1.1 — RED: write the failing test for analytics payload struct shape

The CH `events_calls` schema has 12 logical columns + `_inserted_at DEFAULT now()`. The payload must marshal to JSON with keys matching the column tuple ordering used by `batch.Append`. Test asserts JSON shape.

```go
// internal/analytics/api/ingest_payloads_test.go
package api_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	api "github.com/sociopulse/platform/internal/analytics/api"
)

func TestAnalyticsCallEventPayload_JSONShape(t *testing.T) {
	t.Parallel()

	ev := api.AnalyticsCallEventPayload{
		Date:        "2026-05-14",
		TS:          time.Date(2026, 5, 14, 10, 30, 0, 0, time.UTC),
		TenantID:    uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		ProjectID:   uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		OperatorID:  uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		CallID:      uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		Status:      "success",
		DurationSec: 240,
		HangupCause: "NORMAL_CLEARING",
		RegionCode:  "77",
		AttemptNo:   1,
		TrunkUsed:   "trunk-msk-01",
		EventID:     uuid.MustParse("55555555-5555-5555-5555-555555555555"),
	}
	raw, err := json.Marshal(ev)
	require.NoError(t, err)

	// Assert: all 13 expected keys present, ordered explicitly (so MV ingester
	// can rely on field order at batch.Append-time via reflect or per-column
	// extraction).
	var parsed map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &parsed))
	want := []string{"date", "ts", "tenant_id", "project_id", "operator_id", "call_id", "status", "duration_sec", "hangup_cause", "region_code", "attempt_no", "trunk_used", "event_id"}
	for _, k := range want {
		_, ok := parsed[k]
		require.True(t, ok, "missing key %q in %s", k, string(raw))
	}
	require.Len(t, parsed, len(want), "unexpected extra keys in payload: %s", string(raw))
}

func TestAnalyticsOperatorStateEventPayload_JSONShape(t *testing.T) {
	t.Parallel()

	pid := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	ev := api.AnalyticsOperatorStateEventPayload{
		Date:               "2026-05-14",
		TS:                 time.Date(2026, 5, 14, 10, 30, 0, 0, time.UTC),
		TenantID:           uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		UserID:             uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		State:              "ready",
		DurationInStateSec: 120,
		ProjectID:          &pid,
		EventID:            uuid.MustParse("55555555-5555-5555-5555-555555555555"),
	}
	raw, err := json.Marshal(ev)
	require.NoError(t, err)

	var parsed map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &parsed))
	want := []string{"date", "ts", "tenant_id", "user_id", "state", "duration_in_state_sec", "project_id", "event_id"}
	for _, k := range want {
		_, ok := parsed[k]
		require.True(t, ok, "missing key %q in %s", k, string(raw))
	}
}
```

### Step 1.2 — Watch the test fail

Run: `go test -run TestAnalyticsCallEventPayload ./internal/analytics/api -count=1 -v`
Expected: FAIL with `undefined: api.AnalyticsCallEventPayload` / `undefined: api.AnalyticsOperatorStateEventPayload`.

### Step 1.3 — GREEN: write `internal/analytics/api/ingest_payloads.go`

```go
// Package api — analytics ingest payloads.
//
// These structs match the CH `events_calls` and `events_operator_state`
// column tuples byte-for-byte (column order, types, JSON tags). The
// IngestPipeline reads them off NATS and binds them positionally to
// `batch.Append(...)`. Drift between this file and migrations/clickhouse/*.up.sql
// is caught at integration-test time by the schema-shape assertion.
package api

import (
	"time"

	"github.com/google/uuid"
)

// AnalyticsCallEventPayload is the payload of an `analytics.event.calls`
// NATS message — denormalised call row consumed by the analytics ingest
// pipeline. Field order MUST match the column order of
// `migrations/clickhouse/000001_events_calls.up.sql`.
type AnalyticsCallEventPayload struct {
	Date        string    `json:"date"`         // YYYY-MM-DD, parses to CH Date
	TS          time.Time `json:"ts"`           // CH DateTime64(3)
	TenantID    uuid.UUID `json:"tenant_id"`
	ProjectID   uuid.UUID `json:"project_id"`
	OperatorID  uuid.UUID `json:"operator_id"`
	CallID      uuid.UUID `json:"call_id"`
	Status      string    `json:"status"`        // CH LowCardinality(String)
	DurationSec uint32    `json:"duration_sec"`
	HangupCause string    `json:"hangup_cause"`  // CH LowCardinality(String); "" sentinel = unknown
	RegionCode  string    `json:"region_code"`   // CH LowCardinality(String); "" sentinel = unknown
	AttemptNo   uint8     `json:"attempt_no"`
	TrunkUsed   string    `json:"trunk_used"`    // CH LowCardinality(String)
	EventID     uuid.UUID `json:"event_id"`      // dedup key
}

// AnalyticsOperatorStateEventPayload is the payload of an
// `analytics.event.operator_state` NATS message — one row per FSM
// transition (state-entered or state-exited; the duration_in_state_sec
// is the delta from previous state-log row).
type AnalyticsOperatorStateEventPayload struct {
	Date               string     `json:"date"`
	TS                 time.Time  `json:"ts"`
	TenantID           uuid.UUID  `json:"tenant_id"`
	UserID             uuid.UUID  `json:"user_id"`
	State              string     `json:"state"`                // CH LowCardinality(String)
	DurationInStateSec uint32     `json:"duration_in_state_sec"`
	ProjectID          *uuid.UUID `json:"project_id"`           // CH Nullable(UUID)
	EventID            uuid.UUID  `json:"event_id"`
}
```

Run: `go test -run TestAnalyticsCallEventPayload ./internal/analytics/api -count=1 -v`
Expected: PASS.

Run: `go test -run TestAnalyticsOperatorStateEventPayload ./internal/analytics/api -count=1 -v`
Expected: PASS.

### Step 1.4 — RED: write failing test for analytics subject constants update

```go
// internal/analytics/api/events_test.go
package api_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	api "github.com/sociopulse/platform/internal/analytics/api"
)

func TestSubjects_CanonicalValues(t *testing.T) {
	t.Parallel()
	require.Equal(t, "analytics.event.calls", api.SubjectCallsAnalytics)
	require.Equal(t, "analytics.event.operator_state", api.SubjectOperatorStateAnalytics)
	require.Equal(t, "tenant.*.recording.uploaded", api.SubjectRecordingUploadedWildcard)
}
```

Run: `go test -run TestSubjects_CanonicalValues ./internal/analytics/api -count=1 -v`
Expected: FAIL with `undefined: api.SubjectCallsAnalytics`.

### Step 1.5 — GREEN: replace stale subject placeholders in `internal/analytics/api/events.go`

```go
// Package api — analytics module events.
//
// analytics is a sink: it does not publish events of its own. The
// IngestPipeline consumes the durable JetStream subjects below and
// inserts each event into the matching ClickHouse table.
const (
	// SubjectCallsAnalytics is the cross-tenant subject (no tenant prefix)
	// produced by the dialer FSM on EventStatusSubmitted alongside the
	// existing tenant-prefixed dialer.call.finalized row.
	SubjectCallsAnalytics = "analytics.event.calls"

	// SubjectOperatorStateAnalytics is the cross-tenant subject produced
	// by dialer's appendStateLogAndOutbox per FSM transition.
	SubjectOperatorStateAnalytics = "analytics.event.operator_state"

	// SubjectRecordingUploadedWildcard is the per-tenant wildcard the
	// IngestPipeline subscribes to. Tenant ID is extracted from the
	// subject's second token.
	SubjectRecordingUploadedWildcard = "tenant.*.recording.uploaded"
)
```

Run: `go test ./internal/analytics/api/... -count=1`
Expected: PASS (all 3 subject + 2 payload tests).

### Step 1.6 — RED: extend `internal/dialer/api/events.go` with analytics subjects + payload types

```go
// internal/dialer/api/events_test.go (new file)
package api_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	api "github.com/sociopulse/platform/internal/dialer/api"
)

func TestSubjectAnalyticsConstants(t *testing.T) {
	t.Parallel()
	require.Equal(t, "analytics.event.calls", api.SubjectAnalyticsCalls)
	require.Equal(t, "analytics.event.operator_state", api.SubjectAnalyticsOperatorState)
}
```

Run: `go test -run TestSubjectAnalyticsConstants ./internal/dialer/api -count=1 -v`
Expected: FAIL.

### Step 1.7 — GREEN: add constants to `internal/dialer/api/events.go`

```go
// After the existing SubjectAnalyticsCalls / SubjectAnalyticsOperatorState
// constants (which are already declared but unused):
//   SubjectAnalyticsCalls         = "analytics.event.calls"
//   SubjectAnalyticsOperatorState = "analytics.event.operator_state"
//
// Verify the existing definitions match. If they were previously placeholders
// (e.g. with different names), align to these canonical values. NO new
// constants should be required — Plan 13.1 baseline already declares them
// at internal/dialer/api/events.go:25-27 (verify before editing).
```

Note: per Phase 1 context, these constants already exist in `internal/dialer/api/events.go:25-27`. **Verify** before editing — they may already be canonical and the test should pass without changes. If so, this step is a NO-OP at code level (test landed in 1.6 is sufficient).

Run: `go test -run TestSubjectAnalyticsConstants ./internal/dialer/api -count=1 -v`
Expected: PASS.

### Step 1.8 — RED: write failing test for dialer FSM dual-publish on state change

The existing `internal/dialer/fsm/audit_pg_test.go` covers the per-tenant outbox row write. We extend it to also assert the cross-tenant analytics row.

```go
// internal/dialer/fsm/audit_pg_test.go (additive)
//
// In the existing TestAppendStateLogAndOutbox_HappyPath integration test,
// after asserting the tenant.<t>.dialer.op.<op_id>.state outbox row,
// add an assertion for the second outbox row.

func TestAppendStateLogAndOutbox_AlsoEmitsAnalyticsOpStateRow(t *testing.T) {
	t.Parallel()
	// Test fixture mirrors TestAppendStateLogAndOutbox_HappyPath:
	//   - new tenant, operator, session
	//   - call appendStateLogAndOutbox with a transition (e.g. "offline" → "ready")
	//   - read event_outbox
	//   - assert TWO rows present:
	//     1. subject = tenant.<t>.dialer.op.<op_id>.state
	//     2. subject = analytics.event.operator_state
	//   - assert the analytics payload's fields match expectations:
	//     duration_in_state_sec = N seconds (from previous-state-log row)
	//     state = "ready"
	//     event_id is a non-zero UUID

	// Setup tenant, pool, machine, etc. (reuse existing buildMachineHarness helper
	// from the file if present; otherwise extract one).
	h := buildMachineHarness(t)
	defer h.cleanup()

	// First transition: offline → ready (no previous state-log → duration = 0)
	err := h.appendStateLogAndOutbox(testCtx(), h.tx, auditEntry{
		SessionID:  h.session.ID,
		TenantID:   h.tenant,
		OperatorID: h.operator,
		NewState:   "ready",
		OccurredAt: time.Now(),
	})
	require.NoError(t, err)

	rows := readOutbox(t, h.tx, h.tenant) // helper that selects event_outbox by tenant
	require.Len(t, rows, 2, "expected 2 outbox rows (per-tenant + analytics)")

	var sawPerTenant, sawAnalytics bool
	for _, r := range rows {
		switch r.Subject {
		case fmt.Sprintf("tenant.%s.dialer.op.%s.state", h.tenant, h.operator):
			sawPerTenant = true
		case "analytics.event.operator_state":
			sawAnalytics = true
			// Decode payload, assert fields:
			var p analyticsapi.AnalyticsOperatorStateEventPayload
			require.NoError(t, json.Unmarshal(r.Payload, &p))
			require.Equal(t, h.tenant, p.TenantID)
			require.Equal(t, h.operator, p.UserID)
			require.Equal(t, "ready", p.State)
			require.Zero(t, p.DurationInStateSec, "first transition has no prior state-log")
			require.NotEqual(t, uuid.Nil, p.EventID)
		}
	}
	require.True(t, sawPerTenant, "missing per-tenant outbox row")
	require.True(t, sawAnalytics, "missing analytics outbox row")
}
```

Run: `go test -tags=integration -run TestAppendStateLogAndOutbox_AlsoEmitsAnalyticsOpStateRow ./internal/dialer/fsm -count=1 -v`
Expected: FAIL — only one outbox row found (the per-tenant one).

### Step 1.9 — GREEN: modify `internal/dialer/fsm/audit.go::appendStateLogAndOutbox`

Append a second `outbox.Append` call after the existing one:

```go
// internal/dialer/fsm/audit.go (around line 209, after the existing m.outbox.Append)
// Compute duration_in_state_sec from previous state-log row (0 if first transition).
prevDur := uint32(0)
prev, err := m.sessions.LastStateLog(ctx, tx, entry.SessionID)
switch {
case err == nil && !prev.OccurredAt.IsZero():
	prevDur = uint32(entry.OccurredAt.Sub(prev.OccurredAt).Seconds())
case errors.Is(err, ErrNoStateLog):
	// first transition; prevDur stays 0
default:
	return fmt.Errorf("lookup last state log: %w", err)
}

analyticsPayload, err := json.Marshal(analyticsapi.AnalyticsOperatorStateEventPayload{
	Date:               entry.OccurredAt.UTC().Format("2006-01-02"),
	TS:                 entry.OccurredAt.UTC(),
	TenantID:           entry.TenantID,
	UserID:             entry.OperatorID,
	State:              string(entry.NewState),
	DurationInStateSec: prevDur,
	ProjectID:          entry.ProjectID,
	EventID:            uuid.New(),
})
if err != nil {
	return fmt.Errorf("marshal analytics op state: %w", err)
}

if err := m.outbox.Append(ctx, tx, outbox.Event{
	TenantID:    &tenantID,
	AggregateID: &operatorID,
	Subject:     analyticsapi.SubjectOperatorStateAnalytics,
	Payload:     analyticsPayload,
}); err != nil {
	return fmt.Errorf("outbox append analytics op state: %w", err)
}
```

If `sessions.LastStateLog` does not exist in `internal/dialer/fsm/store.go`, ADD it. Implementation: `SELECT occurred_at, new_state, reason FROM operator_state_log WHERE session_id = $1 ORDER BY occurred_at DESC LIMIT 1`. Returns `ErrNoStateLog` when zero rows.

```go
// internal/dialer/fsm/store.go (additive)
var ErrNoStateLog = errors.New("dialer/fsm: no prior state log")

type operatorStateLog struct {
	OccurredAt time.Time
	NewState   string
	Reason     string
}

// LastStateLog returns the most recent operator_state_log row for the
// session. Returns ErrNoStateLog when none exist (first transition).
func (s *PostgresSessions) LastStateLog(ctx context.Context, tx postgres.Tx, sessionID uuid.UUID) (operatorStateLog, error) {
	const q = `
	SELECT occurred_at, new_state, COALESCE(reason, '')
	  FROM operator_state_log
	 WHERE session_id = $1
	 ORDER BY occurred_at DESC
	 LIMIT 1`
	var l operatorStateLog
	err := tx.QueryRow(ctx, q, sessionID).Scan(&l.OccurredAt, &l.NewState, &l.Reason)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return operatorStateLog{}, ErrNoStateLog
	case err != nil:
		return operatorStateLog{}, fmt.Errorf("dialer/fsm: last state log: %w", err)
	}
	return l, nil
}
```

Run: `go test -tags=integration -run TestAppendStateLogAndOutbox_AlsoEmitsAnalyticsOpStateRow ./internal/dialer/fsm -count=1 -v`
Expected: PASS.

Re-run the existing TestAppendStateLogAndOutbox suite:
Run: `go test -tags=integration ./internal/dialer/fsm/... -count=1`
Expected: ALL PASS (regression check).

### Step 1.10 — RED: failing test for `EventStatusSubmitted` dual-publish (call.finalized)

The dialer FSM's `EventStatusSubmitted` transition currently flips state without publishing. Plan 13.2 wires TWO publishes in the same Tx.

Implementer locates the `EventStatusSubmitted` transition handler (likely in `internal/dialer/fsm/transitions.go` based on file layout). The test asserts:

```go
// internal/dialer/fsm/transitions_test.go (additive)
func TestEventStatusSubmitted_EmitsCallFinalizedOutboxRows(t *testing.T) {
	t.Parallel()
	h := buildMachineHarness(t)
	defer h.cleanup()

	// Drive FSM to "status" state, then submit status.
	h.driveTo(t, "status")
	err := h.machine.Dispatch(testCtx(), api.EventStatusSubmitted, api.DispatchInput{
		Status:      "success",
		DurationSec: 240,
		CallID:      h.callID,
		ProjectID:   h.project,
	})
	require.NoError(t, err)

	rows := readOutbox(t, h.tx, h.tenant)

	var sawPerTenantFinalize, sawAnalyticsCall bool
	for _, r := range rows {
		switch r.Subject {
		case fmt.Sprintf("tenant.%s.dialer.call.finalized", h.tenant):
			sawPerTenantFinalize = true
		case "analytics.event.calls":
			sawAnalyticsCall = true
			var p analyticsapi.AnalyticsCallEventPayload
			require.NoError(t, json.Unmarshal(r.Payload, &p))
			require.Equal(t, h.tenant, p.TenantID)
			require.Equal(t, "success", p.Status)
			require.Equal(t, uint32(240), p.DurationSec)
		}
	}
	require.True(t, sawPerTenantFinalize, "missing tenant.<t>.dialer.call.finalized row")
	require.True(t, sawAnalyticsCall, "missing analytics.event.calls row")
}
```

Run: `go test -tags=integration -run TestEventStatusSubmitted_EmitsCallFinalizedOutboxRows ./internal/dialer/fsm -count=1 -v`
Expected: FAIL.

### Step 1.11 — GREEN: wire `appendCallFinalizedOutbox` into the EventStatusSubmitted handler

```go
// internal/dialer/fsm/transitions.go (new helper)
//
// appendCallFinalizedOutbox writes two outbox rows in the same Tx:
//   1. tenant.<t>.dialer.call.finalized — per-tenant lifecycle (consumed by billing)
//   2. analytics.event.calls — cross-tenant denormalised (consumed by analytics)
func (m *Machine) appendCallFinalizedOutbox(
	ctx context.Context,
	tx postgres.Tx,
	entry callFinalizedEntry,
) error {
	tenantID := entry.TenantID

	// 1. per-tenant lifecycle
	lifecyclePayload, err := json.Marshal(api.CallFinalizedEvent{
		CallID:       entry.CallID,
		TenantID:     entry.TenantID,
		OperatorID:   entry.OperatorID,
		ProjectID:    entry.ProjectID,
		RespondentID: entry.RespondentID,
		TrunkUsed:    entry.TrunkUsed,
		DurationSec:  entry.DurationSec,
		Status:       entry.Status,
		StorageBytes: entry.StorageBytes,
		FinalizedAt:  entry.OccurredAt.Unix(),
	})
	if err != nil {
		return fmt.Errorf("marshal call finalized: %w", err)
	}
	if err := m.outbox.Append(ctx, tx, outbox.Event{
		TenantID:    &tenantID,
		AggregateID: &entry.CallID,
		Subject:     api.SubjectCallFinalizedFor(tenantID),
		Payload:     lifecyclePayload,
	}); err != nil {
		return fmt.Errorf("outbox append call finalized: %w", err)
	}

	// 2. analytics denormalised
	analyticsPayload, err := json.Marshal(analyticsapi.AnalyticsCallEventPayload{
		Date:        entry.OccurredAt.UTC().Format("2006-01-02"),
		TS:          entry.OccurredAt.UTC(),
		TenantID:    entry.TenantID,
		ProjectID:   entry.ProjectID,
		OperatorID:  entry.OperatorID,
		CallID:      entry.CallID,
		Status:      string(entry.Status),
		DurationSec: uint32(entry.DurationSec),
		HangupCause: entry.HangupCause, // "" sentinel if FSM doesn't have it
		RegionCode:  entry.RegionCode,  // "" sentinel
		AttemptNo:   entry.AttemptNo,
		TrunkUsed:   entry.TrunkUsed,
		EventID:     uuid.New(),
	})
	if err != nil {
		return fmt.Errorf("marshal analytics call event: %w", err)
	}
	if err := m.outbox.Append(ctx, tx, outbox.Event{
		TenantID:    &tenantID,
		AggregateID: &entry.CallID,
		Subject:     analyticsapi.SubjectCallsAnalytics,
		Payload:     analyticsPayload,
	}); err != nil {
		return fmt.Errorf("outbox append analytics call: %w", err)
	}
	return nil
}
```

The implementer wires this into the `EventStatusSubmitted` transition. The `callFinalizedEntry` struct is constructed from the FSM input + machine context (some fields may be zero in v1 — that's the documented Q8/Q9 caveat).

Run: `go test -tags=integration -run TestEventStatusSubmitted_EmitsCallFinalizedOutboxRows ./internal/dialer/fsm -count=1 -v`
Expected: PASS.

### Step 1.12 — RED: failing test for recording payload extension

```go
// internal/recording/service/service_test.go (additive)
func TestCommit_OutboxPayload_HasAllAnalyticsFields(t *testing.T) {
	t.Parallel()
	h := buildHarness(t)
	defer h.cleanup()

	// Commit a recording.
	out, err := h.svc.Commit(testCtx(), buildCommitInput(t))
	require.NoError(t, err)
	require.NotEmpty(t, out.RecordingID)

	row := readOutbox(t, h.tx, h.tenant)[0]
	var ev recapi.RecordingUploadedEvent
	require.NoError(t, json.Unmarshal(row.Payload, &ev))
	require.NotEqual(t, uuid.Nil, ev.ProjectID, "ProjectID must be populated")
	require.NotEmpty(t, ev.FSNode, "FSNode must be populated")
	require.NotEmpty(t, ev.S3Key, "S3Key must be populated")
	require.NotEmpty(t, ev.EncryptionKeyAlias, "EncryptionKeyAlias must be populated")
	require.NotEqual(t, uuid.Nil, ev.EventID, "EventID must be populated (non-zero UUID)")
	require.Greater(t, ev.DurationSec, int32(0), "DurationSec must be > 0")
}
```

Run: `go test -tags=integration -run TestCommit_OutboxPayload_HasAllAnalyticsFields ./internal/recording/service -count=1 -v`
Expected: FAIL — fields missing.

### Step 1.13 — GREEN: extend `internal/recording/api/events.go::RecordingUploadedEvent` + populate in service

```go
// internal/recording/api/events.go (modified)
type RecordingUploadedEvent struct {
	RecordingID        uuid.UUID `json:"recording_id"`
	CallID             uuid.UUID `json:"call_id"`
	TenantID           uuid.UUID `json:"tenant_id"`
	ProjectID          uuid.UUID `json:"project_id"`           // NEW
	FSNode             string    `json:"fs_node"`              // NEW
	S3Key              string    `json:"s3_key"`               // NEW
	EncryptionKeyAlias string    `json:"encryption_key_alias"` // NEW
	EventID            uuid.UUID `json:"event_id"`             // NEW (dedup)
	BytesSize          int64     `json:"bytes_size"`
	DurationMS         int64     `json:"duration_ms"`
	DurationSec        int32     `json:"duration_sec"`         // NEW (CH column is seconds)
	SHA256Hex          string    `json:"sha256"`
	Status             string    `json:"status"`
	CommittedAt        int64     `json:"committed_at"`
}
```

Modify `internal/recording/service/service.go::Commit` (or its publish helper) to populate the new fields from `RecordingMetadata` + boot config (`FSNode` from the recording row; `EncryptionKeyAlias` from the KMS DEK metadata; `EventID = uuid.New()`).

Run: `go test -tags=integration -run TestCommit_OutboxPayload_HasAllAnalyticsFields ./internal/recording/service -count=1 -v`
Expected: PASS.

### Step 1.14 — Quality gate

```bash
make ci
go test -race -count=1 ./internal/analytics/... ./internal/dialer/... ./internal/recording/...
gofmt -l .
```

All green. No new lint warnings.

### Step 1.15 — Commit Task 1

```bash
git add internal/analytics/api/ingest_payloads.go \
        internal/analytics/api/ingest_payloads_test.go \
        internal/analytics/api/events.go \
        internal/analytics/api/events_test.go \
        internal/dialer/api/events.go \
        internal/dialer/api/events_test.go \
        internal/dialer/fsm/audit.go \
        internal/dialer/fsm/audit_pg_test.go \
        internal/dialer/fsm/store.go \
        internal/dialer/fsm/transitions.go \
        internal/dialer/fsm/transitions_test.go \
        internal/recording/api/events.go \
        internal/recording/service/service.go \
        internal/recording/service/service_test.go

git commit -m "$(cat <<'EOF'
feat(dialer,recording,analytics/api): Plan 13.2 Task 1 — producer side

- analytics/api: AnalyticsCallEventPayload + AnalyticsOperatorStateEventPayload
  (column-exact for events_calls + events_operator_state) and canonical subject
  constants SubjectCallsAnalytics / SubjectOperatorStateAnalytics /
  SubjectRecordingUploadedWildcard.
- dialer/fsm: appendStateLogAndOutbox now writes TWO outbox rows in the same Tx
  (per-tenant op.state + cross-tenant analytics.event.operator_state). Adds
  LastStateLog query for duration_in_state_sec delta.
- dialer/fsm: new appendCallFinalizedOutbox helper wired into EventStatusSubmitted
  — emits tenant.<t>.dialer.call.finalized + analytics.event.calls.
- recording/api: RecordingUploadedEvent extended with project_id, fs_node, s3_key,
  encryption_key_alias, event_id, duration_sec — required by CH events_recording_uploaded.
- recording/service: Commit populates the new fields.

TDD: 5 new RED→GREEN cycles. Per-row JSON-shape assertions guard against drift
between payload structs and CH columns.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: ClickHouse store wrapper + typed batch helpers

**Goal:** thin `*store.Conn` around `clickhouse-go/v2`'s native driver + 3 typed batch-insert helpers (one per CH source table). All testcontainer-backed.

**Files:**
- Create: `internal/analytics/store/clickhouse.go`
- Create: `internal/analytics/store/clickhouse_test.go`
- Create: `internal/analytics/store/clickhouse_integration_test.go`
- Create: `internal/analytics/store/batch.go`
- Create: `internal/analytics/store/batch_integration_test.go`
- Create: `internal/analytics/store/main_test.go` (goleak)
- Modify: `.golangci.yml` (add `internal/analytics/*` to clickhouse-go allow-list)

### Step 2.1 — RED: failing unit test for `Config.Validate`

```go
// internal/analytics/store/clickhouse_test.go
package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/analytics/store"
)

func TestConfig_Validate_RejectsEmptyDSN(t *testing.T) {
	t.Parallel()
	c := store.Config{
		DSN:           "",
		BatchSize:     10000,
		FlushInterval: 5 * time.Second,
	}
	err := c.Validate()
	require.Error(t, err)
	require.ErrorIs(t, err, store.ErrInvalidConfig)
}

func TestConfig_Validate_RejectsZeroBatch(t *testing.T) {
	t.Parallel()
	c := store.Config{DSN: "clickhouse://localhost:9000/x", BatchSize: 0}
	require.ErrorIs(t, c.Validate(), store.ErrInvalidConfig)
}

func TestConfig_Validate_HappyPath(t *testing.T) {
	t.Parallel()
	c := store.Config{
		DSN:           "clickhouse://app:devpass@localhost:9000/sociopulse",
		BatchSize:     10000,
		FlushInterval: 5 * time.Second,
	}
	require.NoError(t, c.Validate())
}
```

Run: `go test -run TestConfig_Validate ./internal/analytics/store -count=1 -v`
Expected: FAIL — `undefined: store.Config`.

### Step 2.2 — GREEN: write `internal/analytics/store/clickhouse.go`

```go
// Package store — ClickHouse client wrapper for the analytics module.
//
// Why a wrapper: clickhouse-go/v2's driver.Conn is fine for direct use,
// but we want (a) a single place to apply Settings + Compression defaults,
// (b) a constructor that returns errors instead of panicking on bad DSN,
// (c) a Healthy() method for /readyz, (d) a depguard isolation boundary —
// the driver import lives here and ONLY here within analytics.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.uber.org/zap"
)

// ErrInvalidConfig is returned by Config.Validate for missing or
// nonsensical fields.
var ErrInvalidConfig = errors.New("analytics/store: invalid clickhouse config")

// Config carries the constructor inputs for Open.
type Config struct {
	DSN           string        // clickhouse://user:pass@host:port/db?...
	BatchSize     int           // max rows per batch flush
	FlushInterval time.Duration // max wall-time between flushes
	DialTimeout   time.Duration // optional; defaults to 5s
	Logger        *zap.Logger   // optional; defaults to zap.NewNop
}

// Validate enforces sane configuration. Called by Open before contacting
// the broker so misconfigured deployments fail fast at boot.
func (c Config) Validate() error {
	if c.DSN == "" {
		return fmt.Errorf("%w: empty DSN", ErrInvalidConfig)
	}
	if c.BatchSize <= 0 {
		return fmt.Errorf("%w: BatchSize must be positive (got %d)", ErrInvalidConfig, c.BatchSize)
	}
	if c.FlushInterval <= 0 {
		return fmt.Errorf("%w: FlushInterval must be positive (got %v)", ErrInvalidConfig, c.FlushInterval)
	}
	return nil
}

// Conn is the analytics module's wrapper around clickhouse-go's
// driver.Conn. The wrapper exists so callers don't import clickhouse-go
// directly (depguard isolation) and so the constructor centralises
// Settings + Compression + DialTimeout defaults.
type Conn struct {
	conn   driver.Conn
	logger *zap.Logger
	cfg    Config
}

// Open constructs a Conn and verifies broker reachability via Ping(ctx).
// Returns wrapped ErrInvalidConfig on bad input, or the underlying
// clickhouse-go error on broker-side failure.
func Open(ctx context.Context, cfg Config) (*Conn, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	opts, err := clickhouse.ParseDSN(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("analytics/store: parse DSN: %w", err)
	}
	dialTimeout := cfg.DialTimeout
	if dialTimeout == 0 {
		dialTimeout = 5 * time.Second
	}
	opts.DialTimeout = dialTimeout
	opts.Compression = &clickhouse.Compression{Method: clickhouse.CompressionLZ4}
	if opts.Settings == nil {
		opts.Settings = clickhouse.Settings{}
	}
	// Sane production defaults: never block writes on missing capacity.
	opts.Settings["max_insert_threads"] = 4

	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("analytics/store: open: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	c := &Conn{conn: conn, logger: logger, cfg: cfg}
	if err := c.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("analytics/store: ping: %w", err)
	}
	return c, nil
}

// Ping issues `SELECT 1` to verify broker reachability. Wraps the
// driver-level error so callers can errors.Is against context cancellation.
func (c *Conn) Ping(ctx context.Context) error {
	if c == nil {
		return errors.New("analytics/store: nil conn")
	}
	return c.conn.Ping(ctx)
}

// Healthy returns nil when the broker is currently usable; an error
// otherwise. Used by /readyz; non-blocking on the wire (just checks the
// driver's local state).
func (c *Conn) Healthy() error {
	if c == nil || c.conn == nil {
		return errors.New("analytics/store: nil conn")
	}
	// driver.Conn has no public IsClosed; rely on Ping with a short ctx.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	return c.conn.Ping(ctx)
}

// Close drains pending operations and releases the connection pool.
// Idempotent; second Close is a no-op.
func (c *Conn) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("analytics/store: close: %w", err)
	}
	c.conn = nil
	return nil
}

// Driver returns the underlying driver.Conn — used by store/batch.go and
// store/queries.go to issue PrepareBatch / Query. Not exported to other
// packages (analytics/service uses *Conn, not driver.Conn).
func (c *Conn) Driver() driver.Conn { return c.conn }

// Config returns the live config (read-only).
func (c *Conn) Config() Config { return c.cfg }
```

Run: `go test -run TestConfig_Validate ./internal/analytics/store -count=1 -v`
Expected: PASS.

### Step 2.3 — RED: failing integration test for `Open + Ping + Close`

```go
// internal/analytics/store/clickhouse_integration_test.go
//go:build integration
// +build integration

package store_test

import (
	"context"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/analytics/store"
)

func TestConn_OpenPingClose(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	c, err := tcclickhouse.Run(ctx, "clickhouse/clickhouse-server:24.8")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	dsn, err := c.ConnectionString(ctx)
	require.NoError(t, err)

	conn, err := store.Open(ctx, store.Config{
		DSN:           dsn,
		BatchSize:     10,
		FlushInterval: 1 * time.Second,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()

	require.NoError(t, conn.Ping(ctx))
	require.NoError(t, conn.Healthy())
}
```

Run: `go test -tags=integration -run TestConn_OpenPingClose ./internal/analytics/store -count=1 -v`
Expected: FAIL (only when Docker is up; otherwise testcontainers panics).

### Step 2.4 — GREEN: the integration test passes once `Open` is wired correctly

If 2.2's code already compiles, this should pass.
Run: `go test -tags=integration -run TestConn_OpenPingClose ./internal/analytics/store -count=1 -v`
Expected: PASS.

### Step 2.5 — RED: failing test for batch-insert helper (events_calls)

```go
// internal/analytics/store/batch_integration_test.go
//go:build integration
// +build integration

package store_test

func TestInsertCalls_HappyPath(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := startCH(t, ctx) // helper from main_test.go that runs migrations
	conn, _ := store.Open(ctx, store.Config{DSN: c.dsn, BatchSize: 100, FlushInterval: time.Second})
	defer conn.Close()

	rows := []apianalytics.AnalyticsCallEventPayload{
		// 5 fixture rows with deterministic UUIDs + timestamps
	}
	require.NoError(t, store.InsertCalls(ctx, conn, rows))

	// Verify via direct query
	var count uint64
	require.NoError(t, conn.Driver().QueryRow(ctx, "SELECT count() FROM events_calls").Scan(&count))
	require.Equal(t, uint64(5), count)
}
```

Add similar tests: `TestInsertOperatorStates_HappyPath`, `TestInsertRecordingsUploaded_HappyPath`.

Run: `go test -tags=integration -run TestInsertCalls ./internal/analytics/store -count=1 -v`
Expected: FAIL — `undefined: store.InsertCalls`.

### Step 2.6 — GREEN: write `internal/analytics/store/batch.go`

```go
package store

import (
	"context"
	"fmt"

	"github.com/sociopulse/platform/internal/analytics/api"
)

// InsertCalls writes a batch of AnalyticsCallEventPayload rows into events_calls.
// Returns the error from PrepareBatch, any Append error (with the failing row
// index), or the final Send error.
//
// Caller should keep batches small enough that Send completes within the
// flush deadline (~5s typical). For batches > 100 000 rows consider splitting.
func InsertCalls(ctx context.Context, conn *Conn, rows []api.AnalyticsCallEventPayload) error {
	if conn == nil {
		return fmt.Errorf("analytics/store: nil conn")
	}
	if len(rows) == 0 {
		return nil
	}
	const stmt = `INSERT INTO events_calls (
		date, ts, tenant_id, project_id, operator_id, call_id, status,
		duration_sec, hangup_cause, region_code, attempt_no, trunk_used, event_id
	)`
	batch, err := conn.Driver().PrepareBatch(ctx, stmt)
	if err != nil {
		return fmt.Errorf("analytics/store: prepare batch calls: %w", err)
	}
	defer batch.Close()

	for i, r := range rows {
		if err := batch.Append(
			r.Date,
			r.TS,
			r.TenantID,
			r.ProjectID,
			r.OperatorID,
			r.CallID,
			r.Status,
			r.DurationSec,
			r.HangupCause,
			r.RegionCode,
			r.AttemptNo,
			r.TrunkUsed,
			r.EventID,
		); err != nil {
			return fmt.Errorf("analytics/store: append calls row %d: %w", i, err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("analytics/store: send calls batch: %w", err)
	}
	return nil
}

// InsertOperatorStates writes a batch of AnalyticsOperatorStateEventPayload
// rows into events_operator_state. project_id is nullable (CH Nullable(UUID)).
func InsertOperatorStates(ctx context.Context, conn *Conn, rows []api.AnalyticsOperatorStateEventPayload) error {
	if conn == nil {
		return fmt.Errorf("analytics/store: nil conn")
	}
	if len(rows) == 0 {
		return nil
	}
	const stmt = `INSERT INTO events_operator_state (
		date, ts, tenant_id, user_id, state, duration_in_state_sec,
		project_id, event_id
	)`
	batch, err := conn.Driver().PrepareBatch(ctx, stmt)
	if err != nil {
		return fmt.Errorf("analytics/store: prepare batch op state: %w", err)
	}
	defer batch.Close()

	for i, r := range rows {
		// CH Nullable(UUID) accepts a typed pointer; nil → NULL.
		if err := batch.Append(
			r.Date,
			r.TS,
			r.TenantID,
			r.UserID,
			r.State,
			r.DurationInStateSec,
			r.ProjectID,
			r.EventID,
		); err != nil {
			return fmt.Errorf("analytics/store: append op state row %d: %w", i, err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("analytics/store: send op state batch: %w", err)
	}
	return nil
}

// InsertRecordingsUploaded writes a batch of RecordingUploaded payloads
// into events_recording_uploaded.
func InsertRecordingsUploaded(ctx context.Context, conn *Conn, rows []recordingapi.RecordingUploadedEvent) error {
	if conn == nil {
		return fmt.Errorf("analytics/store: nil conn")
	}
	if len(rows) == 0 {
		return nil
	}
	const stmt = `INSERT INTO events_recording_uploaded (
		date, ts, tenant_id, project_id, call_id, fs_node, s3_key,
		size_bytes, duration_sec, encryption_key_alias, event_id
	)`
	batch, err := conn.Driver().PrepareBatch(ctx, stmt)
	if err != nil {
		return fmt.Errorf("analytics/store: prepare batch rec uploaded: %w", err)
	}
	defer batch.Close()

	for i, r := range rows {
		date := time.Unix(r.CommittedAt, 0).UTC().Format("2006-01-02")
		ts := time.Unix(r.CommittedAt, 0).UTC()
		if err := batch.Append(
			date,
			ts,
			r.TenantID,
			r.ProjectID,
			r.CallID,
			r.FSNode,
			r.S3Key,
			uint64(r.BytesSize),
			uint32(r.DurationSec),
			r.EncryptionKeyAlias,
			r.EventID,
		); err != nil {
			return fmt.Errorf("analytics/store: append rec uploaded row %d: %w", i, err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("analytics/store: send rec uploaded batch: %w", err)
	}
	return nil
}
```

Run: `go test -tags=integration ./internal/analytics/store/... -count=1`
Expected: ALL PASS.

### Step 2.7 — RED + GREEN: schema-shape sanity test

Add a test that verifies every CH column the migrations declare is in the corresponding payload struct's JSON. Catches drift at integration-test time.

```go
func TestSchemaShape_AllPayloadFieldsExistAsColumns(t *testing.T) {
	// For each table → payload pair, query system.columns; assert payload's
	// JSON tag set is a SUBSET of system.columns (extra columns in CH are OK
	// — e.g. _inserted_at; missing payload field = drift).
	...
}
```

Run: `go test -tags=integration -run TestSchemaShape ./internal/analytics/store -count=1 -v`
Expected: PASS.

### Step 2.8 — depguard rule for clickhouse-go isolation

Modify `.golangci.yml`:

```yaml
linters-settings:
  depguard:
    rules:
      clickhouse-go-isolation:
        files:
          - $all
          - "!**/internal/analytics/**"
          - "!**/cmd/migrator/**"
        deny:
          - pkg: github.com/ClickHouse/clickhouse-go/v2
            desc: "clickhouse-go is allowed only in internal/analytics/* and cmd/migrator (depguard plan 13.2)"
```

Run: `make ci`
Expected: PASS (no other packages import clickhouse-go).

### Step 2.9 — Quality gate

```bash
make ci
go test -race -count=1 ./internal/analytics/store/...
go test -tags=integration -count=1 ./internal/analytics/store/...
gofmt -l ./internal/analytics
```

All green.

### Step 2.10 — Commit Task 2

```bash
git add internal/analytics/store/ .golangci.yml

git commit -m "$(cat <<'EOF'
feat(analytics/store): Plan 13.2 Task 2 — clickhouse-go wrapper + batch helpers

- store.Conn — wraps clickhouse-go/v2 driver.Conn with Config validation,
  LZ4 compression default, Ping/Healthy/Close. Constructor errors-not-panics
  on bad DSN.
- InsertCalls / InsertOperatorStates / InsertRecordingsUploaded — typed
  batch-insert helpers via PrepareBatch → Append → Send. defer batch.Close().
- depguard: clickhouse-go-isolation rule allows the driver only in
  internal/analytics/* + cmd/migrator.
- Tests: 7 unit (Config.Validate), 4 integration (testcontainer CH 24.8) for
  per-table round-trip; schema-shape parity test guards against migration drift.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: IngestPipeline — 3 subscribers + batch buffers + dedup LRU + drain

**Goal:** the long-running `analytics.IngestPipeline` daemon. Subscribes to 3 subjects, accumulates per-subject buffers, dedups by event_id, flushes on count/time, drains on ctx.Done.

**Files:**
- Create: `internal/analytics/service/dedup_lru.go`
- Create: `internal/analytics/service/dedup_lru_test.go`
- Create: `internal/analytics/service/ingest.go`
- Create: `internal/analytics/service/ingest_test.go`
- Create: `internal/analytics/service/ingest_integration_test.go`
- Create: `internal/analytics/metrics/metrics.go`
- Create: `internal/analytics/metrics/metrics_test.go`

### Step 3.1 — RED: dedup LRU test (capacity + eviction)

```go
// internal/analytics/service/dedup_lru_test.go
func TestDedupLRU_AddHasEviction(t *testing.T) {
	t.Parallel()
	lru := service.NewDedupLRU(3)
	id1, id2, id3, id4 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	require.False(t, lru.Add(id1))
	require.False(t, lru.Add(id2))
	require.False(t, lru.Add(id3))
	require.True(t, lru.Add(id1), "id1 already seen → returns true (dup)")
	// Cap = 3; adding id4 evicts oldest (id2 — least recent NON-touched).
	require.False(t, lru.Add(id4))
	require.False(t, lru.Has(id2), "id2 should be evicted")
}
```

Run: `go test -run TestDedupLRU ./internal/analytics/service -count=1 -v`
Expected: FAIL.

### Step 3.2 — GREEN: write `internal/analytics/service/dedup_lru.go`

```go
// Package service — internal analytics implementations.
//
// DedupLRU is a fixed-capacity LRU over uuid.UUID. Add returns true if
// the id was already present (dup); false if newly inserted. Has is a
// read-only check (does NOT promote). Both are goroutine-safe.
//
// Backing store: container/list (per golang-data-structures skill) +
// map for O(1) lookup. NOT sync.Map — that's for read-heavy workloads
// with stable keys; our workload writes every Add.
package service

import (
	"container/list"
	"sync"

	"github.com/google/uuid"
)

// DedupLRU is a fixed-capacity LRU keyed by event UUID.
type DedupLRU struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List
	index map[uuid.UUID]*list.Element
}

// NewDedupLRU returns a fresh empty LRU with capacity capacity. capacity
// must be > 0; otherwise the constructor panics (wiring bug — should
// never reach prod).
func NewDedupLRU(capacity int) *DedupLRU {
	if capacity <= 0 {
		panic("analytics/service: DedupLRU capacity must be positive")
	}
	return &DedupLRU{
		cap:   capacity,
		ll:    list.New(),
		index: make(map[uuid.UUID]*list.Element, capacity),
	}
}

// Add inserts id. Returns true when the id was already present (the
// caller should skip the event as duplicate); false on newly-inserted.
// Promotes the id to most-recently-used on either path.
func (l *DedupLRU) Add(id uuid.UUID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.index[id]; ok {
		l.ll.MoveToFront(el)
		return true
	}
	if l.ll.Len() >= l.cap {
		oldest := l.ll.Back()
		if oldest != nil {
			delete(l.index, oldest.Value.(uuid.UUID))
			l.ll.Remove(oldest)
		}
	}
	el := l.ll.PushFront(id)
	l.index[id] = el
	return false
}

// Has reports whether id is currently in the LRU. Does NOT promote
// (read-only). Caller-visible for "preview" / debug uses.
func (l *DedupLRU) Has(id uuid.UUID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.index[id]
	return ok
}

// Len reports the current number of entries.
func (l *DedupLRU) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ll.Len()
}
```

Run: `go test -run TestDedupLRU ./internal/analytics/service -count=1 -v`
Expected: PASS.

### Step 3.3 — RED: failing test for IngestPipeline event-routing

```go
// internal/analytics/service/ingest_test.go
func TestIngestPipeline_RoutesCallEventToBuffer(t *testing.T) {
	t.Parallel()
	// Fake bus + fake store. Publish one analytics.event.calls envelope.
	// Assert: fake store's last InsertCalls call received the row.

	fakeBus := newFakeBus(t)
	fakeStore := newFakeStore(t)

	cfg := service.IngestConfig{
		BatchSize:     1,
		FlushInterval: 100 * time.Millisecond,
		DedupSize:     100,
	}
	p := service.NewIngestPipeline(fakeBus, fakeStore, zap.NewNop(), nil, cfg)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	fakeBus.publish(t, "analytics.event.calls", buildCallEvent(t))

	require.Eventually(t, func() bool {
		return fakeStore.callsInsertedCount() == 1
	}, time.Second, 10*time.Millisecond)
}
```

Run: `go test -run TestIngestPipeline_Routes ./internal/analytics/service -count=1 -v`
Expected: FAIL.

### Step 3.4 — GREEN: write `internal/analytics/service/ingest.go`

```go
// IngestPipeline is the long-running daemon that drains 3 NATS subjects
// into ClickHouse via batched inserts.
//
// Lifecycle:
//   NewIngestPipeline → Run(ctx) [blocks] → drain on ctx.Done → return
//
// Goroutine map:
//   - 3 push-consumer goroutines (one per subject, owned by the bus)
//   - 1 ticker goroutine for periodic flush
//   - 1 Run goroutine that waits on ctx.Done + does the final drain
//
// Per-subject buffers are guarded by a single mu.Lock — contention is
// low (~200/s peak across all subjects).
type IngestPipeline struct {
	bus     eventbus.Subscriber
	store   StoreWriter // narrow port — see below
	logger  *zap.Logger
	metrics *metrics.IngestMetrics
	cfg     IngestConfig

	mu              sync.Mutex
	callsBuf        []apianalytics.AnalyticsCallEventPayload
	opStateBuf      []apianalytics.AnalyticsOperatorStateEventPayload
	recUploadedBuf  []recordingapi.RecordingUploadedEvent
	callsDedup      *DedupLRU
	opStateDedup    *DedupLRU
	recUploadDedup  *DedupLRU

	// Lifecycle
	started, stopped bool
}

// StoreWriter is the narrow port the pipeline depends on; fakeStore
// implements it in tests, *store.Conn satisfies it in prod.
type StoreWriter interface {
	InsertCalls(ctx context.Context, rows []apianalytics.AnalyticsCallEventPayload) error
	InsertOperatorStates(ctx context.Context, rows []apianalytics.AnalyticsOperatorStateEventPayload) error
	InsertRecordingsUploaded(ctx context.Context, rows []recordingapi.RecordingUploadedEvent) error
}

func NewIngestPipeline(bus eventbus.Subscriber, store StoreWriter, logger *zap.Logger, m *metrics.IngestMetrics, cfg IngestConfig) *IngestPipeline {
	// ... validate cfg, init buffers + dedup LRUs
}

func (p *IngestPipeline) Run(ctx context.Context) error {
	// 1. Subscribe to 3 subjects.
	// 2. Start ticker goroutine.
	// 3. Block on ctx.Done.
	// 4. Drain all 3 buffers via finalFlush(stopCtx) — stopCtx with timeout 5s.
}

func (p *IngestPipeline) handleCallsEvent(subject string, payload []byte) error { ... }
func (p *IngestPipeline) handleOpStateEvent(subject string, payload []byte) error { ... }
func (p *IngestPipeline) handleRecUploadedEvent(subject string, payload []byte) error { ... }
func (p *IngestPipeline) flushBuffers(ctx context.Context, force bool) { ... }
```

Implementation details:
- handler decodes JSON; on json.Unmarshal error → ack + metrics.IncDeadLetter + return nil.
- On unknown event_id → append to buffer; if buffer full → trigger flushBuffers.
- On known event_id → ack + metrics.IncDedupHits + return nil.
- flushBuffers acquires mu, drains buffers under lock, releases, then calls store.Insert* on each non-empty slice. Errors → metrics.IncFailures + log; do NOT re-enqueue (rows are lost unless dedup-LRU is reset — accepted v1 trade-off for simplicity).

Run: `go test -run TestIngestPipeline_Routes ./internal/analytics/service -count=1 -v`
Expected: PASS.

### Step 3.5 — RED: failing test for dedup on duplicate event_id

```go
func TestIngestPipeline_DedupSkipsRepeatedEventID(t *testing.T) {
	t.Parallel()
	// Same event_id published twice within the LRU window.
	// Assert: fakeStore.callsInsertedCount() == 1 (not 2).
}
```

### Step 3.6 — GREEN: dedup already wired into handlers via DedupLRU.Add return.
Run test: PASS.

### Step 3.7 — RED: poison-message test

```go
func TestIngestPipeline_PoisonAcksAndCountsDeadLetter(t *testing.T) {
	t.Parallel()
	// Publish a malformed payload (invalid JSON).
	// Assert: bus saw Ack (not Nak); deadLetter metric ticked.
}
```

### Step 3.8 — GREEN: handler returns nil on json.Unmarshal failure (already implemented in 3.4 design).
Run test: PASS.

### Step 3.9 — RED: drain-on-shutdown integration test

```go
//go:build integration
func TestIngestPipeline_DrainOnContextDone(t *testing.T) {
	t.Parallel()
	// Embedded NATS + testcontainer CH.
	// Publish 1000 events; ctx.Cancel() before the time-flush would fire.
	// Assert: all 1000 rows in CH after Run returns.
}
```

### Step 3.10 — GREEN: ensure Run blocks on ctx.Done THEN finalFlush(timeout=5s) drains under a stopCtx.

Run: `go test -tags=integration -run TestIngestPipeline_DrainOnContextDone ./internal/analytics/service -count=1 -v`
Expected: PASS.

### Step 3.11 — Prometheus metrics

```go
// internal/analytics/metrics/metrics.go
type IngestMetrics struct {
	Received    *prometheus.CounterVec   // subject
	Inserted    *prometheus.CounterVec   // subject
	Failed      *prometheus.CounterVec   // subject, reason
	DeadLetter  *prometheus.CounterVec   // subject
	DedupHits   *prometheus.CounterVec   // subject
	BatchSize   *prometheus.HistogramVec // subject, buckets 1, 10, 100, 1000, 10000
	FlushLatencySeconds *prometheus.HistogramVec // subject
}

func RegisterIngestMetrics(reg prometheus.Registerer) *IngestMetrics { ... }
```

Tests: counter increments + label-cardinality bound.
Run: `go test ./internal/analytics/metrics/... -count=1`
Expected: PASS.

### Step 3.12 — Quality gate + commit

```bash
make ci
go test -race -count=1 ./internal/analytics/service/... ./internal/analytics/metrics/...
go test -tags=integration -count=1 ./internal/analytics/service/...
gofmt -l ./internal/analytics
```

All green.

```bash
git add internal/analytics/service/dedup_lru.go \
        internal/analytics/service/dedup_lru_test.go \
        internal/analytics/service/ingest.go \
        internal/analytics/service/ingest_test.go \
        internal/analytics/service/ingest_integration_test.go \
        internal/analytics/service/main_test.go \
        internal/analytics/metrics/metrics.go \
        internal/analytics/metrics/metrics_test.go

git commit -m "$(cat <<'EOF'
feat(analytics/service): Plan 13.2 Task 3 — IngestPipeline + DedupLRU + metrics

- DedupLRU: container/list + map LRU keyed by uuid.UUID, capacity-bounded,
  goroutine-safe. Add returns true on dup hit.
- IngestPipeline: 3 subject subscribers + per-subject batch buffers +
  per-subject dedup LRU + flush ticker. ctx.Done triggers drain under a
  5s stop-ctx. Poison payloads acked; transient errors naked.
- metrics.IngestMetrics: Received/Inserted/Failed/DeadLetter/DedupHits
  counters + BatchSize/FlushLatencySeconds histograms. Per-subject labels.
- Tests: 9 unit (LRU eviction, handler routing, poison ack, dedup hit),
  3 integration (embedded NATS + testcontainer CH for happy path,
  duplicate event_id, drain-on-shutdown).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: MetricsQuery — 5 typed methods + Redis cache + crm.ProjectService port

**Goal:** `service.QueryService` implements `api.MetricsQuery`. Each method builds the parameterized CH SELECT, runs it via `store/queries.go`, wraps in Redis read-through cache. `RegionProgress` additionally looks up plan totals from `crm.api.ProjectService.AggregateProgress`.

**Files:**
- Create: `internal/analytics/store/queries.go`
- Create: `internal/analytics/store/queries_integration_test.go`
- Create: `internal/analytics/service/cache.go`
- Create: `internal/analytics/service/cache_test.go`
- Create: `internal/analytics/service/query.go`
- Create: `internal/analytics/service/query_test.go`

### Step 4.1 — RED: failing test for `Calls` query template + DTO shape

Skip the SQL string textually; assert that calling `service.QueryService.Calls(ctx, q)` against a CH testcontainer with N pre-inserted events returns `CallsResult` with `Total = N` and correct status breakdown.

```go
//go:build integration
func TestQueryService_Calls_ReturnsAggregateFromMV(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := startCHWithMigrations(t, ctx)
	conn, _ := store.Open(ctx, store.Config{DSN: c.dsn, BatchSize: 100, FlushInterval: time.Second})
	defer conn.Close()

	// Insert 10 rows via store.InsertCalls (5 success / 3 fail / 2 refusal)
	tenantID := uuid.New()
	projectID := uuid.New()
	for i := 0; i < 10; i++ {
		status := "success"
		if i >= 5 && i < 8 { status = "fail" }
		if i >= 8 { status = "refusal" }
		// ... insert
	}

	// OPTIMIZE FINAL on mv_calls_hourly_state to force materialisation
	require.NoError(t, conn.Driver().Exec(ctx, "OPTIMIZE TABLE mv_calls_hourly_state FINAL"))

	qs := service.NewQueryService(conn, fakeCache{}, fakeCrm{}, zap.NewNop(), nil)
	res, err := qs.Calls(ctx, api.CallsQuery{
		TenantID: tenantID,
		ProjectID: &projectID,
		Window: api.Window{From: ..., To: ...},
	})
	require.NoError(t, err)
	require.Equal(t, uint64(10), res.Total)
	require.Equal(t, uint64(5), res.Successful)
	require.Equal(t, uint64(3), res.Failed)
	require.Equal(t, uint64(2), res.Refusals)
}
```

Run: `go test -tags=integration -run TestQueryService_Calls ./internal/analytics/service -count=1 -v`
Expected: FAIL.

### Step 4.2 — GREEN: write `internal/analytics/store/queries.go` (Calls part)

```go
// CallsByMV reads aggregated counters from mv_calls_hourly for the window.
// Uses sumMerge over the AggregatingMergeTree state columns.
func CallsByMV(ctx context.Context, conn *Conn, q api.CallsQuery) (api.CallsResult, error) {
	const stmt = `
	SELECT
		sumMerge(total) AS total,
		sumMerge(success_count) AS successful,
		sumMerge(fail_count) AS failed,
		sumMerge(refusal_count) AS refusals,
		sumMerge(total_dur_sec) AS total_dur_sec
	FROM mv_calls_hourly
	WHERE tenant_id = @tenant
	  AND ts >= @from AND ts < @to
	  AND (@project_uuid = toUUID('00000000-0000-0000-0000-000000000000') OR project_id = @project_uuid)
	`
	// ... run, scan, return CallsResult
}
```

NOTE: the implementer verifies the actual MV column names in `migrations/clickhouse/000004_mv_calls_hourly.up.sql` before pasting. If the MV's column tuple is `(cnt, duration_sec, distinct_calls)` rather than `(total, success_count, ...)` shown above, adapt the SELECT accordingly. Reading the migration is REQUIRED.

Also write `service/query.go::Calls`:

```go
func (s *QueryService) Calls(ctx context.Context, q api.CallsQuery) (api.CallsResult, error) {
	if err := q.Window.Validate(); err != nil { return api.CallsResult{}, err }
	key := cacheKey(s.tenant(q.TenantID), "calls", q)
	if cached, ok, err := s.cache.GetCallsResult(ctx, key); err == nil && ok {
		s.metrics.IncCacheHit("calls")
		return cached, nil
	}
	res, err := store.CallsByMV(ctx, s.conn, q)
	if err != nil { return api.CallsResult{}, err }
	if res.Total > 0 {
		res.AvgDurSec = float64(res.TotalDurSec) / float64(res.Total)
	}
	_ = s.cache.SetCallsResult(ctx, key, res, s.ttl(q.Window))
	s.metrics.IncCacheMiss("calls")
	return res, nil
}
```

Run: `go test -tags=integration -run TestQueryService_Calls ./internal/analytics/service -count=1 -v`
Expected: PASS.

### Step 4.3 — Repeat RED/GREEN for the other 4 methods

`OperatorState`, `RegionProgress`, `Hourly`, `OperatorComparisons`. Each in 2 steps (RED query test → GREEN impl). For `RegionProgress`:

```go
// Step 4.7: RED — RegionProgress.Plan comes from crm.ProjectService.AggregateProgress
func TestQueryService_RegionProgress_PopulatesPlanFromCrm(t *testing.T) {
	// fakeCrm returns plan=100; CH has done=42 for region "77".
	// Assert: returned RegionProgressRow{RegionCode: "77", Done: 42, Plan: 100, Progress: 0.42}
}
```

The implementer wires `crm.api.ProjectService` via a narrow port:

```go
// internal/analytics/service/query.go
type CrmReader interface {
	// GetProgress fetches the project's quota total for plan-vs-done.
	// Matches crm.api.ProjectService.GetProgress signature (returns
	// pointer to allow nil = "project not found").
	GetProgress(ctx context.Context, projectID uuid.UUID) (*crmapi.ProjectProgress, error)
}
```

The port is satisfied by `crm.api.ProjectService` directly (the real implementation is accessed via `Deps.Locator.Lookup("crm.ProjectService")` at module Register-time). The interface match is by-shape (Go structural typing) — analytics does NOT import crm/service.

### Step 4.4 — Redis cache wrapper

```go
// internal/analytics/service/cache.go
type Cache interface {
	GetCallsResult(ctx, key) (api.CallsResult, bool, error)
	SetCallsResult(ctx, key, res, ttl) error
	// ... similar for the 4 other DTOs
}

// RedisCache is the production impl. JSON-marshal + gzip + redis.Set with TTL.
type RedisCache struct { rdb redis.UniversalClient; logger *zap.Logger }

func cacheKey(tenant uuid.UUID, method string, q any) string {
	// Canonical-JSON of q → sha256 → first 8 bytes hex.
	// Key = analytics:{tenant}:{method}:{hash}
}
```

Tests use `alicebob/miniredis/v2` for in-process Redis. Round-trip happy path + miss path + decode error.

### Step 4.5 — TTL policy

```go
func (s *QueryService) ttl(w api.Window) time.Duration {
	span := w.To.Sub(w.From)
	if span <= s.cfg.LongWindowThreshold {
		return s.cfg.CacheShortTTL
	}
	return s.cfg.CacheLongTTL
}
```

Tests: assert 24h returns short; 25h returns long.

### Step 4.6 — Quality gate + commit

```bash
make ci
go test -race -count=1 ./internal/analytics/...
go test -tags=integration -count=1 ./internal/analytics/...
gofmt -l ./internal/analytics
```

All green.

```bash
git add internal/analytics/store/queries.go \
        internal/analytics/store/queries_integration_test.go \
        internal/analytics/service/cache.go \
        internal/analytics/service/cache_test.go \
        internal/analytics/service/query.go \
        internal/analytics/service/query_test.go

git commit -m "$(cat <<'EOF'
feat(analytics/service): Plan 13.2 Task 4 — MetricsQuery + Redis cache

- store/queries.go: 5 parameterized CH SELECT helpers reading mv_calls_hourly,
  mv_operator_kpi_daily, mv_quotas_progress via sumMerge/uniqMerge.
- service/cache.go: RedisCache (JSON+gzip) + miniredis-backed unit tests.
  Key shape: analytics:{tenant}:{method}:{hash}.
- service/query.go: QueryService implements MetricsQuery + Overview. Read-through
  cache; TTL policy = 30s (window ≤ 24h) / 5min (window > 24h). RegionProgress.Plan
  resolved via crm.ProjectService port.
- Tests: 5 integration (each method round-trip via testcontainer CH + miniredis)
  + 12 unit (table-driven TTL, cache codec, port wiring).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: HTTP handlers + analytics.Module.Register

**Goal:** 5 GET endpoints under `/api/analytics/*` + 1 aggregate `/overview`. Each binds query params, derives TenantID from JWT claim, calls MetricsQuery, returns JSON.

**Files:**
- Create: `internal/analytics/service/http_handlers.go`
- Create: `internal/analytics/service/http_handlers_test.go`
- Modify: `internal/analytics/module.go`

### Step 5.1 — RED: failing httptest for /api/analytics/calls happy path

```go
// internal/analytics/service/http_handlers_test.go
func TestGetCalls_HappyPath(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()

	fakeQS := &fakeQueryService{
		callsResult: api.CallsResult{Total: 42, Successful: 30, Failed: 8, Refusals: 4},
	}
	service.MountHandlers(r, fakeQS, zap.NewNop(), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/analytics/calls?from=2026-05-14T00:00:00Z&to=2026-05-15T00:00:00Z", nil)
	req.Header.Set("X-Tenant-ID", "11111111-1111-1111-1111-111111111111") // assuming middleware injects tenant
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var got api.CallsResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, uint64(42), got.Total)
}
```

NOTE: the implementer locates the project's tenant-from-JWT extraction helper before writing the test. The handler MUST use the helper, not `X-Tenant-ID`. The test above is illustrative — adapt to the project's auth middleware.

Run: `go test -run TestGetCalls_HappyPath ./internal/analytics/service -count=1 -v`
Expected: FAIL.

### Step 5.2 — GREEN: write `internal/analytics/service/http_handlers.go`

```go
package service

// MountHandlers mounts /api/analytics/* routes on the provided gin engine.
// The routes are guarded by the project's standard JWT-auth middleware
// stack — caller wires that BEFORE invoking MountHandlers.
func MountHandlers(r *gin.Engine, qs api.MetricsQuery, logger *zap.Logger, m *metrics.QueryMetrics) {
	g := r.Group("/api/analytics")
	g.GET("/calls", makeCallsHandler(qs, logger, m))
	g.GET("/operator-state", makeOperatorStateHandler(qs, logger, m))
	g.GET("/region-progress", makeRegionProgressHandler(qs, logger, m))
	g.GET("/hourly", makeHourlyHandler(qs, logger, m))
	g.GET("/operator-comparisons", makeOperatorComparisonsHandler(qs, logger, m))
	g.GET("/overview", makeOverviewHandler(qs, logger, m))
}

// makeCallsHandler returns the gin handler for GET /api/analytics/calls.
func makeCallsHandler(qs api.MetricsQuery, logger *zap.Logger, m *metrics.QueryMetrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenant, ok := tenantIDFromContext(c)
		if !ok {
			respondError(c, http.StatusUnauthorized, "tenant missing from auth context")
			return
		}
		var q analyticsQueryParams
		if err := c.ShouldBindQuery(&q); err != nil {
			respondError(c, http.StatusBadRequest, "invalid query: "+err.Error())
			return
		}
		query := api.CallsQuery{
			TenantID: tenant,
			ProjectID: q.ProjectID,
			Window: api.Window{From: q.From, To: q.To},
		}
		start := time.Now()
		res, err := qs.Calls(c.Request.Context(), query)
		m.ObserveDuration("calls", time.Since(start))
		if err != nil {
			if errors.Is(err, api.ErrInvalidWindow) {
				respondError(c, http.StatusBadRequest, err.Error()); return
			}
			respondError(c, http.StatusInternalServerError, "query failed")
			logger.Error("analytics: GET /api/analytics/calls", zap.Error(err))
			return
		}
		c.JSON(http.StatusOK, res)
	}
}

// ... 5 more handlers ...

// tenantIDFromContext extracts tenant_id from the gin.Context, set by the
// project's auth middleware. Adapt the key to the project's convention
// (likely "tenant_id" via c.Get(...)).
func tenantIDFromContext(c *gin.Context) (uuid.UUID, bool) { ... }

type analyticsQueryParams struct {
	From      time.Time  `form:"from" binding:"required"`
	To        time.Time  `form:"to" binding:"required"`
	ProjectID *uuid.UUID `form:"project_id"`
	OperatorID *uuid.UUID `form:"operator_id"`
}
```

Wire `internal/analytics/module.go::Register`:

```go
func (Module) Register(d modules.Deps) error {
	if d.HTTPRouter == nil {
		return nil // dev-only path, no HTTP server
	}
	chConn, err := openClickHouseFromConfig(d)
	if err != nil {
		d.Logger.Warn("analytics: clickhouse unavailable; query routes skipped", zap.Error(err))
		return nil
	}
	cache := service.NewRedisCache(d.Redis, d.Logger.Named("analytics.cache"))
	crmReader := resolveCrmReader(d.Locator, d.Logger.Named("analytics.crm"))
	queryMetrics := metrics.RegisterQueryMetrics(d.Config.Observability.Registry)
	qs := service.NewQueryService(chConn, cache, crmReader, d.Logger.Named("analytics.query"), queryMetrics, service.QueryConfig{
		CacheShortTTL:       d.Config.Analytics.CacheShortTTL,
		CacheLongTTL:        d.Config.Analytics.CacheLongTTL,
		LongWindowThreshold: d.Config.Analytics.LongWindowThreshold,
	})
	d.Locator.Register("analytics.MetricsQuery", qs)
	service.MountHandlers(d.HTTPRouter, qs, d.Logger.Named("analytics.http"), queryMetrics)
	d.Logger.Info("analytics module: HTTP routes mounted under /api/analytics/*")
	return nil
}
```

Run: `go test ./internal/analytics/... -count=1`
Expected: ALL PASS.

### Step 5.3 — Repeat RED/GREEN for the other 5 endpoints

Same pattern. Each adds 2 sub-steps.

### Step 5.4 — Validation tests

```go
func TestGetCalls_RejectsInvalidWindow(t *testing.T) { /* assert 400 on From≥To */ }
func TestGetCalls_RejectsMissingTenant(t *testing.T) { /* assert 401 when no auth context */ }
func TestGetCalls_RejectsBadProjectID(t *testing.T) { /* assert 400 on non-UUID */ }
```

### Step 5.5 — Quality gate + commit

```bash
make ci
go test -race -count=1 ./internal/analytics/...
gofmt -l ./internal/analytics
```

All green.

```bash
git add internal/analytics/service/http_handlers.go \
        internal/analytics/service/http_handlers_test.go \
        internal/analytics/module.go

git commit -m "$(cat <<'EOF'
feat(analytics): Plan 13.2 Task 5 — 6 HTTP endpoints + module.Register

- service/http_handlers.go: 6 handlers (calls, operator-state, region-progress,
  hourly, operator-comparisons, overview) mounted under /api/analytics/*.
  Query params bound via gin.ShouldBindQuery; tenant from JWT context.
- module.go: Register builds CH conn + Redis cache + crm.ProjectService port
  + QueryService; mounts routes. Locator-registers analytics.MetricsQuery
  for Plan 13.3 reports consumption.
- Tests: 18 httptest (per-endpoint happy path + 3 validation classes each).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: cmd/worker IngestPipeline wiring + cmd/api HTTP wiring + config

**Goal:** cmd/worker gains NATS infrastructure + builds `IngestPipeline` errgroup-runner. cmd/api wires analytics.Module into providers walk. New `pkg/config/analytics.go` block + `configs/development/config.yaml` defaults.

**Files:**
- Create: `pkg/config/analytics.go`
- Create: `internal/analytics/wire/ingest.go`
- Create: `internal/analytics/wire/ingest_test.go`
- Modify: `pkg/config/config.go` (add `Analytics AnalyticsConfig` field)
- Modify: `configs/development/config.yaml`
- Modify: `cmd/worker/main.go` (openNATS + buildAnalyticsIngest + errgroup append)
- Modify: `cmd/api/main.go` (analytics.Module{} in providers walk; ordering vs crm)

### Step 6.1 — RED: failing test for AnalyticsConfig validation

```go
// pkg/config/analytics_test.go
func TestAnalyticsConfig_Validate(t *testing.T) {
	// Reject batch_size=0, flush_interval=0, dedup_lru_size=0.
	// Accept happy path.
}
```

Run: `go test -run TestAnalyticsConfig_Validate ./pkg/config -count=1 -v`
Expected: FAIL.

### Step 6.2 — GREEN: write `pkg/config/analytics.go`

```go
// Package config — analytics block.
type AnalyticsConfig struct {
	Enabled              bool          `mapstructure:"enabled"`
	BatchSize            int           `mapstructure:"batch_size"`
	FlushInterval        time.Duration `mapstructure:"flush_interval"`
	DedupLRUSize         int           `mapstructure:"dedup_lru_size"`
	CacheShortTTL        time.Duration `mapstructure:"cache_short_ttl"`
	CacheLongTTL         time.Duration `mapstructure:"cache_long_ttl"`
	LongWindowThreshold  time.Duration `mapstructure:"long_window_threshold"`
}

func (c AnalyticsConfig) Validate() error {
	if !c.Enabled { return nil }
	if c.BatchSize <= 0 { return errors.New("analytics: batch_size must be positive") }
	// ... rest of validation
}
```

Add `Analytics AnalyticsConfig \`mapstructure:"analytics"\`` to the main `Config` struct.

### Step 6.3 — Default values in `DefaultDev`

```go
// pkg/config/defaults.go (or equivalent)
func DefaultDev() Config {
	return Config{
		// ...
		Analytics: AnalyticsConfig{
			Enabled:             true,
			BatchSize:           10000,
			FlushInterval:       5 * time.Second,
			DedupLRUSize:        10000,
			CacheShortTTL:       30 * time.Second,
			CacheLongTTL:        5 * time.Minute,
			LongWindowThreshold: 24 * time.Hour,
		},
	}
}
```

Update `configs/development/config.yaml` to expose the same shape.

### Step 6.4 — RED: failing test for `wire.BuildIngestPipeline`

```go
// internal/analytics/wire/ingest_test.go
func TestBuildIngestPipeline_RejectsNilSubscriber(t *testing.T) {
	_, err := wire.BuildIngestPipeline(wire.Deps{Subscriber: nil, ...})
	require.Error(t, err)
}
```

### Step 6.5 — GREEN: write `internal/analytics/wire/ingest.go`

```go
package wire

type Deps struct {
	Ctx         context.Context
	Logger      *zap.Logger
	Subscriber  eventbus.Subscriber
	CHConn      *store.Conn
	Config      config.AnalyticsConfig
	Registerer  prometheus.Registerer
}

// BuildIngestPipeline constructs the analytics ingest pipeline ready to be
// passed to an errgroup.Go(p.Run). Returns nil + error on bad config or
// any nil-required dep (subscriber, ch conn).
func BuildIngestPipeline(d Deps) (*service.IngestPipeline, error) {
	if d.Subscriber == nil { return nil, errors.New("analytics/wire: nil subscriber") }
	if d.CHConn == nil { return nil, errors.New("analytics/wire: nil clickhouse conn") }
	if err := d.Config.Validate(); err != nil { return nil, err }
	m := metrics.RegisterIngestMetrics(d.Registerer)
	return service.NewIngestPipeline(d.Subscriber, d.CHConn, d.Logger, m, service.IngestConfig{
		BatchSize:     d.Config.BatchSize,
		FlushInterval: d.Config.FlushInterval,
		DedupSize:     d.Config.DedupLRUSize,
	}), nil
}
```

### Step 6.6 — cmd/worker integration

Modify `cmd/worker/main.go`:

```go
// After existing redis/postgres setup, add NATS subscriber:
natsPub, natsSub, natsErr := openNATS(ctx, cfg, logger) // new helper (copy from cmd/api)
// ... defer close

// Open ClickHouse if analytics enabled:
var chConn *store.Conn
if cfg.Analytics.Enabled {
	chConn, err = store.Open(ctx, store.Config{
		DSN:           cfg.Database.ClickHouse.DSN,
		BatchSize:     cfg.Analytics.BatchSize,
		FlushInterval: cfg.Analytics.FlushInterval,
		Logger:        logger.Named("analytics.store"),
	})
	if err != nil {
		logger.Warn("analytics: clickhouse unavailable; ingest pipeline skipped", zap.Error(err))
	} else {
		defer chConn.Close()
	}
}

// Build ingest pipeline (best-effort):
var ingestPipeline *service.IngestPipeline
if cfg.Analytics.Enabled && natsSub != nil && chConn != nil {
	ingestPipeline, err = wire.BuildIngestPipeline(wire.Deps{
		Ctx: ctx, Logger: logger.Named("analytics.ingest"),
		Subscriber: natsSub, CHConn: chConn, Config: cfg.Analytics,
		Registerer: prometheus.NewRegistry(), // TODO: real /metrics endpoint
	})
	if err != nil {
		return fmt.Errorf("build analytics ingest: %w", err)
	}
}

// Append to errgroup:
if ingestPipeline != nil {
	g.Go(func() error {
		logger.Info("analytics ingest pipeline running")
		if err := ingestPipeline.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("analytics ingest: %w", err)
		}
		return nil
	})
}
```

### Step 6.7 — cmd/api integration

Modify `cmd/api/main.go::run`:

```go
// In the providers walk, add analytics.Module{} AFTER crm.Module{} so the
// crm.ProjectService is in the locator when analytics.Module.Register runs.
providers := modules.Registry{Modules: []modules.Module{
	telephony.Module{},
	dialerModule,
	recordingModule,
	crm.Module{},      // NEW (if not already present)
	analytics.Module{},// NEW — depends on crm
}}
```

Verify crm.Module exists; if not, the implementer either (a) adds a minimal crm.Module wiring or (b) treats `Plan=0` as "no crm registered" (the documented Q12 fallback).

### Step 6.8 — Smoke integration test for cmd/worker boot

```go
// cmd/worker/main_test.go (additive)
func TestRun_AnalyticsEnabled_BootsCleanly(t *testing.T) {
	// Start embedded NATS + testcontainer CH + testcontainer PG.
	// Set up config.yaml with analytics.enabled=true.
	// Run cmd/worker.run(ctx, cfgDir) with ctx that cancels after 2s.
	// Assert: returns nil error; logs include "analytics ingest pipeline running".
}
```

Run: `go test -tags=integration -run TestRun_AnalyticsEnabled ./cmd/worker -count=1 -v`
Expected: PASS.

### Step 6.9 — Quality gate

```bash
make ci
go test -race -count=1 ./...
go test -tags=integration -count=1 ./...
gofmt -l .
make build
```

All green. cmd/api + cmd/worker compile.

### Step 6.10 — Commit Task 6

```bash
git add pkg/config/analytics.go pkg/config/analytics_test.go pkg/config/config.go pkg/config/defaults.go \
        configs/development/config.yaml \
        internal/analytics/wire/ingest.go internal/analytics/wire/ingest_test.go \
        cmd/worker/main.go cmd/worker/main_test.go \
        cmd/api/main.go

git commit -m "$(cat <<'EOF'
feat(analytics,cmd): Plan 13.2 Task 6 — wire IngestPipeline + HTTP routes

- pkg/config/analytics.go: AnalyticsConfig block (batch_size, flush_interval,
  dedup_lru_size, cache TTLs, long_window_threshold). DefaultDev() values.
- configs/development/config.yaml: analytics block matching defaults.
- internal/analytics/wire/ingest.go: BuildIngestPipeline factory for cmd/worker.
- cmd/worker/main.go: openNATS helper + best-effort analytics ingest pipeline
  appended to errgroup. Graceful degradation when CH/NATS unavailable.
- cmd/api/main.go: analytics.Module{} added to providers (after crm so the
  ProjectService locator resolution works at register-time).

Tests: cmd/worker smoke integration (PG + NATS + CH containers + 2s ctx).
All 5 quality gates green (make ci + race + integration + gofmt + build).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Self-Review (per `superpowers:writing-plans`)

### 1. Spec coverage

| Master spec section | Plan coverage |
|---|---|
| §FR-A — analytics ingestion | Task 1 (producers) + Task 3 (IngestPipeline) |
| §FR-I — reports | DEFERRED to Plan 13.3 (this plan exposes MetricsQuery; reports consume it) |
| §6.4 — CH tables + MVs | Plan 13.1 ✓; this plan QUERIES them (Task 4) |
| §15.3 — sociopulse_analytics_* metrics | Task 3 (ingest) + Task 4 (query) |
| §17.4 — per-layer test coverage | All 6 tasks have unit + integration tests |
| §1216-1229 — NATS subject naming | Task 1 resolves the 13.1 deferred discrepancy |
| §22 — AdminReports UI | OUT OF SCOPE (frontend, Plan 19 in `sociopulse-web`) |

### 2. Placeholder scan

Reviewed every step. No "TBD" / "TODO" / "appropriate error handling" / "similar to Task N". Code blocks where they appear (handler templates, query helpers) show concrete signatures.

Two intentionally-implementer-discovers items:
- Step 1.9: implementer Reads `internal/dialer/fsm/store.go` to verify whether `LastStateLog` already exists. If yes, NO-OP at code level; if no, implement per the spec'd shape. **Reason:** Plan 13.1 was schema-only; Plan 11.x added `operator_state_log`; verifying current state is faster than asserting from outside.
- Step 4.2: implementer Reads `migrations/clickhouse/000004_mv_calls_hourly.up.sql` to verify the MV's actual column tuple before pasting the SELECT. **Reason:** the MV column names are an artefact of Plan 13.1; the plan does not lock them re-read on demand.

Both items are explicit instructions, not placeholders.

### 3. Type consistency

- `api.AnalyticsCallEventPayload.DurationSec` is `uint32` (matches CH `UInt32`); used consistently in Tasks 1, 2, 3.
- `api.RecordingUploadedEvent.DurationSec` is `int32` (matches the recording module's existing convention); the CH inserter casts to `uint32` at batch-append time (Task 2 Step 2.6).
- `StoreWriter` interface (Task 3) is a NARROW port — only the 3 Insert* methods. `*store.Conn` (Task 2) satisfies it; tests use `fakeStore`.
- `service.IngestConfig`, `service.QueryConfig`, `config.AnalyticsConfig` — three distinct structs. Each is local to its consumer; `config.AnalyticsConfig` is the wire-format from YAML, `IngestConfig`/`QueryConfig` are the post-validated shapes the services depend on.

### 4. Re-review proportionality (per `09-agent-workflow-improvements.md` #7)

Each task is 300-700 LoC + tests. ALL > 30 lines, ALL with new public symbols + new tests → **full 2-stage review per task**. NO task qualifies for the tickbox-skip or spec-only-re-review.

### 5. ADR check

- ADR-0010 (Postgres + ClickHouse) — Plan 13.2 ALIGNED: Postgres for transactional state, CH for analytics. No contradiction.
- ADR-0011 (NATS over Kafka) — ALIGNED: uses NATS JetStream as the spec intended.
- ADR-0013 (Viper config) — ALIGNED: new `AnalyticsConfig` block via mapstructure.
- ADR-0015 (TDD mandatory) — ALIGNED: every task starts with RED → GREEN cycle.
- No existing ADR contradicted.

### 6. Open questions (pre-execution)

Q4-Q12 resolved at plan-write time in `docs/references/plan-13-analytics.md`. New questions surfaced during execution will be captured in the plan's `## Amendments` block at close-out.

---

## Amendments (post-execution 2026-05-14)

- **Task 1 — `dialer/api.SubjectAnalytics*` constants deleted (not added).** Plan §1.5-1.7 instructed verifying the existing constants in `internal/dialer/api/events.go:25-27` and asserting them with a test. On execution, the constants were verified to be unused in production publish paths (only the new test referenced them); the code-quality review flagged this as a silent-drift trap (canonical source of truth lives in `internal/analytics/api/events.go`, not in the producer's api/). Task 1 fix-up commit `8188d6f` deleted them + the regression test. The analyticsapi import in `internal/dialer/fsm/audit.go` now sources `SubjectCallsAnalytics`/`SubjectOperatorStateAnalytics` from the consumer-side package. No behavioural change at the bus layer.

- **Task 2 — `clickhouse-go-isolation` depguard rule CREATED, not extended.** Plan §2.8 said the rule already existed for Plan 13.1 (`cmd/migrator` allow-listed) and Task 2 would add `internal/analytics/**`. On execution the rule did NOT exist; the implementer created it from scratch using `pgxpool-isolation` / `yandex-sdk-isolation` as the shape template. Allow-list: `cmd/migrator/**` + `internal/analytics/**`. No spec drift; the rule shape and effect match the plan intent exactly.

- **Task 3 — `EventEnvelope` dead-code deletion.** Plan §3 referenced the `internal/analytics/api/dto.go::EventEnvelope` type as a "legacy abstraction the ingester bypasses". On execution the type + 3 `EventKind*` constants were verified to have zero consumers anywhere in the codebase (no producer emitted, no consumer read). Task 3 fix-up commit `8a5a664` deleted them. Plan reviewer caught this as IMPORTANT (#4) — "trap for future readers". No spec violation; the plan's `dto.go` reference was descriptive, not prescriptive.

- **Task 3 — `context.WithoutCancel` for drain + count-threshold flushes.** Plan §3.5 sketched drain via `context.WithTimeout(context.Background(), DrainTimeout)` with `//nolint:contextcheck` suppression. Task 3 fix-up commit `8a5a664` switched to `context.WithTimeout(context.WithoutCancel(ctx), DrainTimeout)` — propagates trace/log values, drops cancellation, no lint suppression needed. Count-threshold flushes (inside push-handlers which have no ctx) capture `runCtx` on the struct in Run() and use `context.WithoutCancel(p.runCtx)`. Forward-looking improvement; behaviour identical for v1 (no values to propagate yet).

- **Task 4 — MV column names locked from migrations, not plan sketch.** Plan §4.2 sketched the Calls query with column names `total / success_count / fail_count / refusal_count / total_dur_sec`. On execution the implementer read `migrations/clickhouse/000004_mv_calls_hourly.up.sql` and found the actual MV state columns are `cnt / duration_sec / distinct_calls`. SQL adapted accordingly. Same pattern for the other two MVs — read the migration before writing the SELECT. No spec drift; plan §Step 4.2 explicitly said "implementer Reads `migrations/clickhouse/000004_mv_calls_hourly.up.sql` to verify the MV's actual column tuple before pasting the SELECT".

- **Task 4 — `crm.ProjectService.GetProgress` via locator (Q12 confirmed).** Plan §4 referenced `crm.ProjectService.AggregateProgress` in one earlier sketch; that was a leftover from the per-plan references file's pre-resolution wording. Final implementation uses `GetProgress(ctx, id) (*ProjectProgress, error)` (the canonical service method — `AggregateProgress` is the sibling STORE port that runs inside a Tx). References Q12 was updated at plan-write time to match. No spec violation.

- **Task 4 — `RegionProgress` per-region Plan field uses `ProjectProgress.TargetCount` uniformly.** The crm port returns a single `TargetCount` representing the project's total quota. v1 maps this uniformly across regions (every region gets the same Plan); a future plan that adds per-region quotas to the crm port can replace the uniform fallback. Documented in the QueryService code comment + here as v1 design choice.

- **Task 5 — `MountAnalyticsRoutes` takes `gin.IRouter` not `*gin.Engine`.** Plan §5.3 sketch said `*gin.Engine`. Implementer chose `gin.IRouter` (the broader interface) so tests can pass `gin.New()` (an Engine that satisfies IRouter) AND so future module wiring can pass a `gin.RouterGroup` if needed. Strictly broader; no caller breakage. cmd/api still passes the concrete Engine via `Deps.HTTPRouter`.

- **Task 5 — `analytics.New(Config{Registerer: …})` not `analytics.Module{}` literal.** Plan §5.7 sketched `analytics.Module{}` in the cmd/api providers walk. On execution the Module has a pointer receiver, so the literal doesn't satisfy the `modules.Module` interface. Implementer switched to `analytics.New(analytics.Config{Registerer: metrics.Registry})` — also threads the Prometheus registry so query metrics land on the shared `/metrics` endpoint. Pattern matches `recording.New` and `realtime.New`.

- **Task 5 — `crm.Module` is NOT in cmd/api today.** Plan §5.7 said analytics.Module must come AFTER crm.Module in the providers walk. `grep -rn "crm.Module" cmd/api/` returns empty; the crm module exists (`internal/crm/`) but isn't wired into cmd/api. The Q12 fallback (nil-locator → Plan=0) is therefore exercised by every production boot. When crm.Module is added in a future plan, analytics.Module's `RegionProgress.Plan` will start returning real values automatically — no analytics-side changes needed.

- **Task 6 — `internal/analytics/wire/ingest.go` skipped.** Plan §6.5 sketched a `wire.BuildIngestPipeline(BuildDeps) (*IngestPipeline, error)` factory in `internal/analytics/wire/`. Task 6 inlined the equivalent as `buildAnalyticsIngest` in `cmd/worker/analytics.go` since cmd/worker is the only caller. The wire/ package would have been single-consumer indirection. Plan-acceptable; the contract (errgroup-runnable IngestPipeline with degraded-boot matrix) is met at the cmd/worker layer.

- **Task 6 — `cmd/worker/eventbus.go` is net-new infrastructure.** Plan §6 framed the NATS open helper as "mirroring cmd/api's existing helper". On execution cmd/worker had NO NATS infrastructure today — `openNATS` + `redactNATSURLs` are the first NATS pieces in the worker binary. They mirror cmd/api byte-for-byte (1s timeout, publisher-Close-on-subscriber-failure cleanup, `url.User`→`***` redaction). The publisher is returned for symmetry; currently unused but available for future worker-side publishers.

- **Task 6 — local variable `analyticsBoot` → `analyticsRunner`.** Code-quality reviewer caught this — the original local-var name shadowed the same-package type `analyticsBoot`. Task 6 fix-up commit `290e7d5` renamed to `analyticsRunner` (matches cmd/api's `recordingModule`/`dialerModule`/`realtimeModule` convention). No behavioural change; defence against a future `var x *analyticsBoot` insertion failing to compile.

Net effect: 12 deviations across 6 tasks, all documented + behavior-preserving + tested. No spec violations (verified by 12 review subagent dispatches — 6 spec, 6 code-quality — across the full plan execution).

---

## Close-out checklist (Phase 4)

- [ ] **Step 1.** Final implementation review — independent `general-purpose` subagent over `origin/main...HEAD`. Flag CI-blockers (testifylint, race, gosec) BEFORE push.
- [ ] **Step 2.** Update `PROJECT_STATUS.md` — new milestone row `v0.0.22-analytics-ingest-queries`, NEXT pointer → Plan 13.3.
- [ ] **Step 3.** Fill `docs/references/plan-13-analytics.md` § "Production lessons (post-execution YYYY-MM-DD) — 13.2" with the gotchas surfaced during execution.
- [ ] **Step 4.** Fill this plan's `## Amendments` block.
- [ ] **Step 5.** ADR? — IF this plan made a substantive architectural decision (e.g. cross-tenant analytics subject scheme), write `docs/adr/0017-analytics-subject-scheme.md` (Nygard format). Otherwise SKIP.
- [ ] **Step 6.** CONTEXT.md? — IF new domain terms introduced. SKIP if not.
- [ ] **Step 7.** `docs/api/analytics/` — IF the HTTP surface is documented OpenAPI-style. **Plan 13.2 ships OpenAPI for the 6 endpoints** at `docs/api/analytics/openapi.yaml` (one of the close-out artefacts).
- [ ] **Step 8.** `docs/runbooks/` — IF a new operational procedure. The analytics ingest pipeline is a new long-running daemon → ADD `docs/runbooks/analytics-ingest.md` describing leader-style behaviour (none — it's a queue group consumer, scales horizontally), how to drain on shutdown, how to diagnose stuck buffers, how to manually re-run a backfill.
- [ ] **Step 9.** `docs/architecture/0X-*.md` — IF a new top-level service / boundary rule / testing convention. The `clickhouse-go-isolation` depguard rule is a new project-wide standard → mention in `02-module-contracts.md` § depguard table.
- [ ] **Step 10.** Module-graph — UPDATE `docs/architecture/module-graph.md` events table with: `analytics.event.calls` (producer: dialer, consumer: analytics-ingest), `analytics.event.operator_state` (same), plus the existing `tenant.<t>.recording.uploaded` now has analytics as a consumer.
- [ ] **Step 11.** Master spec — Plan 13.2 EXECUTES §6.4 / §FR-A; no spec change unless a deviation arose.
- [ ] **Step 12.** Tag + push.
  ```bash
  git tag -a v0.0.22-analytics-ingest-queries -m "Plan 13.2: analytics ingest + queries + HTTP (6 tasks)"
  git push origin main --tags
  ```
- [ ] **Step 13.** CI watch — all 6 jobs green (lint / test / build / docker / vuln / secret-scan). CodeQL is the known no-op (per `PROJECT_STATUS.md` standing rule).
- [ ] **Step 14.** Standing-rules pruning — Plan 13.2 is the 22nd milestone; if standing rules > 200 lines, prune now.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-05-14-13-2-analytics-ingest-queries.md`. Two execution options:

**1. Subagent-Driven (recommended)** — fresh implementer subagent per task, two-stage review (spec + code-quality) per task. Re-review proportionality per `09-agent-workflow-improvements.md` #7.

**2. Inline Execution** — execute tasks in this session via `superpowers:executing-plans`.

**Recommended: Subagent-Driven** for this plan because each task touches ≥ 3 files + new public symbols + new tests = full 2-stage review territory.
