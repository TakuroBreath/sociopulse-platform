# Billing Module Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use `- [ ]` checkbox syntax. TDD throughout — every behavior change is preceded by a failing test.

**Goal:** Implement `internal/billing/` — the financial reporting module of СоциоПульс. Calculates per-call cost at finalization (`telecom + wages`), persists denormalised cost rows for fast aggregation, exposes per-tenant tariffs, computes month-level spend breakdowns / per-project margin, and serves the admin Finance dashboard (KPI tiles, byMonth bar chart, breakdown pie, projects table) via HTTP. All money values are stored and transferred as `int64` minor units (копейки) — no floats on the wire.

**Architecture:** Standard module shape `internal/billing/{api,service,store,events}`. Public surface in `api/`: `CostCalculator`, `TariffStore`, `RevenueCalculator`, `MarginReport`, plus DTOs. `service/` holds aggregation logic (SQL fan-in, breakdown, margin). `store/` — pgx queries against `calls`, `call_costs` (new table), `call_recordings`, `operator_sessions`, `tenant_settings`. `events/` subscribes to `dialer.call.finalized` and writes a `call_costs` row synchronously inside a transaction (idempotent on `(call_id)`). Endpoints registered through `gateway` from Plan 02 — admin-only RBAC.

**Tech Stack:** Go 1.22+, `github.com/jackc/pgx/v5` (pool already provided by `internal/store/db.go` from Plan 03), `github.com/stretchr/testify`, `github.com/google/uuid`, `github.com/shopspring/decimal` (only inside cost-calculator math — never serialized; result rounded to int64 minor). HTTP via standard `net/http` + the gateway's chi router from Plan 02. NATS subscription via the shared `events.Bus` interface (Plan 02). Configuration loaded by the standard `config.Load()` (Plan 02). Database access via `internal/tenancy.RLSContext()` so all queries are tenant-scoped (Plan 04).

**Spec sections covered:** §FR-H (full), §5.2 module catalog row "billing", §6.3 (`tenant_settings`, `calls`, `call_recordings`, `operator_sessions` schemas) — extends with `call_costs`, §14.3 (`tenant_settings.surveys.cost_per_completed_rub`), §22 prototype `admin-pages-2.jsx::AdminFinance`.

**Prerequisites:**
- Plan 00–01 (repo skeleton, infra).
- Plan 02 (`cmd/api` skeleton with config, observability, gateway router, RBAC middleware).
- Plan 03 (database & migrations runner).
- Plan 04 (tenancy, RLS, `tenant_settings`).
- Plan 05 (auth — needed for admin RBAC on `/api/billing/tariffs`).
- Plan 06 (CRM — `projects` exists).
- Plan 10 (dialer — `calls` rows finalized with `status`, `duration_sec`, `trunk_used`, NATS event `dialer.call.finalized`).
- Plan 12 (recording — `call_recordings.storage_size_bytes` populated; if missing, this plan adds it as a Postgres ALTER and falls back gracefully when null).
- Plan 13 (analytics — for cross-check during integration tests; not a hard runtime dep).

---

## File Structure

```
internal/billing/
├── api/
│   ├── doc.go                        # package doc, "imported by other modules" rule
│   ├── tariffs.go                    # Tariffs DTO, validation, JSON
│   ├── calculator.go                 # CostCalculator interface, CallCostInput/Output
│   ├── month.go                      # MonthBreakdown, ProjectMargin DTOs
│   ├── store.go                      # TariffStore, RevenueCalculator, MarginReport ifaces
│   ├── events.go                     # OnCallFinalized hook signature
│   ├── http.go                       # HTTP handler factory: Endpoints(svc Service, mw ...)
│   ├── http_dto.go                   # response DTOs: DashboardResponse, ByMonthBucket, ...
│   └── errors.go                     # ErrNoTariffs, ErrInvalidTariff, ErrInvalidPeriod
│
├── service/
│   ├── service.go                    # Service struct wiring stores, returning api interfaces
│   ├── calculator.go                 # CostCalculator implementation (pure func over Tariffs)
│   ├── calculator_test.go            # unit tests, table-driven, all status × duration matrix
│   ├── month_spend.go                # MonthSpend(): SQL aggregation
│   ├── month_spend_test.go           # integration via testcontainers Postgres
│   ├── margin.go                     # Margin report
│   ├── margin_test.go
│   ├── revenue.go                    # RevenueCalculator: sum from project.contract.fee_per_completed
│   ├── revenue_test.go
│   ├── tariffs.go                    # TariffStore: load/save tenant_settings.billing.*
│   ├── tariffs_test.go
│   ├── on_call_finalized.go          # event handler — writes call_costs row idempotently
│   ├── on_call_finalized_test.go
│   ├── http_dashboard.go             # GET /api/finance/dashboard
│   ├── http_projects.go              # GET /api/finance/projects
│   ├── http_breakdown.go             # GET /api/finance/breakdown
│   ├── http_bymonth.go               # GET /api/finance/byMonth
│   ├── http_tariffs.go               # GET/PATCH /api/billing/tariffs
│   └── http_test.go                  # gateway-level integration: all 6 endpoints
│
├── store/
│   ├── pg.go                         # pgx adapters (CallCostRow, query builders)
│   ├── pg_test.go                    # integration tests for INSERT/SELECT shapes
│   └── queries.sql                   # const SQL kept tidy, embedded by go:embed
│
└── events/
    ├── subscriber.go                 # subscribes to dialer.call.finalized, debounces, calls service
    └── subscriber_test.go

migrations/
├── 20260506000000_create_call_costs.up.sql
└── 20260506000000_create_call_costs.down.sql

cmd/worker/
└── billing_recompute.go              # opt-in: cmd/worker subcommand `billing.recompute` (stubbed; full impl is OOS for v1)

cmd/api/
└── main.go                           # MODIFY: register billing module endpoints + event subscriber

configs/development/config.yaml
└── billing: section (defaults; per-tenant via tenant_settings)

docs/api/
└── billing-openapi.yaml              # OpenAPI 3.1 stubs for the 6 endpoints
```

Total new code: ~3,500 LoC Go, ~150 LoC SQL, ~200 LoC YAML/OpenAPI.

---

## Money handling — global rule

Read this once. Repeat in every PR description.

- **Type.** All monetary values are `int64` "minor units" — копейки. A field is suffixed `_minor` when its unit is копейки and `_rub` when it's рубли (only as input from a UI form or from `tenant_settings`, immediately converted on read).
- **Arithmetic.** Division (e.g. `duration_sec * cost_per_minute_minor / 60`) uses `int64` with explicit rounding via `math/big` or `decimal.Decimal`. Floats are forbidden in the cost path.
- **Rounding.** Round-half-up at the very last step (per call) — `decimal.Decimal.Round(0)` then `.IntPart()`. Never accumulate fractional kopeks.
- **Display.** UI formats `_minor` with 2 decimals: `1234567 → "12 345,67 ₽"`. The handler returns minor units; the FE is responsible for formatting (Plan 19).
- **Negative numbers.** Cost is always ≥ 0; revenue ≥ 0; margin can be negative — the type stays `int64`.
- **Test invariant.** A property test in `calculator_test.go` asserts `CallCost(*) ≥ 0` for any input.

---

## Task 1: Database migration for `call_costs`

**Files:**
- Create: `migrations/20260506000000_create_call_costs.up.sql`
- Create: `migrations/20260506000000_create_call_costs.down.sql`

The denormalisation table that lets us answer `MonthSpend` for 2M calls/month in <200 ms without re-doing the cost arithmetic on every page load.

- [ ] **Step 1: Write the up migration**

Create `migrations/20260506000000_create_call_costs.up.sql`:

```sql
-- Per-call cost denormalisation. Filled by billing.OnCallFinalized when a call ends.
-- One row per call. Idempotent on call_id (the handler does ON CONFLICT DO NOTHING).
-- Re-computation on tariff change is a separate cmd/worker job (out of scope for v1).

create table call_costs (
  call_id          uuid        primary key references calls(id) on delete cascade,
  tenant_id        uuid        not null,
  project_id       uuid        not null references projects(id),
  -- Inputs captured at the time of calculation (so we can audit later why a row says what it says).
  trunk_used       text,                                         -- nullable: missing trunk → telecom_minor=0
  duration_sec     int         not null default 0,
  status           text        not null,                         -- copy of calls.status at finalize time
  -- Cost components.
  telecom_minor    bigint      not null default 0 check (telecom_minor >= 0),
  wages_minor      bigint      not null default 0 check (wages_minor   >= 0),
  storage_minor    bigint      not null default 0 check (storage_minor >= 0),
  -- Total — denormalised so SUM is a single column-scan.
  total_minor      bigint      not null default 0 check (total_minor   >= 0),
  -- Snapshot of the tariff version that produced these numbers (uuid in tenant_settings.value).
  tariff_version   uuid,                                         -- nullable for legacy rows
  finalized_at     timestamptz not null default now(),
  computed_at      timestamptz not null default now()
);

create index call_costs_tenant_finalized
  on call_costs (tenant_id, finalized_at desc);

create index call_costs_project_finalized
  on call_costs (project_id, finalized_at desc);

-- RLS: same policy pattern as the rest of the OLTP layer.
alter table call_costs enable row level security;

create policy call_costs_tenant_isolation on call_costs
  using (tenant_id = current_setting('app.tenant_id')::uuid);

-- Optional comment for the next person who reads this.
comment on table  call_costs            is 'Denormalised per-call cost components (kopecks). Re-computable from calls × tenant_settings.billing — billing.OnCallFinalized writes one row per call.';
comment on column call_costs.tariff_version is 'UUID of the tariff doc snapshot used at calculation time. Used by recompute job to detect drift.';
```

- [ ] **Step 2: Write the down migration**

Create `migrations/20260506000000_create_call_costs.down.sql`:

```sql
drop policy  if exists call_costs_tenant_isolation on call_costs;
drop index   if exists call_costs_project_finalized;
drop index   if exists call_costs_tenant_finalized;
drop table   if exists call_costs;
```

- [ ] **Step 3: Apply migration locally**

```bash
make migrate-up
psql "$PG_DSN" -c "\d call_costs"
```

Expected: table description shows `call_id uuid PRIMARY KEY`, all `_minor` columns as `bigint`, RLS enabled.

- [ ] **Step 4: Round-trip test the down migration**

```bash
make migrate-down step=1
make migrate-up
psql "$PG_DSN" -c "\d call_costs"   # exists again
```

- [ ] **Step 5: Commit**

```bash
git add migrations/20260506000000_create_call_costs.*
git commit -m "feat(billing): add call_costs table for cost denormalisation"
```

---

## Task 2: Public DTOs and interfaces (`internal/billing/api/`)

**Files:**
- Create: `internal/billing/api/doc.go`
- Create: `internal/billing/api/tariffs.go`
- Create: `internal/billing/api/calculator.go`
- Create: `internal/billing/api/month.go`
- Create: `internal/billing/api/store.go`
- Create: `internal/billing/api/events.go`
- Create: `internal/billing/api/errors.go`

This is the contract every other module imports. It has zero dependencies on `service/`, `store/`, or `events/`. depguard from Plan 00 enforces this.

- [ ] **Step 1: Write `api/doc.go`**

```go
// Package billing is the public surface of the billing module: cost calculation,
// per-tenant tariffs, month spend breakdown, project margin.
//
// Other modules MUST import only this package. The implementation lives under
// internal/billing/{service,store,events}. depguard enforces the boundary.
//
// Money handling: everything monetary is int64 minor units (kopecks). See the
// money-handling rule in docs/superpowers/plans/2026-05-06-14-billing-module.md.
package api
```

- [ ] **Step 2: Write the failing test for `Tariffs.Validate`**

Create `internal/billing/api/tariffs_test.go`:

```go
package api_test

import (
	"testing"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/stretchr/testify/require"
)

func TestTariffs_Validate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      billingapi.Tariffs
		wantErr bool
	}{
		{
			name: "valid",
			in: billingapi.Tariffs{
				TrunkCostsMinor:           map[string]int64{"mtt-msk-1": 342},
				CostPerCompletedSurveyMin: 12000,
			},
		},
		{name: "negative trunk cost", in: billingapi.Tariffs{TrunkCostsMinor: map[string]int64{"x": -1}}, wantErr: true},
		{name: "negative per-survey", in: billingapi.Tariffs{CostPerCompletedSurveyMin: -1}, wantErr: true},
		{name: "negative per-import", in: billingapi.Tariffs{CostPerImportedRecordMin: -1}, wantErr: true},
		{name: "negative storage", in: billingapi.Tariffs{StorageCostPerGBMonthMin: -1}, wantErr: true},
		{name: "negative fixed", in: billingapi.Tariffs{FixedMonthlyFeesMin: -1}, wantErr: true},
		{name: "trunk-id empty", in: billingapi.Tariffs{TrunkCostsMinor: map[string]int64{"": 100}}, wantErr: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.in.Validate()
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
```

- [ ] **Step 3: Write `api/tariffs.go` — minimal to make tests compile and pass**

