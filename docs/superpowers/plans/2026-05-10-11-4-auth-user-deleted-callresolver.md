# Plan 11.4 — auth user-deleted cache invalidation + CallResolver Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **Plan ID:** 11.4
> **Plan family:** Realtime hardening (after 11.1 / 11.2 / 11.3)
> **Per-plan reference:** `docs/references/plan-11-realtime.md`
> **Cross-cutting reference:** `docs/references/COMMON.md`
> **Related ADRs:** ADR-0011 (NATS over Kafka), ADR-0006 (PgBouncer transaction-mode), ADR-0015 (TDD-mandatory)
> **Architecture docs:** `docs/architecture/02-module-contracts.md`, `docs/architecture/04-testing-strategy.md`, `docs/architecture/09-agent-workflow-improvements.md`

**Goal:** Close two long-standing carry-overs from Plans 11.2 / 11.3 — wire the realtime resolver cache to the lifecycle events that already (or now will) fire from auth and recording, and add the third cross-tenant resolver dimension (CallResolver) that Plan 11.2 deferred until recording metadata shipped.

**Architecture:** The realtime module's `*service.CachedUserResolver` / `*service.CachedProjectResolver` already expose `Invalidate(id)`; Plan 11.3 Task 3's `*events.CacheInvalidator` wires the project side to `tenant.*.crm.project.status_changed`. Plan 11.4 (a) makes `internal/auth/service.UserService.Archive` publish a NEW `tenant.<t>.auth.user.deleted` outbox event (the auth module currently writes only an audit row), (b) introduces a third resolver dimension `rtapi.CallResolver` that maps `call_id → tenant_id` (mirrors UserResolver/ProjectResolver), backed by a tiny `recording.api.CallTenantLookup` BypassRLS read, (c) extends `*CacheInvalidator` to subscribe to two more subjects — `tenant.*.recording.call.deleted` (already published by the Plan 12.4 retention worker) and `tenant.*.auth.user.deleted` (newly published by Task 1) — routing each to the matching `*Cached…Resolver.Invalidate` callback. cmd/api wires the new `callResolverAdapter` and binds all three Invalidate callbacks on the upgraded `CacheInvalidator`. Mirrors Plan 11.3 Task 3 in shape: one Subscribe per subject, no extra goroutines, metric tick on every dispatch outcome.

**Tech Stack:** Go 1.26, `gin-gonic/gin` (v1.10+, ADR-0014), `go.uber.org/zap` (v1.27+, ADR-0012), `nats-io/nats.go` (JetStream push consumer, ADR-0011), `coder/websocket` v1.8.14, `redis/go-redis/v9`, `pgx/v5` via `pkg/postgres` (RLS + BypassRLS), `pkg/outbox.PostgresWriter`, `golang.org/x/sync/singleflight`, `stretchr/testify` (helpers only), `go.uber.org/goleak`, `testcontainers/testcontainers-go` for integration tests.

---

## Amendments (post-execution 2026-05-10)

The plan as written was followed verbatim with three minor adaptations during execution:

- **2026-05-10 — Task 1 test-fake naming.** Plan template referenced `newRecordingOutboxWriter` / `newFakeUserStoreWithTenant`; the actual `internal/auth/service/user_service_test.go` file uses internal `package service` with existing fakes named `fakeStore` / `fakeAudit` / `fakeTxRunner`. Implementer adapted to match: introduced `fakeOutbox` / `newFakeOutbox()` mirroring the existing fake pattern; extended `fakeTxRunner.lastRolledBack()`; updated the existing `newSvc(t)` 3-tuple to a 4-tuple `(*UserService, *fakeStore, *fakeAudit, *fakeOutbox)`. All 22 existing call sites updated. **Spec semantics unchanged** — same behaviour, just consistent local naming.

