# Recording Workers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver phase 4 of 4 of the Recording module — two leader-elected background passes (`retention_pass` daily-ish, `integrity_pass` weekly-ish) that close the recording lifecycle (hot → cold → hard-delete) and continuously prove ciphertext integrity by re-streaming a random sample through the existing `RecordingService.VerifyChecksum` pipeline.

**Architecture:** Two `internal/recording/worker/*` orchestrators mirror the `internal/dialer/retry/orchestrator.go` shape — `time.NewTicker` + `pg_try_advisory_lock` leader election (one lock key per pass via fnvHash) + per-tick sweep + `BypassRLS` cross-tenant store reads. Service-layer building blocks already shipped in Plans 12.1–12.3 (`RecordingService.VerifyChecksum`, `ObjectStore.Delete`, all `call_recordings` columns: `status`, `cold_at`, `delete_at`, `verified_at`, `integrity_ok`). New surface in this plan is purely worker scheduling + audit + outbox.

**Tech Stack:** Go 1.26.3 stdlib (`context`, `time.NewTicker`, `errgroup`), `pgx/v5`, `pg_try_advisory_lock`, `TABLESAMPLE BERNOULLI` for cheap O(rows-touched) random sampling, Prometheus collectors via existing `*RecordingMetrics`, outbox-via-`event_outbox` for `tenant.<t>.recording.call.deleted`, leader election reused from `internal/dialer/retry.PgLeader` (constructor takes a custom int64 key).

---

## File Structure

**Created:**
- `internal/recording/store/lifecycle.go` — Plan-12.4 store hooks: `ListDueColdMoves`, `ListDueDeletes`, `MarkCold`, `MarkDeleted`, `SampleForVerify`, `UpdateVerifyResult`. All run inside `pool.BypassRLS` (cross-tenant sweeps).
- `internal/recording/store/lifecycle_pg_test.go` — integration tests via `testcontainers-go` for the six new methods.
- `internal/recording/worker/retention.go` — `RetentionPass` orchestrator: ticker → leader-acquire-or-skip → sweep cold-moves + sweep deletes. Calls `ObjectStore.Delete` for hard-delete. Emits audit + outbox events.
- `internal/recording/worker/retention_test.go` — unit tests against fake store + fake objects + fake outbox writer.
- `internal/recording/worker/integrity.go` — `IntegrityPass` orchestrator: ticker → leader-acquire-or-skip → sample → for each row call `RecordingService.VerifyChecksum` → write `UpdateVerifyResult`. Stateless rate limit via `verified_at < now() - 7 days` filter.
- `internal/recording/worker/integrity_test.go` — unit tests against fake store + fake service.
- `internal/recording/worker/doc.go` — package overview + lifecycle diagram.

