# Plan 14 — Billing Module (per-call cost, tariffs, margin, finance dashboard)

> **Subagents must read this file BEFORE writing code.** It captures the
> canonical specs, the *current* state of the codebase, the gotchas Plan
> 14's abstract text glosses over, and the open questions.
>
> The plan file (`docs/superpowers/plans/2026-05-06-14-billing-module.md`)
> was written months ago against an idealised codebase. Reality has
> drifted in several ways — this reference is the bridge.

## 1. Canonical specs

- **System design spec:** `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md`
  - §FR-H — Finance / Billing functional requirements (dashboard, tariffs, margin, history)
  - §5.2 — module catalog row "billing"
  - §6.3 — `tenant_settings`, `calls`, `call_recordings`, `operator_sessions` schemas
  - §14.3 — `tenant_settings.surveys.cost_per_completed_rub` precedent for per-tenant numeric tariffs
  - §22 prototype `admin-pages-2.jsx::AdminFinance` — visual contract
- **ADRs:**
  - ADR-0006 (PgBouncer transaction mode) — every API call = one Tx, `SET LOCAL app.tenant_id`
  - ADR-0010 (NATS JetStream) — durable subjects, at-least-once delivery
  - ADR-0012 (zap logging) — billing MUST use `*zap.Logger`, NOT `*slog.Logger`
  - ADR-0014 (gin router) — handlers are `func(c *gin.Context)`, NOT `net/http` `http.Handler`
  - ADR-0015 (TDD mandatory) — every behavior change preceded by a failing test
- **Domain glossary:** `CONTEXT.md`
  - **AHT** (Average Handling Time) — used by capacity model; cost-per-survey is the FR-H KPI
  - **Outbox pattern** — `event_outbox` table + `pkg/outbox` writer/relay; the canonical at-least-once mechanism
  - **Tenant** — unit of isolation; per-tenant tariffs live in `tenant_settings`

## 2. Reality-checked codebase state (verified 2026-05-15)

### 2.1 Already-built `internal/billing/api/` (Plan 00 foundation)

`internal/billing/api/` already contains:

| File | Symbols | Notes |
|---|---|---|
| `dto.go` | `Tariffs`, `CallCostInput`, `CallCostOutput`, `Period`, `Month()`, `MonthBreakdown`, `ProjectMargin`, `DashboardResponse`, `TariffsResponse`, `TariffsPatchRequest` | **NO json tags yet** — must add in Task 2. Field names differ from plan: see §4.1. |
| `interfaces.go` | `CostCalculator`, `TariffStore`, `RevenueCalculator`, `MarginReport`, `SpendReport`, `CallFinalizedHook` | 6 interfaces, names match plan. **`TariffStore.Update` takes `Tariffs` directly** (not a Patch). |
| `errors.go` | `ErrNoTariffs`, `ErrInvalidTariff`, `ErrInvalidPeriod` | 3 sentinels. Map to HTTP: 409, 400, 400. |
| `events.go` | `AuditActionTariffUpdated = "billing.tariff_updated"` | One constant only; billing publishes via `tenant.<t>.audit.event` outbox subject. |

**`module.go`** is a Plan 00 no-op stub — Plan 14 Task 10 fills it.

### 2.2 Field-name mapping (plan abstract → real api)

The plan uses one set of names; `internal/billing/api/dto.go` defines another:

| Plan abstract name | Real api/dto.go name | Notes |
|---|---|---|
| `CostPerCompletedSurveyMin` | `WagePerSurveyMinor` | operator wage per successful survey |
| `CostPerImportedRecordMin` | `RespondentBasesMinor` | paid per imported respondent row |
| `StorageCostPerGBMonthMin` | `StorageMinorPerGBMo` | S3 storage rate |
| `FixedMonthlyFeesMin` | `FixedFeesMinor` | constant monthly overhead |
| `Tariffs.Version uuid.UUID` | `Tariffs.Version int` | monotonic counter, NOT a UUID |
| `MonthBreakdown.BasesMin` | `MonthBreakdown.RespondentBasesMin` | bases-purchase line item |
| `ProjectMargin.CostPerSurveyMinor` | `ProjectMargin.CostPerSrvMn` | abbreviation |