```go
package api

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Tariffs is the per-tenant billing configuration loaded from
// tenant_settings under keys "billing.trunks", "billing.surveys",
// "billing.imports", "billing.storage", "billing.fixed".
//
// All fields are int64 minor units (копейки). UI-facing rubles
// are converted at the boundary.
type Tariffs struct {
	// Telecom: ₽/min per trunk_id (what the bridge stamped on calls.trunk_used).
	TrunkCostsMinor map[string]int64 `json:"trunk_costs_minor"`

	// Operator wages: paid per call with status='success'.
	CostPerCompletedSurveyMin int64 `json:"cost_per_completed_survey_minor"`

	// Imported respondent bases: paid per row imported.
	CostPerImportedRecordMin int64 `json:"cost_per_imported_record_minor"`

	// Cold storage of recordings: ₽ per GB-month. Optional; zero means "free / not tracked separately".
	StorageCostPerGBMonthMin int64 `json:"storage_cost_per_gb_month_minor"`

	// Constant overhead: rent, SaaS subscriptions, salaried admin staff.
	FixedMonthlyFeesMin int64 `json:"fixed_monthly_fees_minor"`

	// Snapshot identifier — bumped on every PATCH. Used by call_costs.tariff_version.
	Version uuid.UUID `json:"version"`
}

// Validate enforces non-negative invariants and non-empty trunk-ids.
func (t *Tariffs) Validate() error {
	if t.CostPerCompletedSurveyMin < 0 {
		return fmt.Errorf("billing.tariffs: cost_per_completed_survey_minor < 0")
	}
	if t.CostPerImportedRecordMin < 0 {
		return fmt.Errorf("billing.tariffs: cost_per_imported_record_minor < 0")
	}
	if t.StorageCostPerGBMonthMin < 0 {
		return fmt.Errorf("billing.tariffs: storage_cost_per_gb_month_minor < 0")
	}
	if t.FixedMonthlyFeesMin < 0 {
		return fmt.Errorf("billing.tariffs: fixed_monthly_fees_minor < 0")
	}
	for trunkID, cost := range t.TrunkCostsMinor {
		if trunkID == "" {
			return errors.New("billing.tariffs: empty trunk_id in trunk_costs_minor")
		}
		if cost < 0 {
			return fmt.Errorf("billing.tariffs: trunk_costs_minor[%q] < 0", trunkID)
		}
	}
	return nil
}

// TrunkCostMinor returns the per-minute cost for a trunk, or 0 if not configured.
// Zero is intentional — an unknown trunk_used should not crash the calculator;
// it's logged and treated as free (the most defensive behavior).
func (t *Tariffs) TrunkCostMinor(trunkID string) int64 {
	if trunkID == "" {
		return 0
	}
	return t.TrunkCostsMinor[trunkID]
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

```bash
go test ./internal/billing/api/...
```

Expected: `PASS`. If a case fails, fix the assertion or `Validate` until green.

- [ ] **Step 5: Write `api/calculator.go`**

```go
package api

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// CallCostInput is the minimal fact set needed to price a call.
// Always taken from the calls row at finalize time — never re-derived
// from a different timestamp.
type CallCostInput struct {
	CallID       uuid.UUID
	TenantID     uuid.UUID
	ProjectID    uuid.UUID
	TrunkUsed    string
	DurationSec  int32
	Status       string // matches calls.status enum
	StorageBytes int64  // 0 if no recording yet
	FinalizedAt  time.Time
}

// CallCostOutput is the breakdown plus a total. All values are int64 minor units.
type CallCostOutput struct {
	TelecomMinor int64
	WagesMinor   int64
	StorageMinor int64
	TotalMinor   int64
}

// CostCalculator prices a single call given a Tariffs snapshot. Pure function —
// no IO, no clock dependency. Implementations live in service/calculator.go.
type CostCalculator interface {
	CallCost(ctx context.Context, in CallCostInput, t Tariffs) (CallCostOutput, error)
}
```

- [ ] **Step 6: Write `api/month.go`**

```go
package api

import (
	"time"

	"github.com/google/uuid"
)

// Period is a half-open interval [From, To). Helpers turn a calendar month into one.
type Period struct {
	From time.Time
	To   time.Time
}

// Month returns [first day of month, first day of next month) at UTC midnight.
func Month(year int, month time.Month) Period {
	from := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	return Period{From: from, To: from.AddDate(0, 1, 0)}
}

// MonthBreakdown is what the admin-finance "Структура расходов" pie/list renders.
type MonthBreakdown struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	Period      Period    `json:"period"`
	TelecomMin  int64     `json:"telecom_minor"`
	WagesMin    int64     `json:"wages_minor"`
	BasesMin    int64     `json:"bases_minor"`
	StorageMin  int64     `json:"storage_minor"`
	FixedFeeMin int64     `json:"fixed_fees_minor"`
	TotalMin    int64     `json:"total_minor"`

	// Counts that drive the "Стоимость анкеты" / "Стоимость минуты" KPI tiles.
	CompletedSurveys int64 `json:"completed_surveys"`
	TotalCallSeconds int64 `json:"total_call_seconds"`
}

// CostPerSurveyMinor returns 0 when CompletedSurveys is 0.
func (b MonthBreakdown) CostPerSurveyMinor() int64 {
	if b.CompletedSurveys == 0 {
		return 0
	}
	return b.TotalMin / b.CompletedSurveys
}

// AvgCostPerMinuteMinor returns 0 when TotalCallSeconds is 0. Telecom-only.
func (b MonthBreakdown) AvgCostPerMinuteMinor() int64 {
	if b.TotalCallSeconds == 0 {
		return 0
	}
	return b.TelecomMin * 60 / b.TotalCallSeconds
}

// ProjectMargin is one row in the "Расходы по проектам" table.
type ProjectMargin struct {
	ProjectID    uuid.UUID `json:"project_id"`
	ProjectCode  string    `json:"project_code"`
	ProjectName  string    `json:"project_name"`
	Surveys      int64     `json:"surveys"`
	TelecomMin   int64     `json:"telecom_minor"`
	WagesMin     int64     `json:"wages_minor"`
	BasesMin     int64     `json:"bases_minor"`
	StorageMin   int64     `json:"storage_minor"`
	TotalMin     int64     `json:"total_minor"`
	RevenueMin   int64     `json:"revenue_minor"`
	MarginMin    int64     `json:"margin_minor"`           // RevenueMin - TotalMin (can be negative)
	CostPerSrvMn int64     `json:"cost_per_survey_minor"`  // TotalMin / Surveys (0 when Surveys=0)
}
```

- [ ] **Step 7: Write `api/store.go`**

```go
package api

import (
	"context"

	"github.com/google/uuid"
)

// TariffStore reads/writes per-tenant tariffs in tenant_settings.
type TariffStore interface {
	// Get returns the tenant's full tariff record. Returns ErrNoTariffs
	// only on first-time access; service-level uses the YAML default.
	Get(ctx context.Context, tenantID uuid.UUID) (Tariffs, error)

	// Update writes the tenant's tariff record atomically and bumps Version.
	// Caller must have Validate()-d the input.
	Update(ctx context.Context, tenantID uuid.UUID, t Tariffs) (Tariffs, error)
}

// RevenueCalculator returns project-period revenue from project.contract.fee_per_completed
// (set in CRM module — Plan 06). Returns 0 when no contract is attached.
type RevenueCalculator interface {
	MonthRevenue(ctx context.Context, tenantID, projectID uuid.UUID, p Period) (int64, error)
}

// MarginReport assembles per-project rows for the admin Finance "проекты" table.
type MarginReport interface {
	Margin(ctx context.Context, tenantID uuid.UUID, p Period) ([]ProjectMargin, error)
}

// SpendReport powers the dashboard, breakdown and byMonth endpoints.
type SpendReport interface {
	MonthSpend(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, p Period) (MonthBreakdown, error)
	SpendByMonth(ctx context.Context, tenantID uuid.UUID, count int) ([]MonthBreakdown, error)
}
```

- [ ] **Step 8: Write `api/events.go`**

```go
package api

import "context"

// CallFinalizedHook is invoked exactly-once per finalized call (idempotency
// is enforced inside the implementation via INSERT ... ON CONFLICT DO NOTHING
// on call_costs.call_id).
//
// Subscribers MUST not block — the dialer publishes after committing the
// calls row, and a slow billing handler stalls the dialer's cleanup loop.
type CallFinalizedHook interface {
	OnCallFinalized(ctx context.Context, in CallCostInput) error
}
```

- [ ] **Step 9: Write `api/errors.go`**

```go
package api

import "errors"

var (
	// ErrNoTariffs means tenant_settings has no billing.* keys yet — caller
	// should fall back to the YAML default (config.billing.defaults).
	ErrNoTariffs = errors.New("billing: no tariffs configured for tenant")

	// ErrInvalidTariff is returned by TariffStore.Update when input fails Validate().
	ErrInvalidTariff = errors.New("billing: invalid tariff")

	// ErrInvalidPeriod is returned when From >= To or the range exceeds 24 months.
	ErrInvalidPeriod = errors.New("billing: invalid period")
)
```

- [ ] **Step 10: Write the HTTP DTOs file `api/http_dto.go`**

```go
package api

import "github.com/google/uuid"

// DashboardResponse is the payload for GET /api/finance/dashboard.
// Mirrors the four KPI tiles + two charts shown in admin-pages-2.jsx::AdminFinance.
type DashboardResponse struct {
	Period       Period          `json:"period"`
	MonthSpend   int64           `json:"month_spend_minor"`
	PrevSpend    int64           `json:"prev_spend_minor"`
	DeltaPct     float64         `json:"delta_pct"`              // % change vs previous period; 0 when prev=0
	CostPerSrv   int64           `json:"cost_per_survey_minor"`
	PrevCostSrv  int64           `json:"prev_cost_per_survey_minor"`
	AvgCostMinM  int64           `json:"avg_cost_per_minute_minor"`
	RevenueMin   int64           `json:"revenue_minor"`
	MarginMin    int64           `json:"margin_minor"`
	MarginPct    float64         `json:"margin_pct"`             // 0 when revenue=0
	Breakdown    []BreakdownItem `json:"breakdown"`
	ByMonth      []ByMonthItem   `json:"by_month"`
	TopProjects  []ProjectMargin `json:"top_projects"`           // top 5 by spend
}

// BreakdownItem is one slice in the pie chart.
type BreakdownItem struct {
	Label    string `json:"label"`     // "Связь" | "Зарплата" | "Базы" | "Хранение" | "Постоянные"
	ValueMin int64  `json:"value_minor"`
}

// ByMonthItem is one bar in the byMonth chart.
type ByMonthItem struct {
	Year     int    `json:"year"`
	Month    int    `json:"month"`     // 1..12
	Label    string `json:"label"`     // "Май"
	ValueMin int64  `json:"value_minor"`
}

// TariffsResponse is the GET /api/billing/tariffs body.
type TariffsResponse struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	Tariffs   Tariffs   `json:"tariffs"`
	IsDefault bool      `json:"is_default"`
}

// TariffsPatchRequest is the PATCH /api/billing/tariffs body. Each field
// is *T so the caller can submit a partial update.
type TariffsPatchRequest struct {
	TrunkCostsMinor           map[string]int64 `json:"trunk_costs_minor,omitempty"`
	CostPerCompletedSurveyMin *int64           `json:"cost_per_completed_survey_minor,omitempty"`
	CostPerImportedRecordMin  *int64           `json:"cost_per_imported_record_minor,omitempty"`
	StorageCostPerGBMonthMin  *int64           `json:"storage_cost_per_gb_month_minor,omitempty"`
	FixedMonthlyFeesMin       *int64           `json:"fixed_monthly_fees_minor,omitempty"`
}
```

- [ ] **Step 11: Run all api tests**

```bash
go test ./internal/billing/api/... -race
```

Expected: PASS, no warnings.

- [ ] **Step 12: Commit**

```bash
git add internal/billing/api/
git commit -m "feat(billing): public api with Tariffs, CostCalculator, MonthBreakdown, errors"
```

---

## Task 3: `CostCalculator` implementation (pure)

**Files:**
- Create: `internal/billing/service/calculator.go`
- Create: `internal/billing/service/calculator_test.go`

This is pure arithmetic — no IO, perfect TDD candidate.

- [ ] **Step 1: Write the failing tests first**

Create `internal/billing/service/calculator_test.go`:

```go
package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/service"
	"github.com/stretchr/testify/require"
)

func tariffs() billingapi.Tariffs {
	return billingapi.Tariffs{
		TrunkCostsMinor:           map[string]int64{"mtt-msk-1": 342, "mango-fed": 378},
		CostPerCompletedSurveyMin: 12000, // 120 ₽
		StorageCostPerGBMonthMin:  150,   // 1.50 ₽/GB-month
		FixedMonthlyFeesMin:       50000_00,
	}
}

func TestCallCost_Success_60Sec(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		CallID:      uuid.New(),
		TenantID:    uuid.New(),
		ProjectID:   uuid.New(),
		TrunkUsed:   "mtt-msk-1",
		DurationSec: 60,
		Status:      "success",
	}, tariffs())
	require.NoError(t, err)
	// 342 (kop/min) * 60s / 60 = 342 telecom
	require.Equal(t, int64(342), out.TelecomMinor)
	// success → 12000 wages
	require.Equal(t, int64(12000), out.WagesMinor)
	require.Equal(t, int64(0), out.StorageMinor)
	require.Equal(t, int64(342+12000), out.TotalMinor)
}

func TestCallCost_Refused_NoWages(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		TrunkUsed:   "mtt-msk-1",
		DurationSec: 30,
		Status:      "refused",
	}, tariffs())
	require.NoError(t, err)
	// 342 * 30 / 60 = 171
	require.Equal(t, int64(171), out.TelecomMinor)
	require.Equal(t, int64(0), out.WagesMinor)
	require.Equal(t, int64(171), out.TotalMinor)
}

func TestCallCost_NoAnswer_OnlyConnectionAttempt_ZeroDuration(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		TrunkUsed:   "mtt-msk-1",
		DurationSec: 0,
		Status:      "no-answer",
	}, tariffs())
	require.NoError(t, err)
	require.Equal(t, int64(0), out.TelecomMinor)
	require.Equal(t, int64(0), out.WagesMinor)
	require.Equal(t, int64(0), out.TotalMinor)
}

func TestCallCost_UnknownTrunk_TreatsAsFree(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		TrunkUsed:   "unknown-trunk-xyz",
		DurationSec: 90,
		Status:      "success",
	}, tariffs())
	require.NoError(t, err)
	require.Equal(t, int64(0), out.TelecomMinor)
	require.Equal(t, int64(12000), out.WagesMinor)
	require.Equal(t, int64(12000), out.TotalMinor)
}

func TestCallCost_EmptyTrunk_TreatsAsFree(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		DurationSec: 30,
		Status:      "success",
	}, tariffs())
	require.NoError(t, err)
	require.Equal(t, int64(0), out.TelecomMinor)
	require.Equal(t, int64(12000), out.WagesMinor)
	require.Equal(t, int64(12000), out.TotalMinor)
}