- **2026-05-10 — Task 1 reviewer fixups (commit `dd9be37`).** Code-quality reviewer flagged: (a) IMPORTANT — stale package-level comment in `internal/auth/api/events.go` claimed "auth does not publish events on its own subjects" — now contradicted by the new `SubjectUserDeleted` block in the same file; (b) MINOR — `TestUserService_Archive_Idempotent` did not pin the outbox-row count post-Plan-11.4. Both fixed inline (~13 lines docs+tests, zero behavior change) per re-review proportionality (`docs/architecture/09-agent-workflow-improvements.md` #7).

- **2026-05-10 — Task 4 sentinel name.** Plan template said use `store.ErrInvalidArgument` OR define one. The existing `internal/recording/store/` package does not define such a sentinel; the canonical "invalid input" sentinel is `rapi.ErrInvalidInput` (api-level). For the nil-callID guard in `LookupTenant`, implementer used a plain `fmt.Errorf("recording.store.LookupTenant: nil callID")` without a sentinel — calls from `cmd/api/recording_resolver.go::Get` fold any error into `ErrCrossTenantSubscribe` via the realtime layer's `verifyTenant` helper, so a dedicated sentinel is unnecessary at the store boundary. Reviewer noted this as MINOR + acceptable.

No spec contradictions; no migrations changed; no ADR contradictions.

---

## Context — verified facts

Every cross-boundary assertion below carries a `Verified by:` citation per `docs/architecture/09-agent-workflow-improvements.md` #1.

### State of the realtime module

- `*events.CacheInvalidator` exists and subscribes to ONE subject (`tenant.*.crm.project.status_changed`). It exposes a single required callback `ProjectInvalidate projectInvalidateFn`, plus `Subscriber`, `Metrics`, `Logger`, `QueueGroup` config fields. Metric: `realtime_cache_invalidations_total{result}` with bounded labels {ok, parse_error, empty_project_id}.
  Verified by: `internal/realtime/events/cache_invalidator.go:1-194` (full file) + `internal/realtime/events/metrics.go:118-159`.
- `*service.CachedUserResolver` and `*service.CachedProjectResolver` already expose `Invalidate(id)` (Plan 11.3 Task 2). Each composes `cache.Delete(id) + group.Forget(id)` so an in-flight singleflight leader is dropped along with the cached entry.
  Verified by: `internal/realtime/service/resolver_cache.go:181-184` (CachedUserResolver) + `:276-279` (CachedProjectResolver).
- `rtapi.UserResolver` / `rtapi.ProjectResolver` are defined; `rtapi.ResolvedTenant{TenantID string}` is the projection. There is NO `CallResolver` today — Plan 11.2 Task 4 explicitly deferred it ("CallID cross-tenant check is intentionally NOT performed here. See Plan 11.2 plan, 'Out of scope' — Plan 12 (recording metadata) introduces the CallStore.Get the third resolver would consume.").
  Verified by: `internal/realtime/api/interfaces.go:75-114` + `internal/realtime/service/rbac.go:175-180` (the comment block).
- `rtapi.LocatorUserResolver` / `LocatorProjectResolver` constants live in `internal/realtime/api/locator.go`. There is no `LocatorCallResolver` constant.
  Verified by: `internal/realtime/api/locator.go:37-46`.
- `realtime.Module.Register` retains `cachedProjects *service.CachedProjectResolver` as a Module field and binds `cachedProjects.Invalidate` as the only `ProjectInvalidate` callback today. `cachedUsers` is built but NOT retained — the binding for `cachedUsers.Invalidate` does NOT exist.
  Verified by: `internal/realtime/module.go:100-115` + `:191-198` + `:262-288` (the CacheInvalidator construction block).

### State of the auth module

- `internal/auth/api/events.go` declares ONLY `AuditAction*` string constants (e.g. `AuditActionLogin`, `AuditActionTOTPEnrolled`). The package-level comment states verbatim: "auth does not publish events on its own subjects. Instead, every successful login, logout, TOTP enrolment, session revocation, and refresh-token replay is mirrored to the audit module via the canonical `tenant.<t>.audit.event` subject."
  Verified by: `internal/auth/api/events.go:1-27`.
- `auth.UserService.Archive(ctx, id)` writes a `user.archived` audit row inside `tx.WithTenant(ctx, tenantID, fn)`. It does NOT call `outbox.Writer.Append`. The struct `UserService` in `service/user_service.go` has fields `tx userTxRunner / store / hasher / audit / clock / dummyHash` — no `outbox outbox.Writer` field today.
  Verified by: `internal/auth/service/user_service.go:69-86` (struct decl) + `:307-332` (Archive impl).
- `internal/auth/module.go` has NO outbox wiring (`grep outbox` returns zero matches in that file). Auth has never imported `pkg/outbox` before this plan.
  Verified by: `grep -n "outbox\|Outbox\|publishEvent\|eventbus\|Subscriber\|Publisher" internal/auth/module.go internal/auth/service/user_service.go` — zero matches in auth.

### State of the recording module

- `recording.api.SubjectRecordingCallDeleted = "tenant.<t>.recording.call.deleted"` is a placeholder constant; concrete subjects are rendered by `SubjectRecordingCallDeletedFor(tenantID uuid.UUID) string` returning `fmt.Sprintf("tenant.%s.recording.call.deleted", tenantID)`. Payload struct: `RecordingCallDeletedEvent{RecordingID uuid.UUID, CallID uuid.UUID, TenantID uuid.UUID, DeletedAt int64, Reason string}`. Reason is "retention" for worker-driven path; "manual" reserved for a future admin-driven hard-delete.
  Verified by: `internal/recording/api/events.go:9-94`.
- The Plan 12.4 retention worker publishes this event via the outbox path inside `pool.WithTenant(row.TenantID, fn) { MarkDeletedTx + audit + outbox.Append }`.
  Verified by: PROJECT_STATUS line 154 (Plan 12.4 description) + `docs/superpowers/plans/2026-05-09-12-4-recording-workers.md:745` (line confirms "subscribe to this to drop CallResolver caches").
- `recording.api.RecordingService.Get(ctx, tenantID, callID uuid.UUID)` requires `tenantID` at the boundary — it cannot serve as a `call_id → tenant_id` resolver because the caller doesn't yet know the tenant. The module needs a NEW lookup port that takes `callID` only and returns `tenantID`, with `BypassRLS` semantics (cross-tenant read).
  Verified by: `internal/recording/api/interfaces.go:13-26`.
- The `call_recordings` table has columns `tenant_id uuid` + `call_id uuid` (the latter `UNIQUE` per the recording rows; see `internal/recording/store/postgres.go:GetByCallID` for the existing per-tenant lookup). Migration `000010_call_recordings_evolve.up.sql` (Plan 12.1) and `000011_admin_grants_call_recordings.up.sql` (Plan 12.4) cover the schema + grants.
  Verified by: `internal/recording/store/postgres.go` `GetByCallID` (used by Plan 12.3) + PROJECT_STATUS line 154 (migration 000011 grants `tenancy_admin` SELECT + UPDATE on `call_recordings`).
- `tenancy_admin` (the role activated by `pool.BypassRLS`) has SELECT on `call_recordings` (Plan 12.4 added this). A BypassRLS-based `call_id → tenant_id` lookup will work with no further migration.
  Verified by: PROJECT_STATUS line 277 (Recording-specific standing rule line) — re-verify by `grep -n tenancy_admin migrations/000011_admin_grants_call_recordings.up.sql` before implementation.

### Cross-cutting infra

- `pkg/outbox.PostgresWriter.Append(ctx, tx, ev outbox.Event) error` is the canonical writer. Signature: `func (w *PostgresWriter) Append(ctx context.Context, tx postgres.Tx, ev Event) error`. `Event{ID int64, TenantID *uuid.UUID, AggregateID *uuid.UUID, Subject string, Payload []byte, CreatedAt time.Time, PublishedAt *time.Time, LastError *string, Attempts int}`. ID/CreatedAt/PublishedAt/LastError/Attempts are populated by the relay; Append callers leave them zero.
  Verified by: `pkg/outbox/writer.go:37-49` + `pkg/outbox/event.go` (full file).
- `event_outbox` is owned by `tenancy_admin` BUT `app` retains full CRUD grants, so `outbox.Writer.Append` works inside a `WithTenant` Tx without role-switching — the same pattern Plan 12.1 uses.
  Verified by: PROJECT_STATUS line 263 (Plan 12.1 standing rule line).
- The cmd/api outbox relay drains via real NATS once Plan 11 Task 4a wired `*pkg/eventbus.NATSPublisher`; subscribers receive via `*pkg/eventbus.NATSSubscriber`.
  Verified by: PROJECT_STATUS line 60 (`pkg/eventbus`) + `cmd/api/main.go:417` (outbox wiring).
- `realtime.Module.Register` accepts `Deps.Subscriber pkg/eventbus.Subscriber`. The CacheInvalidator's lifecycle is tied to the bus's Close (NOT to Register's ctx), so Start MUST receive `context.Background()` (this is intentional — `internal/realtime/module.go:270-275` documents it).
  Verified by: `internal/realtime/module.go:262-288` + `internal/realtime/events/cache_invalidator.go:120-135` (the comment block).

### Architecture decisions locked in this plan

- **Decision 1 (locked).** The new `auth.user.deleted` event is published via the outbox pattern (transactional with the row update + audit), NOT a direct NATS publish. This matches the canonical pattern used by `recording`, `crm`, `dialer`, `tenancy`. Rationale: at-least-once delivery without 2PC; the relay's FOR UPDATE SKIP LOCKED handles retries idempotently.
- **Decision 2 (locked).** `CallResolver` lives in `internal/realtime/api/`; the recording-side lookup is a SEPARATE narrow port `recording.api.CallTenantLookup`. cmd/api adapts one to the other. This mirrors the auth/crm pattern (auth.UserService → userResolverAdapter → rtapi.UserResolver) and preserves the depguard `module-boundaries` rule (realtime never imports recording's service or store).
- **Decision 3 (locked).** `*CacheInvalidator` keeps `ProjectInvalidate` as a REQUIRED callback (panic on nil — preserves Plan 11.3 contract); the new `UserInvalidate` and `CallInvalidate` fields are OPTIONAL. A nil callback skips that subject's `Subscribe` with an INFO log. Rationale: degraded boot (cmd/api without recording or auth modules) must remain operable.
- **Decision 4 (locked).** The `auth.user.deleted` event publishes only on `UserService.Archive` — NOT on hypothetical hard-delete paths. Hard-delete of users is out of scope for v1; if a future plan adds it, it adds publication too. The event's `Reason` field is set to `"archived"` to leave room for `"hard_deleted"` later.

---

## File structure

### Files created

| Path | Responsibility |
|---|---|
| `internal/recording/api/lookup.go` | `CallTenantLookup` port (one method: `LookupTenant(ctx, callID) (uuid.UUID, error)`). Tiny new file; keeps `interfaces.go` focused on RecordingService. |
| `internal/recording/store/lookup.go` | `*PostgresStore.LookupTenant` method via BypassRLS SELECT FROM `call_recordings`. |
| `internal/recording/store/lookup_pg_test.go` | testcontainers integration test (`-tags=integration`) for the BypassRLS lookup. |
| `internal/realtime/service/cached_call_resolver.go` | `CachedCallResolver` mirror — split out of `resolver_cache.go` because that file is already 287 lines and would balloon past 400. |
| `internal/realtime/service/cached_call_resolver_test.go` | Mirror tests for `CachedCallResolver` (TTL, singleflight, Invalidate, ctx-bleed regression). |
| `cmd/api/recording_resolver.go` | `callResolverAdapter` adapter from `recording.api.CallTenantLookup` → `rtapi.CallResolver` + `registerCallResolver`. Split from `cmd/api/realtime.go` to keep that file focused on auth+crm wiring. |
| `cmd/api/recording_resolver_test.go` | Adapter tests (parse-uuid path, lookup-error path, success path). |

### Files modified

| Path | Change |
|---|---|
| `internal/auth/api/events.go` | Add `SubjectUserDeleted` const + `SubjectUserDeletedFor` helper + `UserDeletedEvent` payload struct. |
| `internal/auth/service/user_service.go` | Add `outbox outbox.Writer` field + `outboxClock` already covered; modify `Archive` to call `outbox.Append` inside the existing Tx. |
| `internal/auth/service/user_service_test.go` | Extend `TestUserService_Archive` to assert the outbox row appears with the correct subject + payload. New table-driven test for outbox-append failures. |
| `internal/auth/module.go` | Construct `outbox.NewPostgresWriter()` and pass into `service.NewUserService(...)`. |
| `internal/realtime/api/interfaces.go` | Add `CallResolver` interface. |
| `internal/realtime/api/locator.go` | Add `LocatorCallResolver = "realtime.CallResolver"` constant. |
| `internal/realtime/service/rbac.go` | Extend `TopicRBAC` with `callResolver rtapi.CallResolver` field; new `NewTopicRBACWithCallResolver(users, projects, calls)` constructor; `checkCrossTenant` validates `filter.CallID` when callResolver wired. |
| `internal/realtime/service/rbac_test.go` | Cross-tenant call subscribe tests + selfOnly+callID interaction. |
| `internal/realtime/events/cache_invalidator.go` | Add `UserInvalidate`, `CallInvalidate` (entity-id-string callbacks) + 2 new subscriptions (`auth.user.deleted`, `recording.call.deleted`); rename internal type `projectInvalidateFn` → `entityInvalidateFn` (it's now reused for all 3 dimensions). Extend `handle*` switch to dispatch by subject. |
| `internal/realtime/events/cache_invalidator_test.go` | Add tests for the 2 new subscription paths (auth + recording) + nil-callback degraded paths. |
| `internal/realtime/events/metrics.go` | Extend `result` label values: add `empty_user_id`, `empty_call_id`. Add a SECOND label `subject` (so the metric becomes `realtime_cache_invalidations_total{subject, result}`) — bounded set: 3 subjects × {ok / parse_error / empty_id} = 9 cells. |
| `internal/realtime/module.go` | Retain `cachedUsers` as a `*Module` field; bind ALL 3 `Invalidate` callbacks on the CacheInvalidatorConfig; resolve `LocatorCallResolver` for the new `CachedCallResolver`. |
| `cmd/api/main.go` | Call new `registerCallResolver(locator, logger)` before `realtime.Module.Register` (alongside existing `registerRealtimeResolvers`). |
| `cmd/api/realtime.go` | Doc note that the call dimension now has a parallel adapter in `cmd/api/recording_resolver.go`. No code move; just a one-line comment for navigability. |

### Test seams

- `internal/recording/store/lookup_pg_test.go` uses the existing `testcontainers.PostgresFromMigrations(t, ...)` helper from `pkg/postgres/testcontainers_helper.go` (or equivalent — implementer should verify the exact helper name when reading the Plan 12.4 lifecycle test). Build tag `//go:build integration`.
- `internal/realtime/service/cached_call_resolver_test.go` needs no infra (pure unit).
- `cmd/api/recording_resolver_test.go` uses an in-memory `fakeCallTenantLookup` struct (mirrors `fakeAuthUserGetter` in `cmd/api/realtime_test.go`).
- `internal/auth/service/user_service_test.go` needs an `outbox.Writer` test fake — a `recordingOutboxWriter` slice-backed struct that captures every `Append` call. The recording module's tests already use this pattern; mirror.

---

## Self-verification checklist (run before dispatching the first implementer)

1. ✅ Every cross-boundary assertion in **Context** has `Verified by:` or is marked `Assumed (not verified)`.
2. ✅ Tasks reference concrete files with concrete line ranges where applicable.
3. ✅ No placeholders ("TODO", "fill in", "similar to Task N", "appropriate error handling").
4. ✅ Type/signature consistency: `entityInvalidateFn` (renamed from `projectInvalidateFn`) used uniformly across Tasks 6 and 7. `CallTenantLookup` signature consistent across Tasks 4 and 7. `CachedCallResolver` API consistent across Tasks 3 and 7.
5. ✅ Pre-commit gate (`make ci` + `go test -race -count=1` + `gofmt -l` + `make grep-time-after`) explicitly written into each task's final step.
6. ✅ Plan vocabulary checked against `CONTEXT.md` — every term used (tenant, RLS, outbox, KMS, recording, call, FSM-irrelevant, NATS, JetStream, audit log, BypassRLS, AAD-irrelevant) is in the glossary.
7. ✅ Plan does not contradict any existing ADR. ADR-0011 (NATS over Kafka) — using NATS subjects is on-spec. ADR-0006 (PgBouncer transaction-mode) — `WithTenant` Tx is per-request, on-spec. ADR-0015 (TDD) — every task starts with a failing test.
8. ✅ `docs/references/plan-11-realtime.md` exists and is referenced in the plan header. (We will append a Plan 11.4 section in Phase 4 close-out.)
9. ✅ Paths in the File-structure section match actual scaffolding — verified by `ls internal/auth/api/ internal/recording/api/ internal/realtime/api/ internal/realtime/service/ internal/realtime/events/ cmd/api/`.

---

## Task 1: Auth — publish `tenant.<t>.auth.user.deleted` outbox event on Archive

**Files:**
- Modify: `internal/auth/api/events.go`
- Modify: `internal/auth/service/user_service.go:69-86` (struct decl) and `:307-332` (Archive impl) and the `NewUserService(...)` constructor signature
- Modify: `internal/auth/service/user_service_test.go` (extend `TestUserService_Archive`)
- Modify: `internal/auth/module.go` (wire `outbox.NewPostgresWriter()`)

**Background:** auth currently does NOT publish any NATS-side events; only audit rows. Plan 11.4 introduces the FIRST auth NATS subject. We follow the canonical outbox pattern (matches recording, crm, dialer, tenancy): `WithTenant Tx { store mutation + audit + outbox.Append }`. The relay drains pending event_outbox rows to NATS via FOR UPDATE SKIP LOCKED so the publication is at-least-once without 2PC. No KMS, no encryption — `UserDeletedEvent` carries only opaque IDs (no PII).

- [ ] **Step 1: Write the failing test — auth events constants**

Add a new test file `internal/auth/api/events_test.go`:

```go
package api_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	authapi "github.com/sociopulse/platform/internal/auth/api"
)

// TestSubjectUserDeleted_Constant verifies the canonical subject literal
// matches the plan-11-realtime convention tenant.<t>.<area>.<entity>.<event>.
func TestSubjectUserDeleted_Constant(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "tenant.<t>.auth.user.deleted", authapi.SubjectUserDeleted)
}

// TestSubjectUserDeletedFor_RendersConcreteSubject verifies the
// for-tenant helper produces the runtime subject string the outbox
// relay publishes on.
func TestSubjectUserDeletedFor_RendersConcreteSubject(t *testing.T) {
	t.Parallel()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000abc")
	got := authapi.SubjectUserDeletedFor(tid)
	assert.Equal(t,
		"tenant.00000000-0000-0000-0000-000000000abc.auth.user.deleted",
		got,
	)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -race -run 'TestSubjectUserDeleted' ./internal/auth/api/ -v`
Expected: FAIL — `authapi.SubjectUserDeleted` and `authapi.SubjectUserDeletedFor` are undefined.

- [ ] **Step 3: Add `SubjectUserDeleted` + `UserDeletedEvent` to `internal/auth/api/events.go`**

Append this block to `internal/auth/api/events.go` (keep the existing AuditAction* constants intact; add the imports `"fmt"` + `"time"` + `"github.com/google/uuid"` if not present already):

```go
// NATS subject placeholders for the durable JetStream stream AUTH
// (auth events stream — created by infra alongside the existing
// CRM/RECORDING streams). The "<t>" placeholder is for documentation
// only — code MUST use the SubjectUserDeletedFor helper to render the
// concrete subject for a tenant.
//
// Plan 11.4 introduces the FIRST auth NATS subject. Future auth
// lifecycle events (user.created, user.role_changed, ...) belong on
// sibling tenant.<t>.auth.user.<verb> subjects.
const (
	// SubjectUserDeleted is published after a successful UserService.Archive.
	// Subscribers (currently the realtime CacheInvalidator) treat this as
	// "the user can no longer authenticate; drop any cached entry referring
	// to them". A future hard-delete path would emit the same subject with
	// Reason="hard_deleted".
	SubjectUserDeleted = "tenant.<t>.auth.user.deleted"
)

// SubjectUserDeletedFor returns the concrete NATS subject for the
// auth.user.deleted event for the given tenant. Mirrors the
// crmapi.SubjectProjectStatusFor / recordingapi.SubjectRecordingCallDeletedFor
// pattern.
func SubjectUserDeletedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.auth.user.deleted", tenantID)
}

// UserDeletedEvent is the payload published on
// SubjectUserDeletedFor(tenantID) after UserService.Archive (and any
// future hard-delete path) commits. Subscribers MUST treat the user
// as no longer authenticatable — any cached (user_id → tenant_id)
// resolver entry should be dropped.
//
// Reason is "archived" for the v1 soft-delete path (the only path
// that exists today). A future hard-delete admin endpoint would emit
// "hard_deleted"; archive code paths MUST NOT use that value.
//
// Carries opaque UUIDs only — no PII (phone numbers, emails, names)
// crosses the bus.
type UserDeletedEvent struct {
	UserID    uuid.UUID `json:"user_id"`
	TenantID  uuid.UUID `json:"tenant_id"`
	DeletedAt int64     `json:"deleted_at"` // unix seconds
	Reason    string    `json:"reason"`     // "archived" | "hard_deleted"
}
```

- [ ] **Step 4: Run the constant test to verify it passes**

Run: `go test -race -run 'TestSubjectUserDeleted' ./internal/auth/api/ -v`
Expected: PASS (both subtests).

- [ ] **Step 5: Write the failing test — Archive publishes the outbox event**

Add this test function to `internal/auth/service/user_service_test.go` (use the existing test framework — `main_test.go` provides `goleak.VerifyTestMain`; testify is already imported):

```go
// TestUserService_Archive_PublishesOutboxEvent verifies that Archive
// emits a tenant.<t>.auth.user.deleted outbox row alongside the
// existing audit row. Plan 11.4 Task 1 contract.
func TestUserService_Archive_PublishesOutboxEvent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	tenantID := uuid.New()
	userID := uuid.New()

	// Existing test fixture — see fakeUserStore in user_service_test.go.
	store := newFakeUserStoreWithTenant(userID, tenantID)
	tx := newFakeTxRunner()
	hasher := passwords.NewBoundedHasher(passwords.Default(), 1)
	auditLogger := newRecordingAuditLogger()
	outboxFake := newRecordingOutboxWriter()

	clock := func() time.Time {
		return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	}

	svc := service.NewUserService(tx, store, hasher, auditLogger, outboxFake, clock)

	require.NoError(t, svc.Archive(ctx, userID))

	// Audit row is still emitted (Plan 11.4 doesn't remove existing behaviour).
	require.Len(t, auditLogger.events(), 1)
	require.Equal(t, "user.archived", auditLogger.events()[0].Action)

	// Outbox row is the new behaviour.
	rows := outboxFake.appended()
	require.Len(t, rows, 1, "Archive must append exactly one outbox row")

	got := rows[0]
	require.Equal(t, authapi.SubjectUserDeletedFor(tenantID), got.Subject)
	require.NotNil(t, got.TenantID)
	require.Equal(t, tenantID, *got.TenantID)
	require.NotNil(t, got.AggregateID)
	require.Equal(t, userID, *got.AggregateID)

	var ev authapi.UserDeletedEvent
	require.NoError(t, json.Unmarshal(got.Payload, &ev))
	assert.Equal(t, userID, ev.UserID)
	assert.Equal(t, tenantID, ev.TenantID)
	assert.Equal(t, clock().Unix(), ev.DeletedAt)
	assert.Equal(t, "archived", ev.Reason)
}

// TestUserService_Archive_OutboxAppendErrorRollsBackTx ensures the
// transaction rolls back when outbox append fails — the audit row and
// the user.archived store mutation must NOT commit independently.
func TestUserService_Archive_OutboxAppendErrorRollsBackTx(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	tenantID := uuid.New()
	userID := uuid.New()

	store := newFakeUserStoreWithTenant(userID, tenantID)
	tx := newFakeTxRunner()
	hasher := passwords.NewBoundedHasher(passwords.Default(), 1)
	auditLogger := newRecordingAuditLogger()

	wantErr := errors.New("outbox down")
	outboxFake := newRecordingOutboxWriter().withFailure(wantErr)

	svc := service.NewUserService(tx, store, hasher, auditLogger, outboxFake, nil)

	err := svc.Archive(ctx, userID)
	require.Error(t, err)
	require.ErrorIs(t, err, wantErr,
		"outbox failure must propagate so the WithTenant Tx rolls back")
	// Tx fake reports a rollback when fn returned non-nil.
	require.True(t, tx.lastRolledBack(),
		"outbox.Append failure must roll back the WithTenant Tx")
	// Store should have invoked Archive but the rollback path
	// effectively undoes it from the caller's perspective; the fake
	// stores never see commit so the assertion is on the rollback flag.
}
```

The fixture helpers `newFakeUserStoreWithTenant`, `newFakeTxRunner`, `newRecordingAuditLogger`, `newRecordingOutboxWriter` follow the existing patterns in `user_service_test.go` — implementer adds the new `recordingOutboxWriter` helper alongside the existing audit fake. Pattern reference: `internal/recording/service/service_test.go` has a slice-backed `fakeOutboxWriter`; mirror that here.

- [ ] **Step 6: Run the new tests to verify they fail**

Run: `go test -race -run 'TestUserService_Archive_PublishesOutboxEvent|TestUserService_Archive_OutboxAppendErrorRollsBackTx' ./internal/auth/service/ -v`
Expected: FAIL — `service.NewUserService` signature does not include `outboxFake` argument; tests do not compile.

- [ ] **Step 7: Add the `outbox` field + extend the constructor + modify `Archive`**

Edit `internal/auth/service/user_service.go`:

(a) Extend the struct literal at lines 69-86 by adding an `outbox outbox.Writer` field after `audit auditapi.Logger`:

```go
type UserService struct {
	tx     userTxRunner
	store  authapi.UserStorePort
	hasher passwords.Hasher
	audit  auditapi.Logger
	outbox outbox.Writer // Plan 11.4: appends auth.user.deleted etc. inside same Tx.
	clock  func() time.Time

	dummyHash string
}
```

Add the import `"github.com/sociopulse/platform/pkg/outbox"` at the top.

(b) Modify the constructor signature (line 101+):

```go
// NewUserService constructs a UserService from already-built deps. The
// caller (the module composition root) owns the lifecycle of every
// dependency. clock may be nil — the constructor falls back to
// time.Now so callers do not have to repeat that boilerplate.
//
// auditLogger MUST NOT be nil: every state-changing UserService method
// emits an audit row inside the same transaction as the data write,
// and a misconfigured composition root that registered nil would
// silently drop those rows. Tests that genuinely don't care about the
// audit trail must inject a no-op fake logger explicitly.
//
// outboxWriter MUST NOT be nil — Plan 11.4 (Archive) writes a
// tenant.<t>.auth.user.deleted row inside the same Tx. Tests use a
// recording fake; production passes outbox.NewPostgresWriter().
func NewUserService(
	pool userTxRunner,
	store authapi.UserStorePort,
	hasher passwords.Hasher,
	auditLogger auditapi.Logger,
	outboxWriter outbox.Writer,
	clock func() time.Time,
) *UserService {
	if pool == nil {
		panic("auth/service: NewUserService: pool is required")
	}
	if store == nil {
		panic("auth/service: NewUserService: store is required")
	}
	if hasher == nil {
		panic("auth/service: NewUserService: hasher is required")
	}
	if auditLogger == nil {
		panic("auth/service: NewUserService: auditLogger is required (use a no-op fake in tests, never nil)")
	}
	if outboxWriter == nil {
		panic("auth/service: NewUserService: outboxWriter is required (use a recording fake in tests, never nil)")
	}
	if clock == nil {
		clock = time.Now
	}
	return &UserService{
		tx:        pool,
		store:     store,
		hasher:    hasher,
		audit:     auditLogger,
		outbox:    outboxWriter,
		clock:     clock,
		dummyHash: dummyArgon2idHash,
	}
}
```

(c) Modify `Archive` (line 307-332) to append the outbox row inside the existing Tx, AFTER the audit write:

```go
// Archive implements api.UserService.Archive. Idempotent: archiving a
// user whose archived_at is already set returns nil (the store-level
// idempotency); the audit row is emitted on every call so a re-archive
// is still observable. Plan 11.4: also publishes the
// tenant.<t>.auth.user.deleted outbox event so downstream subscribers
// (realtime resolver cache) can drop stale entries.
func (s *UserService) Archive(ctx context.Context, id uuid.UUID) error {
	if id == uuid.Nil {
		return fmt.Errorf("auth/service: archive: id required")
	}
	tenantID, err := s.resolveTenant(ctx, id)
	if err != nil {
		return err
	}
	now := s.clock().UTC()
	err = s.tx.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		if err := s.store.Archive(ctx, tx, id); err != nil {
			return err
		}
		if err := s.writeAudit(ctx, auditapi.Event{
			TenantID: tenantID,
			Action:   "user.archived",
			Target:   "user:" + id.String(),
		}); err != nil {
			return err
		}
		payload, err := json.Marshal(authapi.UserDeletedEvent{
			UserID:    id,
			TenantID:  tenantID,
			DeletedAt: now.Unix(),
			Reason:    "archived",
		})
		if err != nil {
			return fmt.Errorf("marshal user_deleted payload: %w", err)
		}
		return s.outbox.Append(ctx, tx, outbox.Event{
			TenantID:    &tenantID,
			AggregateID: &id,
			Subject:     authapi.SubjectUserDeletedFor(tenantID),
			Payload:     payload,
		})
	})
	if err != nil {
		if errors.Is(err, authapi.ErrUserNotFound) {
			return err
		}
		return fmt.Errorf("auth/service: archive: %w", err)
	}
	return nil
}
```

Add the imports `"encoding/json"` (likely already present) at the top of the file.

- [ ] **Step 8: Run the failing tests to verify they now pass**

Run: `go test -race -run 'TestUserService_Archive' ./internal/auth/service/ -v`
Expected: PASS — both new subtests + the existing `TestUserService_Archive` continue to pass.

- [ ] **Step 9: Update existing tests that call `NewUserService` with the old signature**

Run: `grep -rn 'service.NewUserService\b\|authservice.NewUserService\b' internal/ cmd/`
For every call site missing the new outbox argument, pass either `outbox.NewPostgresWriter()` (production paths) or `newRecordingOutboxWriter()` (test paths). Expected call sites: `internal/auth/service/user_service_test.go` (existing tests need the param), `internal/auth/module.go` (production wiring — see Step 10).

Run all auth tests: `go test -race -count=1 ./internal/auth/...`
Expected: PASS (zero failures, no goleak panics).

- [ ] **Step 10: Wire `outbox.NewPostgresWriter()` from `internal/auth/module.go`**

Find the `service.NewUserService(...)` call in `internal/auth/module.go` and add the outbox writer:

```go
import (
	// ... existing imports
	"github.com/sociopulse/platform/pkg/outbox"
)

// ... inside Module.Register, where userService is built:
userService := service.NewUserService(
	pool,
	userStore,
	hasher,
	auditLogger,
	outbox.NewPostgresWriter(), // Plan 11.4: emits auth.user.deleted on Archive.
	nil, // clock — defaults to time.Now via constructor
)
```

The implementer should preserve the existing argument-order; the new `outboxWriter` slot is between `auditLogger` and `clock`.

- [ ] **Step 11: Run the full module test + build**

Run:
```bash
go build ./...
go vet ./...
go test -race -count=1 ./internal/auth/...
make grep-time-after
```
Expected: all green. `make grep-time-after` exits 0.

- [ ] **Step 12: Run the canonical pre-commit gate**

```bash
make ci                         # = lint + vet + grep-time-after + test
go test -race -count=1 ./...    # full repo race
gofmt -l .                      # any output = unformatted, fail
```
Expected: zero output from `gofmt -l`; `make ci` exits 0; full repo race passes.

If gopls in the IDE shows residual errors but these commands are green, that's the documented gopls cache pollution (PROJECT_STATUS standing rules) — ignore and proceed.

- [ ] **Step 13: Commit**

```bash
git add internal/auth/api/events.go \
        internal/auth/api/events_test.go \
        internal/auth/service/user_service.go \
        internal/auth/service/user_service_test.go \
        internal/auth/module.go
git commit -m "feat(auth): publish tenant.<t>.auth.user.deleted on Archive (Plan 11.4 Task 1)

UserService.Archive now writes an outbox row alongside the existing
audit row inside the WithTenant Tx. Adds SubjectUserDeleted /
SubjectUserDeletedFor / UserDeletedEvent to internal/auth/api/events.go.
Carries opaque UUIDs only (no PII).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Realtime — `CallResolver` port + `LocatorCallResolver` constant

**Files:**
- Modify: `internal/realtime/api/interfaces.go`
- Modify: `internal/realtime/api/locator.go`

**Background:** Plan 11.2 Task 4 deferred the third resolver dimension explicitly: "Plan 12 (recording metadata) introduces the `CallStore.Get(ctx, callID) → CallMetadata{TenantID}` shape." Plan 12 has shipped (12.1–12.4 closed); the prerequisite is met. We now add the abstract port — implementations land in Tasks 4 (recording-side lookup) and 7 (cmd/api adapter).

- [ ] **Step 1: Write the failing test — locator constant**

Append this test function to `internal/realtime/api/locator_test.go` (create it if absent):

```go
package api_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// TestLocatorCallResolver_Constant pins the locator key string to its
// canonical form. Any drift between this constant and the value cmd/api
// uses to look up the resolver is a wiring bug; the constant is the
// single source of truth.
func TestLocatorCallResolver_Constant(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "realtime.CallResolver", rtapi.LocatorCallResolver)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -race -run 'TestLocatorCallResolver_Constant' ./internal/realtime/api/ -v`
Expected: FAIL — `rtapi.LocatorCallResolver` undefined.

- [ ] **Step 3: Add the locator constant**

Append to `internal/realtime/api/locator.go` (just before the closing `)` of the `const` block):

```go
	// LocatorCallResolver is the locator key for the realtime
	// CallResolver. cmd/api adapts a recording-side
	// CallTenantLookup → CallResolver and registers it under this key
	// BEFORE realtime.Module.Register. Plan 11.4 Task 7 wires this in.
	LocatorCallResolver = "realtime.CallResolver"
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -race -run 'TestLocatorCallResolver_Constant' ./internal/realtime/api/ -v`
Expected: PASS.

- [ ] **Step 5: Write the failing test — CallResolver interface compile-time**

Append to `internal/realtime/api/interfaces_test.go` (create the file if absent):

```go
package api_test

import (
	"context"
	"testing"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// fakeCallResolver is a compile-time conformance probe for the new
// rtapi.CallResolver interface — if the interface signature drifts,
// this test file will stop compiling.
type fakeCallResolver struct{}

func (fakeCallResolver) Get(_ context.Context, _ string) (rtapi.ResolvedTenant, error) {
	return rtapi.ResolvedTenant{}, nil
}

var _ rtapi.CallResolver = fakeCallResolver{}

// TestCallResolver_InterfaceShape is a runtime no-op — the actual
// guarantee is the compile-time `var _ rtapi.CallResolver` line above.
// The function exists so `go test` reports the file as "tested".
func TestCallResolver_InterfaceShape(t *testing.T) {
	t.Parallel()
	var _ rtapi.CallResolver = fakeCallResolver{}
}
```

- [ ] **Step 6: Run the test to verify it fails**

Run: `go test -race -run 'TestCallResolver_InterfaceShape' ./internal/realtime/api/ -v`
Expected: FAIL — `rtapi.CallResolver` undefined.

- [ ] **Step 7: Add the `CallResolver` interface to `interfaces.go`**

Append to `internal/realtime/api/interfaces.go` (after `ProjectResolver`):

```go
// CallResolver maps a call_id to its tenant. Used by TopicRBAC.Allow
// to reject `call.events` subscriptions whose filter.CallID belongs to
// a different tenant than the subscriber's claims.
//
// Same not-found semantics as UserResolver / ProjectResolver — the
// realtime layer folds not-found into cross-tenant rejection so the
// wire response is identical and the client cannot probe call
// existence cross-tenant.
//
// Plan 11.4 Task 4 introduces the recording-side CallTenantLookup
// port; cmd/api adapts that to CallResolver in Plan 11.4 Task 7.
type CallResolver interface {
	// Get resolves call_id to its owning tenant. Returns an error
	// when the call is not resolvable.
	Get(ctx context.Context, callID string) (ResolvedTenant, error)
}
```

- [ ] **Step 8: Run the test to verify it passes**

Run: `go test -race -run 'TestCallResolver_InterfaceShape|TestLocatorCallResolver_Constant' ./internal/realtime/api/ -v`
Expected: PASS (both tests).

- [ ] **Step 9: Run the canonical pre-commit gate**

```bash
make ci
go test -race -count=1 ./internal/realtime/...
gofmt -l internal/realtime/api/
```
Expected: zero output; all green.

- [ ] **Step 10: Commit**

```bash
git add internal/realtime/api/interfaces.go \
        internal/realtime/api/interfaces_test.go \
        internal/realtime/api/locator.go \
        internal/realtime/api/locator_test.go
git commit -m "feat(realtime): add CallResolver port + LocatorCallResolver (Plan 11.4 Task 2)

Mirrors UserResolver/ProjectResolver. Implementations land in Tasks 3
(CachedCallResolver wrapper), 4 (recording-side lookup), 7 (cmd/api
adapter).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Realtime — `CachedCallResolver` (TTL + singleflight + Invalidate)

**Files:**
- Create: `internal/realtime/service/cached_call_resolver.go`
- Create: `internal/realtime/service/cached_call_resolver_test.go`

**Background:** mirrors `*service.CachedUserResolver` and `*service.CachedProjectResolver` (Plan 11.3 Task 2 + Plan 11.2 Task 3). Same 60s TTL, same `sync.Map` LRU, same `singleflight.DoChan` coalescing, same `context.WithoutCancel` ctx-detach pattern (Plan 11.2 Task 3 review IMPORTANT I-1), same `Invalidate(id)` composing `cache.Delete + group.Forget`. Lives in a separate file because the existing `resolver_cache.go` is already 287 lines and would balloon past 400 if a third copy were appended.

- [ ] **Step 1: Write the failing test — cache hit / miss / TTL expiry**

Create `internal/realtime/service/cached_call_resolver_test.go`:

```go
package service_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
)

// fakeCallResolver counts inner calls so cache-hit assertions can
// verify the wrapper's coalescing.
type fakeCallResolver struct {
	mu       sync.Mutex
	calls    atomic.Int64
	want     map[string]rtapi.ResolvedTenant
	errFor   map[string]error
	delay    time.Duration
}

func (f *fakeCallResolver) Get(ctx context.Context, callID string) (rtapi.ResolvedTenant, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return rtapi.ResolvedTenant{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errFor != nil {
		if err, ok := f.errFor[callID]; ok {
			return rtapi.ResolvedTenant{}, err
		}
	}
	return f.want[callID], nil
}

// TestCachedCallResolver_HitDoesNotCallInner verifies the cache hit
// path returns the cached entry without re-querying the inner resolver.
func TestCachedCallResolver_HitDoesNotCallInner(t *testing.T) {
	t.Parallel()

	inner := &fakeCallResolver{
		want: map[string]rtapi.ResolvedTenant{"call-1": {TenantID: "t-1"}},
	}
	c := service.NewCachedCallResolver(inner, 0)

	got, err := c.Get(t.Context(), "call-1")
	require.NoError(t, err)
	require.Equal(t, "t-1", got.TenantID)
	require.Equal(t, int64(1), inner.calls.Load())

	// Second Get within ttl returns the cached value without an
	// inner call.
	got, err = c.Get(t.Context(), "call-1")
	require.NoError(t, err)
	require.Equal(t, "t-1", got.TenantID)
	require.Equal(t, int64(1), inner.calls.Load(), "cache hit must not re-query inner")
}

// TestCachedCallResolver_TTLExpiry verifies that an expired entry
// triggers a re-fetch of the inner resolver.
func TestCachedCallResolver_TTLExpiry(t *testing.T) {
	t.Parallel()

	inner := &fakeCallResolver{
		want: map[string]rtapi.ResolvedTenant{"call-1": {TenantID: "t-1"}},
	}
	// 50ms ttl so the test isn't slow.
	c := service.NewCachedCallResolver(inner, 50*time.Millisecond)

	_, err := c.Get(t.Context(), "call-1")
	require.NoError(t, err)
	require.Equal(t, int64(1), inner.calls.Load())

	time.Sleep(60 * time.Millisecond)

	_, err = c.Get(t.Context(), "call-1")
	require.NoError(t, err)
	require.Equal(t, int64(2), inner.calls.Load(),
		"expired entry must re-query inner")
}

// TestCachedCallResolver_InnerError surfaces inner-resolver errors and
// does NOT cache the failure (matching CachedUserResolver/Project).
func TestCachedCallResolver_InnerError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("call not found")
	inner := &fakeCallResolver{
		want:   map[string]rtapi.ResolvedTenant{},
		errFor: map[string]error{"call-x": wantErr},
	}
	c := service.NewCachedCallResolver(inner, 0)

	_, err := c.Get(t.Context(), "call-x")
	require.ErrorIs(t, err, wantErr)
	require.Equal(t, int64(1), inner.calls.Load())

	// A second Get must re-query — no negative caching.
	_, err = c.Get(t.Context(), "call-x")
	require.ErrorIs(t, err, wantErr)
	require.Equal(t, int64(2), inner.calls.Load(),
		"errors must not be cached")
}

// TestCachedCallResolver_Invalidate drops the cached entry so the next
// Get re-queries the inner resolver.
func TestCachedCallResolver_Invalidate(t *testing.T) {
	t.Parallel()

	inner := &fakeCallResolver{
		want: map[string]rtapi.ResolvedTenant{"call-1": {TenantID: "t-1"}},
	}
	c := service.NewCachedCallResolver(inner, 0)

	_, err := c.Get(t.Context(), "call-1")
	require.NoError(t, err)
	require.Equal(t, int64(1), inner.calls.Load())

	c.Invalidate("call-1")

	_, err = c.Get(t.Context(), "call-1")
	require.NoError(t, err)
	require.Equal(t, int64(2), inner.calls.Load(),
		"Invalidate must drop the cached entry")
}

// TestCachedCallResolver_Invalidate_Idempotent — Invalidate on a
// missing key is a no-op.
func TestCachedCallResolver_Invalidate_Idempotent(t *testing.T) {
	t.Parallel()

	inner := &fakeCallResolver{want: map[string]rtapi.ResolvedTenant{}}
	c := service.NewCachedCallResolver(inner, 0)

	assert.NotPanics(t, func() { c.Invalidate("never-cached") })
}

// TestCachedCallResolver_LeaderCtxCancelDoesNotPoisonDuplicates is the
// ctx-bleed regression guard. A leader whose ctx cancels MUST NOT
// poison concurrent waiters joining the in-flight singleflight call.
// See Plan 11.2 Task 3 review IMPORTANT I-1.
func TestCachedCallResolver_LeaderCtxCancelDoesNotPoisonDuplicates(t *testing.T) {
	t.Parallel()

	inner := &fakeCallResolver{
		want:  map[string]rtapi.ResolvedTenant{"call-1": {TenantID: "t-1"}},
		delay: 100 * time.Millisecond,
	}
	c := service.NewCachedCallResolver(inner, 0)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	dupCtx := context.Background()

	leaderResult := make(chan error, 1)
	dupResult := make(chan struct {
		v   rtapi.ResolvedTenant
		err error
	}, 1)

	go func() {
		_, err := c.Get(leaderCtx, "call-1")
		leaderResult <- err
	}()
	// Let leader register its singleflight key.
	time.Sleep(20 * time.Millisecond)
	go func() {
		v, err := c.Get(dupCtx, "call-1")
		dupResult <- struct {
			v   rtapi.ResolvedTenant
			err error
		}{v, err}
	}()

	// Cancel the leader's ctx — the duplicate must still receive the
	// real result via the detached inner ctx.
	time.Sleep(20 * time.Millisecond)
	cancelLeader()

	leaderErr := <-leaderResult
	dup := <-dupResult

	require.ErrorIs(t, leaderErr, context.Canceled, "leader sees its own ctx-cancel")
	require.NoError(t, dup.err, "duplicate must NOT inherit leader's ctx-cancel")
	require.Equal(t, "t-1", dup.v.TenantID)
}

// TestCachedCallResolver_NewWithNilInnerPanics is the wiring guard.
func TestCachedCallResolver_NewWithNilInnerPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { _ = service.NewCachedCallResolver(nil, 0) })
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race -run 'TestCachedCallResolver' ./internal/realtime/service/ -v`
Expected: FAIL — `service.NewCachedCallResolver` undefined.

- [ ] **Step 3: Implement `CachedCallResolver`**

Create `internal/realtime/service/cached_call_resolver.go`:

```go
// cached_call_resolver.go is the call-id mirror of CachedUserResolver
// + CachedProjectResolver — same TTL + sync.Map + singleflight +
// ctx-detached closure pattern. Lives in its own file because
// resolver_cache.go is already 287 lines and a third copy would push
// past 400. The semantics are identical; see resolver_cache.go for the
// in-depth concurrency commentary.
//
// Plan 11.4 Task 3.
package service

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// CachedCallResolver wraps a rtapi.CallResolver with a 60s sync.Map
// cache + a singleflight.Group for concurrent-miss coalescing.
//
// Zero-value not safe — callers must use NewCachedCallResolver. nil
// inner panics at construction time so the wiring bug surfaces at
// boot rather than first subscribe.
type CachedCallResolver struct {
	inner rtapi.CallResolver
	ttl   time.Duration

	cache sync.Map // callID string → *cachedResolverEntry (defined in resolver_cache.go)
	group singleflight.Group
}

// NewCachedCallResolver wires a CachedCallResolver. ttl ≤ 0 falls back
// to defaultResolverTTL (60s). nil inner panics — wiring bug surfaces
// at boot, not first subscribe. See resolver_cache.go::NewCachedUserResolver
// for the full design rationale.
func NewCachedCallResolver(inner rtapi.CallResolver, ttl time.Duration) *CachedCallResolver {
	if inner == nil {
		panic("service.NewCachedCallResolver: inner must be non-nil")
	}
	if ttl <= 0 {
		ttl = defaultResolverTTL
	}
	return &CachedCallResolver{
		inner: inner,
		ttl:   ttl,
	}
}

// Get resolves callID via the cache, coalescing concurrent misses via
// singleflight. ctx propagates to the inner resolver and the
// singleflight Do call so a cancelled subscribe doesn't block on a
// slow DB; the inner closure runs against a detached ctx so a leader
// whose subscribe cancels does NOT poison concurrent duplicate waiters
// (Plan 11.2 Task 3 review IMPORTANT I-1).
func (c *CachedCallResolver) Get(ctx context.Context, callID string) (rtapi.ResolvedTenant, error) {
	if v, ok := c.cache.Load(callID); ok {
		entry, ok2 := v.(*cachedResolverEntry)
		if !ok2 {
			panic("service: CachedCallResolver cache contains unexpected type")
		}
		if time.Now().Before(entry.expiresAt) {
			return entry.tenant, nil
		}
	}
	ch := c.group.DoChan(callID, func() (any, error) {
		inner, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			resolverInnerTimeout,
		)
		defer cancel()
		got, err := c.inner.Get(inner, callID)
		if err != nil {
			return rtapi.ResolvedTenant{}, err
		}
		entry := &cachedResolverEntry{
			tenant:    got,
			expiresAt: time.Now().Add(c.ttl),
		}
		c.cache.Store(callID, entry)
		return got, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return rtapi.ResolvedTenant{}, res.Err
		}
		tenant, ok := res.Val.(rtapi.ResolvedTenant)
		if !ok {
			panic("service: CachedCallResolver singleflight returned unexpected type")
		}
		return tenant, nil
	case <-ctx.Done():
		c.group.Forget(callID)
		return rtapi.ResolvedTenant{}, ctx.Err()
	}
}

// Invalidate drops the cached entry for callID. Idempotent — no error
// if the key was never cached. Calls singleflight.Forget so any
// in-flight inner call (the leader) is uncached for future joiners —
// they re-query rather than inheriting the leader's (possibly stale)
// result. Used by the events-package cache invalidator (Plan 11.4
// Task 6) to drop entries on tenant.<t>.recording.call.deleted events.
//
// Concurrency: see CachedUserResolver.Invalidate for the full
// concurrency contract.
func (c *CachedCallResolver) Invalidate(callID string) {
	c.cache.Delete(callID)
	c.group.Forget(callID)
}

// Compile-time interface check. Mirrors the pattern at the bottom of
// resolver_cache.go.
var _ rtapi.CallResolver = (*CachedCallResolver)(nil)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race -run 'TestCachedCallResolver' ./internal/realtime/service/ -v`
Expected: PASS — all 7 sub-tests.

Run the full realtime/service package: `go test -race -count=1 ./internal/realtime/service/...`
Expected: PASS (no regressions in CachedUserResolver / CachedProjectResolver tests).

- [ ] **Step 5: Run the canonical pre-commit gate**

```bash
make ci
go test -race -count=1 ./internal/realtime/...
gofmt -l internal/realtime/service/
```
Expected: zero output; all green.

- [ ] **Step 6: Commit**

```bash
git add internal/realtime/service/cached_call_resolver.go \
        internal/realtime/service/cached_call_resolver_test.go
git commit -m "feat(realtime): CachedCallResolver — 60s TTL + singleflight (Plan 11.4 Task 3)

Mirrors CachedUserResolver / CachedProjectResolver (Plan 11.2 Task 3 +
Plan 11.3 Task 2). Same Invalidate() semantics so the cache invalidator
(Plan 11.4 Task 6) can wire .Invalidate as the call-side eviction callback.
Lives in a separate file because resolver_cache.go is already 287 lines.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Recording — `CallTenantLookup` port (BypassRLS call_id → tenant_id)

**Files:**
- Create: `internal/recording/api/lookup.go`
- Create: `internal/recording/store/lookup.go`
- Create: `internal/recording/store/lookup_pg_test.go` (with `//go:build integration`)

**Background:** the existing `RecordingService.Get(ctx, tenantID, callID)` requires the tenant at the boundary; it cannot serve as a `call_id → tenant_id` resolver. We add a tiny new port `recording.api.CallTenantLookup` (one method) and back it with a BypassRLS SELECT — the recording lifecycle worker already uses BypassRLS for cross-tenant sweeps (Plan 12.4), so the pattern is established and `tenancy_admin` already has SELECT on `call_recordings` (migration 000011).

- [ ] **Step 1: Write the failing test — interface conformance**

Create `internal/recording/api/lookup_test.go`:

```go
package api_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	rapi "github.com/sociopulse/platform/internal/recording/api"
)

// fakeCallTenantLookup is a compile-time conformance probe.
type fakeCallTenantLookup struct{}

func (fakeCallTenantLookup) LookupTenant(_ context.Context, _ uuid.UUID) (uuid.UUID, error) {
	return uuid.Nil, nil
}

var _ rapi.CallTenantLookup = fakeCallTenantLookup{}

// TestCallTenantLookup_InterfaceShape — runtime no-op, real assertion
// is the compile-time `var _` above.
func TestCallTenantLookup_InterfaceShape(t *testing.T) {
	t.Parallel()
	var _ rapi.CallTenantLookup = fakeCallTenantLookup{}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -race -run 'TestCallTenantLookup_InterfaceShape' ./internal/recording/api/ -v`
Expected: FAIL — `rapi.CallTenantLookup` undefined.

- [ ] **Step 3: Add the port to `internal/recording/api/lookup.go`**

```go
// Package api — lookup.go declares the narrow CallTenantLookup port the
// realtime CallResolver consumes. The port is intentionally separate
// from RecordingService because:
//
//   - RecordingService.Get requires (tenantID, callID) at the boundary;
//     CallTenantLookup takes only callID and resolves the tenant via
//     BypassRLS — the cross-tenant case the realtime resolver needs.
//   - Keeps the new lookup off the public RecordingService surface so
//     existing consumers (gRPC commit pipeline, HTTP playback, retention
//     worker) need no changes.
//
// Plan 11.4 Task 4.
package api

import (
	"context"

	"github.com/google/uuid"
)

// CallTenantLookup resolves a call_id to its owning tenant via a
// BypassRLS SELECT against call_recordings. Used by cmd/api's
// callResolverAdapter (Plan 11.4 Task 7) to populate the realtime
// CallResolver port.
//
// Implementations MUST return a wrapped sentinel ErrCallNotFound (the
// service-level sentinel — internal/recording/service.ErrCallNotFound)
// when the call_id has no row in call_recordings. The realtime layer
// folds not-found into ErrCrossTenantSubscribe so the wire response
// is identical and clients cannot probe call existence cross-tenant.
type CallTenantLookup interface {
	// LookupTenant resolves call_id to its owning tenant. ctx-aware so
	// the realtime layer can bound the lookup under the subscribe
	// deadline (5s inner timeout via CachedCallResolver).
	LookupTenant(ctx context.Context, callID uuid.UUID) (tenantID uuid.UUID, err error)
}
```

- [ ] **Step 4: Run the interface test to verify it passes**

Run: `go test -race -run 'TestCallTenantLookup_InterfaceShape' ./internal/recording/api/ -v`
Expected: PASS.

- [ ] **Step 5: Write the failing integration test — store impl**

Create `internal/recording/store/lookup_pg_test.go`:

```go
//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/recording/store"
	"github.com/sociopulse/platform/pkg/postgres"
)

// TestPostgresStore_LookupTenant_Found verifies a BypassRLS SELECT
// returns the tenant for a call_id whose recording row exists.
func TestPostgresStore_LookupTenant_Found(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	pool := newPoolForTest(t) // existing helper from store/main_test.go
	defer pool.Close()

	tenantID := uuid.New()
	callID := uuid.New()

	insertCallAndRecording(t, ctx, pool, tenantID, callID) // existing helper

	s := store.NewPostgresStore(pool)

	got, err := s.LookupTenant(ctx, callID)
	require.NoError(t, err)
	assert.Equal(t, tenantID, got)
}

// TestPostgresStore_LookupTenant_NotFound verifies the sentinel error
// for a call_id with no matching row.
func TestPostgresStore_LookupTenant_NotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	pool := newPoolForTest(t)
	defer pool.Close()

	s := store.NewPostgresStore(pool)

	_, err := s.LookupTenant(ctx, uuid.New())
	require.Error(t, err)
	require.True(t, errors.Is(err, store.ErrCallNotFound),
		"missing call_id must return ErrCallNotFound")
}

// TestPostgresStore_LookupTenant_BypassRLS_CrossTenant verifies that
// the lookup works regardless of the caller's RLS-active tenant —
// this is the property the realtime CallResolver needs.
func TestPostgresStore_LookupTenant_BypassRLS_CrossTenant(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	pool := newPoolForTest(t)
	defer pool.Close()

	tenantA := uuid.New()
	tenantB := uuid.New()
	callA := uuid.New()

	insertCallAndRecording(t, ctx, pool, tenantA, callA)

	// Caller's ambient tenant is B (would normally hide tenant A's row);
	// BypassRLS inside LookupTenant must still resolve.
	require.NoError(t, pool.WithTenant(ctx, tenantB, func(_ postgres.Tx) error {
		s := store.NewPostgresStore(pool)
		got, err := s.LookupTenant(ctx, callA)
		require.NoError(t, err)
		assert.Equal(t, tenantA, got)
		return nil
	}))
}
```

The `newPoolForTest` and `insertCallAndRecording` helpers live in `internal/recording/store/main_test.go` and `lifecycle_pg_test.go` per the Plan 12.4 patterns; reuse verbatim.

- [ ] **Step 6: Run the failing integration test to verify it fails**

Run: `go test -race -tags=integration -run 'TestPostgresStore_LookupTenant' ./internal/recording/store/ -v`
Expected: FAIL — `store.NewPostgresStore.LookupTenant` undefined.

- [ ] **Step 7: Implement the lookup**

Create `internal/recording/store/lookup.go`:

```go
// lookup.go provides the call_id → tenant_id BypassRLS read for the
// realtime CallResolver (Plan 11.4 Task 7). Lives in a separate file
// from postgres.go (per-tenant CRUD) and lifecycle.go (Plan 12.4
// retention sweeps) because the use case is distinct: a single
// cross-tenant pinpoint read keyed only by call_id.
//
// Plan 11.4 Task 4.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sociopulse/platform/pkg/postgres"
)

// LookupTenant returns the tenant_id for the row with the given
// call_id. Implements internal/recording/api.CallTenantLookup.
//
// Runs inside pool.BypassRLS — the use case is a cross-tenant resolver
// where the caller does not yet know the tenant. tenancy_admin has
// SELECT on call_recordings (migration 000011_admin_grants_call_recordings;
// Plan 12.4 Task 1 added it). This is verified at runtime: the SELECT
// would fail with permission-denied if the grant were missing.
//
// Returns ErrCallNotFound on no matching row.
func (s *PostgresStore) LookupTenant(ctx context.Context, callID uuid.UUID) (uuid.UUID, error) {
	if callID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("recording.store.LookupTenant: %w: nil callID", ErrInvalidArgument)
	}

	var tenantID uuid.UUID
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		const q = `SELECT tenant_id FROM call_recordings WHERE call_id = $1 LIMIT 1`
		row := tx.QueryRow(ctx, q, callID)
		switch err := row.Scan(&tenantID); {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrCallNotFound
		case err != nil:
			return fmt.Errorf("recording.store.LookupTenant: %w", err)
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	return tenantID, nil
}

// Compile-time interface check. The CallTenantLookup port lives in
// internal/recording/api; the import is one-way (store → api), so the
// assertion lives here near the implementation.
var _ rapiCallTenantLookup = (*PostgresStore)(nil)
```

If a `rapiCallTenantLookup` alias is awkward, a more idiomatic compile-time check is in an external test file (`lookup_pg_test.go` already imports both):

```go
// In lookup_pg_test.go (within the existing package store_test):
var _ rapi.CallTenantLookup = (*store.PostgresStore)(nil)
```

The implementer chooses whichever placement keeps the build clean. Either way the assertion guards against signature drift.

If `ErrInvalidArgument` doesn't already exist as a store-level sentinel, the implementer reuses `store.ErrInvalidInput` if present, OR defines `ErrInvalidArgument` once in `internal/recording/store/errors.go`. The patterns in `internal/recording/store/postgres.go` already define module-level sentinels (`ErrCallNotFound` is one); follow whichever convention the existing file uses.

- [ ] **Step 8: Run the integration test to verify it passes**

Ensure Docker is available locally (testcontainers requires it):

```bash
docker info >/dev/null 2>&1 || (echo "Docker required for integration tests" && exit 1)
go test -race -tags=integration -run 'TestPostgresStore_LookupTenant' ./internal/recording/store/ -v
```
Expected: PASS — all 3 subtests.

- [ ] **Step 9: Run the canonical pre-commit gate**

```bash
make ci
go test -race -count=1 ./internal/recording/...
go test -race -tags=integration -count=1 ./internal/recording/store/...
gofmt -l internal/recording/
```
Expected: zero output; all green.

- [ ] **Step 10: Commit**

```bash
git add internal/recording/api/lookup.go \
        internal/recording/api/lookup_test.go \
        internal/recording/store/lookup.go \
        internal/recording/store/lookup_pg_test.go
git commit -m "feat(recording): CallTenantLookup port (BypassRLS call_id→tenant_id) (Plan 11.4 Task 4)

New tiny port api.CallTenantLookup + PostgresStore.LookupTenant impl
backed by BypassRLS SELECT on call_recordings. Used by the realtime
CallResolver adapter (Plan 11.4 Task 7) to validate cross-tenant
TopicCallEvents subscriptions. tenancy_admin has SELECT on
call_recordings via migration 000011 (Plan 12.4 Task 1).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: Realtime — `TopicRBAC.Allow` uses `CallResolver` for `filter.CallID`

**Files:**
- Modify: `internal/realtime/service/rbac.go`
- Modify: `internal/realtime/service/rbac_test.go` (find the existing file by `ls internal/realtime/service/`; if rbac tests are split across `topic_rbac_test.go` or similar, follow the existing pattern)

**Background:** `TopicRBAC.checkCrossTenant` today validates `filter.OperatorID` and `filter.ProjectID`. Plan 11.4 adds the third dimension: `filter.CallID`. The check is identical in shape — when `r.callResolver != nil && filter.CallID != ""`, call `verifyTenant(ctx, r.callResolver.Get, filter.CallID, claims.TenantID, "call_id")`. The `selfOnly` short-circuit doesn't apply (TopicCallEvents is not selfOnly per `defaultTopicRules`).

- [ ] **Step 1: Write the failing test — cross-tenant call subscribe is rejected**

Append to the existing rbac test file (the existing file in `internal/realtime/service/` is reused). Mirror the existing project-side cross-tenant test:

```go
// TestTopicRBAC_AllowRejectsCrossTenantCallID verifies the new Plan 11.4
// CallResolver dimension. A call_id whose tenant differs from the
// subscriber's claims must be rejected with ErrCrossTenantSubscribe.
func TestTopicRBAC_AllowRejectsCrossTenantCallID(t *testing.T) {
	t.Parallel()

	calls := stubCallResolver(map[string]string{
		"call-other-tenant": "tenant-B",
	})

	rbac := service.NewTopicRBACWithCallResolver(nil, nil, calls)

	err := rbac.Allow(t.Context(),
		rtapi.Claims{TenantID: "tenant-A", UserID: "u-1", Roles: []string{"operator"}},
		rtapi.TopicCallEvents,
		rtapi.SubscriptionFilter{CallID: "call-other-tenant"},
	)
	require.Error(t, err)
	require.True(t, errors.Is(err, rtapi.ErrCrossTenantSubscribe),
		"cross-tenant call subscribe must be folded into ErrCrossTenantSubscribe")
}

// TestTopicRBAC_AllowAcceptsSameTenantCallID verifies the happy path.
func TestTopicRBAC_AllowAcceptsSameTenantCallID(t *testing.T) {
	t.Parallel()

	calls := stubCallResolver(map[string]string{
		"call-same-tenant": "tenant-A",
	})

	rbac := service.NewTopicRBACWithCallResolver(nil, nil, calls)

	err := rbac.Allow(t.Context(),
		rtapi.Claims{TenantID: "tenant-A", UserID: "u-1", Roles: []string{"operator"}},
		rtapi.TopicCallEvents,
		rtapi.SubscriptionFilter{CallID: "call-same-tenant"},
	)
	require.NoError(t, err)
}

// TestTopicRBAC_AllowFoldsCallResolverErrorIntoCrossTenant — a
// resolver-error path (not-found / DB error) must NOT distinguishably
// surface to the wire; the wire error is identical to a tenant
// mismatch so clients cannot probe call existence cross-tenant.
func TestTopicRBAC_AllowFoldsCallResolverErrorIntoCrossTenant(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("call lookup down")
	calls := errCallResolver(wantErr)

	rbac := service.NewTopicRBACWithCallResolver(nil, nil, calls)

	err := rbac.Allow(t.Context(),
		rtapi.Claims{TenantID: "tenant-A", UserID: "u-1", Roles: []string{"operator"}},
		rtapi.TopicCallEvents,
		rtapi.SubscriptionFilter{CallID: "call-x"},
	)
	require.Error(t, err)
	require.True(t, errors.Is(err, rtapi.ErrCrossTenantSubscribe),
		"resolver error must fold into cross-tenant for wire indistinguishability")
	// Ensure the inner error is NOT errors.Is-able through the fold —
	// the verifyTenant helper uses %s, not %w, for exactly this reason.
	require.False(t, errors.Is(err, wantErr),
		"inner resolver error must NOT be reachable via errors.Is past ErrCrossTenantSubscribe")
}

// stubCallResolver and errCallResolver are local test helpers — keep
// them inline at the bottom of the test file (or alongside the existing
// stubUserResolver / stubProjectResolver helpers if they live there).
type stubCallResolverImpl map[string]string

func stubCallResolver(tenantsByCallID map[string]string) stubCallResolverImpl {
	return stubCallResolverImpl(tenantsByCallID)
}

func (s stubCallResolverImpl) Get(_ context.Context, callID string) (rtapi.ResolvedTenant, error) {
	tid, ok := s[callID]
	if !ok {
		return rtapi.ResolvedTenant{}, fmt.Errorf("call %s not found", callID)
	}
	return rtapi.ResolvedTenant{TenantID: tid}, nil
}

type errCallResolverImpl struct{ err error }

func errCallResolver(err error) errCallResolverImpl { return errCallResolverImpl{err: err} }

func (e errCallResolverImpl) Get(_ context.Context, _ string) (rtapi.ResolvedTenant, error) {
	return rtapi.ResolvedTenant{}, e.err
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race -run 'TestTopicRBAC_Allow.*CallID|TestTopicRBAC_AllowFoldsCallResolverError' ./internal/realtime/service/ -v`
Expected: FAIL — `service.NewTopicRBACWithCallResolver` undefined.

- [ ] **Step 3: Extend `TopicRBAC` with the `callResolver` field + new constructor**

Edit `internal/realtime/service/rbac.go`. Modify the struct decl (line 32-36) to add the field:

```go
type TopicRBAC struct {
	rules           map[rtapi.Topic]topicRule
	userResolver    rtapi.UserResolver    // optional; nil = skip cross-tenant on OperatorID
	projectResolver rtapi.ProjectResolver // optional; nil = skip cross-tenant on ProjectID
	callResolver    rtapi.CallResolver    // optional; nil = skip cross-tenant on CallID (Plan 11.4)
}
```

Add a new constructor (just after `NewTopicRBACWithResolvers`, before `defaultTopicRules`):

```go
// NewTopicRBACWithCallResolver wires all three resolvers used for the
// cross-tenant filter check. Plan 11.4 Task 5 — extends
// NewTopicRBACWithResolvers with the call dimension. nil resolvers are
// allowed; the matching dimension simply skips the check.
//
// Production wiring (cmd/api + realtime.Module.Register) supplies all
// three; tests typically supply stubs for the dimension under test
// and nil for the others.
func NewTopicRBACWithCallResolver(
	users rtapi.UserResolver,
	projects rtapi.ProjectResolver,
	calls rtapi.CallResolver,
) *TopicRBAC {
	return &TopicRBAC{
		rules:           defaultTopicRules(),
		userResolver:    users,
		projectResolver: projects,
		callResolver:    calls,
	}
}
```

Modify `checkCrossTenant` (lines 181-196) to validate `filter.CallID`:

```go
// checkCrossTenant enforces the Plan 11.2 Task 4 cross-tenant filter
// check, extended in Plan 11.4 Task 5 to cover the call dimension.
// Resolvers are optional — a nil resolver skips its dimension
// (preserves the Plan 11 behaviour for tests + degraded boot).
//
// selfOnly+matching-userID short-circuit: when the rule is selfOnly
// and filter.OperatorID equals claims.UserID, the user-resolver call
// is skipped — claims.TenantID already established the tenant
// relationship via the auth handshake.
//
// CallID cross-tenant check: when callResolver is wired AND the
// filter has a CallID, the resolver must confirm the call's tenant
// matches the claim. Wire-side indistinguishability is preserved via
// verifyTenant's %s (not %w) error fold. Hub.Broadcast's tenant
// filter remains the upstream defence-in-depth — this check exists so
// the WS client gets an explicit ErrCrossTenantSubscribe rather than
// silent zero-event delivery.
func (r *TopicRBAC) checkCrossTenant(ctx context.Context, rule topicRule, claims rtapi.Claims, filter rtapi.SubscriptionFilter) error {
	if r.userResolver != nil && filter.OperatorID != "" {
		skipResolve := rule.selfOnly && filter.OperatorID == claims.UserID
		if !skipResolve {
			if err := verifyTenant(ctx, r.userResolver.Get, filter.OperatorID, claims.TenantID, "operator_id"); err != nil {
				return err
			}
		}
	}
	if r.projectResolver != nil && filter.ProjectID != "" {
		if err := verifyTenant(ctx, r.projectResolver.Get, filter.ProjectID, claims.TenantID, "project_id"); err != nil {
			return err
		}
	}
	if r.callResolver != nil && filter.CallID != "" {
		if err := verifyTenant(ctx, r.callResolver.Get, filter.CallID, claims.TenantID, "call_id"); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the new tests to verify they pass**

Run: `go test -race -run 'TestTopicRBAC' ./internal/realtime/service/ -v`
Expected: PASS — all new + existing tests.

- [ ] **Step 5: Run the canonical pre-commit gate**

```bash
make ci
go test -race -count=1 ./internal/realtime/...
gofmt -l internal/realtime/service/
```
Expected: zero output; all green.

- [ ] **Step 6: Commit**

```bash
git add internal/realtime/service/rbac.go \
        internal/realtime/service/rbac_test.go
# rbac_test.go path may differ — implementer adjusts as needed.
git commit -m "feat(realtime): TopicRBAC checks CallResolver for filter.CallID (Plan 11.4 Task 5)

Adds the third cross-tenant filter dimension. Resolver errors fold
into ErrCrossTenantSubscribe via %s (not %w) so clients cannot probe
call existence cross-tenant. NewTopicRBACWithCallResolver supersedes
NewTopicRBACWithResolvers for production wiring; the older constructor
is preserved for tests + degraded boot.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: Realtime — extend `*CacheInvalidator` with UserInvalidate + CallInvalidate (2 new subjects)

**Files:**
- Modify: `internal/realtime/events/cache_invalidator.go`
- Modify: `internal/realtime/events/cache_invalidator_test.go`
- Modify: `internal/realtime/events/metrics.go`

**Background:** Plan 11.3 Task 3 wired ONE subscription (`tenant.*.crm.project.status_changed`). Plan 11.4 adds two more — `tenant.*.recording.call.deleted` (event already published by the Plan 12.4 retention worker) and `tenant.*.auth.user.deleted` (newly published by Task 1 of this plan). The shape is identical: one Subscribe per subject, one parser per payload type, one metric tick on every dispatch outcome.

We rename the internal callback type from `projectInvalidateFn` to `entityInvalidateFn` since all three callbacks share the signature `func(idString)`. Decision 3 (locked above): `ProjectInvalidate` remains REQUIRED (panic on nil — preserves Plan 11.3 contract); `UserInvalidate` and `CallInvalidate` are OPTIONAL — nil skips that subject's Subscribe with an INFO log.

The metric becomes `realtime_cache_invalidations_total{subject, result}` with bounded label set: 3 subjects × {ok / parse_error / empty_id} = 9 cells. We rename the existing `empty_project_id` label value to `empty_id` (uniform across subjects) — this is a metric-name evolution; the implementer documents it in the inline comment + the Plan 11.4 close-out lessons file.

- [ ] **Step 1: Write the failing test — recording.call.deleted path**

Append to `internal/realtime/events/cache_invalidator_test.go`:

```go
// fakeCallInvalidator captures Invalidate calls.
type fakeCallInvalidator struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeCallInvalidator) Invalidate(callID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, callID)
}

func (f *fakeCallInvalidator) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// fakeUserInvalidator mirrors fakeProjectInvalidator for user_id.
type fakeUserInvalidator struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeUserInvalidator) Invalidate(userID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
}

func (f *fakeUserInvalidator) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// TestCacheInvalidator_RecordingCallDeletedTriggersCallInvalidate verifies
// that publishing tenant.<t>.recording.call.deleted routes the CallID
// to the call invalidator.
func TestCacheInvalidator_RecordingCallDeletedTriggersCallInvalidate(t *testing.T) {
	t.Parallel()

	bus := eventbus.NewEmbeddedJetStreamForTest(t)
	defer bus.Close()

	pTarget := &fakeProjectInvalidator{}
	cTarget := &fakeCallInvalidator{}
	reg := prometheus.NewRegistry()
	metrics := events.RegisterCacheInvalidatorMetrics(reg)

	inv := events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        bus.Subscriber(),
		ProjectInvalidate: pTarget.Invalidate,
		CallInvalidate:    cTarget.Invalidate,
		Metrics:           metrics,
		Logger:            zaptest.NewLogger(t),
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	require.NoError(t, inv.Start(ctx))
	t.Cleanup(inv.Stop)

	tenantID := uuid.New()
	callID := uuid.New()
	recordingID := uuid.New()
	payload, err := json.Marshal(rapi.RecordingCallDeletedEvent{
		RecordingID: recordingID,
		CallID:      callID,
		TenantID:    tenantID,
		DeletedAt:   time.Now().Unix(),
		Reason:      "retention",
	})
	require.NoError(t, err)

	require.NoError(t, bus.Publisher().Publish(ctx,
		rapi.SubjectRecordingCallDeletedFor(tenantID),
		payload,
	))

	require.Eventually(t, func() bool {
		return len(cTarget.Calls()) >= 1
	}, 2*time.Second, 10*time.Millisecond,
		"CallInvalidate must fire on recording.call.deleted")

	assert.Contains(t, cTarget.Calls(), callID.String())
	assert.Empty(t, pTarget.Calls(),
		"ProjectInvalidate must not fire on a recording event")
}

// TestCacheInvalidator_AuthUserDeletedTriggersUserInvalidate verifies
// that publishing tenant.<t>.auth.user.deleted routes the UserID to
// the user invalidator.
func TestCacheInvalidator_AuthUserDeletedTriggersUserInvalidate(t *testing.T) {
	t.Parallel()

	bus := eventbus.NewEmbeddedJetStreamForTest(t)
	defer bus.Close()

	uTarget := &fakeUserInvalidator{}
	pTarget := &fakeProjectInvalidator{}
	reg := prometheus.NewRegistry()
	metrics := events.RegisterCacheInvalidatorMetrics(reg)

	inv := events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        bus.Subscriber(),
		ProjectInvalidate: pTarget.Invalidate,
		UserInvalidate:    uTarget.Invalidate,
		Metrics:           metrics,
		Logger:            zaptest.NewLogger(t),
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	require.NoError(t, inv.Start(ctx))
	t.Cleanup(inv.Stop)

	tenantID := uuid.New()
	userID := uuid.New()
	payload, err := json.Marshal(authapi.UserDeletedEvent{
		UserID:    userID,
		TenantID:  tenantID,
		DeletedAt: time.Now().Unix(),
		Reason:    "archived",
	})
	require.NoError(t, err)

	require.NoError(t, bus.Publisher().Publish(ctx,
		authapi.SubjectUserDeletedFor(tenantID),
		payload,
	))

	require.Eventually(t, func() bool {
		return len(uTarget.Calls()) >= 1
	}, 2*time.Second, 10*time.Millisecond,
		"UserInvalidate must fire on auth.user.deleted")

	assert.Contains(t, uTarget.Calls(), userID.String())
}

// TestCacheInvalidator_NilUserInvalidate_SkipsAuthSubscription —
// degraded boot path: no user invalidator wired, so the auth
// subscription is NOT registered. Publishing the event must NOT panic
// and the project subscription must remain functional.
func TestCacheInvalidator_NilUserInvalidate_SkipsAuthSubscription(t *testing.T) {
	t.Parallel()

	bus := eventbus.NewEmbeddedJetStreamForTest(t)
	defer bus.Close()

	pTarget := &fakeProjectInvalidator{}
	reg := prometheus.NewRegistry()
	metrics := events.RegisterCacheInvalidatorMetrics(reg)

	inv := events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        bus.Subscriber(),
		ProjectInvalidate: pTarget.Invalidate,
		// UserInvalidate intentionally omitted (nil)
		// CallInvalidate intentionally omitted (nil)
		Metrics: metrics,
		Logger:  zaptest.NewLogger(t),
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	require.NoError(t, inv.Start(ctx))
	t.Cleanup(inv.Stop)

	// Project subscription still works.
	tenantID := uuid.New()
	projectID := uuid.New()
	payload, err := json.Marshal(crmapi.ProjectStatusChangedEvent{
		ProjectID: projectID,
		TenantID:  tenantID,
		OldStatus: crmapi.ProjectStatusActive,
		NewStatus: crmapi.ProjectStatusArchived,
		ChangedAt: time.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, bus.Publisher().Publish(ctx,
		crmapi.SubjectProjectStatusFor(tenantID),
		payload,
	))

	require.Eventually(t, func() bool {
		return len(pTarget.Calls()) >= 1
	}, 2*time.Second, 10*time.Millisecond)
}
```

Add the new imports at the top of the test file: `rapi "github.com/sociopulse/platform/internal/recording/api"`, `authapi "github.com/sociopulse/platform/internal/auth/api"`, `"slices"`. The existing test file already imports the rest.

- [ ] **Step 2: Run the new tests to verify they fail**

Run: `go test -race -run 'TestCacheInvalidator_(RecordingCallDeleted|AuthUserDeleted|NilUserInvalidate)' ./internal/realtime/events/ -v`
Expected: FAIL — `events.CacheInvalidatorConfig.UserInvalidate` and `.CallInvalidate` undefined.

- [ ] **Step 3: Extend `CacheInvalidatorConfig` + handler dispatch**

Edit `internal/realtime/events/cache_invalidator.go`:

(a) Rename type alias (after line 35):

```go
// entityInvalidateFn is the narrow callback signature *CacheInvalidator
// consumes for all three resolver dimensions (project / user / call).
// Production wiring binds (*service.Cached{Project,User,Call}Resolver).Invalidate;
// tests substitute fakes.
type entityInvalidateFn func(id string)

// projectInvalidateFn is preserved as an alias for the public
// CacheInvalidatorConfig field type — the field name is part of the
// stable Plan 11.3 surface; renaming would force every existing
// caller to change. New fields use the canonical entityInvalidateFn.
type projectInvalidateFn = entityInvalidateFn
```

(b) Extend the config struct (replace the existing `CacheInvalidatorConfig` block):

```go
type CacheInvalidatorConfig struct {
	Subscriber eventbus.Subscriber

	// ProjectInvalidate is the function called with every project_id
	// from a parsed crmapi.ProjectStatusChangedEvent. Required —
	// preserved from Plan 11.3 Task 3; nil panics at construction.
	ProjectInvalidate projectInvalidateFn

	// UserInvalidate is the function called with every user_id from
	// a parsed authapi.UserDeletedEvent. Optional — nil skips the
	// auth.user.deleted subscription with an INFO log (degraded boot
	// without auth wiring). Plan 11.4 Task 6.
	UserInvalidate entityInvalidateFn

	// CallInvalidate is the function called with every call_id from
	// a parsed rapi.RecordingCallDeletedEvent. Optional — nil skips
	// the recording.call.deleted subscription with an INFO log
	// (degraded boot without recording wiring). Plan 11.4 Task 6.
	CallInvalidate entityInvalidateFn

	Metrics    *CacheInvalidatorMetrics
	Logger     *zap.Logger
	QueueGroup string
}
```

(c) Add new subject constants (alongside `SubjectProjectStatus`):

```go
// SubjectUserDeleted is the wildcard subject for the auth.user.deleted
// subscription. Built from authapi.SubjectUserDeleted's
// "tenant.<t>.auth.user.deleted" template by replacing the tenant
// placeholder with NATS '*' single-token wildcard.
const SubjectUserDeleted = "tenant.*.auth.user.deleted"

// SubjectRecordingCallDeleted is the wildcard subject for the
// recording.call.deleted subscription. Built from
// rapi.SubjectRecordingCallDeleted's
// "tenant.<t>.recording.call.deleted" template.
const SubjectRecordingCallDeleted = "tenant.*.recording.call.deleted"
```

(d) Modify `Start` to register all wired subscriptions:

```go
func (c *CacheInvalidator) Start(ctx context.Context) error {
	if err := c.cfg.Subscriber.Subscribe(ctx, SubjectProjectStatus, c.cfg.QueueGroup, c.handleProject); err != nil {
		return fmt.Errorf("realtime/events: cache invalidator subscribe %q: %w", SubjectProjectStatus, err)
	}
	if c.cfg.UserInvalidate != nil {
		if err := c.cfg.Subscriber.Subscribe(ctx, SubjectUserDeleted, c.cfg.QueueGroup, c.handleUser); err != nil {
			return fmt.Errorf("realtime/events: cache invalidator subscribe %q: %w", SubjectUserDeleted, err)
		}
	} else {
		c.cfg.Logger.Info("realtime/events: cache invalidator: user subscription skipped (UserInvalidate nil)",
			zap.String("subject", SubjectUserDeleted),
		)
	}
	if c.cfg.CallInvalidate != nil {
		if err := c.cfg.Subscriber.Subscribe(ctx, SubjectRecordingCallDeleted, c.cfg.QueueGroup, c.handleCall); err != nil {
			return fmt.Errorf("realtime/events: cache invalidator subscribe %q: %w", SubjectRecordingCallDeleted, err)
		}
	} else {
		c.cfg.Logger.Info("realtime/events: cache invalidator: call subscription skipped (CallInvalidate nil)",
			zap.String("subject", SubjectRecordingCallDeleted),
		)
	}
	return nil
}
```

(e) Rename the existing `handle` to `handleProject` and add two new dispatchers:

```go
// handleProject is the per-message hook for tenant.*.crm.project.status_changed.
func (c *CacheInvalidator) handleProject(_ string, payload []byte) error {
	var ev crmapi.ProjectStatusChangedEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		c.cfg.Metrics.observe(SubjectProjectStatus, "parse_error")
		c.cfg.Logger.Debug("realtime/events: cache invalidator: drop malformed project payload",
			zap.Int("payload_bytes", len(payload)),
			zap.Error(err),
		)
		return nil
	}
	if ev.ProjectID == uuid.Nil {
		c.cfg.Metrics.observe(SubjectProjectStatus, "empty_id")
		return nil
	}
	c.cfg.ProjectInvalidate(ev.ProjectID.String())
	c.cfg.Metrics.observe(SubjectProjectStatus, "ok")
	return nil
}

// handleUser is the per-message hook for tenant.*.auth.user.deleted.
func (c *CacheInvalidator) handleUser(_ string, payload []byte) error {
	var ev authapi.UserDeletedEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		c.cfg.Metrics.observe(SubjectUserDeleted, "parse_error")
		c.cfg.Logger.Debug("realtime/events: cache invalidator: drop malformed user payload",
			zap.Int("payload_bytes", len(payload)),
			zap.Error(err),
		)
		return nil
	}
	if ev.UserID == uuid.Nil {
		c.cfg.Metrics.observe(SubjectUserDeleted, "empty_id")
		return nil
	}
	c.cfg.UserInvalidate(ev.UserID.String())
	c.cfg.Metrics.observe(SubjectUserDeleted, "ok")
	return nil
}

// handleCall is the per-message hook for tenant.*.recording.call.deleted.
func (c *CacheInvalidator) handleCall(_ string, payload []byte) error {
	var ev rapi.RecordingCallDeletedEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		c.cfg.Metrics.observe(SubjectRecordingCallDeleted, "parse_error")
		c.cfg.Logger.Debug("realtime/events: cache invalidator: drop malformed call payload",
			zap.Int("payload_bytes", len(payload)),
			zap.Error(err),
		)
		return nil
	}
	if ev.CallID == uuid.Nil {
		c.cfg.Metrics.observe(SubjectRecordingCallDeleted, "empty_id")
		return nil
	}
	c.cfg.CallInvalidate(ev.CallID.String())
	c.cfg.Metrics.observe(SubjectRecordingCallDeleted, "ok")
	return nil
}
```

Add the imports `authapi "github.com/sociopulse/platform/internal/auth/api"` and `rapi "github.com/sociopulse/platform/internal/recording/api"` at the top of the file.

(f) Update `internal/realtime/events/metrics.go` to make `observe` accept the new `subject` label. Replace the existing `CacheInvalidatorMetrics`:

```go
// CacheInvalidatorMetrics is the per-handler counter set surfaced
// on /metrics for *CacheInvalidator. Plan 11.4 Task 6 expanded the
// label set from {result} alone to {subject, result} — the same
// counter family covers all three subscription dimensions.
//
// Bounded label combinations:
//   - subject ∈ {SubjectProjectStatus, SubjectUserDeleted, SubjectRecordingCallDeleted}
//   - result ∈ {"ok", "parse_error", "empty_id"}
//
// 9 cells total. The "empty_project_id" label value used in Plan 11.3
// Task 3 is renamed to "empty_id" — uniform across subjects. Operators
// updating dashboards: query previously read by ProjectStatus only;
// after the bump query by subject="tenant.*.crm.project.status_changed".
type CacheInvalidatorMetrics struct {
	invalidations *prometheus.CounterVec
}

func RegisterCacheInvalidatorMetrics(reg prometheus.Registerer) *CacheInvalidatorMetrics {
	if reg == nil {
		panic("events.RegisterCacheInvalidatorMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests")
	}
	m := &CacheInvalidatorMetrics{
		invalidations: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "realtime_cache_invalidations_total",
				Help: "Number of resolver-cache invalidations dispatched, labelled by subject and outcome.",
			},
			[]string{"subject", "result"},
		),
	}
	reg.MustRegister(m.invalidations)
	return m
}