**Implication:** every Go file the plan dictates must be translated to the real names. Migration field names (`call_costs.*_minor`) follow the plan (they're SQL column names, not Go fields); the plan's SQL column names are FINE.

### 2.3 CallFinalizedEvent payload (real dialer publisher)

`internal/dialer/api/events.go:76-90` — **canonical**:

```go
type CallFinalizedEvent struct {
    CallID       uuid.UUID `json:"call_id"`
    TenantID     uuid.UUID `json:"tenant_id"`
    OperatorID   uuid.UUID `json:"operator_id"`     // NOT in plan — bonus field
    ProjectID    uuid.UUID `json:"project_id"`
    RespondentID uuid.UUID `json:"respondent_id"`   // NOT in plan — bonus field
    TrunkUsed    string    `json:"trunk_used"`
    DurationSec  int32     `json:"duration_sec"`
    Status       string    `json:"status"`
    StorageBytes int64     `json:"storage_bytes"`
    FinalizedAt  int64     `json:"finalized_at"`    // **unix seconds**, NOT time.Time
}
```

**Subject:** `tenant.<tenant_uuid>.dialer.call.finalized` (per-tenant). Helper: `dialerapi.SubjectCallFinalizedFor(tenantID)`. Plan's wildcard subscribe should use `tenant.*.dialer.call.finalized`.

**Decode rule:** `FinalizedAt int64` → `time.Unix(raw, 0).UTC()` in subscriber. The plan's example payload uses `"finalized_at": "2026-05-12T18:01:23Z"` — **wrong**, it's int64 unix-seconds in reality.

### 2.4 tenant_settings schema (canonical)

`migrations/000001_init.up.sql:50-56`:

```sql
create table tenant_settings (
  tenant_id uuid not null references tenants(id) on delete cascade,
  key text not null,
  value jsonb not null,
  updated_at timestamptz not null default now(),
  primary key (tenant_id, key)
);
```

**SettingsCache API:** `internal/tenancy/api/settings.go`:
- `Lookup(ctx, tenantID, key) (SettingValue, error)` — returns `ErrSettingNotFound` on miss
- `Set(ctx, tenantID, key, value SettingValue) error`
- `SettingValue` wraps `json.RawMessage`; accessors `AsInt() (int64, ok)`, etc.

**Decision (Plan 14):** TariffStore reads/writes tenant_settings DIRECTLY via the pgx pool inside `pkg/postgres.Pool.WithTenant` — NOT through `SettingsCache.Lookup`. Reasoning: TariffStore.Update is admin-initiated under tenant context (RLS applies); SettingsCache is a read-through cache layered atop tenant_settings, and we want billing's writes to be visible immediately (cache invalidation across pods is publish-driven and we don't want to introduce a new subject for it). Subagent should use the canonical `tenant_settings (tenant_id, key, value, updated_at)` table directly.

### 2.5 projects table — missing `contract_fee_per_completed_minor`

`migrations/000001_init.up.sql:90-104` + `000005_projects_evolve.up.sql` — **no revenue column.** Plan 14 Task 7 ADDS it via:

```sql
-- migrations/000013_billing.up.sql (combined Plan 14 migration — see §5)
alter table projects
  add column if not exists contract_fee_per_completed_minor bigint not null default 0;
comment on column projects.contract_fee_per_completed_minor is
  'Per-completed-survey fee paid by the customer (kopecks). 0 = no contract attached.';
```

**Decision:** combine Plan 14's two migration files (`call_costs` + `projects.contract_fee_per_completed_minor`) into **ONE** migration `000013_billing.up.sql`. Reason: migration sequence is integer-monotonic in this repo (000001…000012); the plan's date-stamped naming (`20260506000000_*.sql`) does not match the repo's convention.

### 2.6 call_recordings — `bytes_size`, NOT `storage_size_bytes`

`migrations/000010_recording_evolve.up.sql:26`:

```sql
-- column added by Plan 12 migration:
bytes_size bigint not null default 0
```

Plan 14 references `call_recordings.storage_size_bytes` — **wrong column name**. Reality: `bytes_size`. The plan's `CallCostInput.StorageBytes` field comes from the dialer event payload (`storage_bytes` JSON field), which the dialer FSM populates from `bytes_size` at finalize time.