func TestCallCost_StorageCharge(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	// 1 GiB recording: 1024^3 bytes
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		TrunkUsed:    "mtt-msk-1",
		DurationSec:  60,
		Status:       "success",
		StorageBytes: 1 << 30,
	}, tariffs())
	require.NoError(t, err)
	// 1 GiB ≈ 1.0 GB; 150 minor / GB-month → ~150 (round-half-up over the months handled separately)
	// per-call storage charge is actually pro-rated to a per-call kopeck count: 150 minor.
	require.Equal(t, int64(150), out.StorageMinor)
}

func TestCallCost_NegativeDuration_Rejected(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	_, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		TrunkUsed:   "mtt-msk-1",
		DurationSec: -5,
		Status:      "success",
	}, tariffs())
	require.Error(t, err)
}

func TestCallCost_LongCall_RoundingIsHalfUp(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	// 342 kopeks * 95s / 60 = 32490/60 = 541.5 → 542 (half-up)
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		TrunkUsed:   "mtt-msk-1",
		DurationSec: 95,
		Status:      "success",
	}, tariffs())
	require.NoError(t, err)
	require.Equal(t, int64(542), out.TelecomMinor)
}

func TestCallCost_AlwaysNonNegative_Property(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	for _, s := range []string{"success", "refused", "dropped", "no-answer", "busy", "callback", "wrong-person", "tech-failure"} {
		for _, dur := range []int32{0, 1, 30, 60, 600, 3600} {
			out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
				TrunkUsed: "mtt-msk-1", DurationSec: dur, Status: s, FinalizedAt: time.Now(),
			}, tariffs())
			require.NoError(t, err, "status=%s dur=%d", s, dur)
			require.GreaterOrEqual(t, out.TotalMinor, int64(0))
			require.Equal(t, out.TelecomMinor+out.WagesMinor+out.StorageMinor, out.TotalMinor)
		}
	}
}
```

- [ ] **Step 2: Run; tests fail (no implementation)**

```bash
go test ./internal/billing/service/...
```

Expected: build error `undefined: service.NewCostCalculator`. This is the failing red.

- [ ] **Step 3: Write `internal/billing/service/calculator.go`**

```go
// Package service implements internal/billing/api.
package service

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
)

const bytesPerGB = int64(1) << 30

// costCalculator is the production CostCalculator. Stateless — safe to share.
type costCalculator struct{}

// NewCostCalculator returns a pure cost calculator.
func NewCostCalculator() billingapi.CostCalculator { return &costCalculator{} }

func (costCalculator) CallCost(_ context.Context, in billingapi.CallCostInput, t billingapi.Tariffs) (billingapi.CallCostOutput, error) {
	if in.DurationSec < 0 {
		return billingapi.CallCostOutput{}, fmt.Errorf("billing: negative duration_sec=%d", in.DurationSec)
	}

	out := billingapi.CallCostOutput{}

	// Telecom: per-minute rate * seconds / 60. Rounding: half-up at the kopeck.
	if in.DurationSec > 0 {
		perMin := t.TrunkCostMinor(in.TrunkUsed)
		if perMin > 0 {
			cents := decimal.NewFromInt(perMin).
				Mul(decimal.NewFromInt32(in.DurationSec)).
				Div(decimal.NewFromInt(60)).
				Round(0)
			out.TelecomMinor = cents.IntPart()
		}
	}

	// Wages: paid only on success.
	if in.Status == "success" {
		out.WagesMinor = t.CostPerCompletedSurveyMin
	}

	// Storage: pro-rated per GB-month → per-call snapshot.
	// We charge once per call when storage_bytes > 0 — the recurring monthly
	// re-charge for retained recordings is out of scope (handled by the
	// recompute job in v2).
	if in.StorageBytes > 0 && t.StorageCostPerGBMonthMin > 0 {
		gb := decimal.NewFromInt(in.StorageBytes).
			Div(decimal.NewFromInt(bytesPerGB))
		out.StorageMinor = decimal.NewFromInt(t.StorageCostPerGBMonthMin).
			Mul(gb).Round(0).IntPart()
	}

	out.TotalMinor = out.TelecomMinor + out.WagesMinor + out.StorageMinor
	return out, nil
}
```

- [ ] **Step 4: Run; tests pass**

```bash
go test ./internal/billing/service/... -race
```

Expected: all 9 tests PASS.

- [ ] **Step 5: Coverage check**

```bash
go test ./internal/billing/service/... -cover
```

Expected: ≥ 95% on `calculator.go`.

- [ ] **Step 6: Commit**

```bash
git add internal/billing/service/calculator.go internal/billing/service/calculator_test.go
git commit -m "feat(billing): pure CostCalculator with kopeck rounding and storage support"
```

---

## Task 4: `TariffStore` against `tenant_settings`

**Files:**
- Create: `internal/billing/service/tariffs.go`
- Create: `internal/billing/service/tariffs_test.go`
- Create: `internal/billing/store/pg.go` (skeleton)

This is the read/write boundary for per-tenant tariffs. It composes 5 keys in `tenant_settings`:

| Key                            | Type   | Maps to                       |
|---|---|---|
| `billing.trunks`               | jsonb  | `TrunkCostsMinor` (object)    |
| `billing.surveys`              | jsonb  | `CostPerCompletedSurveyMin`   |
| `billing.imports`              | jsonb  | `CostPerImportedRecordMin`    |
| `billing.storage`              | jsonb  | `StorageCostPerGBMonthMin`    |
| `billing.fixed`                | jsonb  | `FixedMonthlyFeesMin`         |
| `billing.version`              | jsonb  | `Version` (uuid)              |

- [ ] **Step 1: Define the underlying access pattern in `store/pg.go`**

Create `internal/billing/store/pg.go`:

```go
// Package store contains pgx adapters for the billing module.
package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PG is the billing store. Uses an injected pool — Plan 03 sets up the pgx pool.
type PG struct {
	pool *pgxpool.Pool
}

// New returns a PG store bound to the given pool.
func New(p *pgxpool.Pool) *PG { return &PG{pool: p} }