// observe ticks the (subject, result)-labelled counter. nil-safe.
func (m *CacheInvalidatorMetrics) observe(subject, result string) {
	if m == nil || m.invalidations == nil {
		return
	}
	m.invalidations.WithLabelValues(subject, result).Inc()
}
```

The existing Plan 11.3 Task 3 test that read the metric by `{result: "ok"}` alone needs updating to read by `{subject: "tenant.*.crm.project.status_changed", result: "ok"}`. The `counterValueFromGather` helper in the test file already takes a `map[string]string`; just add the subject key. This is a test-only update — the metric semantics are strictly more informative.

- [ ] **Step 4: Run all CacheInvalidator tests to verify they pass**

Run: `go test -race -run 'TestCacheInvalidator' ./internal/realtime/events/ -v`
Expected: PASS — all existing + new tests.

- [ ] **Step 5: Run the canonical pre-commit gate**

```bash
make ci
go test -race -count=1 ./internal/realtime/...
gofmt -l internal/realtime/events/
```
Expected: zero output; all green.

- [ ] **Step 6: Commit**

```bash
git add internal/realtime/events/cache_invalidator.go \
        internal/realtime/events/cache_invalidator_test.go \
        internal/realtime/events/metrics.go
git commit -m "feat(realtime/events): CacheInvalidator subscribes to user.deleted + recording.call.deleted (Plan 11.4 Task 6)

