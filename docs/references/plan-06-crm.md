# Plan 06 — CRM Module references

> **Plan source**: [`docs/superpowers/plans/2026-05-06-06-crm-module.md`](../superpowers/plans/2026-05-06-06-crm-module.md).
> **Module path**: `internal/crm/`.
> **Depends on**: Plan 03 (migrations), Plan 04 (tenancy.KMSResolver + tenancy.PhoneHasher), Plan 05 (auth middleware + RBAC + audit).

Status: **shipped (`v0.0.8-crm`, 2026-05-08)**.

---

## Canonical specs (must-read)

### Phone numbers (Russia + E.164)
- [**E.164 (ITU-T)**](https://www.itu.int/rec/T-REC-E.164/en) — international phone-number plan; `+<country><subscriber>` with max 15 digits.
- [**Russian numbering plan (Россвязь приказ № 113 / № 142)**](https://rkn.gov.ru/) — RU country code +7, mobile prefixes 9XX, landline ABC+DEF.
- [**libphonenumber (Google)**](https://github.com/google/libphonenumber) — canonical implementation. Go port: [**nyaruka/phonenumbers**](https://github.com/nyaruka/phonenumbers).
  Note:
  - Mobile RU: `+79XXXXXXXXX` — strict 11 digits including +7 (10 after the country code, must start with `9`).
  - Landline RU: `+74YYYYYYYYY` (Moscow), `+78YYYYYYYYY` (toll-free), etc. AVB/DEF schema.
  - **Don't write your own E.164 normaliser** — corner cases (8-as-leading, 7-without-+, double-leading-zero, formatting variations like `8 (495) 555-12-34`, NBSP, em-dash) bite. Use libphonenumber.
  - **Privacy gotcha**: even masked phones should NEVER be logged; `pkg/observability` redaction must catch any phone-shaped string in zap fields.

### CSV / XLSX parsing
- [**Go encoding/csv**](https://pkg.go.dev/encoding/csv) — stdlib; sufficient for our import path. Handle BOM, CRLF, and quoted fields with embedded newlines.
- [**xuri/excelize/v2**](https://github.com/xuri/excelize) — XLSX reader/writer for Go. Stream-mode for large files: `Rows(sheet)` iterator (don't load whole sheet into memory).
  Note:
  - **Use streaming reader**: `f.Rows(sheet)` returns iterator; loading 100k rows via `f.GetRows()` (which materialises everything) blows memory.
  - Excel cell types (date as `serial number`, boolean, formula) — when in doubt treat as string and normalise.
  - **Don't trust column order from spec** — operators export from various CRMs with different headers; map by header name, not index.

### Async tasks (asynq)
- [**hibiken/asynq**](https://github.com/hibiken/asynq) — Redis-backed task queue, idiomatic Go.
  Note:
  - Tasks are JSON-serialised payloads on Redis lists; processor pulls and executes.
  - **Idempotency**: at-least-once delivery — every handler MUST tolerate replay (use `INSERT ... ON CONFLICT DO NOTHING` for inserts).
  - **Retry policy**: configurable per-task; default exponential backoff. For import tasks: max 3 retries, then dead-letter.
  - **Visibility**: dashboard at `asynqmon` (separate binary) — exposes queue state, dead-letter, retried jobs.
  - **Why not NATS JetStream**: asynq has scheduling/cron + per-task retry semantics out of the box; JetStream is for events, asynq for jobs.

### 152-ФЗ subject right to deletion
- [**Статья 21 152-ФЗ**](http://pravo.gov.ru/proxy/ips/?docbody=&firstDoc=1&lastDoc=1&nd=102108261) — оператор обязан прекратить обработку по запросу субъекта; срок — 30 дней с момента подачи запроса.
- [**Постановление ФСТЭК 21**](https://fstec.ru/) — общие требования к защите ПДн.
  Note:
  - **30-day soft-delete grace** is our implementation: row marked `deleted_at` immediately, but actual DELETE happens only after worker.respondents.purge runs at +30 days. This window lets operators reverse a mistaken delete.
  - **Audit trail mandatory**: every delete + every purge writes an `audit_log` row with action label and full context.
  - **Pseudonymization vs anonymization**: we pseudonymize (envelope-encrypted phone) — the link can be re-established via the per-tenant DEK. True anonymization (irreversible) is not in v1.
  - 38-ФЗ vs 152-ФЗ: соц. опросы — 152-ФЗ (ПДн); реклама — 38-ФЗ. Мы только 152-ФЗ. См. ADR-0001/0003. The `is_advertising=true` flag is REJECTED in v1 (`ErrAdvertisingRejected`).

### Quotas
- [**The "leaky bucket" pattern**](https://en.wikipedia.org/wiki/Leaky_bucket) — relevant for surge protection.
- **Quota race condition** — the canonical bug: two operators finish surveys for the same quota cell at the same time, both check IsFull (returns false), both Increment, and the cell goes over target. Our solution: optimistic locking via `SELECT ... FOR UPDATE` in the same transaction that finalizes the call. Redis cache is advisory only; truth is in Postgres.

---

## Reference implementations

- [**nyaruka/phonenumbers**](https://github.com/nyaruka/phonenumbers) — Go port of Google's libphonenumber. Active, well-tested.
  Note: API is `phonenumbers.Parse(input, defaultRegion)` → `*PhoneNumber`; format with `phonenumbers.Format(num, phonenumbers.E164)`.

- [**xuri/excelize/v2**](https://github.com/xuri/excelize) — XLSX library; project's choice for import.
  Files of interest: `rows.go` (streaming iterator), `sheet.go`.

- [**hibiken/asynq**](https://github.com/hibiken/asynq) — task queue.
  Files of interest: `client.go` (Enqueue), `processor.go` (handler dispatch), `examples/`.

- [**dolthub/swiss**](https://github.com/dolthub/swiss) — fast hash map for in-process quota cache. Optional; sync.Map suffices for our scale.

- Reference implementation we already have:
  - `pkg/encryption` — AES-256-GCM `Encrypt/Decrypt` + HMAC-SHA256 `PhoneHasher`. Plan 06 uses these via `tenancy.KMSResolver` + `tenancy.PhoneHasher`.

---

## Production lessons (blog posts, talks)

- [**Asynq blog — "Building reliable background workers in Go"**](https://hibiken.dev/blog/) — practical lessons; idempotency + retry semantics.
- [**Habr — "Парсинг XLSX в Go: гид по граблям"**](https://habr.com/ru/articles/) — search; lots of war stories about excelize edge cases.
- **Big-batch import wisdom**: COPY FROM is 10-100x faster than INSERT-per-row in Postgres. Plan 06 Task 4 uses `pgx.CopyFrom` for the bulk-insert path.

### Russian-language
- [**Habr — "152-ФЗ для разработчика"**](https://habr.com/ru/articles/) — search; pragmatic articles on what 152-ФЗ actually requires.

---

## Lessons learned from Plan 06 implementation (2026-05-08)

After 5 sub-tasks and ~15 commits the CRM module is shipped. These are the things subagents repeatedly tripped on — capture so future plans (Plan 07 surveys, Plan 09 dialer) avoid the same cycles.

1. **NATS publisher slot pattern** — declare the field as `eventbus.Publisher`, accept nil in the constructor, no-op silently when nil. Plan 11 owns NATS wire-up; this lets every state-changing service ship before NATS exists. Used in `ProjectService` for project lifecycle events; `ProgressTracker` for import events. Pattern: explicit `publisher == nil` guard at the top of `publishEvent`, never an unchecked dereference.

2. **Constraint-name discrimination on unique-violation** — generic SQLSTATE 23505 translation is a pitfall. ALWAYS gate the sentinel translation on the EXACT auto-generated constraint name (`projects_tenant_id_code_key`, `respondents_tenant_project_phone_hash_uniq`). A future migration that adds a second unique idx surfaces a distinct error instead of silently masquerading. Same pattern used in `internal/auth/store/user_store.go`.

3. **`pgx.CopyFrom` doesn't support ON CONFLICT** — for batch insert with dedup, use a two-step pattern: (1) call `ExistingHashes(...)` to read the subset of hashes that already exist for (tenant_id, project_id), (2) filter the in-memory batch, (3) `CopyFrom` the survivors. The DB-side UNIQUE constraint is the third layer of defense (catches the race window between step 1 and 3).

4. **excelize streaming `f.Rows(sheet)`, never `GetRows`** — `GetRows` materializes the entire sheet in memory; for 100k rows that's a 10MB+ blow per import. The streaming iterator costs <1MB.

5. **Phone in audit payloads is a recurring violation candidate** — write tests that `json.Marshal(event)` and assert `!strings.Contains(rawJSON, expectedPhone)`. Three subagents over Tasks 3-5 needed reminding to NOT include the phone in `crm.respondent.created` payloads. The `respondent_id` is the audit key; phone is recoverable from the row via admin GetWithPhone (which itself is audited).

6. **30-day soft-delete pattern** — `deleted_at timestamptz` + partial index `WHERE deleted_at IS NOT NULL`. Soft-delete sets `deleted_at = now()`; `PurgeWorker` daily cron hard-deletes `WHERE deleted_at < now() - INTERVAL '30 days'`. Each purge audits per-id with `crm.respondent.purged`.

7. **asynq.Scheduler ≠ asynq.Server** — Scheduler enqueues periodic tasks; Server processes the queue. Both share Redis but need separate `Run` goroutines. `Module.Stop` must drain BOTH (Stop + Shutdown for Server, Shutdown for Scheduler).

8. **Multipart upload size limit** — set BOTH `gin.MaxMultipartMemory` (default 32MB, bump to 50MB) AND a body-size cap on the route (gin's `c.Request.Body = http.MaxBytesReader(...)` pattern). Defense-in-depth.

9. **Russian phone normalization is a 1-line library call** — `phonenumbers.Parse(input, "RU")` + `IsValidNumberForRegion(...)` + `Format(num, E164)`. Don't roll your own — the corner cases (8-as-leading, NBSP, em-dash, parens, scientific-notation from Excel) bite hard. Pre-sanitize by stripping non-digit non-`+` runes before passing to libphonenumber.

10. **Redis status-hash TTL refresh on terminal write** — set TTL only at Init means a job that sat in queue for days has its terminal status expire prematurely. Pipeline `HSet + Expire` together on Finish/Fail.

11. **gocognit refactor pattern (Plan 05 lesson re-confirmed)** — when a method exceeds gocognit:20, extract an inner-tx closure into a helper. `ChangePassword → applyPasswordChange`, `processBatch → stageBatch + filterAgainstDB + persistBatch`, `parseXLSX → readXLSXHeader + readXLSXBody`. Each helper is testable independently.

12. **Hand-rolled fakes > gomock for our scale** — `fakeProjectStore`, `fakeRespondentStore`, `fakeAudit`, `fakeTxRunner`, `fakeKMSResolver`, `fakePhoneHasher` all hand-rolled with `sync.Mutex`. Total ~600 LOC across the crm test suite. gomock would save ~200 LOC at the cost of a codegen step. Skip the codegen.

13. **gopls cache lag during long subagent dispatches** — every long-running implementer subagent leaves gopls reporting phantom errors (undefined symbols, GOPROXY=off, "method unused"). Reality is `go build` clean. Always verify `go build && go test -race` before reacting.

---

## Gotchas (do-not-do list)

1. **DON'T trust phone format from input.** Operators paste anything: `8 (495) 555-12-34`, `+7 495 555 12 34`, `8495 5551234`, even `+79991234567`. Always run through libphonenumber FIRST, store normalized E.164.
2. **DON'T compute phone_hash on the API tier** — must use `tenancy.PhoneHasher` so the per-tenant pepper is applied. A hash without pepper is rainbow-table-vulnerable.
3. **DON'T leak phone in logs** — even at debug level. Use the `pkg/observability` redaction; verify with grep on test outputs.
4. **DON'T skip ON CONFLICT** in import inserts — same phone may appear multiple times in a CSV; without `ON CONFLICT (tenant_id, phone_hash) DO NOTHING` you get unique-violation errors and the whole batch rolls back.
5. **DON'T use `excelize.GetRows`** for large sheets — materializes everything in memory. Use `f.Rows(sheet)` streaming iterator.
6. **DON'T trust quota cache for IsFull-then-Increment** without a `FOR UPDATE` lock — two concurrent operators finishing the last quota cell will both pass the check and increment past target. Redis cache is for read-mostly hot path; the source of truth is the SELECT FOR UPDATE inside the same TX as the call finalization.
7. **DON'T expose plaintext phone in `Get`** — return masked. `GetWithPhone` is admin-only and audited.
8. **DON'T forget the 30-day grace on Delete** — soft-delete sets `deleted_at`, schedules `worker.respondents.purge`. Actual DELETE happens only after grace elapses.
9. **DON'T enqueue an asynq task without idempotency-key** — operators can spam the import button; deduplicate at enqueue time via `asynq.Unique(...)`.
10. **DON'T accept `is_advertising=true`** — returns `ErrAdvertisingRejected`. Our scope is 152-ФЗ surveys, not 38-ФЗ advertising.

---

## Open questions (to resolve during implementation)

1. **Import payload limit**: ✅ **RESOLVED** — both: HTTP-layer `MaxMultipartMemory(50MB)` AND in-parser `importMaxRows = 100_000` cap. Defense-in-depth.
2. **Quota recompute frequency**: ⏸ **DEFERRED** — `QuotaTracker` deferred to Plan 09 (dialer hot-path). The `quotas.recompute` task constant exists in api/events.go.
3. **DNC import format**: ⏸ **DEFERRED** — `DNCManager` deferred to Plan 09. Header conventions TBD.
4. **Respondent search**: ✅ **RESOLVED** — schema has no `full_name` column (data lives in `attributes` jsonb). Implementation uses `lower(attributes::text) LIKE` + `region_code` filter. `pg_trgm` not needed at our v1 scale; documented in `buildSearchPredicate`.
5. **Operator mass-assignment**: ✅ **RESOLVED** — MERGE semantics via `INSERT ... ON CONFLICT (project_id, operator_id) DO NOTHING RETURNING` so the second concurrent assign is a no-op, not an error. Audit emits per-newly-added operator only.
6. **Audit log volume**: confirmed acceptable; one row per state-changing op + one per import row processed. Plan 03 partitioned audit_log monthly.

---

## Workflow note

Subagent dispatching Plan 06 Task N MUST:
1. Read this file before starting.
2. Read [`COMMON.md`](COMMON.md) for cross-cutting concerns.
3. Read [`plan-05-auth.md`](plan-05-auth.md) — Plan 05 lessons learned (timing-safety, gopls noise, gocognit, errorlint pattern) directly apply to Plan 06.
4. Read the actual plan task from `docs/superpowers/plans/2026-05-06-06-crm-module.md`.
5. **Use `context7` MCP** to verify current API of `nyaruka/phonenumbers`, `xuri/excelize/v2`, `hibiken/asynq`, `pgx/v5` (CopyFrom). Don't guess.
6. **Use `WebSearch`** for unfamiliar errors / runtime quirks (especially excelize cell-type edge cases, asynq retry semantics).
7. Apply skill discipline (samber/cc-skills-golang) — `golang-security` for phone redaction, `golang-error-handling` for sentinel-wrapping, `golang-testing` for table-driven + integration build tags.
8. TDD per `superpowers:test-driven-development`.
9. Composition root pattern (Plan 05 lessons): nil-checks for required deps, audit logger fallback if not yet wired, clear error wrapping.

Failure to use runtime tools = high probability of repeating Plan 05's gotchas (gopls noise, errorlint, etc.).