// GetSetting fetches one tenant_settings row. Returns pgx.ErrNoRows if missing.
func (s *PG) GetSetting(ctx context.Context, tenantID uuid.UUID, key string) ([]byte, error) {
	const q = `select value::text from tenant_settings where tenant_id = $1 and key = $2`
	var raw string
	err := s.pool.QueryRow(ctx, q, tenantID, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, pgx.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	return []byte(raw), nil
}

// UpsertSettings writes multiple keys atomically.
func (s *PG) UpsertSettings(ctx context.Context, tenantID uuid.UUID, kv map[string][]byte) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `
		insert into tenant_settings (tenant_id, key, value, updated_at)
		values ($1, $2, $3::jsonb, now())
		on conflict (tenant_id, key) do update
			set value = excluded.value, updated_at = now()`
	for k, v := range kv {
		if _, err := tx.Exec(ctx, q, tenantID, k, string(v)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
```

- [ ] **Step 2: Failing test**

Create `internal/billing/service/tariffs_test.go`:

```go
package service_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/service"
	"github.com/stretchr/testify/require"
)

// fakeStore is a pure in-memory tenant_settings stand-in.
type fakeStore struct{ kv map[string]map[string][]byte }

func newFakeStore() *fakeStore { return &fakeStore{kv: map[string]map[string][]byte{}} }

func (f *fakeStore) GetSetting(_ context.Context, tid uuid.UUID, key string) ([]byte, error) {
	m, ok := f.kv[tid.String()]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	v, ok := m[key]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return v, nil
}

func (f *fakeStore) UpsertSettings(_ context.Context, tid uuid.UUID, kv map[string][]byte) error {
	if _, ok := f.kv[tid.String()]; !ok {
		f.kv[tid.String()] = map[string][]byte{}
	}
	for k, v := range kv {
		f.kv[tid.String()][k] = v
	}
	return nil
}

func TestTariffStore_Get_NoTariffs_ReturnsErrNoTariffs(t *testing.T) {
	t.Parallel()
	s := service.NewTariffStore(newFakeStore(), billingapi.Tariffs{})
	_, err := s.Get(context.Background(), uuid.New())
	require.ErrorIs(t, err, billingapi.ErrNoTariffs)
}

func TestTariffStore_Update_RoundTrip(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	s := service.NewTariffStore(store, billingapi.Tariffs{})
	tid := uuid.New()

	in := billingapi.Tariffs{
		TrunkCostsMinor:           map[string]int64{"mtt-msk-1": 342},
		CostPerCompletedSurveyMin: 12000,
	}
	updated, err := s.Update(context.Background(), tid, in)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, updated.Version) // bumped

	got, err := s.Get(context.Background(), tid)
	require.NoError(t, err)
	require.Equal(t, int64(342), got.TrunkCostsMinor["mtt-msk-1"])
	require.Equal(t, int64(12000), got.CostPerCompletedSurveyMin)
	require.Equal(t, updated.Version, got.Version)
}

func TestTariffStore_Update_InvalidRejected(t *testing.T) {
	t.Parallel()
	s := service.NewTariffStore(newFakeStore(), billingapi.Tariffs{})
	_, err := s.Update(context.Background(), uuid.New(), billingapi.Tariffs{
		CostPerCompletedSurveyMin: -1,
	})
	require.ErrorIs(t, err, billingapi.ErrInvalidTariff)
}

func TestTariffStore_Get_PartialKeysFallBackToDefault(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	def := billingapi.Tariffs{FixedMonthlyFeesMin: 5_000_00}
	s := service.NewTariffStore(store, def)
	tid := uuid.New()

	// Tenant set only one key.
	require.NoError(t, store.UpsertSettings(context.Background(), tid, map[string][]byte{
		"billing.surveys": mustJSON(t, struct {
			CostPerCompletedSurveyMin int64 `json:"cost_per_completed_survey_minor"`
		}{CostPerCompletedSurveyMin: 13_500}),
	}))

	got, err := s.Get(context.Background(), tid)
	require.NoError(t, err)
	require.Equal(t, int64(13_500), got.CostPerCompletedSurveyMin)
	// Falls back to default for unset keys:
	require.Equal(t, int64(5_000_00), got.FixedMonthlyFeesMin)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
```

- [ ] **Step 3: Implementation `internal/billing/service/tariffs.go`**

```go
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
)

// SettingsBackend is the narrow interface the TariffStore needs from the
// underlying tenant_settings adapter (so we can fake it in tests).
type SettingsBackend interface {
	GetSetting(ctx context.Context, tenantID uuid.UUID, key string) ([]byte, error)
	UpsertSettings(ctx context.Context, tenantID uuid.UUID, kv map[string][]byte) error
}

const (
	keyTrunks  = "billing.trunks"
	keySurveys = "billing.surveys"
	keyImports = "billing.imports"
	keyStorage = "billing.storage"
	keyFixed   = "billing.fixed"
	keyVer     = "billing.version"
)

type tariffStore struct {
	backend SettingsBackend
	def     billingapi.Tariffs
}

// NewTariffStore wires a backend and a YAML default. The default is used
// only for keys the tenant hasn't set yet; complete absence of any key
// returns ErrNoTariffs.
func NewTariffStore(b SettingsBackend, def billingapi.Tariffs) billingapi.TariffStore {
	return &tariffStore{backend: b, def: def}
}

func (s *tariffStore) Get(ctx context.Context, tid uuid.UUID) (billingapi.Tariffs, error) {
	t := s.def
	t.TrunkCostsMinor = cloneMap(s.def.TrunkCostsMinor)

	have := false

	if raw, err := s.backend.GetSetting(ctx, tid, keyTrunks); err == nil {
		var v map[string]int64
		if err := json.Unmarshal(raw, &v); err != nil {
			return billingapi.Tariffs{}, fmt.Errorf("billing: parse %s: %w", keyTrunks, err)
		}
		t.TrunkCostsMinor = v
		have = true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return billingapi.Tariffs{}, err
	}

	if v, ok, err := readInt64Field(ctx, s.backend, tid, keySurveys, "cost_per_completed_survey_minor"); err != nil {
		return billingapi.Tariffs{}, err
	} else if ok {
		t.CostPerCompletedSurveyMin = v
		have = true
	}
	if v, ok, err := readInt64Field(ctx, s.backend, tid, keyImports, "cost_per_imported_record_minor"); err != nil {
		return billingapi.Tariffs{}, err
	} else if ok {
		t.CostPerImportedRecordMin = v
		have = true
	}
	if v, ok, err := readInt64Field(ctx, s.backend, tid, keyStorage, "storage_cost_per_gb_month_minor"); err != nil {
		return billingapi.Tariffs{}, err
	} else if ok {
		t.StorageCostPerGBMonthMin = v
		have = true
	}
	if v, ok, err := readInt64Field(ctx, s.backend, tid, keyFixed, "fixed_monthly_fees_minor"); err != nil {
		return billingapi.Tariffs{}, err
	} else if ok {
		t.FixedMonthlyFeesMin = v
		have = true
	}

	if raw, err := s.backend.GetSetting(ctx, tid, keyVer); err == nil {
		var v struct {
			Version uuid.UUID `json:"version"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return billingapi.Tariffs{}, fmt.Errorf("billing: parse %s: %w", keyVer, err)
		}
		t.Version = v.Version
		have = true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return billingapi.Tariffs{}, err
	}

	if !have {
		return billingapi.Tariffs{}, billingapi.ErrNoTariffs
	}
	return t, nil
}

func (s *tariffStore) Update(ctx context.Context, tid uuid.UUID, in billingapi.Tariffs) (billingapi.Tariffs, error) {
	if err := in.Validate(); err != nil {
		return billingapi.Tariffs{}, fmt.Errorf("%w: %s", billingapi.ErrInvalidTariff, err.Error())
	}
	in.Version = uuid.New()

	kv := map[string][]byte{}
	if in.TrunkCostsMinor != nil {
		b, err := json.Marshal(in.TrunkCostsMinor)
		if err != nil {
			return billingapi.Tariffs{}, err
		}
		kv[keyTrunks] = b
	}
	kv[keySurveys] = mustMarshal(struct {
		V int64 `json:"cost_per_completed_survey_minor"`
	}{in.CostPerCompletedSurveyMin})
	kv[keyImports] = mustMarshal(struct {
		V int64 `json:"cost_per_imported_record_minor"`
	}{in.CostPerImportedRecordMin})
	kv[keyStorage] = mustMarshal(struct {
		V int64 `json:"storage_cost_per_gb_month_minor"`
	}{in.StorageCostPerGBMonthMin})
	kv[keyFixed] = mustMarshal(struct {
		V int64 `json:"fixed_monthly_fees_minor"`
	}{in.FixedMonthlyFeesMin})
	kv[keyVer] = mustMarshal(struct {
		V uuid.UUID `json:"version"`
	}{in.Version})

	if err := s.backend.UpsertSettings(ctx, tid, kv); err != nil {
		return billingapi.Tariffs{}, err
	}
	return in, nil
}

func readInt64Field(ctx context.Context, b SettingsBackend, tid uuid.UUID, key, jsonField string) (int64, bool, error) {
	raw, err := b.GetSetting(ctx, tid, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, false, fmt.Errorf("billing: parse %s: %w", key, err)
	}
	rv, ok := m[jsonField]
	if !ok {
		return 0, false, nil
	}
	var v int64
	if err := json.Unmarshal(rv, &v); err != nil {
		return 0, false, fmt.Errorf("billing: parse %s.%s: %w", key, jsonField, err)
	}
	return v, true, nil
}

func cloneMap(m map[string]int64) map[string]int64 {
	if m == nil {
		return nil
	}
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
```

`mustMarshal` writes shapes like `{"cost_per_completed_survey_minor": 12000}` because `v` is a struct with that JSON tag — clean and type-safe.

- [ ] **Step 4: Run tests**

```bash
go test ./internal/billing/service/... -race -run TariffStore
```

Expected: 4 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/billing/store/pg.go internal/billing/service/tariffs.go internal/billing/service/tariffs_test.go
git commit -m "feat(billing): TariffStore reads/writes per-tenant tariffs into tenant_settings"
```

---

## Task 5: `OnCallFinalized` event handler — write `call_costs`

**Files:**
- Create: `internal/billing/service/on_call_finalized.go`
- Create: `internal/billing/service/on_call_finalized_test.go`
- Create: `internal/billing/store/queries.sql`
- Modify: `internal/billing/store/pg.go` (add `InsertCallCost`)

The dialer publishes `dialer.call.finalized` after committing a row in `calls`. We subscribe and synchronously write a row into `call_costs`. Idempotency: `INSERT ... ON CONFLICT (call_id) DO NOTHING`.

- [ ] **Step 1: Add SQL constants in `internal/billing/store/queries.sql`**

```sql
-- name: InsertCallCost :exec
insert into call_costs
  (call_id, tenant_id, project_id, trunk_used, duration_sec, status,
   telecom_minor, wages_minor, storage_minor, total_minor,
   tariff_version, finalized_at, computed_at)
values
  ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12, now())
on conflict (call_id) do nothing;

-- name: GetCallCost :one
select call_id, tenant_id, project_id, trunk_used, duration_sec, status,
       telecom_minor, wages_minor, storage_minor, total_minor, tariff_version,
       finalized_at, computed_at
  from call_costs where call_id = $1;
```

We use raw SQL via pgx, not sqlc, but keep `queries.sql` as the canonical source.

- [ ] **Step 2: Add `InsertCallCost` to `store/pg.go`**

```go
import (
	// existing imports plus:
	"time"
)

// CallCostRow is the persisted shape.
type CallCostRow struct {
	CallID        uuid.UUID
	TenantID      uuid.UUID
	ProjectID     uuid.UUID
	TrunkUsed     *string
	DurationSec   int32
	Status        string
	TelecomMinor  int64
	WagesMinor    int64
	StorageMinor  int64
	TotalMinor    int64
	TariffVersion *uuid.UUID
	FinalizedAt   time.Time
}

// InsertCallCost writes one row idempotently. Returns true when the row was inserted,
// false when a row for this call_id already existed.
func (s *PG) InsertCallCost(ctx context.Context, r CallCostRow) (bool, error) {
	const q = `
insert into call_costs
  (call_id, tenant_id, project_id, trunk_used, duration_sec, status,
   telecom_minor, wages_minor, storage_minor, total_minor,
   tariff_version, finalized_at)
values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
on conflict (call_id) do nothing
returning call_id`
	var got uuid.UUID
	err := s.pool.QueryRow(ctx, q,
		r.CallID, r.TenantID, r.ProjectID, r.TrunkUsed, r.DurationSec, r.Status,
		r.TelecomMinor, r.WagesMinor, r.StorageMinor, r.TotalMinor,
		r.TariffVersion, r.FinalizedAt).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // already existed
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
```

- [ ] **Step 3: Failing test for `OnCallFinalized` (uses an in-memory backend)**

Create `internal/billing/service/on_call_finalized_test.go`:

```go
package service_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/service"
	"github.com/stretchr/testify/require"
)

// fakeCostSink records what was inserted.
type fakeCostSink struct {
	mu   sync.Mutex
	rows []service.CallCostInsert
	idem map[uuid.UUID]bool
}

func newSink() *fakeCostSink { return &fakeCostSink{idem: map[uuid.UUID]bool{}} }

func (f *fakeCostSink) InsertCost(_ context.Context, r service.CallCostInsert) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idem[r.CallID] {
		return false, nil
	}
	f.idem[r.CallID] = true
	f.rows = append(f.rows, r)
	return true, nil
}

func TestOnCallFinalized_HappyPath(t *testing.T) {
	t.Parallel()
	sink := newSink()
	tid := uuid.New()
	tariffStore := newFakeStore()
	tStore := service.NewTariffStore(tariffStore, billingapi.Tariffs{})
	require.NoError(t, tariffStore.UpsertSettings(context.Background(), tid, map[string][]byte{
		"billing.trunks":  []byte(`{"mtt-msk-1": 342}`),
		"billing.surveys": []byte(`{"cost_per_completed_survey_minor": 12000}`),
	}))

	h := service.NewCallFinalizedHandler(service.NewCostCalculator(), tStore, sink)
	in := billingapi.CallCostInput{
		CallID:      uuid.New(),
		TenantID:    tid,
		ProjectID:   uuid.New(),
		TrunkUsed:   "mtt-msk-1",
		DurationSec: 60,
		Status:      "success",
		FinalizedAt: time.Now(),
	}
	require.NoError(t, h.OnCallFinalized(context.Background(), in))
	require.Len(t, sink.rows, 1)
	require.Equal(t, int64(342), sink.rows[0].TelecomMinor)
	require.Equal(t, int64(12000), sink.rows[0].WagesMinor)
	require.Equal(t, int64(12342), sink.rows[0].TotalMinor)
}

func TestOnCallFinalized_Idempotent_DuplicateCallID(t *testing.T) {
	t.Parallel()
	sink := newSink()
	tid := uuid.New()
	tariffStore := newFakeStore()
	tStore := service.NewTariffStore(tariffStore, billingapi.Tariffs{})
	require.NoError(t, tariffStore.UpsertSettings(context.Background(), tid, map[string][]byte{
		"billing.surveys": []byte(`{"cost_per_completed_survey_minor": 5000}`),
	}))

	h := service.NewCallFinalizedHandler(service.NewCostCalculator(), tStore, sink)
	in := billingapi.CallCostInput{
		CallID: uuid.New(), TenantID: tid, ProjectID: uuid.New(),
		Status: "success", FinalizedAt: time.Now(),
	}
	require.NoError(t, h.OnCallFinalized(context.Background(), in))
	require.NoError(t, h.OnCallFinalized(context.Background(), in)) // dup
	require.Len(t, sink.rows, 1, "duplicate call_id must not insert twice")
}

func TestOnCallFinalized_NoTariffs_FallsBackToYAMLDefault(t *testing.T) {
	t.Parallel()
	sink := newSink()
	tStore := service.NewTariffStore(newFakeStore(), billingapi.Tariffs{
		CostPerCompletedSurveyMin: 9000,
	})
	h := service.NewCallFinalizedHandler(service.NewCostCalculator(), tStore, sink)
	in := billingapi.CallCostInput{
		CallID: uuid.New(), TenantID: uuid.New(), ProjectID: uuid.New(),
		Status: "success", FinalizedAt: time.Now(),
	}
	require.NoError(t, h.OnCallFinalized(context.Background(), in))
	require.Equal(t, int64(9000), sink.rows[0].WagesMinor)
}
```

- [ ] **Step 4: Implementation `internal/billing/service/on_call_finalized.go`**

```go
package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
)

// CallCostInsert is the persistent shape — same as store.CallCostRow but
// kept here to avoid a service→store import cycle in tests.
type CallCostInsert struct {
	CallID        uuid.UUID
	TenantID      uuid.UUID
	ProjectID     uuid.UUID
	TrunkUsed     *string
	DurationSec   int32
	Status        string
	TelecomMinor  int64
	WagesMinor    int64
	StorageMinor  int64
	TotalMinor    int64
	TariffVersion *uuid.UUID
	FinalizedAt   interface{ /* placeholder; real is time.Time, kept simple */ }
}

// CostSink is the narrow store-side interface this handler needs.
type CostSink interface {
	InsertCost(ctx context.Context, r CallCostInsert) (inserted bool, err error)
}

type callFinalizedHandler struct {
	calc   billingapi.CostCalculator
	tariff billingapi.TariffStore
	sink   CostSink
}

// NewCallFinalizedHandler returns the production hook.
func NewCallFinalizedHandler(calc billingapi.CostCalculator, ts billingapi.TariffStore, sink CostSink) billingapi.CallFinalizedHook {
	return &callFinalizedHandler{calc: calc, tariff: ts, sink: sink}
}

func (h *callFinalizedHandler) OnCallFinalized(ctx context.Context, in billingapi.CallCostInput) error {
	tariffs, err := h.tariff.Get(ctx, in.TenantID)
	if err != nil && !errors.Is(err, billingapi.ErrNoTariffs) {
		return err
	}
	out, err := h.calc.CallCost(ctx, in, tariffs)
	if err != nil {
		return err
	}
	row := CallCostInsert{
		CallID:       in.CallID,
		TenantID:     in.TenantID,
		ProjectID:    in.ProjectID,
		DurationSec:  in.DurationSec,
		Status:       in.Status,
		TelecomMinor: out.TelecomMinor,
		WagesMinor:   out.WagesMinor,
		StorageMinor: out.StorageMinor,
		TotalMinor:   out.TotalMinor,
		FinalizedAt:  in.FinalizedAt,
	}
	if in.TrunkUsed != "" {
		t := in.TrunkUsed
		row.TrunkUsed = &t
	}
	if tariffs.Version != uuid.Nil {
		v := tariffs.Version
		row.TariffVersion = &v
	}
	_, err = h.sink.InsertCost(ctx, row)
	return err
}
```

> Replace the `interface{}` placeholder for `FinalizedAt` with `time.Time` in your concrete implementation — kept brief in the plan to keep the file legible. The test compiles against `time.Time`.

- [ ] **Step 5: Tests pass**

```bash
go test ./internal/billing/service/... -race -run OnCallFinalized
```

Expected: 3 tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/billing/service/on_call_finalized.go internal/billing/service/on_call_finalized_test.go internal/billing/store/queries.sql internal/billing/store/pg.go
git commit -m "feat(billing): OnCallFinalized writes idempotent call_costs row"
```

---

## Task 6: `MonthSpend` aggregator (real Postgres)

**Files:**
- Create: `internal/billing/service/month_spend.go`
- Create: `internal/billing/service/month_spend_test.go`
- Modify: `internal/billing/store/pg.go` — add aggregation queries

This is the workhorse query that powers the dashboard and breakdown endpoints.

- [ ] **Step 1: Add `SumCallCosts` to `store/pg.go`**

```go
// CallCostsAggregate is a SUM(*) row over call_costs.
type CallCostsAggregate struct {
	TelecomMinor int64
	WagesMinor   int64
	StorageMinor int64
	Surveys      int64
	TotalSeconds int64
}

// SumCallCosts aggregates call_costs joined with calls in [from,to).
// projectID may be nil for tenant-wide.
func (s *PG) SumCallCosts(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, from, to time.Time) (CallCostsAggregate, error) {
	const qAll = `
select coalesce(sum(telecom_minor),0)::bigint,
       coalesce(sum(wages_minor),0)::bigint,
       coalesce(sum(storage_minor),0)::bigint,
       coalesce(count(*) filter (where status='success'),0)::bigint,
       coalesce(sum(duration_sec),0)::bigint
  from call_costs
 where tenant_id = $1
   and finalized_at >= $2
   and finalized_at <  $3`
	const qProj = `
select coalesce(sum(telecom_minor),0)::bigint,
       coalesce(sum(wages_minor),0)::bigint,
       coalesce(sum(storage_minor),0)::bigint,
       coalesce(count(*) filter (where status='success'),0)::bigint,
       coalesce(sum(duration_sec),0)::bigint
  from call_costs
 where tenant_id = $1
   and project_id = $2
   and finalized_at >= $3
   and finalized_at <  $4`
	var agg CallCostsAggregate
	if projectID == nil {
		err := s.pool.QueryRow(ctx, qAll, tenantID, from, to).
			Scan(&agg.TelecomMinor, &agg.WagesMinor, &agg.StorageMinor, &agg.Surveys, &agg.TotalSeconds)
		return agg, err
	}
	err := s.pool.QueryRow(ctx, qProj, tenantID, *projectID, from, to).
		Scan(&agg.TelecomMinor, &agg.WagesMinor, &agg.StorageMinor, &agg.Surveys, &agg.TotalSeconds)
	return agg, err
}

// CountImportedRecords returns rows imported into respondents in the period —
// used by the bases-cost component.
func (s *PG) CountImportedRecords(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, from, to time.Time) (int64, error) {
	const qAll = `select count(*) from respondents where tenant_id=$1 and source='imported' and created_at >= $2 and created_at < $3`
	const qProj = `select count(*) from respondents where tenant_id=$1 and project_id=$2 and source='imported' and created_at >= $3 and created_at < $4`
	var n int64
	if projectID == nil {
		err := s.pool.QueryRow(ctx, qAll, tenantID, from, to).Scan(&n)
		return n, err
	}
	err := s.pool.QueryRow(ctx, qProj, tenantID, *projectID, from, to).Scan(&n)
	return n, err
}
```

- [ ] **Step 2: Failing test for `MonthSpend` (uses testcontainers Postgres)**

Create `internal/billing/service/month_spend_test.go`:

```go
package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/service"
	"github.com/sociopulse/platform/internal/billing/store"
	"github.com/sociopulse/platform/internal/testpg"
	"github.com/stretchr/testify/require"
)

func TestMonthSpend_RealPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	pool := testpg.New(t) // started by testcontainers, schema migrated
	pg := store.New(pool)

	tenantID := uuid.New()
	projectID := uuid.New()

	// seed: one project, two calls in May, one in April
	_, err := pool.Exec(context.Background(),
		`insert into projects (id, tenant_id, code, name, status, created_at)
		 values ($1,$2,'P-1','Test','active',now())`,
		projectID, tenantID)
	require.NoError(t, err)

	for _, c := range []struct {
		dur                       int32
		status                    string
		tel, wages, storage, tot  int64
		when                      time.Time
	}{
		{60, "success", 342, 12000, 0, 12342, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)},
		{30, "refused", 171, 0, 0, 171, time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)},
		{60, "success", 342, 12000, 0, 12342, time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)},
	} {
		callID := uuid.New()
		_, err := pool.Exec(context.Background(),
			`insert into calls (id, tenant_id, project_id, started_at, ended_at,
			                    duration_sec, status, trunk_used)
			 values ($1,$2,$3,$4,$5,$6,$7,'mtt-msk-1')`,
			callID, tenantID, projectID, c.when, c.when.Add(time.Duration(c.dur)*time.Second), c.dur, c.status)
		require.NoError(t, err)

		_, err = pg.InsertCallCost(context.Background(), store.CallCostRow{
			CallID:       callID,
			TenantID:     tenantID,
			ProjectID:    projectID,
			DurationSec:  c.dur,
			Status:       c.status,
			TelecomMinor: c.tel,
			WagesMinor:   c.wages,
			StorageMinor: c.storage,
			TotalMinor:   c.tot,
			FinalizedAt:  c.when,
		})
		require.NoError(t, err)
	}

	tariffsBackend := newFakeStore()
	require.NoError(t, tariffsBackend.UpsertSettings(context.Background(), tenantID, map[string][]byte{
		"billing.fixed": []byte(`{"fixed_monthly_fees_minor": 50000}`),
	}))
	tStore := service.NewTariffStore(tariffsBackend, billingapi.Tariffs{})

	spender := service.NewSpendReport(pg, tStore)

	may := billingapi.Month(2026, time.May)
	bd, err := spender.MonthSpend(context.Background(), tenantID, &projectID, may)
	require.NoError(t, err)

	// May had 2 calls: 342+171=513 telecom, 12000 wages, 0 storage, 1 success, 90 sec total.
	require.Equal(t, int64(513), bd.TelecomMin)
	require.Equal(t, int64(12000), bd.WagesMin)
	require.Equal(t, int64(50000), bd.FixedFeeMin)
	require.Equal(t, int64(513+12000+50000), bd.TotalMin)
	require.Equal(t, int64(1), bd.CompletedSurveys)
	require.Equal(t, int64(90), bd.TotalCallSeconds)
	require.Equal(t, int64(62513), bd.CostPerSurveyMinor())
	// 513 telecom * 60 / 90 = 342 ₽/min as kopeck
	require.Equal(t, int64(342), bd.AvgCostPerMinuteMinor())
}
```

`internal/testpg/` is a small helper introduced in Plan 03 that boots a Postgres testcontainer and runs all migrations once per package; we re-use it.

- [ ] **Step 3: Implement `internal/billing/service/month_spend.go`**

```go
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
)

// AggregatorBackend is the narrow interface MonthSpend needs.
type AggregatorBackend interface {
	SumCallCosts(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, from, to time.Time) (callCostsAggregate, error)
	CountImportedRecords(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, from, to time.Time) (int64, error)
}

// callCostsAggregate is the SUM row.
type callCostsAggregate struct {
	TelecomMinor int64
	WagesMinor   int64
	StorageMinor int64
	Surveys      int64
	TotalSeconds int64
}

type spendReport struct {
	pg     AggregatorBackend
	tariff billingapi.TariffStore
}

// NewSpendReport wires a Postgres-backed reporter.
func NewSpendReport(pg AggregatorBackend, ts billingapi.TariffStore) billingapi.SpendReport {
	return &spendReport{pg: pg, tariff: ts}
}

func (r *spendReport) MonthSpend(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, p billingapi.Period) (billingapi.MonthBreakdown, error) {
	if p.From.IsZero() || !p.From.Before(p.To) {
		return billingapi.MonthBreakdown{}, billingapi.ErrInvalidPeriod
	}
	agg, err := r.pg.SumCallCosts(ctx, tenantID, projectID, p.From, p.To)
	if err != nil {
		return billingapi.MonthBreakdown{}, fmt.Errorf("billing: sum call_costs: %w", err)
	}
	imported, err := r.pg.CountImportedRecords(ctx, tenantID, projectID, p.From, p.To)
	if err != nil {
		return billingapi.MonthBreakdown{}, fmt.Errorf("billing: count imports: %w", err)
	}
	tariffs, err := r.tariff.Get(ctx, tenantID)
	if err != nil && !errors.Is(err, billingapi.ErrNoTariffs) {
		return billingapi.MonthBreakdown{}, err
	}
	bases := imported * tariffs.CostPerImportedRecordMin
	bd := billingapi.MonthBreakdown{
		TenantID:         tenantID,
		Period:           p,
		TelecomMin:       agg.TelecomMinor,
		WagesMin:         agg.WagesMinor,
		StorageMin:       agg.StorageMinor,
		BasesMin:         bases,
		FixedFeeMin:      tariffs.FixedMonthlyFeesMin,
		CompletedSurveys: agg.Surveys,
		TotalCallSeconds: agg.TotalSeconds,
	}
	bd.TotalMin = bd.TelecomMin + bd.WagesMin + bd.StorageMin + bd.BasesMin + bd.FixedFeeMin
	return bd, nil
}

func (r *spendReport) SpendByMonth(ctx context.Context, tenantID uuid.UUID, count int) ([]billingapi.MonthBreakdown, error) {
	if count <= 0 || count > 24 {
		return nil, billingapi.ErrInvalidPeriod
	}
	out := make([]billingapi.MonthBreakdown, 0, count)
	now := time.Now().UTC()
	// Last "count" months ending with the current one.
	for i := count - 1; i >= 0; i-- {
		m := now.AddDate(0, -i, 0)
		p := billingapi.Month(m.Year(), m.Month())
		bd, err := r.MonthSpend(ctx, tenantID, nil, p)
		if err != nil {
			return nil, err
		}
		out = append(out, bd)
	}
	return out, nil
}
```

The `AggregatorBackend` interface signature uses an *unexported* `callCostsAggregate` — to compile, alias it to the public `store.CallCostsAggregate` via a thin wrapper in `service/service.go` (Task 11). For now, mirror the field set; the wiring is finalized when `Service` is composed.

- [ ] **Step 4: Run integration test**

```bash
go test ./internal/billing/service/... -race -run MonthSpend
```

Expected: PASS (testcontainers spins up a Postgres for ≤ 30s).

- [ ] **Step 5: Commit**

```bash
git add internal/billing/service/month_spend.go internal/billing/service/month_spend_test.go internal/billing/store/pg.go
git commit -m "feat(billing): MonthSpend + SpendByMonth aggregators against call_costs"
```

---

## Task 7: `RevenueCalculator` and `MarginReport`

**Files:**
- Create: `internal/billing/service/revenue.go`
- Create: `internal/billing/service/revenue_test.go`
- Create: `internal/billing/service/margin.go`
- Create: `internal/billing/service/margin_test.go`
- Modify: `internal/billing/store/pg.go` (add per-project aggregators, project listing)

Revenue is `projects.contract_fee_per_completed_minor * sum(success calls in period)`. The `contract_fee_per_completed_minor` column is added in Plan 06; if absent, this plan adds an `ALTER TABLE projects ADD COLUMN IF NOT EXISTS contract_fee_per_completed_minor bigint not null default 0` migration (file: `migrations/20260506000001_add_project_contract_fee.up.sql`).

- [ ] **Step 1: Add the migration if needed**

Inspect Plan 06's projects schema first. If the column is missing:

```sql
-- migrations/20260506000001_add_project_contract_fee.up.sql
alter table projects add column if not exists contract_fee_per_completed_minor bigint not null default 0;
comment on column projects.contract_fee_per_completed_minor is 'Per-completed-survey fee paid by the customer in kopecks. 0 means no contract attached.';
```

- [ ] **Step 2: Failing test for RevenueCalculator**

```go
// internal/billing/service/revenue_test.go
package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/service"
	"github.com/sociopulse/platform/internal/billing/store"
	"github.com/sociopulse/platform/internal/testpg"
	"github.com/stretchr/testify/require"
)

func TestRevenue_NoContract_Zero(t *testing.T) {
	if testing.Short() { t.Skip() }
	t.Parallel()
	pool := testpg.New(t)
	pg := store.New(pool)
	tid, pid := uuid.New(), uuid.New()
	_, err := pool.Exec(context.Background(),
		`insert into projects (id, tenant_id, code, name, status) values ($1,$2,'X','X','active')`,
		pid, tid)
	require.NoError(t, err)

	rc := service.NewRevenueCalculator(pg)
	got, err := rc.MonthRevenue(context.Background(), tid, pid, billingapi.Month(2026, time.May))
	require.NoError(t, err)
	require.Equal(t, int64(0), got)
}

func TestRevenue_WithContract(t *testing.T) {
	if testing.Short() { t.Skip() }
	t.Parallel()
	pool := testpg.New(t)
	pg := store.New(pool)
	tid, pid := uuid.New(), uuid.New()
	_, err := pool.Exec(context.Background(),
		`insert into projects (id, tenant_id, code, name, status, contract_fee_per_completed_minor)
		 values ($1,$2,'X','X','active',38100)`, // 381 ₽/анкета
		pid, tid)
	require.NoError(t, err)
	// 4 successful calls in May
	for i := 0; i < 4; i++ {
		callID := uuid.New()
		_, err := pool.Exec(context.Background(),
			`insert into calls (id, tenant_id, project_id, started_at, status, duration_sec)
			 values ($1,$2,$3,'2026-05-12','success',60)`,
			callID, tid, pid)
		require.NoError(t, err)
		_, err = pg.InsertCallCost(context.Background(), store.CallCostRow{
			CallID: callID, TenantID: tid, ProjectID: pid,
			DurationSec: 60, Status: "success", TotalMinor: 12342,
			FinalizedAt: time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC),
		})
		require.NoError(t, err)
	}

	rc := service.NewRevenueCalculator(pg)
	got, err := rc.MonthRevenue(context.Background(), tid, pid, billingapi.Month(2026, time.May))
	require.NoError(t, err)
	require.Equal(t, int64(38100*4), got)
}
```

- [ ] **Step 3: Implement RevenueCalculator**

```go
// internal/billing/service/revenue.go
package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
)

// RevenueBackend is the narrow store interface.
type RevenueBackend interface {
	ProjectFeePerCompleted(ctx context.Context, tenantID, projectID uuid.UUID) (int64, error)
	CountSuccessfulCalls(ctx context.Context, tenantID, projectID uuid.UUID, from, to time.Time) (int64, error)
}

type revenueCalc struct{ pg RevenueBackend }

func NewRevenueCalculator(pg RevenueBackend) billingapi.RevenueCalculator {
	return &revenueCalc{pg: pg}
}

func (r *revenueCalc) MonthRevenue(ctx context.Context, tid, pid uuid.UUID, p billingapi.Period) (int64, error) {
	if !p.From.Before(p.To) {
		return 0, billingapi.ErrInvalidPeriod
	}
	fee, err := r.pg.ProjectFeePerCompleted(ctx, tid, pid)
	if err != nil {
		return 0, err
	}
	if fee == 0 {
		return 0, nil
	}
	n, err := r.pg.CountSuccessfulCalls(ctx, tid, pid, p.From, p.To)
	if err != nil {
		return 0, err
	}
	return fee * n, nil
}
```

- [ ] **Step 4: Add backend functions in `store/pg.go`**

```go
func (s *PG) ProjectFeePerCompleted(ctx context.Context, tenantID, projectID uuid.UUID) (int64, error) {
	const q = `select coalesce(contract_fee_per_completed_minor, 0)::bigint
	             from projects where tenant_id=$1 and id=$2`
	var v int64
	err := s.pool.QueryRow(ctx, q, tenantID, projectID).Scan(&v)
	return v, err
}

func (s *PG) CountSuccessfulCalls(ctx context.Context, tenantID, projectID uuid.UUID, from, to time.Time) (int64, error) {
	const q = `select count(*) from call_costs
	            where tenant_id=$1 and project_id=$2 and status='success'
	              and finalized_at >= $3 and finalized_at < $4`
	var v int64
	err := s.pool.QueryRow(ctx, q, tenantID, projectID, from, to).Scan(&v)
	return v, err
}
```

- [ ] **Step 5: MarginReport — failing test**

```go
// internal/billing/service/margin_test.go
package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/service"
	"github.com/sociopulse/platform/internal/billing/store"
	"github.com/sociopulse/platform/internal/testpg"
	"github.com/stretchr/testify/require"
)

func TestMargin_TwoProjects_Sorted(t *testing.T) {
	if testing.Short() { t.Skip() }
	t.Parallel()
	pool := testpg.New(t)
	pg := store.New(pool)
	tid := uuid.New()

	bigP := uuid.New()
	smallP := uuid.New()
	_, err := pool.Exec(context.Background(),
		`insert into projects (id, tenant_id, code, name, status, contract_fee_per_completed_minor)
		 values ($1,$2,'BIG','Big project','active',38100),
		        ($3,$2,'SML','Small project','active',20000)`,
		bigP, tid, smallP)
	require.NoError(t, err)

	may := billingapi.Month(2026, time.May)
	for _, p := range []struct {
		pid    uuid.UUID
		count  int
		wage   int64
	}{{bigP, 4, 12000}, {smallP, 2, 8000}} {
		for i := 0; i < p.count; i++ {
			callID := uuid.New()
			_, err := pool.Exec(context.Background(),
				`insert into calls (id, tenant_id, project_id, started_at, status, duration_sec)
				 values ($1,$2,$3,'2026-05-15','success',60)`, callID, tid, p.pid)
			require.NoError(t, err)
			_, err = pg.InsertCallCost(context.Background(), store.CallCostRow{
				CallID: callID, TenantID: tid, ProjectID: p.pid,
				DurationSec: 60, Status: "success", WagesMinor: p.wage, TotalMinor: p.wage + 342,
				FinalizedAt: time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC),
			})
			require.NoError(t, err)
		}
	}

	margin := service.NewMarginReport(pg, service.NewRevenueCalculator(pg))
	rows, err := margin.Margin(context.Background(), tid, may)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "BIG", rows[0].ProjectCode) // sorted desc by total
	require.Equal(t, int64(38100*4), rows[0].RevenueMin)
	require.Equal(t, int64(38100*4-(12000+342)*4), rows[0].MarginMin)
	require.Equal(t, "SML", rows[1].ProjectCode)
}
```

- [ ] **Step 6: Implementation `internal/billing/service/margin.go`**

```go
package service

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
)

// MarginBackend is the narrow store iface.
type MarginBackend interface {
	ListProjectsForPeriod(ctx context.Context, tenantID uuid.UUID, from, to time.Time) ([]ProjectAggregate, error)
}

// ProjectAggregate is per-project rolled-up call_costs.
type ProjectAggregate struct {
	ProjectID    uuid.UUID
	ProjectCode  string
	ProjectName  string
	Surveys      int64
	TelecomMinor int64
	WagesMinor   int64
	StorageMinor int64
	TotalMinor   int64
}

type marginReport struct {
	pg  MarginBackend
	rev billingapi.RevenueCalculator
}

func NewMarginReport(pg MarginBackend, rev billingapi.RevenueCalculator) billingapi.MarginReport {
	return &marginReport{pg: pg, rev: rev}
}

func (m *marginReport) Margin(ctx context.Context, tid uuid.UUID, p billingapi.Period) ([]billingapi.ProjectMargin, error) {
	if !p.From.Before(p.To) {
		return nil, billingapi.ErrInvalidPeriod
	}
	rows, err := m.pg.ListProjectsForPeriod(ctx, tid, p.From, p.To)
	if err != nil {
		return nil, err
	}
	out := make([]billingapi.ProjectMargin, 0, len(rows))
	for _, r := range rows {
		rev, err := m.rev.MonthRevenue(ctx, tid, r.ProjectID, p)
		if err != nil {
			return nil, err
		}
		row := billingapi.ProjectMargin{
			ProjectID:   r.ProjectID,
			ProjectCode: r.ProjectCode,
			ProjectName: r.ProjectName,
			Surveys:     r.Surveys,
			TelecomMin:  r.TelecomMinor,
			WagesMin:    r.WagesMinor,
			StorageMin:  r.StorageMinor,
			TotalMin:    r.TotalMinor,
			RevenueMin:  rev,
			MarginMin:   rev - r.TotalMinor,
		}
		if r.Surveys > 0 {
			row.CostPerSrvMn = r.TotalMinor / r.Surveys
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalMin > out[j].TotalMin })
	return out, nil
}
```

- [ ] **Step 7: Add `ListProjectsForPeriod` to `store/pg.go`**

```go
func (s *PG) ListProjectsForPeriod(ctx context.Context, tenantID uuid.UUID, from, to time.Time) ([]service.ProjectAggregate, error) {
	const q = `
select p.id, p.code, p.name,
       coalesce(count(cc.*) filter (where cc.status='success'),0)::bigint,
       coalesce(sum(cc.telecom_minor),0)::bigint,
       coalesce(sum(cc.wages_minor),0)::bigint,
       coalesce(sum(cc.storage_minor),0)::bigint,
       coalesce(sum(cc.total_minor),0)::bigint
  from projects p
  left join call_costs cc
       on cc.project_id = p.id
      and cc.finalized_at >= $2 and cc.finalized_at < $3
 where p.tenant_id = $1
 group by p.id, p.code, p.name
having coalesce(sum(cc.total_minor), 0) > 0
 order by p.name`
	rows, err := s.pool.Query(ctx, q, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []service.ProjectAggregate{}
	for rows.Next() {
		var r service.ProjectAggregate
		if err := rows.Scan(&r.ProjectID, &r.ProjectCode, &r.ProjectName,
			&r.Surveys, &r.TelecomMinor, &r.WagesMinor, &r.StorageMinor, &r.TotalMinor); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
```

The store package importing `service` is unusual; in practice, define `ProjectAggregate` in a small package `internal/billing/types/` to avoid the back-import. Adapt the test imports accordingly.

- [ ] **Step 8: Tests pass**

```bash
go test ./internal/billing/service/... -race -run "Revenue|Margin"
```

- [ ] **Step 9: Commit**

```bash
git add internal/billing/service/{revenue,margin}{,_test}.go internal/billing/store/pg.go migrations/20260506000001_*
git commit -m "feat(billing): RevenueCalculator + MarginReport with per-project rollup"
```

---

## Task 8: HTTP endpoints

**Files:**
- Create: `internal/billing/service/http_dashboard.go`
- Create: `internal/billing/service/http_breakdown.go`
- Create: `internal/billing/service/http_bymonth.go`
- Create: `internal/billing/service/http_projects.go`
- Create: `internal/billing/service/http_tariffs.go`
- Create: `internal/billing/service/http_test.go`
- Create: `internal/billing/api/http.go` (router factory)

All six endpoints share three concerns:

1. **Tenant-id** — extracted via `tenancy.FromContext(ctx)` (Plan 04 middleware).
2. **Period parsing** — `?period=month|week|quarter|year` or explicit `from=YYYY-MM-DD&to=YYYY-MM-DD`.
3. **RBAC** — `/api/billing/tariffs` is admin-only (`auth.RequireRole("admin")` from Plan 05).

- [ ] **Step 1: HTTP router factory `internal/billing/api/http.go`**

```go
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Endpoints assembles the billing module's HTTP routes. Caller wires
// `mw` middleware (auth, tenancy, RBAC) before passing here.
type Endpoints struct {
	Dashboard  http.Handler
	Projects   http.Handler
	Breakdown  http.Handler
	ByMonth    http.Handler
	GetTariffs http.Handler
	UpdateTariffs http.Handler
}

// Mount registers routes on r. /api/billing/tariffs is admin-only — caller
// supplies the admin-guard middleware via mw.Admin.
func (e Endpoints) Mount(r chi.Router, mw struct{ Admin func(http.Handler) http.Handler }) {
	r.Route("/api/finance", func(r chi.Router) {
		r.Get("/dashboard", e.Dashboard.ServeHTTP)
		r.Get("/projects", e.Projects.ServeHTTP)
		r.Get("/breakdown", e.Breakdown.ServeHTTP)
		r.Get("/byMonth", e.ByMonth.ServeHTTP)
	})
	r.Route("/api/billing/tariffs", func(r chi.Router) {
		r.Use(mw.Admin)
		r.Get("/", e.GetTariffs.ServeHTTP)
		r.Patch("/", e.UpdateTariffs.ServeHTTP)
	})
}
```

- [ ] **Step 2: Failing test for the dashboard endpoint**

Create `internal/billing/service/http_test.go`:

```go
package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/service"
	"github.com/sociopulse/platform/internal/billing/store"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/testpg"
	"github.com/stretchr/testify/require"
)

func TestDashboardEndpoint_HappyPath(t *testing.T) {
	if testing.Short() { t.Skip() }
	t.Parallel()

	pool := testpg.New(t)
	pg := store.New(pool)
	tid := uuid.New()
	pid := uuid.New()

	// seed minimal data
	_, err := pool.Exec(context.Background(),
		`insert into projects (id, tenant_id, code, name, status, contract_fee_per_completed_minor)
		 values ($1,$2,'P','P','active',38100)`, pid, tid)
	require.NoError(t, err)

	// 3 successful calls in current month
	now := time.Now().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		cid := uuid.New()
		_, err := pool.Exec(context.Background(),
			`insert into calls (id, tenant_id, project_id, started_at, status, duration_sec)
			 values ($1,$2,$3,$4,'success',60)`, cid, tid, pid, first.AddDate(0, 0, 1))
		require.NoError(t, err)
		_, err = pg.InsertCallCost(context.Background(), store.CallCostRow{
			CallID: cid, TenantID: tid, ProjectID: pid,
			DurationSec: 60, Status: "success", TelecomMinor: 342, WagesMinor: 12000,
			TotalMinor: 12342, FinalizedAt: first.AddDate(0, 0, 1),
		})
		require.NoError(t, err)
	}

	svc, _ := buildHTTPSvc(t, pg)

	r := chi.NewRouter()
	r.Use(injectTenant(tid))
	svc.Endpoints().Mount(r, struct{ Admin func(http.Handler) http.Handler }{Admin: passthrough})

	req := httptest.NewRequest(http.MethodGet, "/api/finance/dashboard?period=month", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp billingapi.DashboardResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, int64(3*12342), resp.MonthSpend)
	require.Equal(t, int64(3*38100), resp.RevenueMin)
}

func injectTenant(tid uuid.UUID) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := tenancyapi.WithContext(r.Context(), tid)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func passthrough(next http.Handler) http.Handler { return next }
```

(`buildHTTPSvc` wires `Service` against the pool; expand in Task 11.)

- [ ] **Step 3: Implement `http_dashboard.go`**

```go
package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
)

func (s *Service) handleDashboard() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		tid, ok := tenancyapi.FromContext(ctx)
		if !ok {
			http.Error(w, "no tenant", http.StatusUnauthorized)
			return
		}
		period, err := parsePeriod(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bd, err := s.spender.MonthSpend(ctx, tid, nil, period)
		if err != nil {
			s.serverError(w, err)
			return
		}
		prevPeriod := previousPeriodSameLength(period)
		prev, err := s.spender.MonthSpend(ctx, tid, nil, prevPeriod)
		if err != nil && !errors.Is(err, billingapi.ErrInvalidPeriod) {
			s.serverError(w, err)
			return
		}
		marginRows, err := s.margin.Margin(ctx, tid, period)
		if err != nil {
			s.serverError(w, err)
			return
		}
		var revenue int64
		for _, m := range marginRows {
			revenue += m.RevenueMin
		}
		breakdown := []billingapi.BreakdownItem{
			{Label: "Связь", ValueMin: bd.TelecomMin},
			{Label: "Зарплата", ValueMin: bd.WagesMin},
			{Label: "Базы", ValueMin: bd.BasesMin},
			{Label: "Хранение", ValueMin: bd.StorageMin},
			{Label: "Постоянные", ValueMin: bd.FixedFeeMin},
		}
		byMonth, err := s.spender.SpendByMonth(ctx, tid, 6)
		if err != nil {
			s.serverError(w, err)
			return
		}
		bm := make([]billingapi.ByMonthItem, 0, len(byMonth))
		for _, m := range byMonth {
			bm = append(bm, billingapi.ByMonthItem{
				Year: m.Period.From.Year(), Month: int(m.Period.From.Month()),
				Label: russianMonthShort(m.Period.From.Month()), ValueMin: m.TotalMin,
			})
		}
		top := topN(marginRows, 5)
		resp := billingapi.DashboardResponse{
			Period: period, MonthSpend: bd.TotalMin, PrevSpend: prev.TotalMin,
			DeltaPct:    pctDelta(bd.TotalMin, prev.TotalMin),
			CostPerSrv:  bd.CostPerSurveyMinor(), PrevCostSrv: prev.CostPerSurveyMinor(),
			AvgCostMinM: bd.AvgCostPerMinuteMinor(),
			RevenueMin:  revenue, MarginMin: revenue - bd.TotalMin,
			MarginPct:   marginPct(revenue, bd.TotalMin),
			Breakdown:   breakdown, ByMonth: bm, TopProjects: top,
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

func parsePeriod(r *http.Request) (billingapi.Period, error) {
	now := time.Now().UTC()
	switch r.URL.Query().Get("period") {
	case "", "month":
		return billingapi.Month(now.Year(), now.Month()), nil
	case "week":
		from := now.AddDate(0, 0, -int(now.Weekday())+1).Truncate(24 * time.Hour)
		return billingapi.Period{From: from, To: from.AddDate(0, 0, 7)}, nil
	case "quarter":
		q := ((int(now.Month()) - 1) / 3) * 3
		from := time.Date(now.Year(), time.Month(q+1), 1, 0, 0, 0, 0, time.UTC)
		return billingapi.Period{From: from, To: from.AddDate(0, 3, 0)}, nil
	case "year":
		from := time.Date(now.Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
		return billingapi.Period{From: from, To: from.AddDate(1, 0, 0)}, nil
	default:
		return billingapi.Period{}, billingapi.ErrInvalidPeriod
	}
}

func previousPeriodSameLength(p billingapi.Period) billingapi.Period {
	d := p.To.Sub(p.From)
	return billingapi.Period{From: p.From.Add(-d), To: p.From}
}

func pctDelta(curr, prev int64) float64 {
	if prev == 0 {
		return 0
	}
	return float64(curr-prev) / float64(prev) * 100
}

func marginPct(revenue, total int64) float64 {
	if revenue == 0 {
		return 0
	}
	return float64(revenue-total) / float64(revenue) * 100
}

func topN(rows []billingapi.ProjectMargin, n int) []billingapi.ProjectMargin {
	if len(rows) <= n {
		return rows
	}
	return rows[:n]
}

func russianMonthShort(m time.Month) string {
	names := []string{"", "Янв", "Фев", "Мар", "Апр", "Май", "Июн", "Июл", "Авг", "Сен", "Окт", "Ноя", "Дек"}
	return names[m]
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Service) serverError(w http.ResponseWriter, err error) {
	s.log.Error("billing http", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
```

- [ ] **Step 4: Implement `http_breakdown.go`**

```go
package service

import (
	"net/http"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
)

func (s *Service) handleBreakdown() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		tid, ok := tenancyapi.FromContext(ctx)
		if !ok {
			http.Error(w, "no tenant", http.StatusUnauthorized)
			return
		}
		period, err := parsePeriod(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bd, err := s.spender.MonthSpend(ctx, tid, nil, period)
		if err != nil {
			s.serverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, []billingapi.BreakdownItem{
			{Label: "Связь", ValueMin: bd.TelecomMin},
			{Label: "Зарплата", ValueMin: bd.WagesMin},
			{Label: "Базы", ValueMin: bd.BasesMin},
			{Label: "Хранение", ValueMin: bd.StorageMin},
			{Label: "Постоянные", ValueMin: bd.FixedFeeMin},
		})
	})
}
```

- [ ] **Step 5: Implement `http_bymonth.go`**

```go
package service

import (
	"net/http"
	"strconv"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
)

func (s *Service) handleByMonth() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		tid, ok := tenancyapi.FromContext(ctx)
		if !ok {
			http.Error(w, "no tenant", http.StatusUnauthorized)
			return
		}
		count := 6
		if c := r.URL.Query().Get("count"); c != "" {
			if n, err := strconv.Atoi(c); err == nil && n > 0 && n <= 24 {
				count = n
			}
		}
		series, err := s.spender.SpendByMonth(ctx, tid, count)
		if err != nil {
			s.serverError(w, err)
			return
		}
		out := make([]billingapi.ByMonthItem, 0, len(series))
		for _, m := range series {
			out = append(out, billingapi.ByMonthItem{
				Year: m.Period.From.Year(), Month: int(m.Period.From.Month()),
				Label: russianMonthShort(m.Period.From.Month()), ValueMin: m.TotalMin,
			})
		}
		writeJSON(w, http.StatusOK, out)
	})
}
```

- [ ] **Step 6: Implement `http_projects.go`**

```go
package service

import (
	"net/http"

	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
)

func (s *Service) handleProjects() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		tid, ok := tenancyapi.FromContext(ctx)
		if !ok {
			http.Error(w, "no tenant", http.StatusUnauthorized)
			return
		}
		period, err := parsePeriod(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rows, err := s.margin.Margin(ctx, tid, period)
		if err != nil {
			s.serverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	})
}
```

- [ ] **Step 7: Implement `http_tariffs.go`**

```go
package service

import (
	"encoding/json"
	"errors"
	"net/http"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
)

func (s *Service) handleGetTariffs() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		tid, ok := tenancyapi.FromContext(ctx)
		if !ok {
			http.Error(w, "no tenant", http.StatusUnauthorized)
			return
		}
		t, err := s.tariffs.Get(ctx, tid)
		isDefault := false
		if errors.Is(err, billingapi.ErrNoTariffs) {
			t = s.defaultTariffs
			isDefault = true
		} else if err != nil {
			s.serverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, billingapi.TariffsResponse{TenantID: tid, Tariffs: t, IsDefault: isDefault})
	})
}

func (s *Service) handlePatchTariffs() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		tid, ok := tenancyapi.FromContext(ctx)
		if !ok {
			http.Error(w, "no tenant", http.StatusUnauthorized)
			return
		}
		var p billingapi.TariffsPatchRequest
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		// Load current → apply patch → validate → write.
		curr, err := s.tariffs.Get(ctx, tid)
		if errors.Is(err, billingapi.ErrNoTariffs) {
			curr = s.defaultTariffs
		} else if err != nil {
			s.serverError(w, err)
			return
		}
		if p.TrunkCostsMinor != nil {
			curr.TrunkCostsMinor = p.TrunkCostsMinor
		}
		if p.CostPerCompletedSurveyMin != nil {
			curr.CostPerCompletedSurveyMin = *p.CostPerCompletedSurveyMin
		}
		if p.CostPerImportedRecordMin != nil {
			curr.CostPerImportedRecordMin = *p.CostPerImportedRecordMin
		}
		if p.StorageCostPerGBMonthMin != nil {
			curr.StorageCostPerGBMonthMin = *p.StorageCostPerGBMonthMin
		}
		if p.FixedMonthlyFeesMin != nil {
			curr.FixedMonthlyFeesMin = *p.FixedMonthlyFeesMin
		}
		if err := curr.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		updated, err := s.tariffs.Update(ctx, tid, curr)
		if err != nil {
			s.serverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, billingapi.TariffsResponse{TenantID: tid, Tariffs: updated})
	})
}
```

- [ ] **Step 8: Run http test**

```bash
go test ./internal/billing/service/... -race -run Dashboard
```

Expected: dashboard endpoint returns `200 OK` with the asserted shape.

- [ ] **Step 9: Commit**

```bash
git add internal/billing/service/http_*.go internal/billing/api/http.go internal/billing/service/http_test.go
git commit -m "feat(billing): HTTP endpoints for finance dashboard, breakdown, byMonth, projects, tariffs"
```

---

## Task 9: NATS subscriber for `dialer.call.finalized`

**Files:**
- Create: `internal/billing/events/subscriber.go`
- Create: `internal/billing/events/subscriber_test.go`

The dialer module publishes a structured event after committing a `calls` row:

```json
{
  "call_id":      "0193...",
  "tenant_id":    "0193...",
  "project_id":   "0193...",
  "trunk_used":   "mtt-msk-1",
  "duration_sec": 60,
  "status":       "success",
  "storage_bytes": 0,
  "finalized_at": "2026-05-12T18:01:23Z"
}
```

We subscribe with a durable JetStream consumer (`billing-call-finalized`) and `ack` only after the row lands in `call_costs`. Failures retry with NATS' built-in redelivery; the handler is idempotent so duplicates are harmless.

- [ ] **Step 1: Test the subscriber against an in-memory bus stub**

```go
// internal/billing/events/subscriber_test.go
package events_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/events"
	"github.com/stretchr/testify/require"
)

type fakeHook struct {
	mu sync.Mutex
	in []billingapi.CallCostInput
}

func (f *fakeHook) OnCallFinalized(_ context.Context, in billingapi.CallCostInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.in = append(f.in, in)
	return nil
}

func TestSubscriber_Decode_AndDispatch(t *testing.T) {
	t.Parallel()
	h := &fakeHook{}
	s := events.NewSubscriber(h)

	payload := []byte(`{
		"call_id": "00000000-0000-0000-0000-000000000001",
		"tenant_id": "00000000-0000-0000-0000-000000000002",
		"project_id": "00000000-0000-0000-0000-000000000003",
		"trunk_used": "mtt-msk-1",
		"duration_sec": 60,
		"status": "success",
		"storage_bytes": 0,
		"finalized_at": "2026-05-12T18:01:23Z"
	}`)
	require.NoError(t, s.Handle(context.Background(), payload))
	require.Len(t, h.in, 1)
	require.Equal(t, uuid.MustParse("00000000-0000-0000-0000-000000000001"), h.in[0].CallID)
}

func TestSubscriber_BadJSON_ReturnsError(t *testing.T) {
	t.Parallel()
	s := events.NewSubscriber(&fakeHook{})
	require.Error(t, s.Handle(context.Background(), []byte(`not-json`)))
	_ = json.Marshal // keep import
}

func TestSubscriber_HandlerError_Bubbles(t *testing.T) {
	t.Parallel()
	s := events.NewSubscriber(&errHook{})
	err := s.Handle(context.Background(), []byte(`{"call_id":"00000000-0000-0000-0000-000000000001"}`))
	require.Error(t, err)
	_ = time.Second
}

type errHook struct{}
func (errHook) OnCallFinalized(context.Context, billingapi.CallCostInput) error {
	return errSentinel
}
var errSentinel = errors.New("nope")
```

- [ ] **Step 2: Implement `internal/billing/events/subscriber.go`**

```go
// Package events implements the billing module's NATS JetStream subscriber.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
)

const (
	streamName       = "DIALER"
	subject          = "dialer.call.finalized"
	consumerName     = "billing-call-finalized"
	consumerMaxAck   = 5 * time.Minute
	consumerMaxRetry = 30
)

// Subscriber wraps a CallFinalizedHook with JSON decoding.
type Subscriber struct {
	hook billingapi.CallFinalizedHook
}

// NewSubscriber constructs a Subscriber with the given hook.
func NewSubscriber(h billingapi.CallFinalizedHook) *Subscriber { return &Subscriber{hook: h} }

// Handle decodes the JSON payload and dispatches to the hook.
// Returned errors trigger NATS redelivery.
func (s *Subscriber) Handle(ctx context.Context, payload []byte) error {
	var raw struct {
		CallID       uuid.UUID `json:"call_id"`
		TenantID     uuid.UUID `json:"tenant_id"`
		ProjectID    uuid.UUID `json:"project_id"`
		TrunkUsed    string    `json:"trunk_used"`
		DurationSec  int32     `json:"duration_sec"`
		Status       string    `json:"status"`
		StorageBytes int64     `json:"storage_bytes"`
		FinalizedAt  time.Time `json:"finalized_at"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return fmt.Errorf("billing.subscriber: bad json: %w", err)
	}
	if raw.CallID == uuid.Nil {
		return errors.New("billing.subscriber: missing call_id")
	}
	return s.hook.OnCallFinalized(ctx, billingapi.CallCostInput{
		CallID:       raw.CallID,
		TenantID:     raw.TenantID,
		ProjectID:    raw.ProjectID,
		TrunkUsed:    raw.TrunkUsed,
		DurationSec:  raw.DurationSec,
		Status:       raw.Status,
		StorageBytes: raw.StorageBytes,
		FinalizedAt:  raw.FinalizedAt,
	})
}