Adds optional UserInvalidate + CallInvalidate callbacks; the matching
subscriptions are skipped (with an INFO log) when the callback is nil
so degraded boot remains operable. Metric realtime_cache_invalidations_total
gains a 'subject' label; the previous 'empty_project_id' result value is
renamed to the uniform 'empty_id'.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: cmd/api wiring — `callResolverAdapter` + bind invalidate callbacks for all 3 cached resolvers

**Files:**
- Create: `cmd/api/recording_resolver.go`
- Create: `cmd/api/recording_resolver_test.go`
- Modify: `cmd/api/main.go` (call new `registerCallResolver` alongside existing `registerRealtimeResolvers`)
- Modify: `internal/realtime/module.go` (retain `cachedUsers`; resolve `LocatorCallResolver`; bind 3 Invalidate callbacks)

**Background:** the closing wiring task. The recording-side `CallTenantLookup` (Task 4) is wrapped by `callResolverAdapter` (this task) into the realtime-side `CallResolver` (Task 2), then registered under `LocatorCallResolver`. realtime.Module.Register resolves the locator entry, builds `*service.CachedCallResolver` (Task 3), constructs `TopicRBAC` via `service.NewTopicRBACWithCallResolver` (Task 5), and binds all three `*Cached*.Invalidate` method values onto the upgraded `CacheInvalidatorConfig` (Task 6).

