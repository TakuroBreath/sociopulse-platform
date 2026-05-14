# Plan 13.2.5 — Critical Fixes — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **Plan ID:** 13.2.5
> **Predecessor:** Plan 13.2 (`v0.0.22-analytics-ingest-queries`)
> **Successor:** Plan 13.3 — reports module (XLSX/CSV/PDF + asynq) — blocked on this plan
> **References (read FIRST):** `docs/references/plan-13.2.5-critical-fixes.md`
> **Audit reports backing this plan:** 6 adversarial reviews completed 2026-05-14 (tenancy, encryption, dialer FSM, outbox+analytics, security+plans, reality audit). See references doc for one-line summaries; full reports persisted in session transcript.
> **Architecture:** `docs/architecture/03-error-handling.md` (sentinel→HTTP map), `docs/architecture/06-observability.md` (Prometheus metrics), `docs/architecture/02-module-contracts.md` § auth/crm/surveys/dialer/analytics
> **ADRs in scope:** ADR-0006 (PgBouncer txn mode), ADR-0010 (Postgres + ClickHouse), ADR-0011 (NATS over Kafka), ADR-0015 (TDD mandatory)

**Goal:** Close 5 CRITICAL findings from the 2026-05-14 quality audit plus the highest-leverage HIGH finding (project-wide AAD helper). After this plan, Plan 13.3 (reports) can ship without inheriting the multi-tenant breach, dialer-incomplete-call, OLAP-data-loss, or OLAP-dupes-on-restart risks.

**Architecture (2-3 sentences):** Six narrow fixes across existing modules, no new modules. Strongest invariant introduced: every `:id`-from-URL admin endpoint verifies the resolved row belongs to `claims.TenantID` via a new `pkg/middleware/tenant.RequireSameTenant(resolveFn)` guard before reaching service. ClickHouse engine choice flips from `MergeTree` to `ReplacingMergeTree(_inserted_at) ORDER BY (tenant_id, event_id)` to push idempotency to storage, with a new `sociopulse_analytics_ingest_dedup_miss_total` counter for observability.

**Tech Stack (no new deps):**
- Existing: gin, zap, pgx/v5, clickhouse-go/v2, NATS JetStream, Prometheus client, testify+goleak+testcontainers
- No new third-party packages

---

## Risk framing — why this plan exists

| Finding | Severity | Where | What breaks if shipped |
|---|---|---|---|
| **C1** Cross-tenant breach in admin endpoints | CRITICAL | `internal/auth`, `internal/crm`, `internal/surveys` transport+service | Admin of Tenant A can Get/Archive/UpdateRoles/ResetPassword users + projects + respondents of Tenant B |
| **C2** Dialer FSM cannot complete a call | CRITICAL | `cmd/worker` missing wiring; `dialer.api.Router.Subscribe` declared, no callers | Calls stuck in `dialing`/`call` until 30s heartbeat watchdog force-offlines |
| **C3** Silent OLAP data loss on CH insert error | CRITICAL | `internal/analytics/service/ingest.go:466-528` | NATS-Acked + LRU-known + CH-error = permanent row loss in OLAP |
| **C4** Guaranteed analytics dupes on restart | CRITICAL | `migrations/clickhouse/000001..3` use `MergeTree` not `ReplacingMergeTree(event_id)` | Restart → empty LRU → in-flight redelivered → dupes in CH |
| **C5** Production Yandex KMS adapter is a stub | CRITICAL | `pkg/encryption/kms_client_yandex.go` returns "SDK not compiled in"; `cmd/recording-uploader/main.go` is `os.Exit(64)` | **Out of scope for this plan** — tracked separately as Plan 14 Task 1 (Production Readiness) since it requires the Yandex SDK integration which is a multi-day effort. This plan ONLY adds an explicit boot-time fail-fast guard so dev binaries cannot accidentally ship as prod. |
| **H** Empty AAD in `kms_resolver.{Encrypt,Decrypt}` | HIGH | `internal/tenancy/store/kms_resolver.go:188,217`; callers pass `nil` AAD | Phone/TOTP swap attack succeeds at AEAD layer (recording flow correctly binds tenantID; nothing else does) |