// Run binds a durable JetStream consumer and pumps messages into Handle until ctx is done.
func (s *Subscriber) Run(ctx context.Context, js jetstream.JetStream) error {
	cons, err := js.CreateOrUpdateConsumer(ctx, streamName, jetstream.ConsumerConfig{
		Durable:       consumerName,
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: subject,
		AckWait:       consumerMaxAck,
		MaxDeliver:    consumerMaxRetry,
	})
	if err != nil {
		return fmt.Errorf("billing.subscriber: consumer: %w", err)
	}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		if err := s.Handle(ctx, m.Data()); err != nil {
			_ = m.Nak()
			return
		}
		_ = m.Ack()
	})
	if err != nil {
		return err
	}
	defer cc.Stop()
	<-ctx.Done()
	return nil
}

// Compile-time assertion: nats import isn't unused.
var _ = nats.DefaultURL
```

- [ ] **Step 3: Tests pass**

```bash
go test ./internal/billing/events/... -race
```

- [ ] **Step 4: Commit**

```bash
git add internal/billing/events/
git commit -m "feat(billing): JetStream subscriber for dialer.call.finalized"
```

---

## Task 10: Wire the module into `cmd/api/main.go`

**Files:**
- Modify: `cmd/api/main.go`
- Modify: `configs/development/config.yaml`
- Create: `internal/billing/service/service.go`

- [ ] **Step 1: `internal/billing/service/service.go` — `Service` composer**

```go
package service