**Decision:** Billing reads `StorageBytes` from the **NATS event payload**, NOT from `call_recordings` table. No JOIN needed. If the event is missing the field (e.g. call without a recording), `StorageBytes = 0` is fine — calculator returns `StorageMinor = 0`.

### 2.7 respondents table — `source` column

`migrations/000001_init.up.sql:136`: `source text check (source in ('imported','rdd'))`.

Plan 14's `CountImportedRecords` query:

```sql
select count(*) from respondents
 where tenant_id=$1 and source='imported' and created_at >= $2 and created_at < $3
```

is **correct**. Constant: `crmapi.SourceImported = "imported"` (`internal/crm/api/dto.go:159`).

### 2.8 Logger — zap, NOT slog (CRITICAL)

The plan's code uses `*slog.Logger` throughout. **The project uses zap exclusively** (ADR-0012). Translation:

| Plan code (slog) | Real code (zap) |
|---|---|
| `log.Error("billing http", "err", err)` | `log.Error("billing http", zap.Error(err))` |
| `log.Info("foo", "k", "v")` | `log.Info("foo", zap.String("k", "v"))` |
| `*slog.Logger` field | `*zap.Logger` field |
| no-op default | `zap.NewNop()` |

### 2.9 HTTP transport — gin, NOT net/http (CRITICAL)

The plan uses `http.HandlerFunc` / `http.Error` / `json.NewEncoder(w).Encode`. **The project uses gin** (ADR-0014).

**Canonical pattern** (mirrors `internal/dialer/transport/http/session_handler.go:165-200` and `internal/reports/transport/`):

```go
func (h *Handlers) Dashboard(c *gin.Context) {
    claims, ok := authmw.ClaimsFromContext(c)
    if !ok { renderUnauthenticated(c); return }
    period, err := parsePeriod(c)
    if err != nil { renderError(c, h.log, billingapi.ErrInvalidPeriod); return }
    resp, err := h.svc.Dashboard(c.Request.Context(), claims.TenantID, period)
    if err != nil { renderError(c, h.log, err); return }
    c.JSON(http.StatusOK, resp)
}
```

**ErrorEnvelope** (project canon, mirrors `internal/dialer/transport/http/dto.go:73-78`):

```go
type ErrorEnvelope struct {
    Code    string `json:"code"`     // e.g. "billing.invalid_tariff"
    Message string `json:"message"`
}
```

Error-mapping table for billing:

| Sentinel | HTTP | Code |
|---|---|---|
| `ErrNoTariffs` | 409 | `billing.no_tariffs` |
| `ErrInvalidTariff` | 400 | `billing.invalid_tariff` |
| `ErrInvalidPeriod` | 400 | `billing.invalid_period` |
| `ErrSettingNotFound` (from tenancy.SettingsCache, wrapped) | 409 | `billing.no_tariffs` |
| RBAC failure (admin-only PATCH) | 403 | `billing.forbidden` |
| Missing/invalid JWT | 401 | `billing.unauthenticated` |
| Other | 500 | `billing.internal` |

### 2.10 RBAC + admin-only PATCH /api/billing/tariffs

Plan says `auth.RequireRole("admin")`. Reality is finer (`internal/reports/module.go:requireAdmin`):

1. Extract `authmw.ClaimsFromContext(c)` — 401 on absence
2. Fast-path: `claims.HasRole(authapi.RoleAdmin) || claims.HasRole(authapi.RoleSupervisor)` → permit
3. Fallback: `RBACChecker.Check(ctx, claims, authapi.ActionBillingTariffUpdate, authapi.ResourceTenantWide("billing"))` (verify the action constant exists in `internal/auth/api/`; if absent, add it)

**Decision:** Mirror `requireAdmin` from `internal/reports/module.go` verbatim. Add `ActionBillingTariffUpdate` if missing from `authapi`.

### 2.11 RequireSameTenant — NOT needed for billing v1

`pkg/middleware/tenant/require_same_tenant.go` defends against path-`:id` cross-tenant attacks. Billing endpoints don't take a tenant-scoped path-`:id`:

- `GET /api/finance/dashboard?period=month` — no resource id
- `GET /api/finance/breakdown` — no id
- `GET /api/finance/byMonth` — no id
- `GET /api/finance/projects?period=month` — no id
- `GET /api/billing/tariffs` — no id (implicit `claims.TenantID`)
- `PATCH /api/billing/tariffs` — no id (implicit `claims.TenantID`)