C5 is deliberately scoped out of this plan beyond a fail-fast guard. Full Yandex SDK adapter lands in Plan 14 (Production Readiness) — see plan-13.2.5 references for rationale.

---

## Task list

Six independent tasks. Wave A (Tasks 1, 4, 6) and Wave B (Tasks 2, 3, 5) can each run in parallel; Wave B starts after Wave A commits land.

### Task 1 — Cross-tenant guard middleware + handler audit

**Status:** `[ ]` Not started

**Files**:
- NEW: `pkg/middleware/tenant/require_same_tenant.go`
- NEW: `pkg/middleware/tenant/require_same_tenant_test.go`
- EDIT: `internal/auth/transport/http/handlers.go` (admin user endpoints)
- EDIT: `internal/auth/service/user_service.go` (resolveTenant signature change)
- EDIT: `internal/crm/transport/http/project_handler.go` (admin project endpoints)
- EDIT: `internal/crm/service/project_service.go` + `respondent_service.go` (lookupRespondent guard)
- EDIT: `internal/surveys/service/survey_service.go` (lookupSurvey guard)
- EDIT: respective `_test.go` files — add integration tests "Tenant A → Tenant B → 404"

**Spec:**
- New middleware: `RequireSameTenant(resolveFn func(ctx, id) (uuid.UUID, error)) gin.HandlerFunc`
  - Reads `:id` from `c.Param("id")`, calls `resolveFn(c.Request.Context(), id)` to get the owning tenant_id (via BypassRLS lookup).
  - Compares against `claims.TenantID` from gin context (set by `auth.AuthRequired` upstream).
  - Mismatch → `c.AbortWithStatus(http.StatusNotFound)` (deliberately 404 to avoid existence-probe). NO body.
  - Match → `c.Next()`.
  - Resolver error of `ErrNotFound` sentinel → 404 too. Other errors → propagate via existing error envelope.
- All admin endpoints currently using the "BypassRLS resolve tenant from row → operate under that tenant" pattern must be refactored to: (a) chain `RequireSameTenant(resolver)` middleware AND (b) pass `claims.TenantID` to the service method instead of trusting the resolver's output.
- Service methods accept `tenantID uuid.UUID` as explicit parameter; the BypassRLS path is removed from these methods.