- [ ] **Step 1: Write the failing test — adapter happy path + parse error**

Create `cmd/api/recording_resolver_test.go`:

```go
package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rapi "github.com/sociopulse/platform/internal/recording/api"
)

// fakeCallTenantLookup captures lookup calls.
type fakeCallTenantLookup struct {
	want   uuid.UUID
	err    error
	called bool
}

func (f *fakeCallTenantLookup) LookupTenant(_ context.Context, _ uuid.UUID) (uuid.UUID, error) {
	f.called = true
	if f.err != nil {
		return uuid.Nil, f.err
	}
	return f.want, nil
}

var _ rapi.CallTenantLookup = (*fakeCallTenantLookup)(nil)

// TestCallResolverAdapter_Get_HappyPath — well-formed UUID returns
// (tenant, nil).
func TestCallResolverAdapter_Get_HappyPath(t *testing.T) {
	t.Parallel()

	wantTenant := uuid.New()
	lookup := &fakeCallTenantLookup{want: wantTenant}
	a := newCallResolverAdapter(lookup)

	got, err := a.Get(t.Context(), uuid.New().String())
	require.NoError(t, err)
	assert.Equal(t, wantTenant.String(), got.TenantID)
	assert.True(t, lookup.called)
}

// TestCallResolverAdapter_Get_MalformedUUID — non-UUID string surfaces
// as a wrapped error (TopicRBAC folds into ErrCrossTenantSubscribe).
func TestCallResolverAdapter_Get_MalformedUUID(t *testing.T) {
	t.Parallel()

	lookup := &fakeCallTenantLookup{}
	a := newCallResolverAdapter(lookup)

	_, err := a.Get(t.Context(), "not-a-uuid")
	require.Error(t, err)
	assert.False(t, lookup.called, "lookup must not be called on malformed UUID")
}

// TestCallResolverAdapter_Get_LookupError — propagate the lookup error
// (TopicRBAC will fold into ErrCrossTenantSubscribe).
func TestCallResolverAdapter_Get_LookupError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("call not found")
	lookup := &fakeCallTenantLookup{err: wantErr}
	a := newCallResolverAdapter(lookup)

	_, err := a.Get(t.Context(), uuid.New().String())
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

// TestNewCallResolverAdapter_NilLookupPanics is the wiring guard.
func TestNewCallResolverAdapter_NilLookupPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { _ = newCallResolverAdapter(nil) })
}
```