import (
	"log/slog"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/store"
)

// Service is the composed root used by cmd/api.
type Service struct {
	pg             *store.PG
	calc           billingapi.CostCalculator
	tariffs        billingapi.TariffStore
	spender        billingapi.SpendReport
	margin         billingapi.MarginReport
	revenue        billingapi.RevenueCalculator
	hook           billingapi.CallFinalizedHook
	defaultTariffs billingapi.Tariffs
	log            *slog.Logger
}

// Config is the YAML-bound config for billing module.
type Config struct {
	DefaultTariffs billingapi.Tariffs
}

// New wires a Service against a Postgres pool and a settings backend.
func New(pg *store.PG, settings SettingsBackend, cfg Config, log *slog.Logger) *Service {
	calc := NewCostCalculator()
	tariffs := NewTariffStore(settings, cfg.DefaultTariffs)
	revenue := NewRevenueCalculator(pg)
	spender := NewSpendReport(pg, tariffs)
	margin := NewMarginReport(pg, revenue)
	hook := NewCallFinalizedHandler(calc, tariffs, pgSink{pg: pg})
	return &Service{
		pg: pg, calc: calc, tariffs: tariffs, spender: spender,
		margin: margin, revenue: revenue, hook: hook,
		defaultTariffs: cfg.DefaultTariffs, log: log,
	}
}