**TDD steps:**
- [ ] Write `TestRequireSameTenant_MatchesAndProceeds` — green path; resolver returns same tenant, request proceeds to handler.
- [ ] Write `TestRequireSameTenant_MismatchReturns404` — Tenant A token, ID belongs to Tenant B, response is 404, handler never called.
- [ ] Write `TestRequireSameTenant_NotFoundResolverReturns404` — resolver returns `ErrNotFound`, response is 404.
- [ ] Write `TestRequireSameTenant_MalformedIDReturns400` — `:id` is not a UUID, response is 400.
- [ ] Watch all tests fail (middleware doesn't exist yet).
- [ ] Implement middleware. Tests green.
- [ ] Add integration tests at the transport-http level for each affected admin endpoint — Tenant A admin calling Tenant B's `:id` returns 404.
- [ ] Refactor admin handlers to chain the middleware + pass `claims.TenantID` explicitly to service methods.
- [ ] Quality gate green.

### Task 2 — Wire telephony → dialer FSM

**Status:** `[ ]` Not started

**Files**:
- EDIT: `cmd/worker/main.go` (subscribe to `tenant.*.dialer.call.*` NATS subjects, route to FSM)
- NEW: `internal/dialer/transport/nats/call_event_subscriber.go`
- NEW: `internal/dialer/transport/nats/call_event_subscriber_test.go`
- EDIT: `internal/dialer/module.go` (wire subscriber via `Module.Register`)
- VERIFY: `internal/telephony/api/events.go` payload schema (ChannelCreated/ChannelAnswered/ChannelHangup/ChannelOriginateFailed)

**Spec:**
- New subscriber consumes telephony NATS events:
  - `tenant.<t>.telephony.channel.answered` → `OperatorFSM.RecordCallStarted(ctx, tenantID, operatorID, callID)`
  - `tenant.<t>.telephony.channel.hangup` with normal cause → `OperatorFSM.RecordCallEnded(ctx, tenantID, operatorID, callID, outcome)`
  - `tenant.<t>.telephony.channel.originate_failed` (busy/SIT/congestion) → `OperatorFSM.FireEvent(ctx, tenantID, operatorID, fsm.EventCallFailed)`
- Idempotent — if the FSM is already in a target state, no error; if in incompatible state, log + ack (don't loop).
- AckExplicit, MaxAckPending sized per spec (200 — covers peak call-rate per cluster).
- Subscriber lifecycle managed by errgroup in `cmd/worker/main.go` like existing daemons.

**TDD steps:**
- [ ] Write `TestCallEventSubscriber_AnsweredEvent_FiresRecordCallStarted` with fake FSM.
- [ ] Write `TestCallEventSubscriber_HangupEvent_FiresRecordCallEnded` with outcome derived from hangup_cause.
- [ ] Write `TestCallEventSubscriber_OriginateFailed_FiresEventCallFailed`.
- [ ] Write `TestCallEventSubscriber_FSMRejectsInvalidTransition_AcksAndLogs` (no retry-storm).
- [ ] Watch fail. Implement subscriber. Pass.
- [ ] Integration test via embedded JetStream + real FSM (Redis testcontainer): publish answered → assert FSM transitions to `call`.
- [ ] Wire in `cmd/worker/main.go` errgroup; verify goleak clean on shutdown.
- [ ] Quality gate green.

### Task 3 — FSM spec drift fix

**Status:** `[ ]` Not started

**Files**:
- EDIT: `internal/dialer/fsm/transitions.go`
- EDIT: `internal/dialer/fsm/transitions_test.go` — full 7×N matrix
- EDIT: `internal/dialer/api/doc.go` (diagram comment if present)

**Spec** (align to CONTEXT.md):
- `pause` reachable from `{ready, dialing, call, status, verify}` (currently only `ready`). Operator panic-pause is a user feature.
- `verify` reachable ONLY via `(status, go_verify) → verify` where the prior `status` transition came from a `success`-class outcome. Current `(ready, go_verify) → verify` is wrong — remove. The "success-class only" invariant is enforced by `Status` having sub-states: `status_success → verify` is the legal path; `status_no_answer → ready` is not.
- Add `Outcome` field to `Status` state OR add intermediate states (`status_success`, `status_no_answer`, `status_busy`, ...). Author's choice; document in transitions.go header. Recommendation: outcome-class enum on `OperatorState` rather than state explosion.

**TDD steps:**
- [ ] Write table-driven `TestTransitions_FullMatrix` enumerating all (state, event) pairs (~7 states × 12 events = ~84 entries). Each entry asserts expected target state OR expected `ErrInvalidTransition`.
- [ ] Watch fail (current matrix has at least the `verify`-from-`ready` and `pause`-from-`{call,dialing,status,verify}` gaps).
- [ ] Update transitions.go to spec. Tests pass.
- [ ] Add `TestTransitions_VerifyOnlyReachableFromSuccessOutcome` — assert that `(status, go_verify)` is rejected unless prior status outcome was `success`-class.
- [ ] Quality gate green.

### Task 4 — CH ReplacingMergeTree migration + dedup-miss metric

**Status:** `[ ]` Not started

**Files**:
- NEW: `migrations/clickhouse/000007_events_calls_replacing.up.sql` + `.down.sql`
- NEW: `migrations/clickhouse/000008_events_operator_state_replacing.up.sql` + `.down.sql`
- NEW: `migrations/clickhouse/000009_events_recording_uploaded_replacing.up.sql` + `.down.sql`
- EDIT: `internal/analytics/service/ingest.go` — add `dedupMissTotal` Prometheus counter, increment on LRU miss followed by CH `ON CLUSTER` duplicate-rejection signal
- EDIT: `internal/analytics/service/ingest_test.go` — verify metric increments on simulated dupe

**Spec:**
- Migrations rename existing tables to `_legacy`, create new with `ReplacingMergeTree(_inserted_at)` engine, `ORDER BY (tenant_id, event_id)`, identical column list plus `_inserted_at DateTime64(3) DEFAULT now64()`.
- Data migration (in `.up.sql`): `INSERT INTO events_calls SELECT *, now64() AS _inserted_at FROM events_calls_legacy WHERE 1`. Drop legacy at end of migration. Idempotent on re-run via `IF EXISTS` / `IF NOT EXISTS`.
- New Prometheus metric `sociopulse_analytics_ingest_dedup_miss_total{subject}` — counts how often a row arrives whose `event_id` is NOT in the LRU but the row is a CH-detected dupe (i.e. `_inserted_at` is older than what we just inserted). For v1, increment via a simple "every batch insert that succeeded but produced row-count 0" probe; CH ReplacingMergeTree dedupes async at merge, so exact counting requires `SELECT count() FROM events_calls FINAL WHERE event_id IN (...)` which is expensive. **Approximation**: instrument the LRU `Add` path: when an event_id is already present, increment `dedup_lru_hit_total`; when restarting and the LRU is cold, every CH `INSERT IGNORE`-like path increments the new counter. Document the imperfection in the metric help text.
- MV tables that depend on the source tables: `mv_calls_hourly`, `mv_operator_kpi_daily`, `mv_quotas_progress` — DROP and recreate with same logic pointing to the new source tables. CH MV creation is a separate down-migration step.

**TDD steps:**
- [ ] Write integration test `TestIngest_DuplicateRowsReplaced` — insert same event_id 2× with different `_inserted_at`, assert `SELECT count() FROM events_calls FINAL WHERE event_id = X` returns 1.
- [ ] Write `TestIngest_DedupMissCounterIncrements` — simulate LRU miss path, assert counter > 0.
- [ ] Watch fail (current tables are `MergeTree`; ingest path has no counter).
- [ ] Apply migration via `cmd/migrator` against testcontainers CH; tests green.
- [ ] MV down-migrations and recreations included.
- [ ] Quality gate green.

### Task 5 — Outbox DLQ alerting metric

**Status:** `[ ]` Not started

**Files**:
- EDIT: `pkg/outbox/relay.go` — periodic SELECT count(*) where attempts ≥ MaxRetry; emit `sociopulse_outbox_parked_rows_total{tenant}`
- EDIT: `pkg/outbox/relay_test.go` — verify metric value matches DB state
- VERIFY: `cmd/worker/main.go` — relay is registered with metrics handler

**Spec:**
- Relay's main drain loop has a tick interval (existing). On every Nth tick (every 60s), run `SELECT tenant_id, count(*) FROM event_outbox WHERE attempts >= $1 AND published_at IS NULL GROUP BY tenant_id` and set gauge. (Counter-style would lose info on retry-success; gauge is correct.)
- Metric type: `*prometheus.GaugeVec`, label `tenant`. Value updates on every poll.
- New constant `relay.dlqPollInterval = 60 * time.Second` exported via Config for ops tuning.
- Do NOT add any auto-retry — operator pages on `sociopulse_outbox_parked_rows_total > 0`. Manual remediation via existing tooling.

**TDD steps:**
- [ ] Write `TestRelay_DLQGauge_IncreasesAsAttemptsExceedLimit` — insert 5 rows with `attempts=10` (== MaxRetry), invoke poll, assert gauge = 5.
- [ ] Write `TestRelay_DLQGauge_PerTenant` — 3 rows tenant A + 2 rows tenant B; assert two label values.
- [ ] Watch fail. Implement. Pass.
- [ ] Quality gate green.

### Task 6 — `encryption.BuildAAD` helper + audit callers

**Status:** `[ ]` Not started

**Files**:
- NEW: `pkg/encryption/aad.go` — `BuildAAD(tenantID uuid.UUID, scope, rowID string) []byte` returning a deterministic canonical encoding
- NEW: `pkg/encryption/aad_test.go`
- EDIT: `internal/tenancy/store/kms_resolver.go` — `Encrypt(ctx, tenantID, scope, rowID, plaintext)` and `Decrypt(ctx, tenantID, scope, rowID, ciphertext)`. Signatures change; callers updated.
- EDIT: `pkg/encryption/aesgcm.go` — already accepts AAD; no change to crypto, only callers.
- EDIT: `internal/auth/service/user_service.go` (phone encrypt/decrypt — `scope="auth.user.phone"`, `rowID=user_id`)
- EDIT: `internal/auth/service/totp.go` (TOTP secret encrypt/decrypt — `scope="auth.totp.secret"`, `rowID=user_id`)
- EDIT: `internal/crm/service/respondent_service.go` (respondent phone — `scope="crm.respondent.phone"`, `rowID=respondent_id`)
- EDIT: respective `_test.go` files — verify AAD-mismatch on swapped tenant/scope/row returns `ErrAuth`

**Spec:**
- `BuildAAD(tenantID, scope, rowID)` returns `[]byte` formed by `<tenant_id>|<scope>|<row_id>` (length-prefixed each field, varint, then bytes — prevents ambiguity attacks; use `binary.AppendUvarint` + bytes). Canonical encoding documented in package doc.
- All callers of `KMSResolver.Encrypt`/`Decrypt` must now pass `scope` and `rowID`. The resolver internally builds AAD via `BuildAAD`.
- Migration / backward-compat: existing ciphertexts encrypted with empty AAD CANNOT be decrypted under the new scheme. Strategy:
  - Add a **version byte** to the wrapped DEK payload (`0x01` for empty-AAD legacy, `0x02` for BuildAAD AAD).
  - Decrypt path: peek version byte → choose AAD strategy. Encrypt path: ALWAYS `0x02`.
  - Existing rows continue to decrypt; new writes are bound.
- Recording flow (`internal/recording/service/service.go:256`) already passes `tenantID` as AAD; refactor to use `BuildAAD(tenantID, "recording.dek", callID)` for consistency. Migration applies same versioning scheme to recording DEKs.

**TDD steps:**
- [ ] Write `TestBuildAAD_Deterministic` — same inputs → same bytes; different inputs → different bytes.
- [ ] Write `TestBuildAAD_NoCollision_BetweenScopes` — `("t", "auth.user.phone", "id")` ≠ `("t", "auth.user", "phone.id")` (length-prefix prevents).
- [ ] Write `TestKMSResolver_Encrypt_v2_RejectsTenantSwap` — encrypt under (T1, scope, rowA); attempt decrypt with (T2, scope, rowA) → `ErrAuth`.
- [ ] Write `TestKMSResolver_Decrypt_v1Legacy_NoAAD_Roundtrip` — old ciphertext with `0x01` version byte decrypts unchanged.
- [ ] Watch fail. Implement. Pass.
- [ ] Update all callers; integration tests verify cross-row/cross-tenant ciphertext swap is rejected at the AEAD layer.
- [ ] Quality gate green.

---

## Definition of Done (close-out checklist)

- [ ] All 6 tasks marked `[x]` complete
- [ ] `make ci` green locally (`lint vet grep-time-after test`)
- [ ] `go test -race -count=1 -tags=integration ./...` green (testcontainers required)
- [ ] `golangci-lint run ./...` zero issues
- [ ] No new TODOs / FIXMEs introduced; if any are added, they reference a follow-up plan ID in the comment
- [ ] PROJECT_STATUS.md updated (move 🎯 NEXT to Plan 13.3; add tag-line for v0.0.23-critical-fixes)
- [ ] `docs/references/plan-13.2.5-critical-fixes.md` "Production lessons" section filled
- [ ] Plan close-out commit pushed to origin/main; CI green (6 jobs); tag `v0.0.23-critical-fixes`

## Out of scope (deliberate)

- Production Yandex KMS adapter — Plan 14 Task 1
- `internal/audit/module.go` stub fill — Plan 14 Task 2
- Operator FSM Redis↔PG reconciler on boot — Plan 14 Task 3
- Outbox partitioning + GC — Plan 14 Task 4
- JWT `WithAudience` claim — Plan 14 Task 5
- Refresh-token hashing in Redis — Plan 14 Task 6
- Go base image bump 1.26.1 → 1.26.3 — Plan 14 Task 7 (CI-only change)
- Full FSM crash-recovery (orphan `operator_sessions` reaper) — Plan 14 (separate)
- Pepper envelope encryption — long-term backlog (CLAUDE.md acknowledged)