- [ ] **Step 2: Run the failing tests**

Run: `go test -race -run 'TestCallResolverAdapter|TestNewCallResolverAdapter' ./cmd/api/ -v`
Expected: FAIL — `newCallResolverAdapter` undefined.

- [ ] **Step 3: Implement the adapter + register in `cmd/api/recording_resolver.go`**

```go
package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/modules"
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	rapi "github.com/sociopulse/platform/internal/recording/api"
)

// locatorRecordingCallTenantLookup mirrors the recording module's
// publication key for the new CallTenantLookup port. cmd/api looks up
// this key to build the realtime CallResolver adapter; recording.Module
// publishes under the same name.
const locatorRecordingCallTenantLookup = "recording.CallTenantLookup"

// callResolverAdapter projects rapi.CallTenantLookup onto
// rtapi.CallResolver. The wire-string call_id is parsed via uuid.Parse —
// a malformed UUID surfaces as a wrapped error that TopicRBAC.Allow
// folds into ErrCrossTenantSubscribe (security: client cannot probe
// call existence cross-tenant).
//
// Mirrors userResolverAdapter / projectResolverAdapter from realtime.go.
type callResolverAdapter struct {
	lookup rapi.CallTenantLookup
}

// newCallResolverAdapter wraps a rapi.CallTenantLookup. nil lookup
// panics — the wiring bug surfaces at cmd/api boot rather than first
// subscribe.
func newCallResolverAdapter(lookup rapi.CallTenantLookup) *callResolverAdapter {
	if lookup == nil {
		panic("cmd/api: newCallResolverAdapter: lookup must be non-nil")
	}
	return &callResolverAdapter{lookup: lookup}
}

// Get implements rtapi.CallResolver.
func (a *callResolverAdapter) Get(ctx context.Context, callID string) (rtapi.ResolvedTenant, error) {
	id, err := uuid.Parse(callID)
	if err != nil {
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: parse call_id %q: %w", callID, err)
	}
	tenantID, err := a.lookup.LookupTenant(ctx, id)
	if err != nil {
		return rtapi.ResolvedTenant{}, fmt.Errorf("cmd/api: lookup tenant for call %s: %w", id, err)
	}
	return rtapi.ResolvedTenant{TenantID: tenantID.String()}, nil
}

// Compile-time interface check.
var _ rtapi.CallResolver = (*callResolverAdapter)(nil)

// registerCallResolver looks up recording.CallTenantLookup in the
// locator and registers the rtapi.CallResolver adapter under
// LocatorCallResolver. Mirrors registerUserResolver /
// registerProjectResolver.
//
// Order matters: this MUST run AFTER recording.Module.Register (which
// publishes recording.CallTenantLookup) AND BEFORE realtime.Module.Register
// (which looks up rtapi.LocatorCallResolver). Missing-but-tolerated
// paths log INFO and skip the registration; type-mismatched entries
// log WARN and skip. Either way the boot does not abort.
func registerCallResolver(locator modules.ServiceLocator, logger *zap.Logger) {
	if locator == nil {
		logger.Info("realtime resolvers: locator missing, skipping call resolver registration")
		return
	}
	v, ok := locator.Lookup(locatorRecordingCallTenantLookup)
	if !ok {
		logger.Info("realtime resolvers: recording.CallTenantLookup missing; CallResolver disabled (degraded boot)")
		return
	}
	lookup, ok := v.(rapi.CallTenantLookup)
	if !ok {
		logger.Warn("realtime resolvers: recording.CallTenantLookup registered with wrong type; CallResolver disabled",
			zap.String("got_type", fmt.Sprintf("%T", v)),
		)
		return
	}
	locator.Register(rtapi.LocatorCallResolver, rtapi.CallResolver(newCallResolverAdapter(lookup)))
	logger.Info("realtime resolvers: CallResolver registered from recording.CallTenantLookup")
}
```

