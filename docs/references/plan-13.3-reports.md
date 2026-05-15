# Plan 13.3 — Reports Module — Reference Pack

> Per `CLAUDE.md` rule #2a: this file is the **curated reading list**
> for Plan 13.3 (reports module). Every implementer subagent MUST be
> told to read this BEFORE writing code.
>
> Authoritative-spec links, established library gotchas, and project
> conventions specific to reports. Production lessons block at the
> bottom is filled at close-out (CLAUDE.md rule #8).

## Cross-references

- **Cross-cutting:** [`docs/references/COMMON.md`](COMMON.md) — 152-ФЗ, Yandex Cloud, Postgres, NATS, outbox.
- **Series predecessors:** [`plan-13-analytics.md`](plan-13-analytics.md) — analytics 13.1+13.2+13.2.5 lessons; many CH and NATS gotchas carry over.
- **Master spec §FR-I, §6.3 `reports_jobs`, §15.3, §17, §22:**
  [`docs/superpowers/specs/2026-05-06-sociopulse-system-design.md`](../superpowers/specs/2026-05-06-sociopulse-system-design.md).
- **Combo plan, Part A only:** [`docs/superpowers/plans/2026-05-06-13-analytics-reports.md`](../superpowers/plans/2026-05-06-13-analytics-reports.md)
  — the original Plan 13 spec covering analytics tasks 1–4 (no Part B was ever written; reports work was deferred). File-structure section (lines 86–164) is the reports-module layout anchor.
- **Analytics consumer surface:** [`docs/architecture/analytics-mv.md`](../architecture/analytics-mv.md) — `mv_*` read patterns; Plan 13.3 renderers consume these.
- **Module contracts:** [`docs/architecture/02-module-contracts.md`](../architecture/02-module-contracts.md).
- **CONTEXT glossary:** [`CONTEXT.md`](../../CONTEXT.md) — vocabulary canon.

---

## Canonical specs

### §FR-I — Reports (master spec)

| Sub-FR | Requirement |
|---|---|
| FR-I1 | Six pre-defined reports: `operator_efficiency`, `project_summary`, `calls_by_status`, `finance`, `quality_control`, `hourly_activity`. |
| FR-I2 | Custom report endpoint `POST /api/reports/custom` returns `202 Accepted` + `jobID` for async path. |
| FR-I3 | Async via job queue (`asynq` per ADR/lib choice) for `period > 30 d OR estimated_rows > 100k`; persist artifact in S3; **24 h presigned download URL**; every export audit-logged. |

### §6.3 — `reports_jobs` Postgres table (spec)

```sql
reports_jobs (
  id              text PRIMARY KEY,                  -- asynq task id
  tenant_id       uuid NOT NULL,
  kind            text NOT NULL,                     -- ReportKind enum
  format          text NOT NULL,                     -- ExportFormat enum
  params          jsonb NOT NULL,
  window_from     timestamptz NOT NULL,
  window_to       timestamptz NOT NULL,
  state           text NOT NULL,                     -- JobState enum
  started_at      timestamptz,
  finished_at     timestamptz,
  bytes_size      bigint,
  filename        text,
  download_url    text,                              -- presigned, 24h TTL; populated on succeed
  error           text,
  created_by      uuid NOT NULL,                     -- user_id
  notify_user_id  uuid NOT NULL,                     -- realtime notify subject
  created_at      timestamptz NOT NULL DEFAULT now()
)
```

RLS: `(tenant_id = current_setting('app.tenant_id')::uuid)` USING + WITH CHECK.
`tenancy_admin` BYPASSRLS grants: `SELECT, INSERT, UPDATE` (no DELETE — terminal states are flags, not row deletion).

### §15.3 — metrics

- `sociopulse_reports_jobs_total{tenant_id, kind, format, state}` — counter (state transitions)
- `sociopulse_reports_render_duration_seconds{kind, format}` — histogram
- `sociopulse_reports_artifact_bytes{kind, format}` — histogram (or summary)
- `sociopulse_reports_download_url_signed_total{tenant_id, kind}` — counter (presigned-URL minting)

### §17 — testing strategy

- **Unit:** renderer-per-kind smoke (excelize-Open round-trip; csv.Reader column-count; PDF page-count via `pdf` reader or simply byte-prefix check).
- **Integration (`-tags=integration`):** end-to-end async-job — enqueue → asynq Server picks up → store transitions → outbox event → presigned URL signed.
- **Coverage target:** ≥80 % on `service/` and `templates/`.

### §22 — UI consumer

`admin-pages-2.jsx::AdminReports` (in `sociopulse-web` repo, Plan 19) — produces the screen that calls every HTTP endpoint Plan 13.3 ships. UI work is **out of scope here**; we deliver only the API surface + worker.

---

## Reference implementations

### asynq (already in deps: v0.26.0)

- **Existing usage:** `internal/crm/module.go` lines 49, 79–98 wires `asynq.Client + asynq.Server + asynq.Scheduler` for respondent-import jobs. Mirror that shape.
- **Existing task-type pattern:** `internal/recording/api/events.go:60` declares `TaskRetentionPass = "recording:retention.pass"`. Reports declares `TaskJobRun = "reports:job.run"` — already in `internal/reports/api/events.go:42`.
- **Library docs to fetch via `context7`** when wiring: `github.com/hibiken/asynq` — `Client.EnqueueContext`, `Server.Run`, `Server.Shutdown`, `ServeMux.HandleFunc`, `Task` payload encoding (we use JSON bytes of `api.JobInput`).
- **Queue naming:** project convention is per-module queue. Reports uses `"reports"` queue (crm uses `"crm"`, recording uses `"recording"`). Tune `Concurrency: max(2, runtime.NumCPU()/4)`, `Queues: map[string]int{"reports":1}`, `StrictPriority: false`.

### excelize (already in deps: v2.10.1)

- **Library docs to fetch via `context7`** when wiring: `github.com/xuri/excelize/v2` — `NewFile`, `SetCellValue`, `SetCellStyle`, `MergeCell`, `AddTable`, `Save` / `WriteTo`.
- **Sheet naming:** ASCII-only, ≤31 chars (Excel constraint). Use `report.<kind>` (`report.efficiency`, `report.calls_status`, etc.).
- **Date cells:** use `f.SetCellStyle` with `NumFmt: 22` (`m/d/yy h:mm`) or explicit ISO-8601 string format for portability; default is float "Excel serial date" which UTF-8 spreadsheet readers don't render.

### signintech/gopdf (NEW dep to add)

- `github.com/signintech/gopdf` — active fork; `gopdf.GoPdf` is the writer.
- Workflow: `pdf := gopdf.GoPdf{}; pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4}); pdf.AddPage(); pdf.AddTTFFont("default", "<path>")`. Default font must be added before any `Cell`/`Text` call or output panics.
- **Font embedding:** Cyrillic content requires embedded TTF — ASCII-only fonts (Helvetica baseline) cannot render Russian. Use `DejaVuSans.ttf` (free, Latin+Cyrillic+Greek) bundled under `internal/reports/templates/common/fonts/`. Embed via `go:embed`.
- **No images in v1** — `gopdf` supports `Image()` but we ship summary tables only. Decision: defer chart-image rendering to a later plan.
- **PDF for big result sets** (combo-plan rule, line 211): cap detail rows at 5 000; fall back to "summary only" and attach an XLSX side-car. Out of v1 scope — Plan 13.3 ships single-artifact PDFs only and `ErrTooLarge` for over-limit cases.

### S3 / ObjectStore extension

- **Current port:** `internal/recording/storage.ObjectStore` (`Get`, `Delete` only) — see `internal/recording/storage/store.go:21-33`.
- **Plan 13.3 extends** the port with `Put(ctx, bucket, key, payload, contentType) error` and `PresignedURL(ctx, bucket, key, ttl time.Duration) (string, error)`. Both methods are new — production `yandex_s3` build will use `s3.PutObject` + `s3.NewPresignClient().PresignGetObject(ttl)`.
- **Local impl** (`LocalObjectStore`): `Put` stores bytes; `PresignedURL` returns a stub URL like `http://localobjectstore/<bucket>/<key>?stub=true&expires=<unix>` — fine for tests; production uses build-tag-gated Yandex SDK adapter (Plan 01).
- **Bucket naming:** `sociopulse-reports-<tenant_uuid>` — parallel to `sociopulse-recordings-<tenant_uuid>` (per CONTEXT.md S3 entry). Tenant-scoped bucket is the isolation primitive.

### Audit-log integration

- **Audit module is currently a no-op stub** (`internal/audit/module.go` line 25-28: `Plan 03 Task 7 fills this in`). No `auditapi.Logger` implementation registered in `Locator`; no callers (`grep -rln "internal/audit/api" internal cmd` → empty).
- **Plan 13.3 strategy:** publish `tenant.<t>.audit.event` to NATS (via outbox, atomic with `reports_jobs` row transition). The future audit Service (Plan 03 Task 7) subscribes durably and persists to `audit_log`.
- **Payload shape:** marshal `auditapi.Event` (already defined in `internal/audit/api/dto.go:34-45`) — `Action="reports.export"`, `Target="report:<job_id>"`, `Payload={kind, format, window_from, window_to, params, bytes_size}`.
- **Tenant subject:** `tenant.<tenant_uuid>.audit.event` (matches the standard `tenant.<t>.<module>.<event>` convention).

### `analytics.ServiceRO` consumption

- **Single dependency:** Reports renderers call `analytics.ServiceRO` (= `MetricsQuery` + `Overview`) — see `internal/analytics/api/interfaces.go:32-36`. **NEVER reach into `internal/analytics/store`** (depguard `module-boundaries` would block it anyway).
- **Window discipline:** `analyticsapi.Window` is `[From, To)` half-open with `Validate()` enforcing `<= 365 d`. Reports must pre-validate before queueing — `ErrInvalidWindow` surfaces from `Window.Validate()` bare (no wrap), per Plan 13.2 lesson #3.
- **Region/operator KPI:** lives behind the `MetricsQuery.RegionProgress` + `MetricsQuery.OperatorComparisons` methods; data fetcher per kind selects the appropriate sub-call.
- **`OperatorComparisons` carries `DisplayName`** — populated server-side by analytics (Plan 13.2 lesson #8: cached 5 min, eventual on rename — accept for reports too).

### `pkg/middleware/tenant.RequireSameTenant`

- **Already in place** (Plan 13.2.5 Task 1): `pkg/middleware/tenant/require_same_tenant.go:81-132`.
- **Signature:** `func RequireSameTenant(resolveFn ResolveTenantFn, opts ...Option) gin.HandlerFunc`. `ResolveTenantFn = func(ctx, id uuid.UUID) (uuid.UUID, error)`.
- **Apply to reports** where path-param `:jobID` would otherwise allow probing across tenants: resolver = `func(ctx, jobID) (tenantID, error)` over the `reports_jobs` table via `BypassRLS`.
- **404-by-default** behaviour matches recording/crm; do NOT 403 (existence-probe defence).

### `pkg/outbox`

- **Writer interface:** `Append(ctx context.Context, tx postgres.Tx, ev Event) error` — `pkg/outbox/writer.go:17-22`.
- **Event:** `Subject + Payload + AggregateID + TenantID` — `pkg/outbox/event.go:29-72`. ID is server-generated (zero on Append).
- **Project convention:** state-flip + audit-event + outbox-publish all in one `WithTenant(tenantID, fn)` Tx. Reports follows: `MarkSucceededTx` writes `state='succeeded' + finished_at + bytes_size + filename + download_url` AND appends `tenant.<t>.audit.event` AND appends `tenant.<t>.reports.report.ready` to the outbox in the same transaction.

### Migration numbering

- **Latest committed:** `000011_admin_grants_call_recordings.up.sql` / `.down.sql` (Plan 12.4).
- **Plan 13.3 claims:** `000012_reports_jobs.up.sql` / `.down.sql`.
- **golang-migrate convention:** zero-padded 6 digits; up + down required. `make migrate-up` / `make migrate-down` drive locally.

---

## Project conventions (vocabulary + style)

- **CONTEXT.md vocabulary:** terms `report`, `tenant`, `respondent`, `operator`, `project`, `survey schema`, `AHT`, `S3`, `PII` are canonical. Never invent synonyms.
- **HTTP error envelope:** `{ "code": "<stable_string>", "message": "<human>" }` — `pkg/httputil/error_handler.go` + `internal/recording/transport/http.ErrorEnvelope`. (Plan 13.2 lesson #5.) Stable code values per kind:
  - `reports.unknown_kind`, `reports.unsupported_format`, `reports.invalid_params`, `reports.job_not_found`, `reports.too_large`, `reports.canceled`, `reports.async_required` (new — used when synchronous path refuses), `reports.window_invalid`.
- **UUID query parsing:** use `parseRequiredUUID(c, "x")` helper (Plan 13.2 lesson #4) — `gin`'s built-in form binding doesn't decode `uuid.UUID`.
- **`context.WithoutCancel`** for drains (`asynq.Server.Shutdown` grace period, outbox-publish on succeed-tx) — Plan 13.2 lesson #11.
- **JSON tags use `omitzero` for `time.Time`** (Go 1.24+ feature) over `omitempty`, which doesn't work on zero time. Already standard in `auditapi.Event`.

---

## Gotchas

### Carried over from Plan 13.2 (analytics)

1. **Window.Validate bare-sentinel return** — `errors.Is(err, analyticsapi.ErrInvalidWindow)` at HTTP layer; never wrap.
2. **`gin` UUID binding broken** — use explicit `uuid.Parse(c.Query("x"))`.
3. **Error envelope = `{code,message}`** not `{error: "..."}`.
4. **goleak suppression list** — keep specific top-functions, never `runtime_pollWait` (overly broad).
5. **`context.WithoutCancel` for outlive-parent work** — drains, finalisers.

### Reports-specific (anticipated)

6. **excelize `f.SetCellValue` with `time.Time` produces float "Excel serial" cells by default.** Excel desktop renders these; Numbers and LibreOffice may not. **Fix:** apply a date-format style (`NumFmt:22`) or pre-format to RFC3339 string. Tests should assert the rendered string via `f.GetCellValue(sheet, axis)` not the raw cell type.

7. **`gopdf` panics on `Cell()` without an `AddTTFFont` prior call** — silent panic into a nil-pointer-deref in `gopdf.parsifal`. Always `AddTTFFont` immediately after `AddPage`. Test the smallest happy path through `tempfile + pdf.Output` end-to-end.

8. **`asynq` worker error contract:** returning `error` schedules retry per `RetryDelayFunc`. Permanent failures (poison payload, unknown kind) MUST wrap via `asynq.SkipRetry` (`fmt.Errorf("...: %w", asynq.SkipRetry)`) — otherwise the job retries forever and burns the redis-queue budget. State transition: permanent fail → `JobFailed` + `error` populated + outbox-publish "ready" with error context.

9. **`asynq.Server.Shutdown` is graceful-only:** call from a separate goroutine on `ctx.Done()`, give it `~10s` grace, then return. cmd/worker's existing pattern (analytics ingest + recording retention) is the template.

10. **Presigned URL TTL discipline:** 24 h is the contract (FR-I3). Yandex S3 presigner accepts `time.Duration`; pass `24 * time.Hour` literally. The URL leaks the bucket name → the bucket-naming rule (`sociopulse-reports-<tenant>`) is *itself* a tenant identifier; that's accepted (the URL is gated behind `RequireSameTenant` already; tenant IDs are not secrets).

11. **Custom report (`POST /api/reports/custom`) is always async.** `IsAsyncRequired(period, est)` returns `true` unconditionally for custom — the user explicitly asks for "async receipt" by hitting the custom endpoint. Pre-defined exports take the sync path *unless* threshold trips. This is per combo-plan rule, line 209.

12. **PDF row cap:** `ErrTooLarge` is the canonical signal for "render does not fit single-artifact constraints". Plan 13.3 sets the threshold at 5 000 detail rows for PDF (per combo-plan rule, line 211). The runner returns `ErrTooLarge` synchronously; the caller MUST either retry with `format=xlsx` or via `/api/reports/custom` for the async fallback.

13. **`outbox.Writer.Append` requires the same `tx` as the row write.** A `MarkSucceededTx(ctx, jobID, ...)` method takes a `postgres.Tx` and does row update + audit-event Append + report-ready Append atomically. If `Append` runs outside the tx, the event ships even when the row update rolls back — split-brain. **Discipline:** every state-transition method has a `*Tx` variant that consumes a caller-managed tx.

14. **Idempotency on asynq retry:** worker processor MUST tolerate "already finished" `reports_jobs` rows (job retried after timeout but original completed). Use `WHERE state IN ('queued','running') RETURNING ...` on the state-flip query; `RowsAffected==0` → return `nil` (no-op, ack the task). Matches Plan 12.4's `errStaleSkip` pattern but inline-handled inside the consumer.

### Cross-cutting from previous plans

15. **gopls cache pollution after subagent dispatches** — always `make ci && go test -race -count=1 ./...` before trusting IDE diagnostics.
16. **Testifylint:** `require.Positive(t, n)` over `require.Greater(t, n, int64(0))`.
17. **`time.After` in for-loops banned** by `make grep-time-after` — use `time.NewTicker` or `context.AfterFunc`.
18. **`git add` specific files only** — never `git add .` / `-A`.

---

## Open questions (to resolve during execution OR carry forward)

- **Q1.** Should the renderer dispatcher live in `service/runner.go` or as a separate `service/dispatch.go` file? Combo plan (line 98) uses `runner.go` for the dispatcher; keep that. Tasks 5 & 7 verify.
- **Q2.** Do we expose `bytes_size` (artifact size) and `filename` on `GET /api/reports/jobs/:id` even when `state=running`? **Decision (will be tested):** `bytes_size=0, filename=""` while `state` ∈ `{queued, running}`; populated on `succeeded`. Surface explicitly in the DTO marshal.
- **Q3.** Is presigned URL minting eager (at `MarkSucceeded`) or lazy (on `GET /jobs/:id/download`)? **Decision:** eager — sign at `MarkSucceeded`, persist into `reports_jobs.download_url`, return that on `GET /jobs/:id/download` with a 302 redirect. Simpler; one S3 call instead of one per download. Trade-off: if a URL expires before the user clicks, they get a 403 from S3 — we'll document the 24 h window in the API response.
- **Q4.** Where do we persist the rendered artifact in tests? Local `ObjectStore.Put` writes in-memory bytes; integration test asserts the artifact is retrievable via the recorded URL by hitting the LocalObjectStore Get with the parsed bucket/key. OK.
- **Q5.** Asynq's `unique=true` constraint or our own dedup? **Decision:** rely on `asynq.WithUniqueOption`-style deduplication per `(tenant_id, kind, params_hash)` is risky for legitimate re-runs. Skip uniqueness; each `Enqueue` produces a new job.

---

## Production lessons (post-execution 2026-05-15)

> Filled at close-out of Plan 13.3 — what was actually learned during
> execution that's not in the canonical specs. Read this BEFORE
> touching the reports module in any future plan.

1. **`pgxmock` is NOT in deps.** Task 2 spec proposed `pgxmock`-based unit tests; it's not in `go.mod`. Project convention is testcontainers-backed integration tests + pure-helper unit tests (matches `internal/recording/store/lifecycle_pg_test.go`). Stuck with project convention: pure-helper unit tests for `encodeCursor`/`decodeCursor`/`clampLimit`/`buildListQuery`, integration tests in `pg_pg_test.go` (`//go:build integration`) for end-to-end CRUD + *Tx scenarios.

2. **Audit module is a stub (Plan 03 Task 7 deferred).** `internal/audit/module.go` is no-op; no `auditapi.Logger` impl exists; nothing in the repo imports `internal/audit/api`. Reports emit `tenant.<t>.audit.event` via outbox; future Audit Service will subscribe. Reports never calls a non-existent `auditapi.Logger.Write`.

3. **ObjectStore port had only Get + Delete.** Task 1 extended it with `Put + PresignedURL` in place rather than extracting to `pkg/objectstore`. The first cross-module consumer (reports) joins the recording-owned port; rule-of-three not yet hit. **Gotcha**: adding methods to a shared interface requires updating ALL implementations including test fakes. `fakeObjectStore` in `recording/worker/retention_test.go` did not satisfy the extended interface under `-tags=integration`; caught by Task 2 implementer running `golangci-lint --build-tags=integration` (which `make ci` does not run). Fix-up commit `9824f73` stubbed the new methods. **Lesson**: when extending a shared port, grep all implementers AND test fakes with the integration build tag.

4. **`internal/audit/api.Event` has `omitempty` on `ActorID *uuid.UUID`.** AuditEmitter originally always sent `&actorID` even for the zero uuid (system actor). The marshalled JSON would carry `"actor_id":"00000000-..."` instead of omitting. Conditional pointer assignment (`if actorID != uuid.Nil { evt.ActorID = &actorID }`) is the fix — preserves the `omitempty` semantics for system-initiated exports.

5. **`pgx.ErrNoRows == postgres.ErrNoRows`.** `pkg/postgres/tx.go:49` re-exports `pgx.ErrNoRows`. Checking both in a switch is redundant; pick one.

6. **Migration 000012 was an EVOLVE not a CREATE.** `migrations/000001_init.up.sql:297` already had a Plan 03 `reports_jobs` table with a different shape (`uuid id`, `status`, `requested_by`, `result_s3_key`). Task 2 implementer wrote an ALTER-based migration that drops legacy columns + adds Plan 13.3 columns + swaps PK uuid→text. Empty-table guard added in fix-up `4ed3ed3` defends against future seed-data side-channels. **Lesson**: `pre-execution sanity check` per CLAUDE.md verify-before-assert needs to grep `migrations/*.sql` for the target table name, not just `select * from information_schema.tables`.

7. **`pgxpool` testcontainer bootstrap is Postgres-superuser.** RLS is bypassed for the test runner. Negative cross-tenant scenarios cannot be exercised end-to-end; the substitution is a "RLS policy exists" structural test (`reports_jobs::regclass`). Same gap as recording's `_pg_test.go`. Re-engineering the testcontainer to use a non-superuser role is a project-wide tooling change — not in Plan 13.3 scope.

8. **Templates ↔ service import cycle.** Task 5's `runner.go` imports all 6 template packages for the dispatch table. Templates already imported `service` for the data struct types (`service.OperatorEfficiencyData` etc.). Cycle. Fix: extract data structs to `internal/reports/templates/data/data.go` (leaf package, stdlib + analyticsapi + uuid only); both templates and `service` import it; `service/data.go` re-exports the names via type aliases for backwards compatibility. **Lesson**: when a future-planned dispatcher imports leaf packages, the data types they exchange MUST live in a third leaf package — don't dump them in the dispatcher's own package.

9. **`pkg/middleware/tenant.RequireSameTenant` uses uuid PK.** Reports' `jobID` is text (asynq task id, not uuid). Built a reports-specific `jobIDTenantGuard` in `routes.go` that adapts the same 404-on-mismatch semantic. Future tasks dealing with non-uuid PKs should either extend the canonical middleware (`ResolveTenantFn[K comparable]`) or copy this pattern.

10. **`ListJobsFilter.TenantID` was added in Task 6.** Interface `JobQueue.List(ctx, filter)` has no tenantID param; HTTP handler reads `claims.TenantID` from gin auth context and injects it via filter field. Service-layer `Queue.List` then enters `pool.WithTenant(filter.TenantID)` before delegating to the store. Cleanest alternative to a ctx-key dance.

11. **`asynq.SkipRetry` double-`%w` wrap.** `fmt.Errorf("permanent: %w: %w", cause, asynq.SkipRetry)` is the Go 1.20+ idiom for "this error chains both the cause AND the SkipRetry sentinel". Use `errors.Is(err, asynq.SkipRetry)` to discriminate. The double-`%w` syntax is non-obvious — when reviewing the consumer's permanent-fail paths, look for this exact pattern.

12. **`gopdf` panics without `AddTTFFont`.** Silent nil-deref inside `gopdf.parsifal` if you call `Cell()` before loading any font. Plan 13.3 bundles `DejaVuSans.ttf` (757 KB, embedded via `go:embed`) for Cyrillic support — load it immediately after `AddPage()`. **Lesson**: any PDF code path needs an early "happy path" test that calls `Output()` end-to-end.

13. **`excelize` v2 date cells default to "Excel serial float".** Non-Excel readers (Numbers, LibreOffice) don't render this format. Use `common.DateStyle` (NumFmt 22, "m/d/yy h:mm") or pre-format dates to RFC3339 strings. Tests should assert the rendered string via `f.GetCellValue(sheet, axis)` not the raw cell type.

14. **`gopdf.Close()` returns an error.** `defer pdf.Close()` silently drops it. Use `defer func() { _ = pdf.Close() }()` for consistency with the excelize pattern and to make a future close-time error visible if the lint policy ever flips.

15. **`gopls` cache pollution after every subagent dispatch.** LSP shows "could not import" / "undefined" errors after a subagent finishes; `go build` directly confirms green. Always reality-check via direct commands before chasing IDE diagnostics. Documented as a project standing rule but worth repeating: it's the #1 source of false-positive Plan-13.3-task-feedback (Task 1, 2, 3, 4, 5, 7 all had a gopls-pollution incident).

16. **Migration empty-table guard pattern.** `do $$ begin if (select count(*) from <table>) > 0 then raise exception '<msg>'; end if; end$$;` at the start of an evolve migration. Defends against side-channel seeding (e.g., a future test fixture or staging seed). Cheap insurance — added in fix-up `4ed3ed3` after code review flagged the down-then-up cycle could silently drop legacy data.

17. **DRY helper extraction in code-quality reviews.** Task 4 originally duplicated `sha256 + RenderResult assembly` 18 times across renderers (~12 lines × 18 = ~200 LoC of mechanical glue). Code-quality reviewer flagged it; fix-up `59a19c6` extracted `common.NewRenderResult(payload, kind, mime, windowFrom)`. Watch for this in future "mechanical replication" tasks — even when each leaf file owns its specific data, the trailing glue is centralisation-bait.

18. **`internal/reports/api/dto.go` field-ordering matters.** `TenantID uuid.UUID` added as the FIRST field of `ListJobsFilter` because filter callers construct it positionally in some tests. Adding to the end would require updating every constructor. Either move bareword struct literals to keyed literals OR add new fields at the appropriate position.

19. **`pkg/config.ReportsConfig` already existed.** Plan 14 ground prep had created `pkg/config/reports.go` with `AsyncThresholdPeriodDays`/`AsyncThresholdRecords`/`JobTTL`/`PresignedURLTTL`. Plan 13.3 extended it with `AsynqConcurrency` + `QueueName`. Always grep existing config before adding new section — `find pkg/config -name "*.go" -exec grep -l <module> {} \;`.

20. **`docs/api/<module>/v1/` is the project convention.** `docs/api/recording/v1/` exists with `recording.proto`. Plan 13.3 added `docs/api/reports/v1/openapi.yaml`. Future modules should mirror.