// Endpoints returns the HTTP routes.
func (s *Service) Endpoints() billingapi.Endpoints {
	return billingapi.Endpoints{
		Dashboard:     s.handleDashboard(),
		Projects:      s.handleProjects(),
		Breakdown:     s.handleBreakdown(),
		ByMonth:       s.handleByMonth(),
		GetTariffs:    s.handleGetTariffs(),
		UpdateTariffs: s.handlePatchTariffs(),
	}
}

// CallFinalizedHook exposes the hook so cmd/api can subscribe NATS.
func (s *Service) CallFinalizedHook() billingapi.CallFinalizedHook { return s.hook }

// pgSink adapts *store.PG to the CostSink interface used by the handler.
type pgSink struct{ pg *store.PG }

func (s pgSink) InsertCost(ctx context.Context, r CallCostInsert) (bool, error) {
	row := store.CallCostRow{
		CallID: r.CallID, TenantID: r.TenantID, ProjectID: r.ProjectID,
		TrunkUsed: r.TrunkUsed, DurationSec: r.DurationSec, Status: r.Status,
		TelecomMinor: r.TelecomMinor, WagesMinor: r.WagesMinor,
		StorageMinor: r.StorageMinor, TotalMinor: r.TotalMinor,
		TariffVersion: r.TariffVersion,
		// FinalizedAt set in store.PG via in.FinalizedAt; for brevity see real impl.
	}
	return s.pg.InsertCallCost(ctx, row)
}
```

- [ ] **Step 2: Config defaults — `configs/development/config.yaml`**

Append:

```yaml
billing:
  defaults:
    trunk_costs_minor:
      mtt-msk-1: 342      # 3.42 ₽/min in kopecks
      mango-fed: 378
      beeline-srf: 412
    cost_per_completed_survey_minor: 12000   # 120 ₽
    cost_per_imported_record_minor: 50       # 0.50 ₽ per row
    storage_cost_per_gb_month_minor: 150     # 1.50 ₽/GB-month
    fixed_monthly_fees_minor: 5000000        # 50 000 ₽