- [ ] **Step 4: Run the adapter tests to verify they pass**

Run: `go test -race -run 'TestCallResolverAdapter|TestNewCallResolverAdapter' ./cmd/api/ -v`
Expected: PASS — all 4 subtests.

- [ ] **Step 5: Publish the recording-side `CallTenantLookup` to the locator**

Edit `internal/recording/module.go`:

(a) Add a constant alongside `LocatorRecordingService`:

```go
// LocatorCallTenantLookup is the locator key for the BypassRLS
// call_id → tenant_id lookup port. cmd/api adapts this to
// rtapi.CallResolver. Plan 11.4 Task 7.
const LocatorCallTenantLookup = "recording.CallTenantLookup"
```

(b) Inside `Register`, after the store is built, add:

```go
// Plan 11.4 Task 7: publish the cross-tenant call_id → tenant_id
// lookup port. cmd/api adapts this to rtapi.CallResolver before
// realtime.Module.Register runs.
d.Locator.Register(LocatorCallTenantLookup, rapi.CallTenantLookup(rstore))
```

The `rstore` variable name follows the existing local naming inside `Register`; the implementer adapts to whatever the actual binding is named (likely `rstore` or `recordingStore`).

- [ ] **Step 6: Update `cmd/api/main.go` to call `registerCallResolver`**