All endpoints derive tenantID from `claims.TenantID` (already JWT-verified). No path-`:id` to validate. **Skip RequireSameTenant** for billing.

### 2.12 Pool access patterns

`pkg/postgres/pool.go`:

```go
func (p *Pool) WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
func (p *Pool) BypassRLS(ctx context.Context, fn func(Tx) error) error
```

**Decisions for billing:**
- **HTTP handlers** (admin reads dashboards, edits tariffs) → `pool.WithTenant(claims.TenantID, fn)`. RLS scopes every SELECT/INSERT.
- **`OnCallFinalized` event handler** → `pool.WithTenant(event.TenantID, fn)`. Even though the worker isn't request-scoped, we have the tenantID from the event payload; staying inside RLS gives defence in depth (a forged event with a wrong call_id will hit RLS denial).
- **NO `BypassRLS` in billing.** The module is fully tenant-scoped.

### 2.13 Outbox pattern for audit

`pkg/outbox/writer.go:17`:

```go
type Writer interface {
    Append(ctx context.Context, tx postgres.Tx, ev Event) error
}
```

`pkg/outbox/event.go:29-72`:

```go
type Event struct {
    TenantID    *uuid.UUID   // nullable for cross-tenant
    AggregateID *uuid.UUID
    Subject     string       // required, non-empty
    Payload     []byte       // JSON
}
```

**Audit pattern (mirrors `internal/reports/service/audit.go`):**

1. Build payload conforming to `auditapi.Event` (`internal/audit/api/events.go`)
2. Subject: `tenant.<tenant_uuid>.audit.event`
3. `outbox.Writer.Append(ctx, tx, ev)` inside the same `WithTenant` Tx as the tariff write — atomic chain-of-custody

**Action for billing:**
- On `TariffStore.Update` success: write `auditapi.Event{Action: "billing.tariff_updated", Target: "tariff:<tenantID>", Payload: {...diff or full snapshot...}}` to outbox in the same Tx.
- On `OnCallFinalized` success (call_costs INSERT): **NO audit row** per call — too noisy. Audit only the explicit human-driven tariff change.

### 2.14 NATS subscriber pattern (eventbus.Subscriber)

`pkg/eventbus/publisher.go:22-34`:

```go
type Subscriber interface {
    Subscribe(ctx context.Context, subject string, queue string, handler func(subject string, payload []byte) error) error
}
```

**Pattern (mirrors `internal/dialer/transport/nats/call_event_subscriber.go`):**

1. Subject: `tenant.*.dialer.call.finalized` (wildcard — bus delivers all tenants)
2. Queue: `billing-call-finalized` (load-balance across replicas; ONE replica per message)
3. Handler decodes payload + calls `OnCallFinalized` — returning error triggers redelivery
4. Run inside a goroutine started by `Module.Register`; ctx-cancel cleans up

**Idempotency:** `INSERT ... ON CONFLICT (call_id) DO NOTHING` in `call_costs` makes redelivery safe.

**Plan deviation:** Plan 14 Task 9's snippet uses raw `nats.go` + `jetstream` API. **Use `eventbus.Subscriber` instead** — the project's abstraction. Subagent should look at `internal/dialer/transport/nats/call_event_subscriber.go` for the canonical pattern.

### 2.15 Module wiring (modules.Deps + locator)

`internal/modules/module.go`:

```go
type Deps struct {
    Ctx        context.Context
    Logger     *zap.Logger
    Config     *config.Config
    Pool       *postgres.Pool
    Redis      redis.UniversalClient
    EventBus   eventbus.Publisher
    Subscriber eventbus.Subscriber
    HTTPRouter *gin.Engine
    GRPCServer *grpc.Server
    Locator    ServiceLocator
}
```

`internal/reports/module.go:131-247` is the **canonical exemplar** for billing's `Module.Register`. Key elements:

1. Validate `d.Logger`, `d.Config`, `d.Pool`, `d.HTTPRouter` — `nil` → log WARN, return nil (degraded boot)
2. Resolve dependencies via `d.Locator.Lookup("auth.RBACChecker")` etc.
3. Build store (`internal/billing/store/pgx.New(d.Pool)`)
4. Build service (`internal/billing/service.New(...)`)
5. Build outbox audit-emitter (`outbox.NewPostgresWriter()` → `service.NewAuditEmitter(writer)`)
6. Mount HTTP via `internal/billing/transport/http.Register(d.HTTPRouter, RouterDeps{...})`
7. Start NATS subscriber goroutine using `d.Subscriber`
8. Publish locator entries: `d.Locator.Register("billing.SpendReport", spendReport)` etc. — for cmd/worker
9. `logger.Info("billing module registered", zap....)` — success

**Module load order (cmd/api/main.go):** billing depends on auth (RBACChecker), so register **after** auth/tenancy and **after** any module that publishes locator entries billing consumes. Place after Reports.

### 2.16 Config — adding BillingConfig

`pkg/config/config.go:24-42` defines top-level `Config`. **Add** field:

```go
Billing BillingConfig `mapstructure:"billing"`
```

Add validator entry in `Config.Validate()`:

```go
{"billing", c.Billing.Validate},
```

`pkg/config/billing.go` (NEW):

```go
type BillingConfig struct {
    Defaults billingapi.Tariffs `mapstructure:"defaults"`
}

func (b BillingConfig) Validate() error {
    return b.Defaults.Validate()  // Validate to be added in Task 2
}
```

YAML in `configs/development/config.yaml`:

```yaml
billing:
  defaults:
    trunk_costs_minor:
      mtt-msk-1: 342
      mango-fed: 378
      beeline-srf: 412
    wage_per_survey_minor: 12000
    respondent_bases_minor: 50
    storage_minor_per_g_b_mo: 150
    fixed_fees_minor: 5000000
```

**Naming:** mapstructure converts CamelCase to snake_case; `StorageMinorPerGBMo` → `storage_minor_per_g_b_mo` is awkward. **Decision:** add `mapstructure:"storage_minor_per_gb_mo"` tags to `Tariffs` fields where the auto-naming is ugly.

### 2.17 Testing — testcontainers + goleak

Plan references `internal/testpg.New(t)`. **Does not exist.** Real pattern:

- `pkg/postgres/main_test.go` — `goleak.VerifyTestMain(m)`
- Integration tests use `testcontainers-go/modules/postgres` directly with `//go:build integration` tag
- Unit tests for stores use `pgxmock` is **NOT** in `go.mod` — pure-function unit tests are preferred; integration tests are testcontainer-backed

**Decision:** billing follows the reports module's testing layout:
- `service/calculator_test.go` — pure-function unit tests, no DB
- `service/tariffs_test.go` — uses a `fakeSettingsBackend` in-memory stand-in
- `store/pgx/pg_test.go` (build tag `integration`) — testcontainers
- `service/month_spend_pg_test.go` (build tag `integration`) — testcontainers, end-to-end aggregation
- `transport/http/handlers_test.go` — gin TestMode + in-memory service fake

### 2.18 Trunk catalog

**No hardcoded trunk list exists.** Trunk names are FreeSWITCH operational config (XML on FS-VMs). The plan's `mtt-msk-1`/`mango-fed`/`beeline-srf` are reasonable defaults but **must not be enforced as enum values** — TariffStore's `TrunkCostsMinor` is an open `map[string]int64`. Unknown trunk_used → calculator returns `TelecomMinor = 0` (defensive, no panic).

### 2.19 Plan 13.2.5 backlog: `POST /api/calls/:id/hangup` lacks tenant scope

PROJECT_STATUS says: _"Out-of-scope finding for Plan 14 backlog: `POST /api/calls/:id/hangup` lacks tenant scope."_

**Decision: defer to Plan 14.5 / standalone fix-up.** Reason: this is a dialer-module security fix, NOT a billing concern. Mixing it into the billing plan would muddle the diff and the close-out tag. Track as a separate `needs-info` issue: `git log` will surface it if missed.

## 3. Reference implementations & library docs

### Library docs (verify via `context7` before using)