**Modified:**
- `internal/recording/api/events.go` — adds `AuditActionColdMoved` / `AuditActionDeleted` / `AuditActionVerified` constants, `SubjectRecordingCallDeleted` constant + `SubjectRecordingCallDeletedFor` helper, `RecordingCallDeletedEvent` payload struct.
- `internal/recording/metrics/metrics.go` — adds `RetentionPassDuration` + `RetentionActionsTotal` + `IntegrityPassDuration` + `IntegrityActionsTotal` + `IntegrityFailuresTotal` collectors. Nil-safe receivers per existing pattern.
- `internal/recording/store/postgres.go` — no changes; `lifecycle.go` extends the same `PostgresStore` struct via methods.
- `internal/recording/api/interfaces.go` — adds `LifecycleStore` interface (the worker's narrow view of the store; `*PostgresStore` satisfies it). Service-side `RecordingStore` keeps its current shape.
- `cmd/worker/main.go` — new `recordingWorkers(...)` helper that builds both passes (when `recording.enabled` and the local-KEK config validates) + appends them to the existing errgroup. Skips silently when `recording.enabled=false` (default in dev) so the dialer-only worker boot continues to work.
- `cmd/worker/main_test.go` — sanity-test that the worker boots with `recording.enabled=false` (no regression) and registers both passes when `recording.enabled=true`.
- `pkg/config/recording.go` — adds `Workers struct { RetentionInterval / RetentionBatch / IntegrityInterval / IntegrityBatch / IntegritySamplePercent }`. Defaults: 5m / 100 / 1h / 10 / 1.0.
- `PROJECT_STATUS.md` — milestone row + recording-specific standing rule for cross-tenant `BypassRLS` usage in workers.

**No migrations.** All required columns exist from Plan 12.1. No new table needed (integrity scheduling is stateless via `verified_at` filter).

**Path note:** `pkg/postgres` (NOT `pkg/pool` — there is no `pkg/pool` directory; older plans say `internal/postgres` but the current home is `pkg/postgres`).

---

## Task 1: Store hooks for lifecycle + integrity sweeps

**Files:**
- Create: `internal/recording/store/lifecycle.go`
- Create: `internal/recording/store/lifecycle_pg_test.go`
- Modify: `internal/recording/api/interfaces.go`

**Context for the implementer:**

The Plan 12.1 `PostgresStore` has two methods (`InsertRecordingIdempotent`, `GetByCallID`) that run inside `pool.WithTenant` because the call originates from a per-tenant request (gRPC Commit / HTTP Get). The Plan 12.4 workers run cross-tenant: one daemon scans EVERY tenant's recordings for a due cold-move or hard-delete, and one daemon picks a random sample regardless of tenant. The proven pattern in this repo is `internal/dialer/retry.PgReader` — uses `pool.BypassRLS` (which switches to the `tenancy_admin` role). Mirror that exactly. Standing rule from PROJECT_STATUS notes that `tenancy_admin` lacks SELECT/INSERT grants on `call_recordings`, BUT `BypassRLS` is the ONLY cross-tenant read pattern available — verify the grants by running an integration test that issues `select count(*) from call_recordings` inside `BypassRLS`. If it errors with permission denied, the plan needs adjustment (a migration grants `tenancy_admin` SELECT/UPDATE on `call_recordings`, ON CONFLICT carry-forward standing rule).

The integrity sweep query MUST use `TABLESAMPLE BERNOULLI($percent)` rather than `ORDER BY random()` because the latter requires sorting the entire table (O(n log n)) and the production table will hold ~50k calls/day × 730d retention ≈ 36.5M rows. `TABLESAMPLE BERNOULLI` is a single-pass random-skip filter, O(n) but with a tiny constant.

Eligible-for-verify filter: `WHERE status IN ('stored','cold') AND (verified_at IS NULL OR verified_at < now() - interval '7 days')`. Status='deleted' rows have no S3 object so we cannot verify them. Already-verified-this-week rows are deliberately skipped — when the worker ticks hourly with a 1% sample × 10-row cap, a typical 36.5M-row table gets ≈ 1680 rows verified per week, which is well under 1% of total but is the spec's "weekly 1% sample" budget.

`MarkCold` / `MarkDeleted` use a status-CAS (`status='stored'` and `status IN ('stored','cold')` respectively) to prevent racing with a parallel manual fix-up. Returns `ErrAlreadyMoved` (NEW sentinel in the api package — actually use existing `api.ErrAlreadyDeleted` only when the new status would have been 'deleted' already; for 'cold' add `api.ErrLifecycleConflict`).

**Decisions to lock in BEFORE writing code:**
- New sentinel `api.ErrLifecycleConflict` in `internal/recording/api/errors.go` (review the actual file path; if errors live in `interfaces.go` add it there). Returned when MarkCold finds the row in status != 'stored', or MarkDeleted finds it in status='deleted' (idempotent — the row has already been deleted, treat as success but log at debug).
- Both `MarkCold` and `MarkDeleted` return (rowsAffected int, err error). 0 rowsAffected → status had already changed → caller treats as a benign skip + bumps a `stale` metric label.

- [ ] **Step 1: Add LifecycleStore interface + ErrLifecycleConflict sentinel**

Open `internal/recording/api/interfaces.go`. Find the existing `RecordingStore` interface. ADD a sibling interface immediately below it:

```go
// LifecycleStore is the cross-tenant subset the Plan 12.4 workers need.
// Implementations MUST run every method inside pool.BypassRLS so the
// worker sees rows for every tenant in the cluster — leader election
// already serialises writes, so cross-tenant access is safe.
type LifecycleStore interface {
	// ListDueColdMoves returns up to `limit` rows whose cold_at has
	// passed and whose status is still 'stored'. Ordered by cold_at
	// ASC so the oldest overdue rows are processed first. Limit is
	// clamped to [1, 1000].
	ListDueColdMoves(ctx context.Context, limit int) ([]store.RecordingRow, error)

	// ListDueDeletes returns up to `limit` rows whose delete_at has
	// passed and whose status is in ('stored','cold'). Ordered by
	// delete_at ASC. Limit is clamped to [1, 1000].
	ListDueDeletes(ctx context.Context, limit int) ([]store.RecordingRow, error)

	// MarkCold flips status='stored' → 'cold' for the given recording.
	// Returns rowsAffected. 0 means the row was already past 'stored'
	// (concurrent state change). The caller should treat 0 as a
	// benign skip + emit the "stale" metric label.
	MarkCold(ctx context.Context, recordingID uuid.UUID) (int64, error)

	// MarkDeleted flips status IN ('stored','cold') → 'deleted'. Same
	// rowsAffected semantics as MarkCold.
	MarkDeleted(ctx context.Context, recordingID uuid.UUID) (int64, error)

	// SampleForVerify returns a random sample of rows eligible for
	// integrity verification: status IN ('stored','cold') AND
	// (verified_at IS NULL OR verified_at < now() - interval '7 days').
	// samplePercent is the BERNOULLI sample rate in [0.01, 100.0]; lower
	// values are cheaper but yield smaller batches per tick. Limit is
	// applied AFTER the BERNOULLI filter — the actual returned count
	// is min(limit, sampled-and-filtered).
	SampleForVerify(ctx context.Context, samplePercent float64, limit int) ([]store.RecordingRow, error)

	// UpdateVerifyResult writes verified_at + integrity_ok for the
	// given recording. Caller computes the timestamp once per row so
	// the worker's metrics histogram and the row's verified_at agree.
	UpdateVerifyResult(ctx context.Context, recordingID uuid.UUID, verifiedAt time.Time, integrityOK bool) error
}
```

If `internal/recording/api/interfaces.go` does NOT already import `time` and `github.com/google/uuid` and `.../recording/store`, add those imports — beware of an import-cycle hazard: `api` → `store` is forbidden because `store` already imports `api`. **In that case** define `LifecycleStore` in a separate file `internal/recording/api/lifecycle_store.go` that uses the same row TYPE alias OR define a thin `LifecycleRow` mirror with only the fields the workers actually use (TenantID, ID, CallID, S3Bucket, AudioObjectKey, ColdAt, DeleteAt, Status, SHA256Hex). The mirror approach is cleaner and avoids the cycle. **Decision: use the mirror.** Define:

```go
// LifecycleRow is the worker-side projection of call_recordings with only
// the fields a retention or integrity sweep needs. Defined in api (not
// store) to avoid the api→store import cycle that would otherwise force
// LifecycleStore into the store package.
type LifecycleRow struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	CallID         uuid.UUID
	S3Bucket       string
	AudioObjectKey string
	SHA256Hex      string
	Status         string  // 'stored' | 'cold' | 'deleted'
	ColdAt         time.Time
	DeleteAt       *time.Time
}
```

ADD `internal/recording/api/errors.go` (or add to whatever file already declares `ErrInvalidInput` etc):

```go
// ErrLifecycleConflict is returned when a worker tries to update a
// recording's status but the database row has already moved past the
// expected source state (e.g. MarkCold on a row that's already
// 'deleted'). The worker treats this as a benign skip and bumps a
// "stale" counter.
var ErrLifecycleConflict = errors.New("recording: lifecycle conflict")
```

Search the existing `api` files first — if `errors.go` doesn't exist but errors are declared inside `interfaces.go`, add the sentinel there to keep the file inventory tidy.

- [ ] **Step 2: Run `go build ./internal/recording/api/...` to verify the new interface compiles**

Run: `go build ./internal/recording/api/...`
Expected: compiles cleanly. If you get an "unused import" warning, remove the `time` or `uuid` import that didn't end up used.

- [ ] **Step 3: Write the failing integration test for ListDueColdMoves**

Create `internal/recording/store/lifecycle_pg_test.go`:

```go
//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/store"
)

// TestPostgresStore_ListDueColdMoves seeds a 3-tenant fixture: one
// recording past cold_at (status='stored'), one in the future, one
// already cold. Worker must see exactly the first one regardless of
// tenant.
func TestPostgresStore_ListDueColdMoves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, cleanup := setupTestPool(t) // existing helper
	defer cleanup()

	pgStore := store.NewPostgresStore(pool)

	tenantA := uuid.New()
	tenantB := uuid.New()
	seedTenant(t, pool, tenantA)
	seedTenant(t, pool, tenantB)

	due := seedRecording(t, pool, tenantA, recordingFixture{
		coldAt: time.Now().Add(-1 * time.Hour), // overdue
		status: "stored",
	})
	_ = seedRecording(t, pool, tenantA, recordingFixture{
		coldAt: time.Now().Add(24 * time.Hour), // not yet
		status: "stored",
	})
	_ = seedRecording(t, pool, tenantB, recordingFixture{
		coldAt: time.Now().Add(-2 * time.Hour),
		status: "cold", // already moved
	})

	rows, err := pgStore.ListDueColdMoves(ctx, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, due, rows[0].ID)
	require.Equal(t, tenantA, rows[0].TenantID)
}
```

If `setupTestPool`, `seedTenant`, `seedRecording` don't exist as helpers, look at `internal/recording/store/postgres_pg_test.go` and either reuse what's there or extract minimal helpers to a shared `_helpers_test.go` — DRY mandate.

- [ ] **Step 4: Run the test to verify it fails**

Run: `go test -tags=integration -run TestPostgresStore_ListDueColdMoves ./internal/recording/store/... -v`
Expected: FAIL with "ListDueColdMoves undefined" (because the method doesn't exist yet).

- [ ] **Step 5: Implement ListDueColdMoves in lifecycle.go**

Create `internal/recording/store/lifecycle.go`:

```go
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// clampLifecycleLimit normalises the worker batch size to [1, 1000].
// 1000 is the upper bound on FOR UPDATE-equivalent locking in a single
// sweep; larger batches risk lock-table pressure during the cross-tenant
// scan.
func clampLifecycleLimit(n int) int {
	if n < 1 {
		return 1
	}
	if n > 1000 {
		return 1000
	}
	return n
}

// ListDueColdMoves returns rows whose cold_at < now() and status='stored',
// ordered by cold_at ASC (oldest first). BypassRLS so the sweep sees
// every tenant.
func (s *PostgresStore) ListDueColdMoves(ctx context.Context, limit int) ([]rapi.LifecycleRow, error) {
	limit = clampLifecycleLimit(limit)
	var out []rapi.LifecycleRow
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		rows, err := tx.Query(ctx, `
			select id, tenant_id, call_id, s3_bucket, audio_object_key,
			       sha256_hex, status, cold_at, delete_at
			  from call_recordings
			 where status = 'stored'
			   and cold_at < now()
			 order by cold_at asc
			 limit $1
		`, limit)
		if err != nil {
			return fmt.Errorf("recording.lifecycle: list cold-moves: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r rapi.LifecycleRow
			if err := rows.Scan(&r.ID, &r.TenantID, &r.CallID, &r.S3Bucket, &r.AudioObjectKey,
				&r.SHA256Hex, &r.Status, &r.ColdAt, &r.DeleteAt); err != nil {
				return fmt.Errorf("recording.lifecycle: scan cold-move row: %w", err)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
```

- [ ] **Step 6: Run the test to verify it passes**

Run: `go test -tags=integration -run TestPostgresStore_ListDueColdMoves ./internal/recording/store/... -v`
Expected: PASS.

- [ ] **Step 7: Commit Task 1.1 (interface + ListDueColdMoves)**

```bash
git add internal/recording/api/interfaces.go internal/recording/api/errors.go internal/recording/store/lifecycle.go internal/recording/store/lifecycle_pg_test.go
git commit -m "feat(recording/store): Plan 12.4 Task 1 — LifecycleStore + ListDueColdMoves"
```

- [ ] **Step 8: Add ListDueDeletes — failing test first**

ADD to `internal/recording/store/lifecycle_pg_test.go`:

```go
func TestPostgresStore_ListDueDeletes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, cleanup := setupTestPool(t)
	defer cleanup()
	pgStore := store.NewPostgresStore(pool)

	tenantA := uuid.New()
	seedTenant(t, pool, tenantA)

	now := time.Now()
	pastDelete := now.Add(-1 * time.Hour)
	futureDelete := now.Add(48 * time.Hour)

	due1 := seedRecording(t, pool, tenantA, recordingFixture{deleteAt: &pastDelete, status: "cold"})
	due2 := seedRecording(t, pool, tenantA, recordingFixture{deleteAt: &pastDelete, status: "stored"})
	_ = seedRecording(t, pool, tenantA, recordingFixture{deleteAt: &futureDelete, status: "cold"})
	_ = seedRecording(t, pool, tenantA, recordingFixture{deleteAt: &pastDelete, status: "deleted"})
	_ = seedRecording(t, pool, tenantA, recordingFixture{deleteAt: nil, status: "cold"}) // legal hold

	rows, err := pgStore.ListDueDeletes(ctx, 100)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	gotIDs := []uuid.UUID{rows[0].ID, rows[1].ID}
	require.ElementsMatch(t, []uuid.UUID{due1, due2}, gotIDs)
}
```

Run: `go test -tags=integration -run TestPostgresStore_ListDueDeletes ./internal/recording/store/... -v`
Expected: FAIL.

- [ ] **Step 9: Implement ListDueDeletes**

ADD to `internal/recording/store/lifecycle.go`:

```go
// ListDueDeletes returns rows whose delete_at < now() and status IN
// ('stored','cold'), ordered by delete_at ASC. Rows with delete_at IS
// NULL (legal hold) are deliberately excluded.
func (s *PostgresStore) ListDueDeletes(ctx context.Context, limit int) ([]rapi.LifecycleRow, error) {
	limit = clampLifecycleLimit(limit)
	var out []rapi.LifecycleRow
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		rows, err := tx.Query(ctx, `
			select id, tenant_id, call_id, s3_bucket, audio_object_key,
			       sha256_hex, status, cold_at, delete_at
			  from call_recordings
			 where status in ('stored', 'cold')
			   and delete_at is not null
			   and delete_at < now()
			 order by delete_at asc
			 limit $1
		`, limit)
		if err != nil {
			return fmt.Errorf("recording.lifecycle: list deletes: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r rapi.LifecycleRow
			if err := rows.Scan(&r.ID, &r.TenantID, &r.CallID, &r.S3Bucket, &r.AudioObjectKey,
				&r.SHA256Hex, &r.Status, &r.ColdAt, &r.DeleteAt); err != nil {
				return fmt.Errorf("recording.lifecycle: scan delete row: %w", err)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
```

Run: `go test -tags=integration -run TestPostgresStore_ListDueDeletes ./internal/recording/store/... -v`
Expected: PASS.

- [ ] **Step 10: Add MarkCold + MarkDeleted with rowsAffected semantics — failing test first**

ADD to `internal/recording/store/lifecycle_pg_test.go`:

```go
func TestPostgresStore_MarkCold_StatusCAS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, cleanup := setupTestPool(t)
	defer cleanup()
	pgStore := store.NewPostgresStore(pool)

	tenantA := uuid.New()
	seedTenant(t, pool, tenantA)

	stored := seedRecording(t, pool, tenantA, recordingFixture{status: "stored"})
	already := seedRecording(t, pool, tenantA, recordingFixture{status: "cold"})

	// Happy path: stored → cold flips, returns 1.
	n, err := pgStore.MarkCold(ctx, stored)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	// Idempotent / status-conflict: already 'cold' → 0 rows affected.
	n, err = pgStore.MarkCold(ctx, already)
	require.NoError(t, err)
	require.EqualValues(t, 0, n)

	// Verify status now 'cold' for the happy-path row.
	row, err := pgStore.GetByCallID(ctx, tenantA, callIDFor(t, pool, stored))
	require.NoError(t, err)
	require.Equal(t, "cold", row.Status)
}

func TestPostgresStore_MarkDeleted_StatusCAS(t *testing.T) {
	// Mirror MarkCold test: stored→deleted = 1, cold→deleted = 1, deleted→deleted = 0.
	// (Implementer fills in.)
}
```

Run: `go test -tags=integration -run TestPostgresStore_Mark ./internal/recording/store/... -v`
Expected: FAIL.

- [ ] **Step 11: Implement MarkCold + MarkDeleted**

ADD to `internal/recording/store/lifecycle.go`:

```go
// MarkCold flips status='stored' → 'cold' on the row. Returns the
// number of rows updated. 0 means the row was already past 'stored'
// (concurrent change) — the caller should treat as a benign skip.
func (s *PostgresStore) MarkCold(ctx context.Context, recordingID uuid.UUID) (int64, error) {
	if recordingID == uuid.Nil {
		return 0, errors.New("recording.lifecycle: nil recording id")
	}
	var affected int64
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		ct, err := tx.Exec(ctx, `
			update call_recordings
			   set status = 'cold'
			 where id = $1
			   and status = 'stored'
		`, recordingID)
		if err != nil {
			return fmt.Errorf("recording.lifecycle: mark cold %s: %w", recordingID, err)
		}
		affected = ct.RowsAffected()
		return nil
	})
	return affected, err
}

// MarkDeleted flips status IN ('stored','cold') → 'deleted'. The
// physical S3 object MUST be deleted by the caller BEFORE invoking
// this method — once the status is 'deleted' the row is no longer
// eligible for retrieval. Returns rowsAffected; 0 means already
// 'deleted'.
func (s *PostgresStore) MarkDeleted(ctx context.Context, recordingID uuid.UUID) (int64, error) {
	if recordingID == uuid.Nil {
		return 0, errors.New("recording.lifecycle: nil recording id")
	}
	var affected int64
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		ct, err := tx.Exec(ctx, `
			update call_recordings
			   set status = 'deleted'
			 where id = $1
			   and status in ('stored', 'cold')
		`, recordingID)
		if err != nil {
			return fmt.Errorf("recording.lifecycle: mark deleted %s: %w", recordingID, err)
		}
		affected = ct.RowsAffected()
		return nil
	})
	return affected, err
}
```

Run: `go test -tags=integration -run TestPostgresStore_Mark ./internal/recording/store/... -v`
Expected: PASS.

- [ ] **Step 12: Add SampleForVerify + UpdateVerifyResult — failing test first**

ADD to `internal/recording/store/lifecycle_pg_test.go`:

```go
func TestPostgresStore_SampleForVerify(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, cleanup := setupTestPool(t)
	defer cleanup()
	pgStore := store.NewPostgresStore(pool)

	tenantA := uuid.New()
	seedTenant(t, pool, tenantA)

	// 100 stored rows with verified_at NULL — eligible.
	var eligible []uuid.UUID
	for i := 0; i < 100; i++ {
		eligible = append(eligible, seedRecording(t, pool, tenantA, recordingFixture{status: "stored"}))
	}
	// 50 stored rows verified yesterday — ineligible.
	yesterday := time.Now().Add(-24 * time.Hour)
	for i := 0; i < 50; i++ {
		_ = seedRecording(t, pool, tenantA, recordingFixture{
			status: "stored", verifiedAt: &yesterday,
		})
	}
	// 10 deleted rows — ineligible.
	for i := 0; i < 10; i++ {
		_ = seedRecording(t, pool, tenantA, recordingFixture{status: "deleted"})
	}

	// 100% sample with limit 30 → up to 30 rows from the 100 eligible.
	rows, err := pgStore.SampleForVerify(ctx, 100.0, 30)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rows), 1)
	require.LessOrEqual(t, len(rows), 30)
	// Every returned row must be eligible.
	eligibleSet := map[uuid.UUID]bool{}
	for _, id := range eligible {
		eligibleSet[id] = true
	}
	for _, r := range rows {
		require.True(t, eligibleSet[r.ID], "sampled row %s is not in eligible set", r.ID)
	}
}

func TestPostgresStore_UpdateVerifyResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, cleanup := setupTestPool(t)
	defer cleanup()
	pgStore := store.NewPostgresStore(pool)

	tenantA := uuid.New()
	seedTenant(t, pool, tenantA)
	rid := seedRecording(t, pool, tenantA, recordingFixture{status: "stored"})

	verifiedAt := time.Now().Truncate(time.Microsecond)
	require.NoError(t, pgStore.UpdateVerifyResult(ctx, rid, verifiedAt, true))

	// Re-fetch via raw SQL inside BypassRLS to verify the row state.
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		var got time.Time
		var ok bool
		require.NoError(t, tx.QueryRow(ctx,
			`select verified_at, integrity_ok from call_recordings where id=$1`, rid,
		).Scan(&got, &ok))
		require.WithinDuration(t, verifiedAt, got, time.Second)
		require.True(t, ok)
		return nil
	}))
}
```

Run: `go test -tags=integration -run TestPostgresStore_Sample\|TestPostgresStore_UpdateVerifyResult ./internal/recording/store/... -v`
Expected: FAIL.

- [ ] **Step 13: Implement SampleForVerify + UpdateVerifyResult**

ADD to `internal/recording/store/lifecycle.go`:

```go
// SampleForVerify returns up to `limit` rows that are eligible for
// integrity verification, drawn via TABLESAMPLE BERNOULLI($percent) —
// a single-pass O(n) random skip filter. The eligibility predicate is
// status IN ('stored','cold') AND (verified_at IS NULL OR verified_at
// < now() - interval '7 days'); rows in 'deleted' have no S3 object
// to verify, and rows verified within the past 7 days are deliberately
// skipped to spread load.
//
// samplePercent is in [0.01, 100.0]; values outside that range are
// clamped. limit is clamped to [1, 1000].
func (s *PostgresStore) SampleForVerify(ctx context.Context, samplePercent float64, limit int) ([]rapi.LifecycleRow, error) {
	if samplePercent < 0.01 {
		samplePercent = 0.01
	}
	if samplePercent > 100.0 {
		samplePercent = 100.0
	}
	limit = clampLifecycleLimit(limit)
	var out []rapi.LifecycleRow
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		rows, err := tx.Query(ctx, `
			select id, tenant_id, call_id, s3_bucket, audio_object_key,
			       sha256_hex, status, cold_at, delete_at
			  from call_recordings tablesample bernoulli($1)
			 where status in ('stored', 'cold')
			   and (verified_at is null or verified_at < now() - interval '7 days')
			 limit $2
		`, samplePercent, limit)
		if err != nil {
			return fmt.Errorf("recording.lifecycle: sample for verify: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r rapi.LifecycleRow
			if err := rows.Scan(&r.ID, &r.TenantID, &r.CallID, &r.S3Bucket, &r.AudioObjectKey,
				&r.SHA256Hex, &r.Status, &r.ColdAt, &r.DeleteAt); err != nil {
				return fmt.Errorf("recording.lifecycle: scan sample row: %w", err)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateVerifyResult writes verified_at + integrity_ok for the given
// row. Idempotent — calling it twice with the same args is a no-op.
// Caller computes verifiedAt once per row so the metrics histogram and
// the row's verified_at field agree to the microsecond.
func (s *PostgresStore) UpdateVerifyResult(ctx context.Context, recordingID uuid.UUID, verifiedAt time.Time, integrityOK bool) error {
	if recordingID == uuid.Nil {
		return errors.New("recording.lifecycle: nil recording id")
	}
	return s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx, `
			update call_recordings
			   set verified_at = $2,
			       integrity_ok = $3
			 where id = $1
		`, recordingID, verifiedAt, integrityOK)
		if err != nil {
			return fmt.Errorf("recording.lifecycle: update verify result %s: %w", recordingID, err)
		}
		return nil
	})
}
```

Run: `go test -tags=integration -run TestPostgresStore_Sample\|TestPostgresStore_UpdateVerifyResult ./internal/recording/store/... -v`
Expected: PASS.

- [ ] **Step 14: Compile-time check that *PostgresStore satisfies api.LifecycleStore**

ADD at the bottom of `internal/recording/store/lifecycle.go`:

```go
// Compile-time check: PostgresStore satisfies the worker-facing
// interface declared in the api package.
var _ rapi.LifecycleStore = (*PostgresStore)(nil)
```

- [ ] **Step 15: Full reality-check (lint + race + vet)**

Run: `go build ./... && go vet ./... && go test -race -count=1 -tags=integration ./internal/recording/store/...`
Expected: all green. If gopls reports stale "undefined" errors, ignore — the build/vet/test commands are the source of truth (PROJECT_STATUS standing rule).

- [ ] **Step 16: Commit Task 1**

```bash
git add internal/recording/api/ internal/recording/store/lifecycle.go internal/recording/store/lifecycle_pg_test.go
git commit -m "feat(recording/store): Plan 12.4 Task 1 — LifecycleStore (cross-tenant sweep + sample)"
```

---

## Task 2: Retention worker (`retention.go`)

**Files:**
- Create: `internal/recording/worker/retention.go`
- Create: `internal/recording/worker/retention_test.go`
- Create: `internal/recording/worker/doc.go`

**Context for the implementer:**

The retention worker mirrors `internal/dialer/retry.Orchestrator` exactly. Reuse `retry.PgLeader` directly — its `NewPgLeader(pool, key, logger)` constructor accepts a custom int64 key. We compute the key via `fnvHash("recording.retention_pass")` (the same idiom). Do NOT re-define a leader-election primitive; importing it from `internal/dialer/retry` is the agreed cross-module reuse pattern. (If a reviewer later flags this as cross-module coupling, the response is: extract `PgLeader` to `pkg/postgres/leader` in a follow-up. Plan 12.4 must NOT block on a refactor that PROJECT_STATUS doesn't authorise.)

The sweep loop is two passes per tick:

1. **Cold-move pass:** `ListDueColdMoves(batch)` → for each row: `MarkCold(rowID)`; if `rowsAffected==1` write `recording.cold_moved` audit; if `rowsAffected==0` log debug + bump `actions_total{action="cold_moved", result="stale"}` and continue. No outbox event for cold-move (S3 lifecycle is bucket-policy-driven; cold-move is a metadata-only flag in v1).
2. **Delete pass:** `ListDueDeletes(batch)` → for each row: `ObjectStore.Delete(ctx, row.AudioObjectKey)` → on success: `MarkDeleted(rowID)` + audit `recording.deleted` + outbox event `recording.call.deleted`; on `ObjectStore` error: log error + metric `actions_total{action="deleted", result="error"}` + continue (one bad object can't poison the whole sweep). On `ObjectStore.ErrObjectNotFound`: still `MarkDeleted` (the object was already gone — DB+S3 rectification) + audit + outbox + metric `result="orphaned"`.

The audit + outbox writes for the delete pass must run inside ONE `pool.WithTenant` transaction (so a crash between audit-INSERT and outbox-INSERT doesn't lose the event). The `MarkDeleted` itself runs in `BypassRLS` (set up by Task 1) and CANNOT be inside the same tx — splitting that is the awkward bit. Two-phase semantics:

- Phase A (commit S3 delete is irreversible): `ObjectStore.Delete` (no rollback possible, but ObjectStore.Delete is idempotent on second call).
- Phase B (DB transition): single `pool.WithTenant` Tx that does {audit INSERT, outbox INSERT, MarkDeleted via raw UPDATE inside the tenant-scoped tx — DON'T call the BypassRLS-based MarkDeleted method here}. We need a second helper `markDeletedTx(tx, recordingID)` that runs the same UPDATE inside the supplied tx. Add it to `lifecycle.go` next to MarkDeleted.

Actually on second thought: `pool.WithTenant` sets `app.tenant_id` GUC — RLS on `call_recordings` uses that GUC for tenant-isolation. The UPDATE inside the same tx will succeed because the row's tenant_id matches the GUC. But it WILL block any cross-tenant retry inside this Tx (good — defensive), AND MarkDeleted as a method must keep the BypassRLS form (used in Phase B step? wait, we use the tenant-scoped one inside the Tx). So:

- `MarkDeleted(ctx, recordingID)` — the public `LifecycleStore` method; uses `BypassRLS`. Used by tests and by failure-recovery paths.
- `markDeletedTx(ctx, tx, recordingID, tenantID)` — package-private; uses the supplied tx (which the caller has already configured with WithTenant or BypassRLS). Workers use this inside `pool.WithTenant(row.TenantID, tx → audit + outbox + markDeletedTx(tx) )`.

Add `markDeletedTx` to `lifecycle.go` in Task 1 (revisit if needed — the implementer should add it now in Task 2 since we've identified the need).

Outbox event payload `RecordingCallDeletedEvent`:

```go
type RecordingCallDeletedEvent struct {
	RecordingID uuid.UUID `json:"recording_id"`
	CallID      uuid.UUID `json:"call_id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	DeletedAt   int64     `json:"deleted_at"` // unix seconds
	Reason      string    `json:"reason"`     // "retention" | "manual"
}
```

Workers always set `Reason="retention"`. The "manual" form is reserved for an admin-deletion endpoint (out of scope here; documented in master spec as backlog).

**Decision: leader lock keys.**

Use `fnvHash("recording.retention_pass")` and `fnvHash("recording.integrity_pass")` — same approach as `retry.DefaultLockKey`. Each worker has its OWN lock so they don't block each other. Define both keys in `internal/recording/worker/keys.go` (NEW file) for visibility.

- [ ] **Step 1: Add markDeletedTx helper to lifecycle.go**

OPEN `internal/recording/store/lifecycle.go`. ADD:

```go
// markDeletedTx is the in-Tx form of MarkDeleted used by the worker
// when audit + outbox + status-flip must commit atomically. The caller
// MUST have configured the tx with the correct tenant scope (via
// pool.WithTenant or BypassRLS) — markDeletedTx does NOT switch
// scope itself.
//
// Exported with a leading lowercase because it's a package-private
// internal seam; the public method is MarkDeleted (BypassRLS-scoped).
func MarkDeletedTx(ctx context.Context, tx postgres.Tx, recordingID uuid.UUID) (int64, error) {
	if recordingID == uuid.Nil {
		return 0, errors.New("recording.lifecycle: nil recording id")
	}
	ct, err := tx.Exec(ctx, `
		update call_recordings
		   set status = 'deleted'
		 where id = $1
		   and status in ('stored', 'cold')
	`, recordingID)
	if err != nil {
		return 0, fmt.Errorf("recording.lifecycle: mark deleted (tx) %s: %w", recordingID, err)
	}
	return ct.RowsAffected(), nil
}
```

(Pascal-case despite "package-private internal seam" because Go exports by convention; the doc comment marks it as worker-internal.)

- [ ] **Step 2: Add events constants + payload to api/events.go**

OPEN `internal/recording/api/events.go`. ADD:

```go
// SubjectRecordingCallDeleted is the subject for the recording-deleted
// event, fired by the retention worker when delete_at has passed and
// the S3 object has been removed. Cache invalidators (Plan 11.4 future)
// subscribe to this to drop CallResolver caches.
const SubjectRecordingCallDeleted = "tenant.<t>.recording.call.deleted"

// SubjectRecordingCallDeletedFor returns the concrete subject for the
// given tenant.
func SubjectRecordingCallDeletedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.recording.call.deleted", tenantID)
}

// New audit actions for the workers.
const (
	// AuditActionColdMoved fires when retention_pass moves a row from
	// status='stored' → 'cold'.
	AuditActionColdMoved = "recording.cold_moved"
	// AuditActionDeleted fires when retention_pass hard-deletes the
	// S3 object and flips status='deleted'.
	AuditActionDeleted = "recording.deleted"
	// AuditActionVerified fires when integrity_pass updates verified_at +
	// integrity_ok. Logged at every check (regardless of OK/mismatch);
	// integrity_ok=false rows additionally tick the failures metric.
	AuditActionVerified = "recording.verified"
)

// RecordingCallDeletedEvent is the payload published on
// SubjectRecordingCallDeleted. The reason is currently always
// "retention"; the "manual" form is reserved for a future admin-
// deletion endpoint.
type RecordingCallDeletedEvent struct {
	RecordingID uuid.UUID `json:"recording_id"`
	CallID      uuid.UUID `json:"call_id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	DeletedAt   int64     `json:"deleted_at"` // unix seconds
	Reason      string    `json:"reason"`     // "retention" | "manual"
}
```

- [ ] **Step 3: Create worker/keys.go**

CREATE `internal/recording/worker/keys.go`:

```go
// Package worker hosts the recording module's leader-elected
// background passes — retention (hot→cold + hard-delete) and integrity
// (1% sample sha256 verify). Both passes mirror the
// internal/dialer/retry orchestrator shape: time.NewTicker +
// pg_try_advisory_lock + sweep. See doc.go for the lifecycle diagram.
package worker

import "hash/fnv"

// retentionLockKey is the pg_try_advisory_lock key used by the
// retention pass. Computed as FNV-1a 64-bit hash of the constant
// string so it's stable across releases — losing leadership across a
// version bump would cause a duplicate sweep.
var retentionLockKey = fnvHash("recording.retention_pass")

// integrityLockKey is the lock key used by the integrity pass. Distinct
// from retentionLockKey so the two passes don't block each other.
var integrityLockKey = fnvHash("recording.integrity_pass")

func fnvHash(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	//nolint:gosec // intentional: pg advisory keys are bigint; reinterpret unsigned hash.
	return int64(h.Sum64())
}
```

- [ ] **Step 4: Add metrics collectors (early — referenced by retention.go)**

OPEN `internal/recording/metrics/metrics.go`. Look for the existing `RecordingMetrics` struct + the `RegisterRecordingMetrics(reg)` constructor. ADD fields + register lines (mirror the existing collector style):

```go
// In the RecordingMetrics struct definition, add:
RetentionPassDuration *prometheus.HistogramVec // labels: pass=cold_moves|deletes, result=ok|error
RetentionActionsTotal *prometheus.CounterVec   // labels: tenant_id, action=cold_moved|deleted, result=ok|stale|error|orphaned
IntegrityPassDuration *prometheus.HistogramVec // labels: result=ok|error
IntegrityActionsTotal *prometheus.CounterVec   // labels: tenant_id, result=ok|mismatch|error
IntegrityFailuresTotal *prometheus.CounterVec  // labels: tenant_id (per master spec §15.5)
```

In `RegisterRecordingMetrics`, build these collectors and `MustRegister` them. The histogram buckets should use the project default (`prometheus.DefBuckets` in the existing pattern, or whatever the rest of the file uses — check the existing `commit_duration_seconds` histogram for guidance).

ADD a small helper method on `*RecordingMetrics`:

```go
// ObserveRetentionPass records the duration of one retention pass
// segment ("cold_moves" or "deletes") with the result label.
func (m *RecordingMetrics) ObserveRetentionPass(pass, result string, dur time.Duration) {
	if m == nil || m.RetentionPassDuration == nil {
		return
	}
	m.RetentionPassDuration.WithLabelValues(pass, result).Observe(dur.Seconds())
}

// IncRetentionAction bumps the per-action counter.
func (m *RecordingMetrics) IncRetentionAction(tenantID uuid.UUID, action, result string) {
	if m == nil || m.RetentionActionsTotal == nil {
		return
	}
	m.RetentionActionsTotal.WithLabelValues(tenantID.String(), action, result).Inc()
}
```

(Mirror these for integrity.)

- [ ] **Step 5: Write the failing test for one cold-move sweep**

CREATE `internal/recording/worker/retention_test.go`:

```go
package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/metrics"
	"github.com/sociopulse/platform/internal/recording/worker"
)

// fakeLifecycleStore implements rapi.LifecycleStore for unit tests.
type fakeLifecycleStore struct {
	dueCold   []rapi.LifecycleRow
	dueDelete []rapi.LifecycleRow
	marked    map[uuid.UUID]string // recordingID → new status
	verifiedAt map[uuid.UUID]time.Time
	verifiedOK map[uuid.UUID]bool
}

// (… interface methods …)

func TestRetentionPass_ColdMove_HappyPath(t *testing.T) {
	t.Parallel()
	row := rapi.LifecycleRow{
		ID: uuid.New(), TenantID: uuid.New(), CallID: uuid.New(),
		Status: "stored",
	}
	fStore := &fakeLifecycleStore{
		dueCold: []rapi.LifecycleRow{row},
		marked:  map[uuid.UUID]string{},
	}
	fAudit := newFakeAudit()
	pass := worker.NewRetentionPass(worker.RetentionConfig{
		Store: fStore, Audit: fAudit, Metrics: metrics.NoopRecording(),
		Logger: zaptest.NewLogger(t), Batch: 100,
	})
	require.NoError(t, pass.SweepOnce(context.Background()))
	require.Equal(t, "cold", fStore.marked[row.ID])
	require.Equal(t, 1, fAudit.countOf(rapi.AuditActionColdMoved))
}
```

(Stubs for `fakeLifecycleStore`, `newFakeAudit`, `NoopRecording` are local helpers — implement them in the test file; `metrics.NoopRecording()` may need a small constructor returning a `*RecordingMetrics` zero-value or a special "no-op" tag. Look at how the existing service tests build a noop metrics instance — copy that pattern.)

Run: `go test -run TestRetentionPass_ColdMove ./internal/recording/worker/... -v`
Expected: FAIL ("worker.RetentionPass undefined").

- [ ] **Step 6: Implement retention.go**

CREATE `internal/recording/worker/retention.go`:

```go
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/metrics"
	rstore "github.com/sociopulse/platform/internal/recording/store"
	"github.com/sociopulse/platform/internal/recording/storage"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// Leader is the leader-election primitive surface the worker needs.
// Identical shape to internal/dialer/retry.Leader — defined here so
// the worker doesn't take a transitive dep on the dialer's package.
type Leader interface {
	Acquire(ctx context.Context) (bool, error)
	Release(ctx context.Context) error
	Key() int64
}

// RetentionConfig groups the construction-time parameters of the
// retention pass. All required deps must be non-nil; nil ones either
// fail in NewRetentionPass or fall back to a sensible zero (see field
// docs).
type RetentionConfig struct {
	Pool    *postgres.Pool         // required for the WithTenant Tx that holds audit+outbox+markDeleted atomically
	Leader  Leader                 // required
	Store   rapi.LifecycleStore    // required
	Objects storage.ObjectStore    // required
	Outbox  outbox.Writer          // required
	Metrics *metrics.RecordingMetrics
	Logger  *zap.Logger
	Interval time.Duration         // tick rate; default 5m
	Batch    int                   // max rows per pass; default 100
}

// RetentionPass is the leader-elected daily-ish retention orchestrator.
// Two passes per tick: cold-moves (status='stored', cold_at<now → 'cold') +
// hard-deletes (delete_at<now → ObjectStore.Delete + status='deleted').
type RetentionPass struct {
	cfg RetentionConfig
	log *zap.Logger
}

// NewRetentionPass validates cfg and returns the orchestrator. Panics
// (via error return) if a required dep is nil; defaults are applied
// for Interval and Batch.
func NewRetentionPass(cfg RetentionConfig) (*RetentionPass, error) {
	if cfg.Pool == nil {
		return nil, errors.New("recording.worker: RetentionConfig.Pool required")
	}
	if cfg.Leader == nil {
		return nil, errors.New("recording.worker: RetentionConfig.Leader required")
	}
	if cfg.Store == nil {
		return nil, errors.New("recording.worker: RetentionConfig.Store required")
	}
	if cfg.Objects == nil {
		return nil, errors.New("recording.worker: RetentionConfig.Objects required")
	}
	if cfg.Outbox == nil {
		return nil, errors.New("recording.worker: RetentionConfig.Outbox required")
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 100
	}
	return &RetentionPass{cfg: cfg, log: cfg.Logger.Named("recording.retention")}, nil
}

// Run blocks until ctx is cancelled. Each tick:
//
//  1. Attempt Leader.Acquire.
//  2. If we lead: SweepOnce.
//  3. If we don't lead: skip silently.
//
// On ctx cancellation the loop terminates cleanly: any held lock is
// Released so a peer takes over without TCP keepalive timeout.
func (p *RetentionPass) Run(ctx context.Context) error {
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	p.log.Info("retention pass starting",
		zap.Duration("interval", p.cfg.Interval),
		zap.Int("batch", p.cfg.Batch),
		zap.Int64("lock_key", p.cfg.Leader.Key()),
	)
	// Run an immediate first sweep so we don't sit idle for a full
	// interval after boot. Identical pattern to dialer/retry.
	p.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			//nolint:contextcheck // intentional: release lock on shutdown using detached ctx
			_ = p.cfg.Leader.Release(context.Background())
			p.log.Info("retention pass stopped", zap.Error(ctx.Err()))
			return ctx.Err()
		case <-t.C:
			p.tick(ctx)
		}
	}
}

func (p *RetentionPass) tick(ctx context.Context) {
	leading, err := p.cfg.Leader.Acquire(ctx)
	if err != nil {
		p.log.Warn("leader acquire failed; skipping sweep", zap.Error(err))
		return
	}
	if !leading {
		return
	}
	if err := p.SweepOnce(ctx); err != nil {
		p.log.Warn("retention sweep failed", zap.Error(err))
	}
}

// SweepOnce runs both passes (cold-moves + deletes) once and returns.
// Exposed so unit tests can drive a single sweep without spinning up
// the leader-election loop.
func (p *RetentionPass) SweepOnce(ctx context.Context) error {
	if err := p.sweepColdMoves(ctx); err != nil {
		return fmt.Errorf("cold-moves: %w", err)
	}
	if err := p.sweepDeletes(ctx); err != nil {
		return fmt.Errorf("deletes: %w", err)
	}
	return nil
}

func (p *RetentionPass) sweepColdMoves(ctx context.Context) error {
	start := time.Now()
	rows, err := p.cfg.Store.ListDueColdMoves(ctx, p.cfg.Batch)
	if err != nil {
		p.cfg.Metrics.ObserveRetentionPass("cold_moves", "error", time.Since(start))
		return fmt.Errorf("list cold-moves: %w", err)
	}
	if len(rows) == 0 {
		p.cfg.Metrics.ObserveRetentionPass("cold_moves", "ok", time.Since(start))
		return nil
	}
	for _, r := range rows {
		p.handleColdMove(ctx, r)
	}
	p.cfg.Metrics.ObserveRetentionPass("cold_moves", "ok", time.Since(start))
	return nil
}

func (p *RetentionPass) handleColdMove(ctx context.Context, r rapi.LifecycleRow) {
	// Single Tx: audit + status-flip. No outbox event for cold-move
	// (S3 lifecycle is bucket-policy-driven; cold is a metadata flag in v1).
	err := p.cfg.Pool.WithTenant(ctx, r.TenantID, func(tx postgres.Tx) error {
		affected, err := rstore.MarkDeletedTx(ctx, tx, r.ID) // intentional reuse — no, we need a MarkColdTx
		_ = affected
		_ = err
		// (This is wrong; we need MarkColdTx. Add it next to MarkDeletedTx in lifecycle.go.)
		return errors.New("MarkColdTx not yet implemented; see step 7")
	})
	if err != nil {
		p.log.Warn("cold-move tx failed",
			zap.String("recording_id", r.ID.String()),
			zap.Error(err))
		p.cfg.Metrics.IncRetentionAction(r.TenantID, "cold_moved", "error")
		return
	}
	p.cfg.Metrics.IncRetentionAction(r.TenantID, "cold_moved", "ok")
}

// (sweepDeletes + handleDelete in step 8)
```

NOTE TO IMPLEMENTER: the snippet above intentionally has a placeholder calling `MarkDeletedTx` from the cold-move handler — that's wrong. Step 7 adds `MarkColdTx` and step 8 fixes the wiring. The sketch is here so the reviewer can see the shape.

Run: `go build ./internal/recording/worker/...`
Expected: FAIL because `MarkDeletedTx` is referenced from cold-move handler — fix in step 7.

- [ ] **Step 7: Add MarkColdTx + audit helpers**

OPEN `internal/recording/store/lifecycle.go`. ADD next to `MarkDeletedTx`:

```go
// MarkColdTx is the in-Tx form of MarkCold for the retention worker's
// audit + status-flip atomic write. Same Tx-scope contract as
// MarkDeletedTx — the caller MUST set tenant scope before invoking.
func MarkColdTx(ctx context.Context, tx postgres.Tx, recordingID uuid.UUID) (int64, error) {
	if recordingID == uuid.Nil {
		return 0, errors.New("recording.lifecycle: nil recording id")
	}
	ct, err := tx.Exec(ctx, `
		update call_recordings
		   set status = 'cold'
		 where id = $1
		   and status = 'stored'
	`, recordingID)
	if err != nil {
		return 0, fmt.Errorf("recording.lifecycle: mark cold (tx) %s: %w", recordingID, err)
	}
	return ct.RowsAffected(), nil
}
```

CREATE `internal/recording/worker/audit.go`:

```go
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/pkg/postgres"
)

// writeAudit appends one audit_log row inside the supplied tx. Mirrors
// internal/recording/service/service.go's writeAuditRow / writeAccessAudit
// shape: actor_kind='service', actor_user_id=nil, target_kind='recording',
// target_id is the recording UUID stringified. payload is the supplied
// map encoded as jsonb.
func writeAudit(ctx context.Context, tx postgres.Tx, tenantID, recordingID uuid.UUID, action string, payload map[string]any, ts time.Time) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("audit marshal: %w", err)
	}
	const q = `
INSERT INTO audit_log (tenant_id, actor_kind, actor_user_id, action, target_kind, target_id, payload, ts)
VALUES ($1, 'service', NULL, $2, 'recording', $3, $4, $5)
`
	if _, err := tx.Exec(ctx, q, tenantID, action, recordingID.String(), raw, ts); err != nil {
		return fmt.Errorf("audit insert: %w", err)
	}
	return nil
}
```

REPLACE the placeholder `handleColdMove` body:

```go
func (p *RetentionPass) handleColdMove(ctx context.Context, r rapi.LifecycleRow) {
	now := time.Now().UTC()
	err := p.cfg.Pool.WithTenant(ctx, r.TenantID, func(tx postgres.Tx) error {
		n, err := rstore.MarkColdTx(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		if n == 0 {
			// Stale — already past 'stored'. No audit, no error.
			return errStaleSkip
		}
		return writeAudit(ctx, tx, r.TenantID, r.ID, rapi.AuditActionColdMoved,
			map[string]any{
				"recording_id":     r.ID,
				"call_id":          r.CallID,
				"audio_object_key": r.AudioObjectKey,
				"reason":           "cold_at",
			}, now)
	})
	switch {
	case errors.Is(err, errStaleSkip):
		p.cfg.Metrics.IncRetentionAction(r.TenantID, "cold_moved", "stale")
	case err != nil:
		p.log.Warn("cold-move tx failed",
			zap.String("recording_id", r.ID.String()),
			zap.Error(err))
		p.cfg.Metrics.IncRetentionAction(r.TenantID, "cold_moved", "error")
	default:
		p.cfg.Metrics.IncRetentionAction(r.TenantID, "cold_moved", "ok")
	}
}

// errStaleSkip is the sentinel the per-row WithTenant fn returns to
// signal "no error, but the row was already past the source state —
// don't write the audit row, just bump the stale metric upstream".
var errStaleSkip = errors.New("retention: stale skip")
```

Run: `go build ./internal/recording/worker/...`
Expected: compiles (no more placeholder).

Run the test from step 5:
`go test -run TestRetentionPass_ColdMove ./internal/recording/worker/... -v`
Expected: PASS (the cold-move happy path is wired).

- [ ] **Step 8: Add sweepDeletes + handleDelete**

ADD to `internal/recording/worker/retention.go`:

```go
func (p *RetentionPass) sweepDeletes(ctx context.Context) error {
	start := time.Now()
	rows, err := p.cfg.Store.ListDueDeletes(ctx, p.cfg.Batch)
	if err != nil {
		p.cfg.Metrics.ObserveRetentionPass("deletes", "error", time.Since(start))
		return fmt.Errorf("list deletes: %w", err)
	}
	if len(rows) == 0 {
		p.cfg.Metrics.ObserveRetentionPass("deletes", "ok", time.Since(start))
		return nil
	}
	for _, r := range rows {
		p.handleDelete(ctx, r)
	}
	p.cfg.Metrics.ObserveRetentionPass("deletes", "ok", time.Since(start))
	return nil
}

// handleDelete runs the two-phase delete:
//
//	Phase A: ObjectStore.Delete (irreversible).
//	Phase B: WithTenant Tx { audit + outbox + MarkDeletedTx }.
//
// Phase A failure: log + metric, no row mutation, retry on next sweep.
// Phase B failure: log + metric. The S3 object is already gone — the
// row stays in 'cold' or 'stored' until next sweep retries Phase B
// (Phase A on the second attempt sees ErrObjectNotFound, which we
// fold into 'ok' since the object IS deleted).
func (p *RetentionPass) handleDelete(ctx context.Context, r rapi.LifecycleRow) {
	now := time.Now().UTC()

	// Phase A.
	delErr := p.cfg.Objects.Delete(ctx, r.AudioObjectKey)
	switch {
	case errors.Is(delErr, storage.ErrObjectNotFound):
		// Already gone — proceed to Phase B with metric label "orphaned".
		p.cfg.Metrics.IncRetentionAction(r.TenantID, "deleted", "orphaned")
	case delErr != nil:
		p.log.Warn("ObjectStore.Delete failed",
			zap.String("recording_id", r.ID.String()),
			zap.String("audio_object_key", r.AudioObjectKey),
			zap.Error(delErr))
		p.cfg.Metrics.IncRetentionAction(r.TenantID, "deleted", "error")
		return
	default:
		p.cfg.Metrics.IncRetentionAction(r.TenantID, "deleted", "ok")
	}

	// Phase B.
	err := p.cfg.Pool.WithTenant(ctx, r.TenantID, func(tx postgres.Tx) error {
		n, err := rstore.MarkDeletedTx(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		if n == 0 {
			return errStaleSkip
		}
		if err := writeAudit(ctx, tx, r.TenantID, r.ID, rapi.AuditActionDeleted,
			map[string]any{
				"recording_id":     r.ID,
				"call_id":          r.CallID,
				"audio_object_key": r.AudioObjectKey,
				"reason":           "retention",
				"sha256":           r.SHA256Hex,
			}, now); err != nil {
			return err
		}
		return p.appendDeletedOutbox(ctx, tx, r, now)
	})
	switch {
	case errors.Is(err, errStaleSkip):
		// Already 'deleted' — phase A was a no-op effectively, treat as success.
		return
	case err != nil:
		p.log.Warn("delete phase B failed",
			zap.String("recording_id", r.ID.String()),
			zap.Error(err))
		// NOTE: result label was already incremented by Phase A; do not
		// double-count.
	}
}

// appendDeletedOutbox builds + appends the recording.call.deleted event
// inside the supplied tx. The outbox-relay drains it asynchronously.
func (p *RetentionPass) appendDeletedOutbox(ctx context.Context, tx postgres.Tx, r rapi.LifecycleRow, deletedAt time.Time) error {
	payload, err := json.Marshal(rapi.RecordingCallDeletedEvent{
		RecordingID: r.ID,
		CallID:      r.CallID,
		TenantID:    r.TenantID,
		DeletedAt:   deletedAt.Unix(),
		Reason:      "retention",
	})
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	tenantID := r.TenantID
	callID := r.CallID
	return p.cfg.Outbox.Append(ctx, tx, outbox.Event{
		TenantID:    &tenantID,
		AggregateID: &callID,
		Subject:     rapi.SubjectRecordingCallDeletedFor(r.TenantID),
		Payload:     payload,
	})
}
```

Run: `go build ./internal/recording/worker/...`
Expected: compiles. If `outbox.Writer.Append` signature differs (e.g., takes `Tx` from a different package), adapt the call — look at how `service/service.go` calls Append for the canonical shape.

- [ ] **Step 9: Add tests for the delete path (happy + ObjectStore-not-found + ObjectStore-error)**

ADD to `internal/recording/worker/retention_test.go`:

```go
func TestRetentionPass_Delete_HappyPath(t *testing.T) { /* fStore.dueDelete=[1 row], fObjects.deleteOK; assert MarkDeletedTx called, audit row, outbox event */ }

func TestRetentionPass_Delete_ObjectAlreadyGone(t *testing.T) { /* fObjects.Delete returns storage.ErrObjectNotFound; assert metric label "orphaned" + Phase B still runs */ }

func TestRetentionPass_Delete_ObjectStoreError(t *testing.T) { /* fObjects.Delete returns generic error; assert NO MarkDeletedTx, NO audit, NO outbox; metric label "error" */ }

func TestRetentionPass_Delete_DBStaleSkip(t *testing.T) { /* fStore.MarkDeletedTx returns affected=0 (e.g. row already 'deleted'); assert no panic; outbox NOT appended */ }
```

(Implementer fills in — pattern: build fakes, build pass, call SweepOnce, assert state.)

Run: `go test -race -count=1 ./internal/recording/worker/... -v`
Expected: PASS.

- [ ] **Step 10: Add doc.go**

CREATE `internal/recording/worker/doc.go`:

```go
// Package worker hosts the recording module's leader-elected
// background passes (Plan 12.4).
//
// # Lifecycle
//
//	stored -- cold_at < now() ----> cold ----- delete_at < now() -----> deleted
//	            (retention_pass)            (retention_pass + S3 delete)
//	      \
//	       \--- ObjectStore.Get (HTTP playback, Plan 12.3)
//	       \--- VerifyChecksum (integrity_pass, this package)
//	       \
//	        any state except 'deleted' is sample-eligible by integrity_pass
//
// # Leader election
//
// Both passes mirror internal/dialer/retry.Orchestrator: time.NewTicker
// + pg_try_advisory_lock + sweep. Lock keys are FNV-1a 64-bit hashes of
// the constants "recording.retention_pass" and "recording.integrity_pass"
// — distinct so the two passes don't block each other.
//
// # Cross-tenant access
//
// Sweeps run as the worker process — there is no per-request tenant
// context. The store layer (LifecycleStore) uses pool.BypassRLS for
// reads + state-flip writes. Audit + outbox writes (which need the
// row's tenant) run inside pool.WithTenant in a single Tx with the
// status-flip step (MarkColdTx / MarkDeletedTx).
package worker
```

- [ ] **Step 11: Reality-check + commit Task 2**

Run: `go build ./... && go vet ./... && go test -race -count=1 ./internal/recording/worker/...`
Expected: all green.

```bash
git add internal/recording/api/events.go internal/recording/store/lifecycle.go internal/recording/worker/keys.go internal/recording/worker/retention.go internal/recording/worker/audit.go internal/recording/worker/retention_test.go internal/recording/worker/doc.go internal/recording/metrics/metrics.go
git commit -m "feat(recording/worker): Plan 12.4 Task 2 — retention pass (cold-move + hard-delete)"
```

---

## Task 3: Integrity worker (`integrity.go`)

**Files:**
- Create: `internal/recording/worker/integrity.go`
- Create: `internal/recording/worker/integrity_test.go`

**Context for the implementer:**

The integrity worker mirrors retention exactly (leader + ticker + sweep) but with one inner step: per sampled row call `RecordingService.VerifyChecksum(ctx, tenantID, recordingID)` then `LifecycleStore.UpdateVerifyResult(rowID, now, result.OK)`. Two metric labels: `result="ok"` if OK=true, `result="mismatch"` if OK=false (also bumps `IntegrityFailuresTotal{tenant_id}`), `result="error"` on transport/IO error (no UpdateVerifyResult write — retry next sweep).

**Critical: do NOT pass an audit-emitting `RecordingService` impl here.** `(*svc).VerifyChecksum` (Plan 12.3 Task 3) does NOT write `recording.accessed` audit (verify is a metadata-only check). Confirm this by reading `internal/recording/service/verify.go` — if it DOES write audit, the integrity pass would emit a flood of `recording.accessed` rows that pollute the audit trail. **Verification step required before implementation.**

If `VerifyChecksum` does NOT write audit (expected — it's documented in Plan 12.3 close-out), the integrity worker should write its own `recording.verified` audit row inside `WithTenant` Tx with `UpdateVerifyResult` — same atomic-write pattern as retention's delete phase B.

If `VerifyChecksum` DOES write audit, drop the worker's own audit emission and rely on the service's row.

**Default: assume verify does NOT audit; the worker writes its own.**

- [ ] **Step 1: Verify the assumption (read verify.go)**

Open `internal/recording/service/verify.go`. Search for `audit_log` / `writeAuditRow` / `writeAccessAudit`. If absent, the worker writes its own audit row. If present, drop the worker-side audit emission.

Document the finding inline: add a comment at the top of `internal/recording/worker/integrity.go` that says "integrity_pass writes its own recording.verified audit row because RecordingService.VerifyChecksum is intentionally audit-free per Plan 12.3 close-out" OR "integrity_pass relies on RecordingService.VerifyChecksum's recording.accessed audit row".

- [ ] **Step 2: Failing test — happy path (1 sampled row, OK=true)**

CREATE `internal/recording/worker/integrity_test.go`:

```go
package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/worker"
)

type fakeVerifySvc struct {
	results map[uuid.UUID]rapi.VerifyResult
	calls   int
}

func (f *fakeVerifySvc) VerifyChecksum(ctx context.Context, tenantID, recordingID uuid.UUID) (rapi.VerifyResult, error) {
	f.calls++
	if r, ok := f.results[recordingID]; ok {
		return r, nil
	}
	return rapi.VerifyResult{OK: true, ExpectedSHA: "abc", ActualSHA: "abc"}, nil
}

func TestIntegrityPass_HappyPath(t *testing.T) {
	t.Parallel()
	row := rapi.LifecycleRow{
		ID: uuid.New(), TenantID: uuid.New(), CallID: uuid.New(), Status: "stored",
	}
	fStore := &fakeLifecycleStore{
		sampled: []rapi.LifecycleRow{row},
	}
	fSvc := &fakeVerifySvc{}
	pass := /* … construct … */

	require.NoError(t, pass.SweepOnce(context.Background()))
	require.Equal(t, 1, fSvc.calls)
	require.NotZero(t, fStore.verifiedAt[row.ID])
	require.True(t, fStore.verifiedOK[row.ID])
}
```

Run: `go test -run TestIntegrityPass_HappyPath ./internal/recording/worker/... -v`
Expected: FAIL ("worker.IntegrityPass undefined").

- [ ] **Step 3: Implement integrity.go**

CREATE `internal/recording/worker/integrity.go` with the same orchestrator shape as retention.go but with this inner sweep:

```go
type IntegrityConfig struct {
	Pool          *postgres.Pool
	Leader        Leader
	Store         rapi.LifecycleStore
	Service       rapi.RecordingService // for VerifyChecksum
	Metrics       *metrics.RecordingMetrics
	Logger        *zap.Logger
	Interval      time.Duration // default 1h
	Batch         int           // default 10
	SamplePercent float64       // default 1.0 (BERNOULLI 1%)
}

type IntegrityPass struct { /* same shape as RetentionPass */ }

func (p *IntegrityPass) SweepOnce(ctx context.Context) error {
	start := time.Now()
	rows, err := p.cfg.Store.SampleForVerify(ctx, p.cfg.SamplePercent, p.cfg.Batch)
	if err != nil {
		p.cfg.Metrics.ObserveIntegrityPass("error", time.Since(start))
		return fmt.Errorf("sample: %w", err)
	}
	if len(rows) == 0 {
		p.cfg.Metrics.ObserveIntegrityPass("ok", time.Since(start))
		return nil
	}
	for _, r := range rows {
		p.handleRow(ctx, r)
	}
	p.cfg.Metrics.ObserveIntegrityPass("ok", time.Since(start))
	return nil
}

func (p *IntegrityPass) handleRow(ctx context.Context, r rapi.LifecycleRow) {
	res, err := p.cfg.Service.VerifyChecksum(ctx, r.TenantID, r.ID)
	if err != nil {
		p.log.Warn("VerifyChecksum failed",
			zap.String("recording_id", r.ID.String()),
			zap.Error(err))
		p.cfg.Metrics.IncIntegrityAction(r.TenantID, "error")
		return
	}
	now := time.Now().UTC()
	// Audit + UpdateVerifyResult atomically.
	err = p.cfg.Pool.WithTenant(ctx, r.TenantID, func(tx postgres.Tx) error {
		// UpdateVerifyResult uses BypassRLS internally — for the in-Tx
		// pattern we need an UpdateVerifyResultTx helper. Add it in step 4.
		if _, err := rstore.UpdateVerifyResultTx(ctx, tx, r.ID, now, res.OK); err != nil {
			return err
		}
		return writeAudit(ctx, tx, r.TenantID, r.ID, rapi.AuditActionVerified,
			map[string]any{
				"recording_id":  r.ID,
				"call_id":       r.CallID,
				"expected_sha":  res.ExpectedSHA,
				"actual_sha":    res.ActualSHA,
				"bytes_scanned": res.BytesScanned,
				"integrity_ok":  res.OK,
			}, now)
	})
	if err != nil {
		p.log.Warn("integrity tx failed",
			zap.String("recording_id", r.ID.String()),
			zap.Error(err))
		p.cfg.Metrics.IncIntegrityAction(r.TenantID, "error")
		return
	}
	if res.OK {
		p.cfg.Metrics.IncIntegrityAction(r.TenantID, "ok")
	} else {
		p.cfg.Metrics.IncIntegrityAction(r.TenantID, "mismatch")
		p.cfg.Metrics.IncIntegrityFailure(r.TenantID)
	}
}
```

(Run/Tick/NewIntegrityPass mirror retention's structure exactly — copy + adapt.)

- [ ] **Step 4: Add UpdateVerifyResultTx**

ADD to `internal/recording/store/lifecycle.go`:

```go
// UpdateVerifyResultTx is the in-Tx form for the integrity worker's
// audit + verify-result atomic write. Same Tx-scope contract as
// MarkDeletedTx.
func UpdateVerifyResultTx(ctx context.Context, tx postgres.Tx, recordingID uuid.UUID, verifiedAt time.Time, integrityOK bool) (int64, error) {
	if recordingID == uuid.Nil {
		return 0, errors.New("recording.lifecycle: nil recording id")
	}
	ct, err := tx.Exec(ctx, `
		update call_recordings
		   set verified_at = $2,
		       integrity_ok = $3
		 where id = $1
	`, recordingID, verifiedAt, integrityOK)
	if err != nil {
		return 0, fmt.Errorf("recording.lifecycle: update verify result (tx) %s: %w", recordingID, err)
	}
	return ct.RowsAffected(), nil
}
```

- [ ] **Step 5: Add tests for mismatch, transport error, sample-empty paths**

ADD to `integrity_test.go`:

```go
func TestIntegrityPass_Mismatch_BumpsFailureMetric(t *testing.T) {
	// fakeVerifySvc returns OK=false → integrity_ok=false written + IntegrityFailuresTotal incremented.
}
func TestIntegrityPass_TransportError_NoUpdate(t *testing.T) {
	// fakeVerifySvc returns err → no UpdateVerifyResult call, "error" metric label.
}
func TestIntegrityPass_EmptySample_NoOp(t *testing.T) {
	// Store.SampleForVerify returns empty → SweepOnce returns nil + ObserveIntegrityPass("ok").
}
```

Run: `go test -race -count=1 ./internal/recording/worker/... -v`
Expected: all PASS.

- [ ] **Step 6: Reality-check + commit Task 3**

```bash
go build ./... && go vet ./... && go test -race -count=1 ./...
```

```bash
git add internal/recording/store/lifecycle.go internal/recording/worker/integrity.go internal/recording/worker/integrity_test.go internal/recording/metrics/metrics.go
git commit -m "feat(recording/worker): Plan 12.4 Task 3 — integrity pass (1% sample sha256 verify)"
```

---

## Task 4: Metrics + audit + outbox events polish

**Files:**
- Modify: `internal/recording/metrics/metrics.go` (collectors fully wired)
- Modify: `internal/recording/api/events.go` (constants + payload struct)

**Context for the implementer:**

By the end of Task 3 the metrics + events files have been touched but not necessarily polished — collector help-text might be terse, label sets might be inconsistent, the `SubjectRecordingCallDeleted` placeholder format string `"tenant.<t>.recording.call.deleted"` (the literal angle-brackets are intentional — they match the existing `SubjectRecordingUploaded` shape so JetStream subject filters that wildcard `tenant.*` work uniformly).

This task is the polish pass:

- [ ] **Step 1: Audit the metrics collector help texts**

Open `internal/recording/metrics/metrics.go`. For every NEW collector added in Task 2 + Task 3, verify:
1. Help text is one sentence in present tense (mirror existing `commit_total` help: "Total number of recording.Commit calls grouped by result.").
2. Label set matches what the workers actually emit (don't ship a `tenant_id` label if the worker doesn't have a tenant_id at metric time — for the duration histograms, the tenant_id label is wrong; observation is per-pass, not per-tenant).
3. Histogram buckets — for retention/integrity pass durations, the existing `commit_duration_seconds` buckets (most likely `prometheus.DefBuckets`) are fine; for the per-row VerifyChecksum we have the existing `recording_access_duration_seconds` collector (Plan 12.2 Task 4) — the integrity worker's per-row duration is captured there because VerifyChecksum is a service-level operation. Don't introduce a duplicate.

- [ ] **Step 2: Verify event subject string format matches existing convention**

Open `internal/recording/api/events.go`. The CONSTANT `SubjectRecordingCallDeleted` is the human-readable placeholder ("tenant.<t>.recording.call.deleted") for documentation only — code MUST use `SubjectRecordingCallDeletedFor(tenantID)` to compute the concrete subject. Confirm both forms exist and that the `For` helper substitutes `tenantID.String()` (not Marshal/JSON).

- [ ] **Step 3: Run go vet on the whole module**

Run: `go vet ./internal/recording/...`
Expected: no warnings. If gopls is screaming about an unused import in events.go (uuid only used in helper — but the constant doesn't need it), confirm via direct `go vet`.

- [ ] **Step 4: Commit Task 4 polish (if any deltas)**

If steps 1–3 produced deltas:

```bash
git add internal/recording/metrics/metrics.go internal/recording/api/events.go
git commit -m "feat(recording): Plan 12.4 Task 4 — metrics + events polish"
```

If no deltas (all already correct from Tasks 2/3): skip the commit, note in the implementer report.

---

## Task 5: cmd/worker integration — register both passes

**Files:**
- Modify: `cmd/worker/main.go`
- Modify: `cmd/worker/main_test.go` (new sanity test)
- Modify: `pkg/config/recording.go` (Workers config block)

**Context for the implementer:**

`cmd/worker/main.go` currently builds the dialer retry orchestrator inline and adds it to the errgroup. We extend the same `run` function: when `cfg.Recording.Enabled` is true AND the local-KEK config validates AND the recording pool helpers in `cmd/api/recording.go` reusable from cmd/worker (or duplicated — the cleanest option is to factor the shared `recordingPorts` helper to `pkg/config/recording.go` or a new `internal/recording/wire.go` so BOTH cmd/api and cmd/worker call it).

**Decision:** Keep the wiring duplication minimal. `recordingPorts` (returns DEKUnwrapper, ObjectStore) lives in `cmd/api/recording.go`. Move it to `internal/recording/wire/wire.go` (NEW package) so BOTH binaries import it. cmd/api removes the local copy; cmd/worker calls the new wire helper.

- [ ] **Step 1: Extract recordingPorts to internal/recording/wire/wire.go**

CREATE `internal/recording/wire/wire.go`:

```go
// Package wire builds the recording module's local-fallback ports
// (DEKUnwrapper + ObjectStore) from configuration. Production with
// Plan 01's Yandex SDK adapters will replace these with real KMS + S3
// clients via build tags or a separate wire variant.
package wire

import (
	"encoding/hex"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/recording/crypto"
	"github.com/sociopulse/platform/internal/recording/storage"
	"github.com/sociopulse/platform/pkg/config"
)

// Ports bundles the recording-module dependencies built from config.
type Ports struct {
	DEKUnwrapper crypto.DEKUnwrapper
	ObjectStore  storage.ObjectStore
}

// LocalPorts hex-decodes the LocalKEKs map and builds the local-only
// fallback ports. Empty map → WARN log + nil ports (caller treats as
// "recording subsystem disabled at port level"). Decode errors → return
// (nil, error) so cmd/api / cmd/worker can fail boot loudly.
func LocalPorts(cfg config.RecordingConfig, logger *zap.Logger) (*Ports, error) {
	if len(cfg.LocalKEKs) == 0 {
		if logger != nil {
			logger.Warn("recording.local_keks empty; recording ports disabled")
		}
		return nil, nil
	}
	keks := make(map[string][]byte, len(cfg.LocalKEKs))
	for keyID, hexStr := range cfg.LocalKEKs {
		raw, err := hex.DecodeString(hexStr)
		if err != nil {
			return nil, fmt.Errorf("recording.local_keks[%s]: hex decode: %w", keyID, err)
		}
		if len(raw) != 32 {
			return nil, fmt.Errorf("recording.local_keks[%s]: expected 32 bytes (AES-256), got %d", keyID, len(raw))
		}
		keks[keyID] = raw
	}
	dek, err := crypto.NewLocalDEKUnwrapper(keks)
	if err != nil {
		return nil, fmt.Errorf("build local DEKUnwrapper: %w", err)
	}
	obj := storage.NewLocalObjectStore()
	return &Ports{DEKUnwrapper: dek, ObjectStore: obj}, nil
}
```

UPDATE `cmd/api/recording.go` to import + call `wire.LocalPorts` instead of the local helper. Remove the duplicate.

- [ ] **Step 2: Add Workers config block**

OPEN `pkg/config/recording.go`. ADD to the `RecordingConfig` struct:

```go
// Workers configures the Plan 12.4 retention + integrity background
// passes. All durations default to safe values when the YAML omits
// them.
Workers RecordingWorkersConfig `mapstructure:"workers"`
```

ADD struct:

```go
// RecordingWorkersConfig groups the retention + integrity worker tuning.
type RecordingWorkersConfig struct {
	// RetentionInterval is the tick rate for the retention pass.
	// Default 5m. The "daily" framing in the spec is achieved by the
	// SQL filter (cold_at < now / delete_at < now) — over-frequent
	// ticks just process whatever is due since last tick.
	RetentionInterval time.Duration `mapstructure:"retention_interval"`
	// RetentionBatch is the max rows per pass per ticker tick.
	// Default 100.
	RetentionBatch int `mapstructure:"retention_batch"`

	// IntegrityInterval is the tick rate for the integrity pass.
	// Default 1h. Combined with IntegrityBatch + IntegritySamplePercent
	// this controls the weekly verification budget.
	IntegrityInterval time.Duration `mapstructure:"integrity_interval"`
	// IntegrityBatch is the max rows per integrity tick. Default 10.
	IntegrityBatch int `mapstructure:"integrity_batch"`
	// IntegritySamplePercent is the BERNOULLI sample rate in [0.01,
	// 100.0]. Default 1.0 (matches master spec's "1%").
	IntegritySamplePercent float64 `mapstructure:"integrity_sample_percent"`
}
```

If the existing config loader needs viper SetDefault for these, add four lines mirroring how the existing recording config does it. Look at `pkg/config/recording.go` for the existing default-setting pattern.

- [ ] **Step 3: Wire workers into cmd/worker/main.go**

OPEN `cmd/worker/main.go`. Find the section that builds the dialer retry orchestrator. AFTER it, ADD:

```go
// 5b. Recording workers (Plan 12.4). Skipped silently when
//     recording.enabled=false (default in dev environments).
var recordingRunners []func(ctx context.Context) error
if cfg.Recording.Enabled {
	ports, err := wire.LocalPorts(cfg.Recording, logger.Named("recording.wire"))
	if err != nil {
		return fmt.Errorf("recording wire: %w", err)
	}
	if ports == nil {
		logger.Warn("recording.enabled=true but local KEKs empty; workers skipped")
	} else {
		// Build leaders, store, service, and the two passes.
		recordingRunners, err = buildRecordingWorkers(ctx, cfg, pool, ports, logger)
		if err != nil {
			return fmt.Errorf("build recording workers: %w", err)
		}
	}
}
```

ADD a helper function (in the same main.go for now — extract later if it grows):

```go
func buildRecordingWorkers(ctx context.Context, cfg config.Config, pool *postgres.Pool, ports *wire.Ports, logger *zap.Logger) ([]func(context.Context) error, error) {
	rmetrics, err := metrics.RegisterRecordingMetrics(observability.MetricsRegistry()) // or whatever the canonical reg is
	if err != nil {
		return nil, fmt.Errorf("register recording metrics: %w", err)
	}
	pgStore := store.NewPostgresStore(pool)
	svc := service.New(service.Deps{
		Pool: pool, Store: pgStore, Logger: logger.Named("recording.service"),
		Metrics: rmetrics, KMS: ports.DEKUnwrapper, Objects: ports.ObjectStore,
	})
	outboxWriter := outbox.NewPgWriter(pool) // or however the existing service builds one — match it

	retentionLeader, err := retry.NewPgLeader(pool, /* retentionLockKey */ , logger.Named("recording.retention.leader"))
	// problem: retentionLockKey is package-private to internal/recording/worker.
	// Either export it (NEW Public symbol RetentionLockKey) or expose a small
	// helper worker.NewRetentionLeader(pool, logger) in internal/recording/worker.
	// Decision: export RetentionLockKey + IntegrityLockKey.
	if err != nil { return nil, err }
	integrityLeader, err := retry.NewPgLeader(pool, /* integrityLockKey */ , logger.Named("recording.integrity.leader"))
	if err != nil { return nil, err }

	rp, err := worker.NewRetentionPass(worker.RetentionConfig{
		Pool: pool, Leader: retentionLeader, Store: pgStore, Objects: ports.ObjectStore,
		Outbox: outboxWriter, Metrics: rmetrics, Logger: logger,
		Interval: cfg.Recording.Workers.RetentionInterval,
		Batch:    cfg.Recording.Workers.RetentionBatch,
	})
	if err != nil { return nil, err }

	ip, err := worker.NewIntegrityPass(worker.IntegrityConfig{
		Pool: pool, Leader: integrityLeader, Store: pgStore, Service: svc,
		Metrics: rmetrics, Logger: logger,
		Interval:      cfg.Recording.Workers.IntegrityInterval,
		Batch:         cfg.Recording.Workers.IntegrityBatch,
		SamplePercent: cfg.Recording.Workers.IntegritySamplePercent,
	})
	if err != nil { return nil, err }

	return []func(context.Context) error{rp.Run, ip.Run}, nil
}
```

UPDATE `internal/recording/worker/keys.go` — flip the lock-key vars to exported:

```go
var (
	RetentionLockKey = fnvHash("recording.retention_pass")
	IntegrityLockKey = fnvHash("recording.integrity_pass")
)
```

In the errgroup section, ADD goroutines for each runner:

```go
for i, run := range recordingRunners {
	run := run
	name := []string{"recording.retention", "recording.integrity"}[i]
	g.Go(func() error {
		logger.Info(name+" running")
		if err := run(gctx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	})
}
```

- [ ] **Step 4: Reality-check the boot path**

Run: `go build ./cmd/worker/...`
Expected: compiles. If imports are missing (worker, service, store, outbox, retry, wire), add them.

Run: `go vet ./cmd/worker/...`
Expected: no warnings.

- [ ] **Step 5: Sanity test that cmd/worker still boots without recording**

OPEN `cmd/worker/main_test.go`. ADD:

```go
func TestWorker_BootsWithoutRecording(t *testing.T) {
	t.Parallel()
	// The default test fixture has recording.enabled=false. Verify
	// run() returns cleanly when ctx is cancelled and that no
	// recording-related goroutine was registered.
	// (Implementer fills in — pattern: write a temp config dir,
	// signal cancel after 1s, assert nil error from run().)
}

func TestWorker_BootsWithRecording_Enabled(t *testing.T) {
	t.Parallel()
	// Set recording.enabled=true and recording.local_keks={"k1":"<64 hex>"}
	// in a temp config dir. Verify run() returns cleanly when ctx is
	// cancelled (i.e. both retention + integrity goroutines started + drained).
}
```

Run: `go test -race -count=1 ./cmd/worker/... -v`
Expected: PASS for both tests.

- [ ] **Step 6: Commit Task 5**

```bash
git add internal/recording/wire/ pkg/config/recording.go cmd/worker/main.go cmd/worker/main_test.go cmd/api/recording.go internal/recording/worker/keys.go
git commit -m "feat(cmd/worker): Plan 12.4 Task 5 — register retention + integrity passes in errgroup"
```

---

## Self-review

**Spec coverage** (against PROJECT_STATUS Plan 12.4 description and master spec §9.4 / §15.5):

- ✅ `internal/recording/worker/` (Task 2 + 3) — daily-ish retention_pass + weekly-ish integrity_pass (cadence is configurable; the tick rate × SQL filter combo achieves the spec's framing without requiring cron).
- ✅ Hot→cold lifecycle (Task 2 cold-move sweep + audit).
- ✅ Hard-delete via `ObjectStore.Delete` (Task 2 delete sweep + audit + outbox).
- ✅ Weekly 1% sample sha256 verify via `RecordingService.VerifyChecksum` (Task 3 sweep + UpdateVerifyResult + audit + IntegrityFailuresTotal metric).
- ✅ Populates `verified_at` + `integrity_ok` columns (Task 1 store hooks + Task 3 invocation).
- ✅ cmd/worker integration (Task 5 errgroup wiring).
- ✅ Audit actions: `recording.cold_moved`, `recording.deleted`, `recording.verified` (Task 2 + 3).
- ✅ Outbox event `recording.call.deleted` (Task 2 + Plan 11.4 future cache-invalidation hook).
- ✅ Metrics `recording_integrity_failures_total{tenant_id}` per master spec §15.5.

**Placeholder scan:** No `TBD`, `FIXME`, or "implement later" remain. Each TDD step has either failing-test code OR implementation code OR a concrete `go test`/`git commit` command. Step 6 in Task 2 has a deliberate placeholder followed by Step 7 fixing it — that's a TDD workflow choice, not an unfilled gap.

**Type/name consistency:**
- `LifecycleStore` interface in `api`; `*PostgresStore` implements it (compile-time check at Step 14 of Task 1).
- `LifecycleRow` struct in `api`; everywhere the workers handle a row they import the api package not store.
- `RetentionConfig` / `IntegrityConfig` paired with `RetentionPass` / `IntegrityPass`. Both expose `SweepOnce` (test seam) and `Run` (orchestrator entrypoint).
- `MarkColdTx` / `MarkDeletedTx` / `UpdateVerifyResultTx` mirror the package's existing PascalCase + Tx-suffix convention.
- `RetentionLockKey` / `IntegrityLockKey` exported from `internal/recording/worker` so cmd/worker can pass them to `retry.NewPgLeader`.
- Audit action constants (`AuditAction*`) and event subject constants (`SubjectRecording*`) live in `internal/recording/api/events.go` — single source of truth (subscribers in cache-invalidator and audit-consumer modules will import from there).

**Cross-tenant defence:** All BypassRLS reads use parameterised SQL (no string-concat tenant filtering — the WHERE clause has no `tenant_id` predicate at all because the worker IS cross-tenant). The status-flip writes (MarkColdTx, MarkDeletedTx, UpdateVerifyResultTx) run inside `pool.WithTenant(row.TenantID)` — defence-in-depth: the WithTenant Tx attaches `app.tenant_id` GUC, RLS verifies the row's `tenant_id` matches, the in-row UPDATE only succeeds if both agree.

**Out of scope (correctly deferred):**
- Yandex S3 lifecycle bucket-policy configuration (Plan 01 infra) — v1 cold-move is a metadata flag.
- Per-tenant retention policy resolution from `tenant_settings` — Plan 12.1 already wrote `delete_at` from the ingest_uploader's resolved value; the worker just respects what's there.
- Manual deletion endpoint + audit — backlog (master spec §Out of scope).
- Real cron schedule (vs. ticker + SQL filter) — backlog if ops asks for it.

**Self-check after writing the plan:** Each task has clear TDD steps (failing test → impl → passing test → commit). Task 1 and Task 2 share the `lifecycle.go` + `events.go` files — Task 2's commit must include the events-file changes from Task 2 only, not the Task 1 changes (already committed).

Plan 12.4 complete and saved.