Find the existing line `registerRealtimeResolvers(locator, logger)` in `cmd/api/main.go` and add a parallel call right after:

```go
registerRealtimeResolvers(locator, logger)
registerCallResolver(locator, logger) // Plan 11.4 Task 7
```

The call must remain BEFORE `realtime.Module.Register(...)`.

- [ ] **Step 7: Update `realtime.Module.Register` to retain `cachedUsers` + resolve `LocatorCallResolver` + bind 3 callbacks**

Edit `internal/realtime/module.go`:

(a) Add a `cachedUsers *service.CachedUserResolver` and `cachedCalls *service.CachedCallResolver` field on the `Module` struct (alongside the existing `cachedProjects`):

```go
type Module struct {
	// ... existing fields
	cachedUsers    *service.CachedUserResolver
	cachedProjects *service.CachedProjectResolver
	cachedCalls    *service.CachedCallResolver // Plan 11.4 Task 7
	// ... rest
}
```

(b) Inside `Register`, after the existing `cachedUsers := service.NewCachedUserResolver(...)` line (line 192), retain the field + resolve the call resolver:

```go
rawUsers, rawProjects := resolveResolversFromLocator(d.Locator, logger)
cachedUsers := service.NewCachedUserResolver(rawUsers, 0)
cachedProjects := service.NewCachedProjectResolver(rawProjects, 0)
m.cachedUsers = cachedUsers
m.cachedProjects = cachedProjects

// Plan 11.4 Task 7: resolve the optional CallResolver. Falls back to
// emptyCallResolver{} when cmd/api hasn't wired the recording adapter
// (degraded boot) — emptyCallResolver rejects every cross-tenant
// lookup, which is strictly safer than no check.
rawCalls := resolveCallResolverFromLocator(d.Locator, logger)
cachedCalls := service.NewCachedCallResolver(rawCalls, 0)
m.cachedCalls = cachedCalls

rbac := service.NewTopicRBACWithCallResolver(cachedUsers, cachedProjects, cachedCalls)
```

(c) Add `resolveCallResolverFromLocator` next to `resolveResolversFromLocator` (likely in `internal/realtime/resolver_lookup.go`):

```go
// resolveCallResolverFromLocator picks the production call resolver
// adapter when cmd/api has registered it; otherwise returns the
// emptyCallResolver fallback that rejects every cross-tenant lookup.
// Mirrors the behaviour of resolveResolversFromLocator. Plan 11.4 Task 7.
func resolveCallResolverFromLocator(locator modules.ServiceLocator, logger *zap.Logger) rtapi.CallResolver {
	if locator == nil {
		logger.Info("realtime: call resolver — locator missing, using empty fallback")
		return emptyCallResolver{}
	}
	v, ok := locator.Lookup(rtapi.LocatorCallResolver)
	if !ok {
		logger.Info("realtime: call resolver — locator entry missing, using empty fallback (degraded boot)")
		return emptyCallResolver{}
	}
	res, ok := v.(rtapi.CallResolver)
	if !ok {
		logger.Warn("realtime: call resolver — locator entry wrong type, using empty fallback",
			zap.String("got_type", fmt.Sprintf("%T", v)),
		)
		return emptyCallResolver{}
	}
	return res
}

// emptyCallResolver is the fallback that rejects every cross-tenant
// call lookup. Returned when cmd/api hasn't wired the recording
// adapter; preserves the security envelope by failing closed.
type emptyCallResolver struct{}

func (emptyCallResolver) Get(_ context.Context, _ string) (rtapi.ResolvedTenant, error) {
	return rtapi.ResolvedTenant{}, fmt.Errorf("call resolver not wired")
}

var _ rtapi.CallResolver = emptyCallResolver{}
```

(d) Bind all three Invalidate callbacks on the CacheInvalidator config (modify the existing `if d.Subscriber != nil` block):

```go
if d.Subscriber != nil {
	cacheInvalidatorMetrics := events.RegisterCacheInvalidatorMetrics(reg)
	invalidator := events.NewCacheInvalidator(events.CacheInvalidatorConfig{
		Subscriber:        d.Subscriber,
		ProjectInvalidate: cachedProjects.Invalidate,
		UserInvalidate:    cachedUsers.Invalidate, // Plan 11.4 Task 6
		CallInvalidate:    cachedCalls.Invalidate, // Plan 11.4 Task 6
		Metrics:           cacheInvalidatorMetrics,
		Logger:            logger.Named("cache_invalidator"),
	})
	if err := invalidator.Start(context.Background()); err != nil {
		logger.Warn("realtime: cache invalidator start failed; cross-tenant cache will rely on TTL-only invalidation",
			zap.Error(err),
		)
	} else {
		m.cacheInvalidator = invalidator
		logger.Info("realtime: cache invalidator started",
			zap.Strings("subjects", []string{
				events.SubjectProjectStatus,
				events.SubjectUserDeleted,
				events.SubjectRecordingCallDeleted,
			}),
		)
	}
}
```

- [ ] **Step 8: Run the full realtime + cmd/api test suite**

```bash
go test -race -count=1 ./internal/realtime/... ./cmd/api/...
```
Expected: PASS — zero failures, no goleak panics.

- [ ] **Step 9: Run the canonical pre-commit gate**

```bash
make ci
go test -race -count=1 ./...
gofmt -l .
make grep-time-after
```
Expected: zero output from `gofmt -l`; `make ci` exits 0; full repo race passes.

- [ ] **Step 10: Commit**

```bash
git add cmd/api/recording_resolver.go \
        cmd/api/recording_resolver_test.go \
        cmd/api/main.go \
        internal/realtime/module.go \
        internal/realtime/resolver_lookup.go \
        internal/recording/module.go
git commit -m "feat(cmd/api,realtime): wire callResolverAdapter + bind 3 cache invalidate callbacks (Plan 11.4 Task 7)

Closing wiring step. cmd/api adapts recording.CallTenantLookup →
rtapi.CallResolver and registers under LocatorCallResolver before
realtime.Module.Register runs. Module.Register builds CachedCallResolver
and binds .Invalidate (alongside cachedUsers / cachedProjects) on the
upgraded CacheInvalidator. emptyCallResolver fallback preserves
fail-closed semantics on degraded boot.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review (post-write)

Skimming the plan against the spec one more time:

**1. Spec coverage:**
- ✅ Auth `tenant.<t>.auth.user.deleted` event publication — Task 1
- ✅ `*CacheInvalidator` extension to subscribe to `recording.call.deleted` — Task 6
- ✅ `*CacheInvalidator` extension to subscribe to `auth.user.deleted` — Task 6
- ✅ `CallResolver` introduction — Tasks 2 (port) + 3 (cached wrapper) + 4 (recording lookup) + 5 (TopicRBAC) + 7 (cmd/api adapter)
- ✅ "Same shape as Plan 11.3 Task 3" — Task 6 explicitly mirrors

**2. Placeholder scan:** No "TODO", "fill in", "similar to Task N", or "appropriate error handling" patterns. Every step has either complete code or an exact command.

**3. Type consistency:**
- `CacheInvalidatorConfig.{ProjectInvalidate, UserInvalidate, CallInvalidate}` — all three carry the same `entityInvalidateFn = func(string)` signature (defined Task 6, used Tasks 6 + 7).
- `CallTenantLookup.LookupTenant(ctx, callID uuid.UUID) (uuid.UUID, error)` — defined Task 4, consumed Task 7 (`callResolverAdapter`).
- `CallResolver.Get(ctx, callID string) (ResolvedTenant, error)` — defined Task 2, used Tasks 3 / 5 / 7.
- `CachedCallResolver.{Get, Invalidate}` — defined Task 3, used Task 7.
- Locator keys: `LocatorCallResolver = "realtime.CallResolver"` (Task 2 / 7); `LocatorCallTenantLookup = "recording.CallTenantLookup"` (Task 4 / 7).

**4. Domain vocabulary:** every term is in `CONTEXT.md` (tenant, RLS, BypassRLS, outbox, NATS, JetStream, audit log, recording, call, operator, FSM-irrelevant for this plan).

**5. ADR conformance:** ADR-0011 (NATS over Kafka) — the new subjects use NATS; on-spec. ADR-0006 (PgBouncer transaction-mode) — `WithTenant` Tx is per-request; on-spec. ADR-0015 (TDD) — every task starts with a failing test, then minimal impl, then commit; on-spec.

**6. depguard:** None of the planned changes import `pgxpool` outside the allowed packages. Realtime never imports recording's service or store (only `recording/api`). Auth gains a new `pkg/outbox` import — explicitly allowed by `module-boundaries` (pkg/* is shared infrastructure). No banned-stdlib usage.

**7. CI risk:** The metric label set evolves from `{result}` to `{subject, result}` — existing dashboards (if any) will need a query update. The risk is observability-only, not behavioural. The Plan 11.3 Task 3 test that read `{result: "ok"}` is updated in Task 6 Step 3.

---

## Plan execution choice

After Phase 2 (this plan) is saved, the orchestrator (sociopulse-pipeline skill controller) proceeds to Phase 3 via `superpowers:subagent-driven-development`. Each task is dispatched to a fresh implementer subagent with:

- Full task text from this plan (NOT the plan path — controller passes the text inline)
- Path to `docs/references/plan-11-realtime.md` (pre-existing — Plan 11 reference)
- Explicit references to relevant `golang-*` skills (per `CLAUDE.md` workflow rule #3 — `golang-concurrency` for the Stop/Start path; `golang-testing` for testcontainers + goleak; `golang-error-handling` for the sentinel folds; `golang-modernize` proactively)
- TDD requirement: `superpowers:test-driven-development`, RED → GREEN → REFACTOR
- Tooling requirement: use `context7` to verify any unfamiliar library API; use `WebSearch` on unfamiliar errors
- Path-correction note: check `internal/<X>` vs `pkg/<X>` against actual scaffolding before writing imports
- Quality bar: `make ci` + `go test -race -count=1` + `gofmt -l` + `make grep-time-after` all green before reporting DONE

After EACH task: 2-stage review (spec compliance, then code quality), with re-review proportionality per `docs/architecture/09-agent-workflow-improvements.md` #7 (≤5 lines tickbox-fix → controller inline; 6–30 lines → spec-only re-review; >30 lines → full 2-stage re-review).

After ALL tasks pass review:
- Final implementation review over `origin/main...HEAD` (catches CI-blockers)
- Phase 4 close-out: `PROJECT_STATUS.md` milestone row + standing rules + `docs/references/plan-11-realtime.md` Production lessons + plan amendments + (if architectural) ADR + (if domain term) `CONTEXT.md` + module-graph events table + tag `v0.0.20-auth-user-deleted-callresolver` + CI watch (6 jobs).