- `github.com/shopspring/decimal` v1.4+ — money rounding. `decimal.NewFromInt(x).Mul(y).Div(z).Round(0).IntPart()` pattern.
- `github.com/jackc/pgx/v5` — pool, Tx, scanning. Use `pgx.ErrNoRows` for missing rows.
- `github.com/google/uuid` — `uuid.New()`, `uuid.MustParse()`.
- `go.uber.org/zap` v1.27+ — `zap.Error(err)`, `zap.Int64("k", v)`, `zap.String`, `zap.Duration`.
- `github.com/gin-gonic/gin` v1.10+ — `c.ShouldBindJSON`, `c.JSON`, `c.AbortWithStatusJSON`, `c.Param`.
- `pkg/eventbus.Subscriber.Subscribe` — handler signature `func(subject string, payload []byte) error`.
- `pkg/outbox.PostgresWriter.Append` — inside a `WithTenant` Tx.

**Rule:** if the implementer is uncertain about ANY library's API, run `mcp__plugin_context7_context7__resolve-library-id` → `query-docs` BEFORE writing code. Don't guess from training data.

### Reference implementations in this repo

- **`internal/reports/`** — canonical "module with HTTP + outbox audit + locator + degraded-boot" pattern.
- **`internal/dialer/transport/http/`** — canonical "gin handler + ErrorEnvelope + JWT claims" pattern.
- **`internal/dialer/transport/nats/call_event_subscriber.go`** — canonical "subscribe to wildcard subject via eventbus.Subscriber" pattern.
- **`internal/recording/store/recording_pg.go`** — canonical "store with WithTenant pool + Tx variants" pattern.

## 4. Gotchas — the rakes Plan 14 doesn't mention

### 4.1 Field renaming (already covered in §2.2 — DON'T blindly copy plan code)

Every Go file dictated by the plan must be translated to the **real** field names in `internal/billing/api/dto.go`. Compile errors are immediate; subtle bugs include JSON-tag mismatches (frontend depends on the wire shape).

### 4.2 Money arithmetic — round at the last step ONLY

Plan §money-handling is correct: `decimal.NewFromInt(perMin).Mul(decimal.NewFromInt32(dur)).Div(decimal.NewFromInt(60)).Round(0).IntPart()`. Do NOT round intermediate values. Do NOT use `float64` anywhere on the cost path. The plan's TestCallCost_LongCall_RoundingIsHalfUp (95s × 342 kop/min → 542) verifies half-up rounding.

### 4.3 Idempotency invariant — `ON CONFLICT (call_id) DO NOTHING`

`call_costs.call_id` is PRIMARY KEY. INSERT must use `ON CONFLICT (call_id) DO NOTHING`. The NATS handler returns nil on conflict (i.e. ACK), not error. **Test invariant:** publishing the same `dialer.call.finalized` event twice produces exactly ONE `call_costs` row.

### 4.4 Storage cost — per-call snapshot, NOT recurring

Plan 14 explicitly defers monthly re-charge to v2: `OnCallFinalized` charges storage ONCE per call. `(bytes_size / 1 GiB) * StorageMinorPerGBMo` rounded to int64. A call without a recording (`bytes_size = 0`) charges `0`.

### 4.5 Test invariant: `TotalMinor == Telecom + Wages + Storage`

Plan's `TestCallCost_AlwaysNonNegative_Property` is the canonical property test. Every implementer must keep it green. If a future tariff component (e.g. SMS-survey reimbursement) is added, the test must extend, NOT the invariant be weakened.

### 4.6 Period parsing — UTC always

`Period.From / To` are `time.Time` in UTC. `parsePeriod(c)` reads `?period=week|month|quarter|year` AND defaults to `month`. Explicit `?from=YYYY-MM-DD&to=YYYY-MM-DD` is allowed for ad-hoc queries (Plan Task 8 doesn't include this — **add it** in Task 8 if it doesn't bloat the diff, otherwise defer to Plan 14.x). Decision: skip explicit from/to in v1; only `?period=` enum.

### 4.7 testifylint negatives

`require.Positive(t, n)` — not `require.Greater(t, n, int64(0))` (Plan 13.2.5 lesson). Reviewer/lint catches this. Apply to all billing tests.

### 4.8 Module-graph events

After Plan 14 lands:
- **New consumed subject** (cross-tenant wildcard): `tenant.*.dialer.call.finalized` (already published by dialer; we add a consumer)
- **New published subject:** `tenant.<t>.audit.event` with `Action="billing.tariff_updated"` (audit consumer is still stub-only)

Update `docs/architecture/module-graph.md` in close-out.

### 4.9 depguard — module-boundaries