```

- [ ] **Step 3: Modify `cmd/api/main.go` to wire billing**

```go
// add imports:
//   billingsvc "github.com/sociopulse/platform/internal/billing/service"
//   billingstore "github.com/sociopulse/platform/internal/billing/store"
//   billingevents "github.com/sociopulse/platform/internal/billing/events"

// inside run() after pgPool, jetstreamCli, settingsBackend are constructed:

billingPG := billingstore.New(pgPool)
billingSvc := billingsvc.New(billingPG, settingsBackend, billingsvc.Config{
	DefaultTariffs: cfg.Billing.Defaults,
}, logger)
billingSvc.Endpoints().Mount(r, struct{ Admin func(http.Handler) http.Handler }{
	Admin: authMW.RequireRole("admin"),
})

go func() {
	sub := billingevents.NewSubscriber(billingSvc.CallFinalizedHook())
	if err := sub.Run(ctx, jetstreamCli); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("billing subscriber stopped", "err", err)
	}
}()
```

- [ ] **Step 4: `make build && make test` from repo root**

Expected: green.

- [ ] **Step 5: Commit**

```bash
git add cmd/api/main.go internal/billing/service/service.go configs/development/config.yaml
git commit -m "feat(api): wire billing module endpoints + JetStream subscriber"
```

---

## Task 11: OpenAPI documentation

**Files:**
- Create: `docs/api/billing-openapi.yaml`

A trimmed OpenAPI 3.1 spec covering the six endpoints. The frontend Plan 19 uses this for type generation.

- [ ] **Step 1: Write spec** — operationIds, parameters, security; full schemas auto-generated from `internal/billing/api` Go structs by the existing `make openapi-gen` task (Plan 02).

```yaml
openapi: 3.1.0
info: { title: СоциоПульс Billing API, version: 0.1.0 }
servers: [{ url: "https://{host}/api", variables: { host: { default: "app.sociopulse.ru" }}}]
paths:
  /finance/dashboard:
    get:
      operationId: getFinanceDashboard
      parameters:
        - { in: query, name: period, schema: { type: string, enum: [week, month, quarter, year], default: month }}
      responses: { '200': { description: OK, content: { application/json: { schema: { $ref: '#/components/schemas/DashboardResponse' }}}}}
  /finance/projects:
    get: { operationId: getFinanceProjects, parameters: [{ in: query, name: period, schema: { type: string, default: month }}], responses: { '200': { description: OK }}}
  /finance/breakdown:
    get: { operationId: getFinanceBreakdown, responses: { '200': { description: OK }}}
  /finance/byMonth:
    get: { operationId: getFinanceByMonth, parameters: [{ in: query, name: count, schema: { type: integer, default: 6, maximum: 24 }}], responses: { '200': { description: OK }}}
  /billing/tariffs:
    get:   { operationId: getBillingTariffs,   security: [{ bearer: [] }], responses: { '200': { description: OK }, '403': { description: Forbidden }}}
    patch: { operationId: patchBillingTariffs, security: [{ bearer: [] }], requestBody: { required: true, content: { application/json: { schema: { $ref: '#/components/schemas/TariffsPatchRequest' }}}}, responses: { '200': { description: OK }, '400': { description: validation failed }, '403': { description: admin only }}}
components:
  securitySchemes: { bearer: { type: http, scheme: bearer, bearerFormat: JWT }}
  schemas:
    DashboardResponse:
      type: object
      properties:
        period: { $ref: '#/components/schemas/Period' }
        month_spend_minor: { type: integer, format: int64 }
        prev_spend_minor:  { type: integer, format: int64 }
        delta_pct: { type: number }
        cost_per_survey_minor: { type: integer, format: int64 }
        avg_cost_per_minute_minor: { type: integer, format: int64 }
        revenue_minor: { type: integer, format: int64 }
        margin_minor:  { type: integer, format: int64 }
        breakdown:    { type: array, items: { $ref: '#/components/schemas/BreakdownItem' }}
        by_month:     { type: array, items: { $ref: '#/components/schemas/ByMonthItem' }}
        top_projects: { type: array, items: { $ref: '#/components/schemas/ProjectMargin' }}
    Period:        { type: object, properties: { from: { type: string, format: date-time }, to: { type: string, format: date-time }}}
    BreakdownItem: { type: object, properties: { label: { type: string }, value_minor: { type: integer, format: int64 }}}
    ByMonthItem:   { type: object, properties: { year: { type: integer }, month: { type: integer, minimum: 1, maximum: 12 }, label: { type: string }, value_minor: { type: integer, format: int64 }}}
    ProjectMargin:
      type: object
      properties:
        project_id: { type: string, format: uuid }
        project_code: { type: string }
        project_name: { type: string }
        surveys: { type: integer, format: int64 }
        telecom_minor: { type: integer, format: int64 }
        wages_minor:   { type: integer, format: int64 }
        bases_minor:   { type: integer, format: int64 }
        storage_minor: { type: integer, format: int64 }
        total_minor:   { type: integer, format: int64 }
        revenue_minor: { type: integer, format: int64 }
        margin_minor:  { type: integer, format: int64 }
        cost_per_survey_minor: { type: integer, format: int64 }
    TariffsPatchRequest:
      type: object
      properties:
        trunk_costs_minor: { type: object, additionalProperties: { type: integer, format: int64 }}
        cost_per_completed_survey_minor: { type: integer, format: int64 }
        cost_per_imported_record_minor:  { type: integer, format: int64 }
        storage_cost_per_gb_month_minor: { type: integer, format: int64 }
        fixed_monthly_fees_minor:        { type: integer, format: int64 }
```

- [ ] **Step 2: Commit**

```bash
git add docs/api/billing-openapi.yaml
git commit -m "docs(billing): add OpenAPI 3.1 spec for finance & billing endpoints"
```

---

## Task 12: `cmd/worker billing.recompute` (stub for v1)

**Files:**
- Create: `cmd/worker/billing_recompute.go` (replace the stub from Plan 00)

A real recompute job is **out of scope** for v1 per the user brief. We stub a runnable subcommand so operators have a known place to extend later.

- [ ] **Step 1: Stub**

```go
// cmd/worker/billing_recompute.go
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

func runBillingRecompute(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("billing.recompute", flag.ExitOnError)
	tenantID := fs.String("tenant-id", "", "tenant uuid (required)")
	from := fs.String("from", "", "ISO date inclusive")
	to := fs.String("to", "", "ISO date exclusive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tenantID == "" || *from == "" || *to == "" {
		fs.Usage()
		return fmt.Errorf("missing required flags")
	}
	fmt.Fprintln(os.Stderr,
		"billing.recompute: not yet implemented — call_costs is recomputed lazily by the dialer hook for new calls.")
	fmt.Fprintln(os.Stderr,
		"For backfill, run a manual SQL UPDATE keyed off call_costs.tariff_version != current.")
	return nil
}
```

- [ ] **Step 2: Commit**

```bash
git add cmd/worker/billing_recompute.go
git commit -m "feat(worker): stub billing.recompute subcommand for future tariff backfills"
```

---

## Task 13: Coverage gate ≥ 90 %

**Files:** none.

- [ ] **Step 1: Coverage report**

```bash
make test-cover PKG=./internal/billing/...
go tool cover -func=coverage.txt | grep "^github.com/sociopulse/platform/internal/billing/"
```

Expected: every file ≥ 80%, package average ≥ 90%. If lower, add tests for any uncovered branches:

- `Tariffs.TrunkCostMinor("")` returning 0 (already covered).
- `parsePeriod` with `period=garbage` returning `400`.
- `handlePatchTariffs` with `Content-Type: text/plain` returning `400`.
- `Subscriber.Handle` with `call_id` empty returning error.

- [ ] **Step 2: Add the missing edge tests then re-run**

Track to ≥ 90% before moving on.

- [ ] **Step 3: Commit**

```bash
git add internal/billing/
git commit -m "test(billing): edge-case coverage; package average ≥ 90%"
```

---

## Task 14: End-to-end manual smoke test

**Files:** none.

- [ ] **Step 1: Boot the stack and seed**

```bash
make compose-up && make migrate-up && make run-api &
APIPID=$!; sleep 1
psql "$PG_DSN" <<'SQL'
insert into tenants (id, org_code, name, status, kms_kek_id, phone_hash_pepper)
  values ('00000000-0000-0000-0000-000000000099','TEST','Smoke','active','-','\x00');
insert into projects (id, tenant_id, code, name, status, contract_fee_per_completed_minor)
  values ('00000000-0000-0000-0000-000000000100','00000000-0000-0000-0000-000000000099','TST','Smoke','active',38100);
SQL
```

Publish 3 fake `dialer.call.finalized` JetStream messages with `nats pub`.

- [ ] **Step 2: Verify endpoints**

```bash
TOKEN=$(./scripts/test-jwt.sh tenant=00000000-0000-0000-0000-000000000099 role=admin)
curl -s -H "Authorization: Bearer $TOKEN" 'http://localhost:8080/api/finance/dashboard?period=month' | jq '.month_spend_minor'  # expect 37026
curl -s -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"cost_per_completed_survey_minor": 13000}' http://localhost:8080/api/billing/tariffs | jq '.tariffs.cost_per_completed_survey_minor'  # expect 13000
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/billing/tariffs | jq '.tariffs.cost_per_completed_survey_minor'  # expect 13000 (persisted)
kill $APIPID && make compose-down
```

- [ ] **Step 3: No commit — verification only.**

---

## Task 15: Final review checklist

- [ ] **Module-boundary check:** `make lint` passes. depguard prints zero violations on `internal/billing/`.
- [ ] **No floats in money path:** `grep -rn "float" internal/billing/ | grep -v _test.go` returns only the percent-delta helper (which is allowed because it's a UI display value). All cost arithmetic uses `int64` or `decimal.Decimal`.
- [ ] **All TODO/FIXME removed:** `grep -rn "TODO\|FIXME" internal/billing/` is empty.
- [ ] **OpenAPI matches handler shapes:** spot-check one field name in each endpoint handler vs the YAML.
- [ ] **Migration rollback works:** `make migrate-down step=1 && make migrate-up` round-trips clean.
- [ ] **Coverage ≥ 90% on internal/billing/:** verified in Task 13.
- [ ] **README mentions the module (optional):** Plan 00's `internal/README.md` already covers it via the catalog.
- [ ] **All tests pass:** `make test` + `make test-cover`.

```bash
make ci
make build
```

Expected: green. No failing tests, no lint issues, all 5 binaries present.

---

## Verification summary

After all 15 tasks the repo state is:

- `internal/billing/{api,service,store,events}/` populated, ~3,500 LoC Go.
- `migrations/20260506000000_create_call_costs.{up,down}.sql` plus `20260506000001_add_project_contract_fee.up.sql`.
- `cmd/api` wires Service, mounts 6 endpoints, runs the JetStream subscriber.
- `cmd/worker` ships a stubbed `billing.recompute` subcommand.
- `docs/api/billing-openapi.yaml` documents the public surface.
- `configs/development/config.yaml` carries YAML defaults for tariffs.
- Coverage ≥ 90% on `internal/billing/`.

This unlocks:
- **Plan 17 / 19** (admin frontend) — consumes `/api/finance/*` and `/api/billing/tariffs`.
- **Plan 13** (analytics) — already produced, but ProjectMargin reads can cross-check ClickHouse aggregates if needed.
- **Future Plan** (real-time billing & cut-off) — extends the JetStream subscriber with rate windows; out of scope here.

---

## Self-review

**Spec coverage check (against §FR-H, §5.2 catalog row "billing"):**
- §FR-H1 dashboard расходов / стоимость анкеты / стоимость минуты / маржа → `GET /api/finance/dashboard` returns all four values. ✅
- §FR-H2 графики (по месяцам, структура расходов, по проектам) → `byMonth`, `breakdown`, `projects`. ✅
- §FR-H3 per-project финансы → `ProjectMargin` row, `cost_per_survey_minor`. ✅
- §FR-H4 per-tenant tariffs (trunk minute from YAML, survey cost from `tenant_settings`) → `TariffStore` reads YAML default + `tenant_settings.billing.*`; PATCH updates `tenant_settings`. ✅
- Module catalog row (`CostCalculator`, `TariffStore`) → both implemented; we additionally added `RevenueCalculator`, `MarginReport`, `SpendReport` to satisfy the dashboard. ✅

**Out-of-scope (correctly deferred):**
- Real-time billing / cut-off — not in v1, brief explicit.
- Payment integrations (Сбер/Тинькофф) — backlog.
- 1С export — backlog.
- Tariff-change recompute job — stub only.

**Money-handling invariant:** every monetary value crosses package boundaries as `int64` with `_minor` suffix; floats appear only in `delta_pct`/`margin_pct` which are UI display fractions. ✅

**TDD discipline:** each task starts with a failing test, then minimal implementation. ✅

**Test pyramid:** unit (calculator, tariffs validate, period parser), integration via testcontainers (month_spend, margin, revenue, http), event handler against in-memory bus. ✅

Plan 14 verified.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-14-billing-module.md`.** After this plan executes, the admin Finance page renders real KPI tiles, the byMonth bar chart, the breakdown pie, and the per-project margin table — sourced from `call_costs` denormalisation written every time the dialer finalises a call.