`internal/billing/{service,store,events,transport}` MUST NOT be imported from any other `internal/<X>`. Other modules access billing **only** through `internal/billing/api/`. If a downstream module needs `RevenueCalculator`, publish it to locator: `d.Locator.Register("billing.RevenueCalculator", rev)`.

### 4.10 store organisation

The plan's "back-import" warning (Task 7 §step 7) is correct: defining `ProjectAggregate` in `service/` and importing from `store/` creates a cycle. Fix per the plan's footnote: **put `ProjectAggregate` in a leaf package** like `internal/billing/store/types/aggregate.go` (or a `internal/billing/types/`), depended upon by both `service/` and `store/pgx/`. Mirrors `internal/reports/templates/data/` from Plan 13.3.

### 4.11 Audit row content — what to log

For `billing.tariff_updated`:
- `ActorID`: `claims.UserID` (the admin who PATCH'd)
- `ActorKind`: `auditapi.ActorUser`
- `Target`: `tariff:<tenant_uuid>` (each tenant has exactly one tariff record)
- `Payload`: `{"version_before": 7, "version_after": 8, "changed_keys": ["wage_per_survey_minor", "fixed_fees_minor"]}` — keep PII-free; numeric values themselves are NOT PII but **changed_keys list** suffices for audit; the full numeric values live in `tenant_settings.billing.*` queryable separately.

### 4.12 Migration sequence

Combine plan's two migrations into ONE pair:
- `migrations/000013_billing.up.sql`
- `migrations/000013_billing.down.sql`

Contents (per §2.5 + Plan 14 Task 1):
1. `CREATE TABLE call_costs (...)` + RLS policy + 2 indexes + comments
2. `ALTER TABLE projects ADD COLUMN IF NOT EXISTS contract_fee_per_completed_minor bigint NOT NULL DEFAULT 0` + comment

Down migration drops in reverse order. **Empty-table guard NOT needed** for `call_costs` (zero-row at creation); needed for `projects` ALTER only if rollback would lose data — `DROP COLUMN contract_fee_per_completed_minor` discards values, **add empty-table guard** if the column has been written-to in prod (which it won't be at first deploy). Keep DROP simple for v1; production rollback is a fire drill anyway.

### 4.13 Coverage gate ≥ 90% — practical target ≥ 80%

The plan's 90% target is aspirational. Reality: `service/` should be ≥ 85% (calculator pure functions hit 100%, HTTP handlers around 80% due to error branches). `store/pgx/` is integration-test-covered; per-line coverage from `go test -tags=integration` may show gaps, but functional correctness is what matters. Don't add tests just to hit a number — every test must catch a bug class.

## 5. Open questions (resolved before execution)

| Q | Resolution |
|---|---|
| Combine 2 migrations into 1? | **YES** — `000013_billing.{up,down}.sql`. Single PR-able artefact. |
| Use SettingsCache.Lookup or direct tenant_settings access? | **Direct** via pgx.WithTenant. Avoids cross-pod cache-invalidation subjects in v1. |
| `RequireSameTenant` middleware? | **NO** — billing endpoints are claims-only (no path-`:id`). |
| Fix `POST /api/calls/:id/hangup` cross-tenant bug here? | **NO** — defer; not a billing concern. Track as separate issue. |
| Audit on every call_costs INSERT? | **NO** — too noisy; only audit human-driven tariff changes. |
| Storage recurring charge per month? | **NO** in v1 — per-call snapshot only (plan explicit). |
| explicit `?from=&to=` period query? | **NO** in v1 — enum `period=week|month|quarter|year` only. |
| `Tariffs.Version int` vs `uuid.UUID`? | **int** (real `api/dto.go`); plan's uuid is wrong. Increment on every Update. |
| `auth.ActionBillingTariffUpdate` — does it exist? | **VERIFY**. Add to `internal/auth/api/` if missing (alongside `ActionReportGenerate`). |
| `cmd/worker billing.recompute` — Task 12 stub? | **YES** — keep the stub as a future-extension marker. |
| Coverage gate? | ≥ 80% target; 90% aspirational. No fake-it-to-hit-the-number. |

## 6. Production lessons (post-execution YYYY-MM-DD)

> Filled in during Phase 4 close-out. Document the gotchas that *actually* bit us, NOT the ones we anticipated above. Future agents reading this section save real time.

(empty — populate post-implementation)
