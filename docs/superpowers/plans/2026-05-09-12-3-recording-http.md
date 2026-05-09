# Plan 12.3 — Recording HTTP Delivery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Wire `RecordingService.OpenAudioStream` (Plan 12.2) into the public HTTP API; implement `RecordingService.Search` (cursor-paginated) and `RecordingService.VerifyChecksum` (manual SHA-256 integrity check); mount `/api/calls/:call_id/recording`, `/api/recordings/search`, `/api/calls/:call_id/recording/verify` under JWT + RBAC.

**Architecture:** Phase 3 of 4 of Plan 12 (Recording Module). Replaces Plan 12.1's two foundation-phase stubs (Search, VerifyChecksum) with real implementations. HTTP transport mirrors the gin-based pattern used by `internal/dialer/transport/http` and `internal/auth/transport/http`. Cursor pagination uses the Plan 12.1 `(tenant_id, committed_at DESC, id DESC)` index for keyset-style next-page resolution; the cursor itself is base64-url-encoded JSON. VerifyChecksum reads the ciphertext from `ObjectStore.Get`, computes `sha256` stream-style WITHOUT decryption (the row stores ciphertext SHA per the proto contract), and compares against `row.SHA256Hex`. NO retention/integrity workers (Plan 12.4).

**Tech Stack:** Go 1.26.3, gin, zap, pgx/v5, `pkg/middleware/auth.JWTMiddleware` for JWT + claims, project-standard `requireRole` pattern for RBAC.

---

## Implementer corrections — READ FIRST

The same set of corrections from Plans 12.1+12.2 carries over. Most relevant for this plan:

| Body says | Use this instead |
|---|---|
| `pgtest.AcquirePool` | Each integration test package writes its own `startPGContainer(t)` helper — copy from `internal/recording/store/postgres_pg_test.go` (Plan 12.1 Task 3). |
| `pool.QueryRow` (direct) | `pool.RawQueryRow(...)` for non-tx reads. |
| `pool.BypassRLS` | `pool.WithTenant(ctx, tenantID, fn)` — `tenancy_admin` lacks grants on `calls`/`call_recordings`. |
| `Locator.Set / Get` | `Locator.Register(name, svc)` and `Lookup(name) (any, bool)`. |
| Module path `sociopulse/sociopulse` | `github.com/sociopulse/platform`. |

When in doubt, mirror `internal/dialer/transport/http/` (Plan 10) for HTTP transport idioms and `internal/recording/store/postgres.go` (Plan 12.1) for store-layer SQL idioms.

---

## Carry-forward rules (from Plans 09–12.2)

1. **No `init()` MustRegister** — metrics via `RegisterXMetrics(reg) (*M, error)` constructor.
2. **`*zap.Logger` nil-safe** — `zap.NewNop()` fallback.
3. **Sentinel error aliasing** — `var ErrXxx = api.ErrXxx`.
4. **Compile-time interface check** — `var _ Iface = (*Impl)(nil)`.
5. **Tests** — `t.Parallel()` + `t.Cleanup()` + `t.Context()`. testifylint: `require.ErrorIs` (not `require.True(errors.Is(...))`); `require.NotErrorIs` for negative; `assert.NoError` (not `require.NoError`) inside non-test goroutines.
6. **`goleak.VerifyTestMain`** per package.
7. **No `time.After` in select-loops** — `time.NewTimer`.
8. **Modernize** — `any`, range over int, `slices`/`maps`. NO `tc := tc` shadow loop variable (Go 1.22+ already fresh per iteration).
9. **gopls cache pollution** — reality-check via direct `go build && go test -race`.
10. **Module path** `github.com/sociopulse/platform`.
11. **Logger** is zap.
12. **Error fold for sentinel-indistinguishability** — `fmt.Errorf("%w: %s", api.ErrXxx, child.Error())`.
13. **gocyclo cap 15** — split high-complexity validators / handlers if hit.

---

## Scope of Plan 12.3 vs other Plan 12 phases

| Concern | Phase | Status |
|---|---|---|
| Migration 000010 + Proto + RecordingStore + Commit + outbox + gRPC mTLS | Plan 12.1 | ✅ shipped (`v0.0.16`) |
| KMS port + S3 port + AES-GCM decrypt + OpenAudioStream real impl | Plan 12.2 | ✅ shipped (`v0.0.17`) |
| HTTP delivery (download + search + verify) | **Plan 12.3** | this plan |
| Workers (`retention_pass` daily + `integrity_pass` weekly) | Plan 12.4 | deferred |
| Yandex Cloud KMS / Object Storage real adapters | Plan 01 (infra) | deferred |

---

## File Structure

```text
internal/recording/
├── store/
│   ├── search.go                                    # NEW — Plan 12.3 — cursor SearchRecordings
│   └── search_pg_test.go                            # NEW — integration tests
├── service/
│   ├── search.go                                    # NEW — Plan 12.3 — Search real impl
│   ├── search_test.go                               # NEW — integration tests
│   ├── verify.go                                    # NEW — Plan 12.3 — VerifyChecksum real impl
│   ├── verify_test.go                               # NEW — integration tests
│   └── service.go                                   # MODIFY — remove Search + VerifyChecksum stubs
├── transport/
│   └── http/
│       ├── routes.go                                # NEW — Mount(group, deps)
│       ├── dto.go                                   # NEW — request/response shapes
│       ├── errors.go                                # NEW — sentinel→HTTP envelope mapping
│       ├── middleware.go                            # NEW — claimsFromContext + requireRole
│       ├── recording_handler.go                     # NEW — GET /calls/:id/recording
│       ├── search_handler.go                        # NEW — GET /recordings/search
│       ├── verify_handler.go                        # NEW — POST /calls/:id/recording/verify
│       ├── handlers_test.go                         # NEW — handler unit tests (httptest)
│       └── main_test.go                             # NEW — goleak
└── module.go                                        # MODIFY — Mount HTTP transport when HTTPRouter present

cmd/api/
└── main.go                                          # MODIFY — pass HTTPRouter into recording module Deps (already done?)
```

---

## Cursor pagination design

The Plan 12.1 migration created `call_recordings_search_idx ON call_recordings (tenant_id, committed_at DESC, id DESC)`. Keyset pagination uses tuple comparison:

```sql
-- First page:
SELECT … FROM call_recordings
WHERE tenant_id = $1
  AND … (other filters)
ORDER BY committed_at DESC, id DESC
LIMIT $N;

-- Subsequent pages (cursor = (cursor_committed_at, cursor_id)):
SELECT … FROM call_recordings
WHERE tenant_id = $1
  AND … (other filters)
  AND (committed_at, id) < ($cursor_committed_at, $cursor_id)
ORDER BY committed_at DESC, id DESC
LIMIT $N;
```

The tuple `<` operator on PostgreSQL implements lexicographic ordering: `(c1,i1) < (c2,i2)` iff `c1 < c2 OR (c1 = c2 AND i1 < i2)`. With both columns DESC in the index, the strictly-less-than operator picks the next page.

Cursor wire format:

```go
type cursor struct {
    CommittedAt time.Time `json:"c"`
    ID          uuid.UUID `json:"i"`
}
// encode: base64.URLEncoding.EncodeToString(json.Marshal(cursor))
// decode: json.Unmarshal(base64.URLEncoding.DecodeString(...))
```

Empty string cursor = first page. NextCursor in response is empty when HasMore=false.

---

## Filter coverage

`api.SearchQuery` exposes `ProjectID`, `OperatorID`, `Status []string`, `From *time.Time`, `To *time.Time`, `Cursor string`, `Limit int`. Plan 12.3 supports all six:

| Filter | SQL | Notes |
|---|---|---|
| ProjectID | `JOIN calls ON calls.id = call_recordings.call_id WHERE calls.project_id = $X` | one-to-one PK lookup |
| OperatorID | same JOIN, `calls.operator_id = $X` | nullable column — match only when set |
| Status | `call_recordings.status = ANY($X)` | values in {stored, cold, deleted} |
| From / To | `call_recordings.committed_at >= $X` and `< $Y` | half-open |
| Cursor | tuple compare as above | encoded in opaque base64 |
| Limit | `LIMIT N+1` then trim — `HasMore = len(rows) > N` | clamp 1..200, default 50 |

The JOIN path is cheap (calls.id is PK; call_recordings.call_id has a UNIQUE constraint from Plan 12.1).

---

## Task 1 — `internal/recording/store/search.go` — cursor-paginated SearchRecordings

**Goal:** Add `(*PostgresStore).Search(ctx, tenantID, q SearchQ) ([]RecordingRow, error)` with the keyset-pagination SQL above.

**Files:**
- Create: `internal/recording/store/search.go`
- Create: `internal/recording/store/search_pg_test.go` (integration tests, `//go:build integration`)

### `SearchQ` input

```go
package store

import (
	"time"

	"github.com/google/uuid"
)

// SearchQ is the cursor-paginated input to PostgresStore.Search.
// All pointers are optional filters; nil/zero means "no filter on this dimension".
// CursorCommittedAt+CursorID together encode the position from a previous page —
// both must be non-zero (or both zero for first page).
type SearchQ struct {
	ProjectID         *uuid.UUID
	OperatorID        *uuid.UUID
	Status            []string
	From              *time.Time
	To                *time.Time
	CursorCommittedAt *time.Time
	CursorRecordingID *uuid.UUID
	Limit             int
}
```

### Implementation

```go
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/pkg/postgres"
)

// Search returns up to q.Limit rows matching the filter. Rows are ordered by
// (committed_at DESC, id DESC) — same order as the supporting index from
// migration 000010. Pagination is keyset-style via (CursorCommittedAt,
// CursorRecordingID); pass both as nil for the first page.
//
// Caller-side cursor encoding lives in service.Search — the store stays
// SQL-only.
//
// Implementation note: the JOIN with `calls` is required to filter by
// project_id / operator_id. The join is one-to-one (calls.id is PK,
// call_recordings.call_id is UNIQUE) so the planner can satisfy it via
// nested-loop on the index, no hash table needed.
func (s *PostgresStore) Search(ctx context.Context, tenantID uuid.UUID, q SearchQ) ([]RecordingRow, error) {
	if q.Limit <= 0 || q.Limit > 200 {
		return nil, fmt.Errorf("recording.store: search limit must be 1..200; got %d", q.Limit)
	}
	if (q.CursorCommittedAt == nil) != (q.CursorRecordingID == nil) {
		return nil, errors.New("recording.store: cursor requires BOTH committed_at AND recording_id")
	}

	sql, args := buildSearchSQL(tenantID, q)

	var rows []RecordingRow
	err := s.pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		r, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return fmt.Errorf("recording.store: search query: %w", err)
		}
		defer r.Close()

		for r.Next() {
			var row RecordingRow
			if scanErr := r.Scan(
				&row.ID, &row.CallID, &row.TenantID, &row.S3Bucket, &row.AudioObjectKey, &row.DEKObjectKey,
				&row.KMSKeyID, &row.EncryptedDEK, &row.BytesSize, &row.DurationMS, &row.SHA256Hex,
				&row.Codec, &row.SampleRate, &row.Status, &row.CommittedAt, &row.DeleteAt, &row.ColdAt,
				&row.RecordedAt, &row.VerifiedAt, &row.IntegrityOK, &row.IngestAgentID,
			); scanErr != nil {
				return fmt.Errorf("recording.store: scan: %w", scanErr)
			}
			rows = append(rows, row)
		}
		return r.Err()
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// buildSearchSQL constructs the SQL string + parameter slice. Split into a
// helper so each test can assert SQL shape independently of pgx driver.
func buildSearchSQL(tenantID uuid.UUID, q SearchQ) (string, []any) {
	args := []any{tenantID}
	whereTerms := []string{"cr.tenant_id = $1"}

	addArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	needsJoin := q.ProjectID != nil || q.OperatorID != nil
	if q.ProjectID != nil {
		whereTerms = append(whereTerms, "c.project_id = "+addArg(*q.ProjectID))
	}
	if q.OperatorID != nil {
		whereTerms = append(whereTerms, "c.operator_id = "+addArg(*q.OperatorID))
	}
	if len(q.Status) > 0 {
		whereTerms = append(whereTerms, "cr.status = ANY("+addArg(q.Status)+")")
	}
	if q.From != nil {
		whereTerms = append(whereTerms, "cr.committed_at >= "+addArg(*q.From))
	}
	if q.To != nil {
		whereTerms = append(whereTerms, "cr.committed_at < "+addArg(*q.To))
	}
	if q.CursorCommittedAt != nil && q.CursorRecordingID != nil {
		// Tuple compare — both columns indexed DESC, so strictly-less-than
		// gives us the next page.
		whereTerms = append(whereTerms,
			"(cr.committed_at, cr.id) < ("+addArg(*q.CursorCommittedAt)+", "+addArg(*q.CursorRecordingID)+")")
	}

	join := ""
	if needsJoin {
		join = "JOIN calls c ON c.id = cr.call_id"
	}

	limitArg := addArg(q.Limit)

	sql := fmt.Sprintf(`
SELECT cr.id, cr.call_id, cr.tenant_id, cr.s3_bucket, cr.audio_object_key, cr.dek_object_key,
       cr.kms_key_id, cr.encrypted_dek, cr.bytes_size, cr.duration_ms, cr.sha256_hex,
       cr.codec, cr.sample_rate, cr.status, cr.committed_at, cr.delete_at, cr.cold_at,
       cr.recorded_at, cr.verified_at, cr.integrity_ok, cr.ingest_agent_id
FROM call_recordings cr
%s
WHERE %s
ORDER BY cr.committed_at DESC, cr.id DESC
LIMIT %s
`, join, joinAnd(whereTerms), limitArg)
	return sql, args
}

// joinAnd is a tiny helper — strings.Join with " AND " separator.
func joinAnd(terms []string) string {
	out := ""
	for i, t := range terms {
		if i > 0 {
			out += " AND "
		}
		out += t
	}
	return out
}
```

### Tests

`internal/recording/store/search_pg_test.go` (build tag `//go:build integration`):

1. `TestSearch_RejectsBadLimit` — Limit=0 / Limit=201 → error.
2. `TestSearch_RejectsHalfCursor` — CursorCommittedAt set + CursorRecordingID nil → error.
3. `TestSearch_FirstPageReturnsLatestFirst` — seed 5 recordings, search Limit=10 → all 5, ordered by committed_at DESC.
4. `TestSearch_KeysetPaginationWalksAllRecords` — seed 5, search Limit=2 → 2 rows, then with cursor → 2 more, then with cursor → 1, then 0. Verify no overlap, no skip.
5. `TestSearch_ProjectIDFilter` — seed 3 across 2 projects; filter by project_A → only 2.
6. `TestSearch_OperatorIDFilter` — seed 3 across 2 operators; filter by operator_A → only 2.
7. `TestSearch_StatusFilter` — seed 3 with statuses {stored, cold, deleted}; filter `status IN (stored, cold)` → only 2.
8. `TestSearch_PeriodFilter` — seed 3 with committed_at spread across 3 hours; filter From=t1, To=t3 → only the middle one.
9. `TestSearch_ComboFilters` — verify ProjectID + Status + From/To compose correctly.
10. `TestSearch_TenantIsolation` — seed 2 tenants × 3 recordings; search tenant_A → only 3, none from tenant_B.

### TDD steps

- [ ] **Step 1: Write all 10 failing tests** (template the helpers from `postgres_pg_test.go`).
- [ ] **Step 2: Run** — should FAIL with "undefined: store.SearchQ" / "(*PostgresStore).Search undefined".
- [ ] **Step 3: Implement `store/search.go`** per the sketch.
- [ ] **Step 4: Run** — confirm 10/10 PASS under `-tags=integration -race -count=1`.
- [ ] **Step 5: Commit**

```bash
git add internal/recording/store/search.go internal/recording/store/search_pg_test.go
git commit -m "feat(recording/store): Plan 12.3 Task 1 — cursor-paginated Search"
```

### Acceptance
- 10 tests green.
- Index `call_recordings_search_idx` is used (verify via `EXPLAIN ANALYZE` of the plain-tenant query in a manual test).
- Tenant isolation airtight (RLS policy + explicit WHERE clause).
- `goleak` clean.

---

## Task 2 — `internal/recording/service/search.go` — Service.Search real impl

**Goal:** Replace the foundation-phase `Search` stub with cursor encode/decode + store call + row→DTO mapping.

**Files:**
- Create: `internal/recording/service/search.go`
- Modify: `internal/recording/service/service.go` — remove the `Search` stub method.
- Create: `internal/recording/service/search_test.go` (integration tests, `//go:build integration`)

### Cursor encoding

```go
package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	rapi "github.com/sociopulse/platform/internal/recording/api"
)

// cursor is the wire-format intermediate between SearchQuery.Cursor (opaque
// string) and the store's keyset position. Encoded as base64-url JSON so the
// API consumer can treat it as opaque.
type cursor struct {
	CommittedAt time.Time `json:"c"`
	ID          uuid.UUID `json:"i"`
}

// encodeCursor returns the URL-safe base64 of {committed_at, id} JSON.
// Empty input (zero time + nil UUID) returns the empty string — used when
// HasMore=false to signal "no next page".
func encodeCursor(committedAt time.Time, id uuid.UUID) string {
	if committedAt.IsZero() && id == uuid.Nil {
		return ""
	}
	payload, _ := json.Marshal(cursor{CommittedAt: committedAt, ID: id})
	return base64.URLEncoding.EncodeToString(payload)
}

// decodeCursor parses the wire string. Empty string yields zero values
// (interpreted as "first page" by the store). Malformed input is folded
// into ErrInvalidInput so the HTTP layer maps to 400.
func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	if s == "" {
		return time.Time{}, uuid.Nil, nil
	}
	raw, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: cursor decode: %s", ErrInvalidInput, err.Error())
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: cursor unmarshal: %s", ErrInvalidInput, err.Error())
	}
	if c.CommittedAt.IsZero() || c.ID == uuid.Nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: cursor missing committed_at or id", ErrInvalidInput)
	}
	return c.CommittedAt, c.ID, nil
}
```

### Search implementation

```go
// Search satisfies rapi.RecordingService. Maps the public SearchQuery to the
// store-layer SearchQ, normalises the limit, decodes the cursor, calls the
// store, and packs results into SearchResult including the next cursor.
//
// Limit normalisation: 0 → 50 (default per dto.go); >200 → clamped to 200.
// Status validation: each entry must be in {stored,cold,deleted}; otherwise
// ErrInvalidInput.
//
// Pagination semantics: the store is asked for Limit+1 rows; if it returns
// Limit+1 we set HasMore=true and trim the last row to compute NextCursor
// from the LAST RETURNED row (not the trimmed extra). HasMore=false means
// the page is exhaustive — NextCursor is empty.
func (s *svc) Search(ctx context.Context, tenantID uuid.UUID, q rapi.SearchQuery) (rapi.SearchResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	if err := validateSearchStatus(q.Status); err != nil {
		return rapi.SearchResult{}, err
	}

	cursorCA, cursorID, err := decodeCursor(q.Cursor)
	if err != nil {
		return rapi.SearchResult{}, err
	}

	storeQ := store.SearchQ{
		ProjectID:  q.ProjectID,
		OperatorID: q.OperatorID,
		Status:     q.Status,
		From:       q.From,
		To:         q.To,
		Limit:      limit + 1, // peek one extra for HasMore
	}
	if !cursorCA.IsZero() {
		storeQ.CursorCommittedAt = &cursorCA
		storeQ.CursorRecordingID = &cursorID
	}

	rows, err := s.store.Search(ctx, tenantID, storeQ)
	if err != nil {
		return rapi.SearchResult{}, fmt.Errorf("recording.search: %w", err)
	}

	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit] // trim the peek row
	}

	items := make([]rapi.RecordingMetadata, 0, len(rows))
	for _, r := range rows {
		items = append(items, rowToMetadata(r))
	}

	nextCursor := ""
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		nextCursor = encodeCursor(last.CommittedAt, last.ID)
	}

	return rapi.SearchResult{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

// validateSearchStatus rejects status values outside the schema check
// constraint. The store SQL would 22P02 on bad values; we surface
// ErrInvalidInput up-front for a cleaner client error.
func validateSearchStatus(status []string) error {
	allowed := map[string]struct{}{"stored": {}, "cold": {}, "deleted": {}}
	for _, st := range status {
		if _, ok := allowed[st]; !ok {
			return fmt.Errorf("%w: status %q not in {stored,cold,deleted}", ErrInvalidInput, st)
		}
	}
	return nil
}

// rowToMetadata is the canonical store→api projection. Mirrors the same
// mapping in svc.Get so the wire representation is consistent across
// /api/calls/:id/recording (single-record), /api/recordings/search (page
// items), and gRPC RecordingService.Get (Plan 12.1).
func rowToMetadata(r store.RecordingRow) rapi.RecordingMetadata {
	var deleteAt time.Time
	if r.DeleteAt != nil {
		deleteAt = *r.DeleteAt
	}
	return rapi.RecordingMetadata{
		RecordingID:    r.ID,
		CallID:         r.CallID,
		TenantID:       r.TenantID,
		S3Bucket:       r.S3Bucket,
		AudioObjectKey: r.AudioObjectKey,
		BytesSize:      r.BytesSize,
		Duration:       time.Duration(r.DurationMS) * time.Millisecond,
		SHA256Hex:      r.SHA256Hex,
		Status:         r.Status,
		CommittedAt:    r.CommittedAt,
		DeleteAt:       deleteAt,
		ColdAt:         r.ColdAt,
		VerifiedAt:     r.VerifiedAt,
	}
}
```

> Modify `service.go` Get method to use `rowToMetadata` so the projection has a single source of truth. (One-line refactor; existing test stays green.)

### Tests

`internal/recording/service/search_test.go` (build tag `//go:build integration`):

1. `TestService_Search_FirstPage` — commit 3 recordings, search → 3 items, HasMore=false, NextCursor="".
2. `TestService_Search_PaginatesViaNextCursor` — commit 3, Limit=2 → 2 items + cursor; pass cursor → 1 item + empty cursor.
3. `TestService_Search_BadCursorReturnsInvalidInput` — `errors.Is(err, rapi.ErrInvalidInput)` on garbage cursor.
4. `TestService_Search_BadStatusReturnsInvalidInput` — `Status=["bogus"]` → `ErrInvalidInput`.
5. `TestService_Search_ProjectIDFilter` — commit 3 across 2 projects → filter project_A → 2.
6. `TestService_Search_LimitClampedTo200` — Limit=999 → store called with Limit=201 (peek +1), result still bounded at 200.
7. `TestService_Search_LimitDefault50` — Limit=0 → defaults to 50.
8. `TestService_Search_TenantIsolation` — seed 2 tenants → search tenant_A → only tenant_A's rows.

### TDD steps + commit

```bash
go build ./...
go test -tags=integration -race -count=1 -timeout 5m ./internal/recording/service/...
git add internal/recording/service/search.go internal/recording/service/search_test.go internal/recording/service/service.go
git commit -m "feat(recording/service): Plan 12.3 Task 2 — Search real impl + cursor codec"
```

### Acceptance
- 8 search tests green.
- Foundation-phase `Search not implemented` stub removed.
- `rowToMetadata` shared between Get and Search.
- Cursor wire format is opaque base64-url-encoded JSON (test asserts a malformed cursor → ErrInvalidInput).

---

## Task 3 — `internal/recording/service/verify.go` — VerifyChecksum real impl

**Goal:** Replace the foundation-phase `VerifyChecksum` stub with: fetch ciphertext from `ObjectStore.Get`, stream-compute `sha256`, compare with `row.SHA256Hex`. Note: per Plan 12.1 proto contract, `sha256` field stores the **CIPHERTEXT** sha256 (not plaintext) — so we don't need to decrypt.

**Files:**
- Create: `internal/recording/service/verify.go`
- Modify: `internal/recording/service/service.go` — remove the `VerifyChecksum` stub.
- Create: `internal/recording/service/verify_test.go` (integration tests, `//go:build integration`)

### Implementation

```go
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/storage"
	"github.com/sociopulse/platform/internal/recording/store"
)

// VerifyChecksum fetches the ciphertext from object storage and recomputes
// its sha256, comparing against call_recordings.sha256_hex (which the
// ingest-uploader populated from the SAME ciphertext per the Plan 12.1
// proto contract). Returns VerifyResult{OK, Expected, Actual, BytesScanned,
// DurationMS}.
//
// VerifyChecksum does NOT decrypt — verification is over the encrypted
// blob, not the plaintext audio. This keeps the integrity worker fast
// (no KMS round-trip per pass) and decoupled from the playback path.
//
// The HTTP layer (Plan 12.3 Task 4) wraps this in POST
// /api/calls/{call_id}/recording/verify so admins can trigger an on-demand
// integrity check without waiting for the weekly Plan 12.4 sweep.
func (s *svc) VerifyChecksum(ctx context.Context, tenantID, callID uuid.UUID) (rapi.VerifyResult, error) {
	if s.objects == nil {
		return rapi.VerifyResult{}, fmt.Errorf("%w: recording storage not wired", ErrInvalidInput)
	}

	start := time.Now()

	row, err := s.store.GetByCallID(ctx, tenantID, callID)
	if errors.Is(err, store.ErrCallNotFound) {
		return rapi.VerifyResult{}, ErrNotFound
	}
	if err != nil {
		return rapi.VerifyResult{}, fmt.Errorf("recording.verify: %w", err)
	}
	if row.Status == "deleted" {
		return rapi.VerifyResult{}, ErrAlreadyDeleted
	}

	rc, err := s.objects.Get(ctx, row.S3Bucket, row.AudioObjectKey)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
			return rapi.VerifyResult{}, ErrNotFound
		}
		return rapi.VerifyResult{}, fmt.Errorf("recording.verify.object: %w", err)
	}
	defer rc.Close()

	hasher := sha256.New()
	bytesScanned, err := io.Copy(hasher, rc)
	if err != nil {
		return rapi.VerifyResult{}, fmt.Errorf("recording.verify.read: %w", err)
	}
	actual := hex.EncodeToString(hasher.Sum(nil))

	return rapi.VerifyResult{
		OK:           actual == row.SHA256Hex,
		ExpectedSHA:  row.SHA256Hex,
		ActualSHA:    actual,
		BytesScanned: bytesScanned,
		DurationMS:   time.Since(start).Milliseconds(),
	}, nil
}
```

### Tests

`internal/recording/service/verify_test.go` (build tag `//go:build integration`):

1. `TestService_Verify_HappyPath` — commit + seed object → verify → OK=true, Expected==Actual, BytesScanned==len(ciphertext).
2. `TestService_Verify_TamperedObject` — commit + seed object with FLIPPED byte → verify → OK=false, Expected!=Actual.
3. `TestService_Verify_NotFound` — bogus call_id → `errors.Is(err, rapi.ErrNotFound)`.
4. `TestService_Verify_AlreadyDeleted` — commit, set status='deleted', verify → `ErrAlreadyDeleted`.
5. `TestService_Verify_ObjectMissing` — commit but skip seed → `ErrNotFound` (NOT ErrObjectNotFound — hide storage shape).
6. `TestService_Verify_NotWired` — service built without Objects → `ErrInvalidInput` with marker `"not wired"`.

Use `buildServiceWithCrypto` from Task 4 of Plan 12.2 (already exists in service_test.go).

### TDD steps + commit

```bash
go test -tags=integration -race -count=1 -timeout 5m ./internal/recording/service/...
git add internal/recording/service/verify.go internal/recording/service/verify_test.go internal/recording/service/service.go
git commit -m "feat(recording/service): Plan 12.3 Task 3 — VerifyChecksum (ciphertext sha256)"
```

### Acceptance
- 6 verify tests green.
- Foundation-phase `VerifyChecksum not implemented` stub removed.
- `ErrObjectNotFound` HIDDEN behind `ErrNotFound` (consistent with OpenAudioStream).
- VerifyChecksum does NOT call `kms.DecryptDEK` or `decryptor.Decrypt` — pure ciphertext sha256.

---

## Task 4 — `internal/recording/transport/http/` — Mount + 3 handlers + RBAC

**Goal:** Stand up the gin transport with JWT + RBAC + 3 endpoints. Mirrors `internal/dialer/transport/http/` structure.

**Files:**
- Create: `internal/recording/transport/http/routes.go` — `Mount(group, deps)` + RoutePrefix.
- Create: `internal/recording/transport/http/dto.go` — request/response shapes.
- Create: `internal/recording/transport/http/errors.go` — sentinel→HTTP envelope mapping.
- Create: `internal/recording/transport/http/middleware.go` — `claimsFromContext` + `requireRole`.
- Create: `internal/recording/transport/http/recording_handler.go` — GET /calls/:call_id/recording.
- Create: `internal/recording/transport/http/search_handler.go` — GET /recordings/search.
- Create: `internal/recording/transport/http/verify_handler.go` — POST /calls/:call_id/recording/verify.
- Create: `internal/recording/transport/http/handlers_test.go` — httptest-based handler unit tests.
- Create: `internal/recording/transport/http/main_test.go` — goleak.

### `Deps`

```go
package http

import (
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	rapi "github.com/sociopulse/platform/internal/recording/api"
)

// Deps captures the recording HTTP transport's collaborators.
// Validator + RBAC may be nil for tests that mock auth at a higher level.
type Deps struct {
	Service rapi.RecordingService

	Validator authapi.ClaimsValidator
	RBAC      authapi.RBACChecker
	Logger    *zap.Logger
}

// RoutePrefix is the canonical mount point for recording endpoints.
const RoutePrefix = "/api"
```

### `Mount`

```go
package http

import (
	"github.com/gin-gonic/gin"

	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// Mount attaches recording HTTP routes to group. group is the API root
// (typically /api), so the final paths are:
//
//	GET  /api/calls/:call_id/recording
//	GET  /api/recordings/search
//	POST /api/calls/:call_id/recording/verify
//
// All three require an authenticated JWT and either admin or supervisor
// role; verify additionally requires admin (an audit-grade action).
func Mount(group *gin.RouterGroup, d Deps) {
	if d.Service == nil {
		return // nothing to mount without a service
	}

	authed := group.Group("")
	if d.Validator != nil {
		authed.Use(authmw.JWTMiddleware(d.Validator))
	}

	rb := newHandlers(d)

	// admin/supervisor reads
	authed.GET("/calls/:call_id/recording", requireRole(authapi.RoleAdmin, authapi.RoleSupervisor), rb.streamRecording)
	authed.GET("/recordings/search", requireRole(authapi.RoleAdmin, authapi.RoleSupervisor), rb.searchRecordings)

	// admin-only on verify (writes audit, may incur cost via a future S3 GET)
	authed.POST("/calls/:call_id/recording/verify", requireRole(authapi.RoleAdmin), rb.verifyChecksum)
}

// handlers groups the three handlers so they share the same Deps closure.
type handlers struct {
	d Deps
}

func newHandlers(d Deps) *handlers { return &handlers{d: d} }
```

### Middleware

```go
package http

import (
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// claimsFromContext is the central read point for the JWT-attached claims.
// On miss (route not under JWTMiddleware) we abort with 401.
func claimsFromContext(c *gin.Context) (authapi.Claims, bool) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorEnvelope{
			Code:    "auth.token_invalid",
			Message: "authentication required",
		})
		return authapi.Claims{}, false
	}
	return claims, true
}

// requireRole returns a gin middleware enforcing the caller holds at least
// one of the supplied roles. Mirrors internal/dialer/transport/http.requireRole.
func requireRole(roles ...authapi.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authmw.ClaimsFromContext(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorEnvelope{
				Code:    "auth.token_invalid",
				Message: "authentication required",
			})
			return
		}
		hasRole := slices.ContainsFunc(roles, func(r authapi.Role) bool {
			return slices.Contains(claims.Roles, r)
		})
		if !hasRole {
			c.AbortWithStatusJSON(http.StatusForbidden, ErrorEnvelope{
				Code:    "auth.forbidden",
				Message: "insufficient role",
			})
			return
		}
		c.Next()
	}
}
```

### DTOs

```go
package http

import (
	"time"

	"github.com/google/uuid"
)

// ErrorEnvelope is the project-wide error response shape.
type ErrorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SearchResponse is the paginated /api/recordings/search payload.
type SearchResponse struct {
	Items      []RecordingMetadataDTO `json:"items"`
	NextCursor string                 `json:"next_cursor,omitempty"`
	HasMore    bool                   `json:"has_more"`
}

// RecordingMetadataDTO is the JSON projection of api.RecordingMetadata.
// Field names use snake_case per project convention.
type RecordingMetadataDTO struct {
	RecordingID    uuid.UUID `json:"recording_id"`
	CallID         uuid.UUID `json:"call_id"`
	TenantID       uuid.UUID `json:"tenant_id"`
	BytesSize      int64     `json:"bytes_size"`
	DurationMS     int64     `json:"duration_ms"`
	SHA256Hex      string    `json:"sha256"`
	Status         string    `json:"status"`
	CommittedAt    time.Time `json:"committed_at"`
	DeleteAt       time.Time `json:"delete_at,omitempty"`
	ColdAt         time.Time `json:"cold_at"`
	VerifiedAt    *time.Time `json:"verified_at,omitempty"`
}

// VerifyResponse is the POST /verify payload.
type VerifyResponse struct {
	OK           bool   `json:"ok"`
	ExpectedSHA  string `json:"expected_sha"`
	ActualSHA    string `json:"actual_sha"`
	BytesScanned int64  `json:"bytes_scanned"`
	DurationMS   int64  `json:"duration_ms"`
}
```

### Errors mapping

```go
package http

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	rapi "github.com/sociopulse/platform/internal/recording/api"
)

// renderServiceError maps a recording.api sentinel to its HTTP envelope.
// Caller invokes this and returns; the function never panics on nil.
func renderServiceError(c *gin.Context, err error) {
	if err == nil {
		return
	}
	switch {
	case errors.Is(err, rapi.ErrInvalidInput):
		c.JSON(http.StatusBadRequest, ErrorEnvelope{
			Code: "recording.invalid_input", Message: err.Error(),
		})
	case errors.Is(err, rapi.ErrNotFound):
		c.JSON(http.StatusNotFound, ErrorEnvelope{
			Code: "recording.not_found", Message: "recording not found",
		})
	case errors.Is(err, rapi.ErrAlreadyDeleted):
		c.JSON(http.StatusGone, ErrorEnvelope{
			Code: "recording.already_deleted", Message: "recording has been deleted",
		})
	case errors.Is(err, rapi.ErrCallNotFound):
		c.JSON(http.StatusPreconditionFailed, ErrorEnvelope{
			Code: "recording.call_not_found", Message: "call not found",
		})
	default:
		c.JSON(http.StatusInternalServerError, ErrorEnvelope{
			Code: "recording.internal_error", Message: "internal server error",
		})
	}
}
```

### `recording_handler.go`

```go
package http

import (
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// streamRecording handles GET /api/calls/:call_id/recording.
// Streams the decrypted plaintext audio to the response body.
//
// v1 trade-off: Accept-Ranges: none. Plan 12.2 buffers the entire plaintext
// in RAM before returning, so the response is a single contiguous chunk.
// v2 chunked-envelope (deferred) will support Range / partial content.
func (h *handlers) streamRecording(c *gin.Context) {
	claims, ok := claimsFromContext(c)
	if !ok {
		return
	}

	callID, err := uuid.Parse(c.Param("call_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorEnvelope{
			Code: "recording.invalid_input", Message: "call_id must be a UUID",
		})
		return
	}

	stream, err := h.d.Service.OpenAudioStream(c.Request.Context(), claims.TenantID, callID, nil)
	if err != nil {
		renderServiceError(c, err)
		return
	}
	defer stream.Reader.Close()

	c.Header("Content-Type", stream.ContentType)
	c.Header("Content-Length", strconv.FormatInt(stream.ContentLength, 10))
	c.Header("Accept-Ranges", "none")
	c.Header("Cache-Control", "private, no-store") // chain-of-custody

	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, stream.Reader); err != nil {
		// Connection broke mid-write; can't change status now (already 200).
		// Log only — gin's middleware will surface the partial response.
		if h.d.Logger != nil {
			h.d.Logger.Warn("recording stream interrupted",
				zap.String("call_id", callID.String()),
				zap.Error(err))
		}
	}
}
```

> **Implementer note**: import `"go.uber.org/zap"` in the handler file or accept the file's import set drifts. The `h.d.Logger != nil` guard mirrors the Plan 12.1/12.2 nil-safe pattern.

### `search_handler.go`

```go
package http

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	rapi "github.com/sociopulse/platform/internal/recording/api"
)

// searchRecordings handles GET /api/recordings/search.
// Query params (all optional):
//
//	project_id uuid
//	operator_id uuid
//	status     comma-separated subset of {stored,cold,deleted}
//	from       RFC3339 timestamp (inclusive)
//	to         RFC3339 timestamp (exclusive)
//	cursor     opaque base64 from previous page's next_cursor
//	limit      1..200, default 50
func (h *handlers) searchRecordings(c *gin.Context) {
	claims, ok := claimsFromContext(c)
	if !ok {
		return
	}

	q, err := parseSearchQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorEnvelope{
			Code: "recording.invalid_input", Message: err.Error(),
		})
		return
	}

	result, err := h.d.Service.Search(c.Request.Context(), claims.TenantID, q)
	if err != nil {
		renderServiceError(c, err)
		return
	}

	resp := SearchResponse{
		Items:      make([]RecordingMetadataDTO, 0, len(result.Items)),
		NextCursor: result.NextCursor,
		HasMore:    result.HasMore,
	}
	for _, m := range result.Items {
		resp.Items = append(resp.Items, RecordingMetadataDTO{
			RecordingID: m.RecordingID,
			CallID:      m.CallID,
			TenantID:    m.TenantID,
			BytesSize:   m.BytesSize,
			DurationMS:  m.Duration.Milliseconds(),
			SHA256Hex:   m.SHA256Hex,
			Status:      m.Status,
			CommittedAt: m.CommittedAt,
			DeleteAt:    m.DeleteAt,
			ColdAt:      m.ColdAt,
			VerifiedAt:  m.VerifiedAt,
		})
	}
	c.JSON(http.StatusOK, resp)
}

// parseSearchQuery extracts and validates query params. Empty params yield
// nil pointers / zero values which the service treats as "no filter".
func parseSearchQuery(c *gin.Context) (rapi.SearchQuery, error) {
	q := rapi.SearchQuery{}

	if v := c.Query("project_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return rapi.SearchQuery{}, fmt.Errorf("project_id: %w", err)
		}
		q.ProjectID = &id
	}
	if v := c.Query("operator_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return rapi.SearchQuery{}, fmt.Errorf("operator_id: %w", err)
		}
		q.OperatorID = &id
	}
	if v := c.Query("status"); v != "" {
		q.Status = strings.Split(v, ",")
	}
	if v := c.Query("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return rapi.SearchQuery{}, fmt.Errorf("from: %w", err)
		}
		q.From = &t
	}
	if v := c.Query("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return rapi.SearchQuery{}, fmt.Errorf("to: %w", err)
		}
		q.To = &t
	}
	q.Cursor = c.Query("cursor")
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return rapi.SearchQuery{}, fmt.Errorf("limit: %w", err)
		}
		q.Limit = n
	}
	return q, nil
}
```

> **Implementer note**: also import `"fmt"` and `"strings"` — the `parseSearchQuery` helper uses both.

### `verify_handler.go`

```go
package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// verifyChecksum handles POST /api/calls/:call_id/recording/verify.
// Synchronously fetches the ciphertext and recomputes its sha256.
// 200 OK with VerifyResponse on success (OK=true if matches; OK=false
// if doesn't — both are 200 because the verify itself succeeded).
//
// Mismatched sha (OK=false) is the canonical signal for the operator
// to investigate; we don't 5xx because the recording exists and the
// verify completed — the storage layer just delivered different bytes
// than what was committed.
func (h *handlers) verifyChecksum(c *gin.Context) {
	claims, ok := claimsFromContext(c)
	if !ok {
		return
	}

	callID, err := uuid.Parse(c.Param("call_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorEnvelope{
			Code: "recording.invalid_input", Message: "call_id must be a UUID",
		})
		return
	}

	result, err := h.d.Service.VerifyChecksum(c.Request.Context(), claims.TenantID, callID)
	if err != nil {
		renderServiceError(c, err)
		return
	}

	c.JSON(http.StatusOK, VerifyResponse{
		OK:           result.OK,
		ExpectedSHA:  result.ExpectedSHA,
		ActualSHA:    result.ActualSHA,
		BytesScanned: result.BytesScanned,
		DurationMS:   result.DurationMS,
	})
}
```

### Tests

`internal/recording/transport/http/handlers_test.go` — uses a fake `rapi.RecordingService` (no real DB). Build-tag-FREE since the fake covers all cases.

```go
package http_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	rapi "github.com/sociopulse/platform/internal/recording/api"
	rhttp "github.com/sociopulse/platform/internal/recording/transport/http"
)

// fakeRecordingService implements rapi.RecordingService for handler tests.
type fakeRecordingService struct {
	streamFn func(context.Context, uuid.UUID, uuid.UUID, *rapi.ByteRange) (rapi.AudioStream, error)
	searchFn func(context.Context, uuid.UUID, rapi.SearchQuery) (rapi.SearchResult, error)
	verifyFn func(context.Context, uuid.UUID, uuid.UUID) (rapi.VerifyResult, error)
}

func (f *fakeRecordingService) Commit(context.Context, rapi.CommitInput) (rapi.CommitOutput, error) {
	return rapi.CommitOutput{}, nil
}
func (f *fakeRecordingService) Get(context.Context, uuid.UUID, uuid.UUID) (rapi.RecordingMetadata, error) {
	return rapi.RecordingMetadata{}, nil
}
func (f *fakeRecordingService) Search(ctx context.Context, t uuid.UUID, q rapi.SearchQuery) (rapi.SearchResult, error) {
	if f.searchFn != nil { return f.searchFn(ctx, t, q) }
	return rapi.SearchResult{}, nil
}
func (f *fakeRecordingService) OpenAudioStream(ctx context.Context, t, call uuid.UUID, br *rapi.ByteRange) (rapi.AudioStream, error) {
	if f.streamFn != nil { return f.streamFn(ctx, t, call, br) }
	return rapi.AudioStream{}, nil
}
func (f *fakeRecordingService) VerifyChecksum(ctx context.Context, t, call uuid.UUID) (rapi.VerifyResult, error) {
	if f.verifyFn != nil { return f.verifyFn(ctx, t, call) }
	return rapi.VerifyResult{}, nil
}

// buildTestRouter mounts the recording transport with claims pre-attached
// to every request via a setup middleware (no JWT validation in these tests).
func buildTestRouter(t *testing.T, svc rapi.RecordingService, roles ...authapi.Role) (*gin.Engine, uuid.UUID) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	tenantID := uuid.Must(uuid.NewV7())
	api.Use(func(c *gin.Context) {
		claims := authapi.Claims{
			UserID:   uuid.Must(uuid.NewV7()),
			TenantID: tenantID,
			Roles:    roles,
		}
		c.Set("claims", claims) // pkg/middleware/auth.JWTMiddleware uses this key
		c.Next()
	})
	rhttp.Mount(api, rhttp.Deps{Service: svc})
	return r, tenantID
}

func TestStreamRecording_OK(t *testing.T) {
	t.Parallel()
	svc := &fakeRecordingService{
		streamFn: func(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ *rapi.ByteRange) (rapi.AudioStream, error) {
			payload := []byte("audio-bytes")
			return rapi.AudioStream{
				Reader:        io.NopCloser(strings.NewReader(string(payload))),
				ContentType:   "audio/ogg",
				ContentLength: int64(len(payload)),
			}, nil
		},
	}
	r, _ := buildTestRouter(t, svc, authapi.RoleAdmin)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/calls/"+uuid.Must(uuid.NewV7()).String()+"/recording", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "audio/ogg", w.Header().Get("Content-Type"))
	require.Equal(t, "none", w.Header().Get("Accept-Ranges"))
	require.Equal(t, "audio-bytes", w.Body.String())
}

func TestStreamRecording_NotFound(t *testing.T) {
	t.Parallel()
	svc := &fakeRecordingService{
		streamFn: func(context.Context, uuid.UUID, uuid.UUID, *rapi.ByteRange) (rapi.AudioStream, error) {
			return rapi.AudioStream{}, rapi.ErrNotFound
		},
	}
	r, _ := buildTestRouter(t, svc, authapi.RoleAdmin)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/calls/"+uuid.Must(uuid.NewV7()).String()+"/recording", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestStreamRecording_BadCallID(t *testing.T) {
	t.Parallel()
	r, _ := buildTestRouter(t, &fakeRecordingService{}, authapi.RoleAdmin)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/calls/not-a-uuid/recording", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestStreamRecording_RBACForbidden(t *testing.T) {
	t.Parallel()
	r, _ := buildTestRouter(t, &fakeRecordingService{}, authapi.RoleOperator) // wrong role
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/calls/"+uuid.Must(uuid.NewV7()).String()+"/recording", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestSearch_OK(t *testing.T) {
	t.Parallel()
	svc := &fakeRecordingService{
		searchFn: func(_ context.Context, _ uuid.UUID, q rapi.SearchQuery) (rapi.SearchResult, error) {
			require.Equal(t, 50, q.Limit) // default
			return rapi.SearchResult{HasMore: false, Items: []rapi.RecordingMetadata{}}, nil
		},
	}
	r, _ := buildTestRouter(t, svc, authapi.RoleSupervisor)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/recordings/search", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"has_more":false`)
}

func TestSearch_BadLimitReturns400(t *testing.T) {
	t.Parallel()
	r, _ := buildTestRouter(t, &fakeRecordingService{}, authapi.RoleAdmin)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/recordings/search?limit=abc", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSearch_BadFromReturns400(t *testing.T) {
	t.Parallel()
	r, _ := buildTestRouter(t, &fakeRecordingService{}, authapi.RoleAdmin)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/recordings/search?from=not-a-time", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSearch_RBACForbidden(t *testing.T) {
	t.Parallel()
	r, _ := buildTestRouter(t, &fakeRecordingService{}, authapi.RoleOperator)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/recordings/search", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestVerify_OK(t *testing.T) {
	t.Parallel()
	svc := &fakeRecordingService{
		verifyFn: func(context.Context, uuid.UUID, uuid.UUID) (rapi.VerifyResult, error) {
			return rapi.VerifyResult{OK: true, ExpectedSHA: "abc", ActualSHA: "abc", BytesScanned: 100, DurationMS: 5}, nil
		},
	}
	r, _ := buildTestRouter(t, svc, authapi.RoleAdmin)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/calls/"+uuid.Must(uuid.NewV7()).String()+"/recording/verify", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"ok":true`)
}

func TestVerify_RBACSupervisorForbidden(t *testing.T) {
	t.Parallel()
	// Verify is admin-only — supervisor should be denied.
	r, _ := buildTestRouter(t, &fakeRecordingService{}, authapi.RoleSupervisor)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/calls/"+uuid.Must(uuid.NewV7()).String()+"/recording/verify", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestVerify_AlreadyDeleted(t *testing.T) {
	t.Parallel()
	svc := &fakeRecordingService{
		verifyFn: func(context.Context, uuid.UUID, uuid.UUID) (rapi.VerifyResult, error) {
			return rapi.VerifyResult{}, rapi.ErrAlreadyDeleted
		},
	}
	r, _ := buildTestRouter(t, svc, authapi.RoleAdmin)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/calls/"+uuid.Must(uuid.NewV7()).String()+"/recording/verify", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusGone, w.Code)
}

func TestRoutes_NoServiceDoesNotMount(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	rhttp.Mount(api, rhttp.Deps{}) // nil service
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/recordings/search", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code, "route must not be registered with nil service")
}
```

`internal/recording/transport/http/main_test.go`:

```go
package http_test

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }
```

### TDD steps + commit

```bash
go build ./...
go test -race -count=1 ./internal/recording/transport/http/...
git add internal/recording/transport/http/
git commit -m "feat(recording/transport/http): Plan 12.3 Task 4 — Mount + 3 handlers + RBAC"
```

### Acceptance
- All ~10 handler tests green.
- 401 on missing claims, 403 on wrong role, 400 on malformed UUID/cursor/from-time/limit, 404 on ErrNotFound, 410 on ErrAlreadyDeleted, 412 on ErrCallNotFound, 500 default.
- Stream endpoint sets `Accept-Ranges: none`, `Cache-Control: private, no-store`.
- `Mount` is a no-op when `Deps.Service` is nil.
- `goleak` clean.

---

## Task 5 — Module + cmd/api wiring of HTTP transport

**Goal:** Mount the new HTTP routes from `internal/recording.Module.Register` when `Deps.HTTPRouter != nil`.

**Files:**
- Modify: `internal/recording/module.go` — add transport mount block.
- Verify: `cmd/api/main.go` — `Deps.HTTPRouter` is already passed through to recording (Plan 12.1 wiring).

### `module.go` patch

Find the `Register` method. After the gRPC server construction block, append:

```go
// Mount HTTP routes when a router is supplied. The auth Validator + RBAC
// adapters live in the locator (auth.LocatorClaimsValidator /
// auth.LocatorRBACChecker — Plan 05); we look them up at Register time.
// Nil locator entries fall through to "no auth middleware" — only safe
// in dev/test where the harness pre-attaches claims.
if d.HTTPRouter != nil {
	apiGroup := d.HTTPRouter.Group("/api")
	rhttp.Mount(apiGroup, rhttp.Deps{
		Service:   svc,
		Validator: lookupClaimsValidator(d.Locator),
		RBAC:      lookupRBACChecker(d.Locator),
		Logger:    logger,
	})
	logger.Info("recording HTTP routes mounted under /api")
}
```

Add the imports:

```go
"github.com/sociopulse/platform/internal/auth/api"
rhttp "github.com/sociopulse/platform/internal/recording/transport/http"
```

Add lookup helpers below `Register`:

```go
// lookupClaimsValidator returns the auth ClaimsValidator from the locator,
// or nil if absent. Recording mounts without auth middleware in that case
// — relies on the test harness or upstream proxy for identity.
func lookupClaimsValidator(loc modules.ServiceLocator) authapi.ClaimsValidator {
	if loc == nil {
		return nil
	}
	v, ok := loc.Lookup("auth.ClaimsValidator")
	if !ok {
		return nil
	}
	cv, _ := v.(authapi.ClaimsValidator)
	return cv
}

// lookupRBACChecker returns the auth RBACChecker from the locator, or nil
// if absent. Plan 12.3 currently uses transport-level requireRole only;
// the RBACChecker in Deps is wired for future fine-grained checks.
func lookupRBACChecker(loc modules.ServiceLocator) authapi.RBACChecker {
	if loc == nil {
		return nil
	}
	v, ok := loc.Lookup("auth.RBACChecker")
	if !ok {
		return nil
	}
	rb, _ := v.(authapi.RBACChecker)
	return rb
}
```

> **Implementer note**: confirm the locator key strings match the auth module's actual keys (`grep -n 'Locator.*Validator\|Locator.*RBAC' internal/auth/`). If they're `auth.LocatorClaimsValidator = "auth.ClaimsValidator"` etc., the strings above match. If the keys differ, adapt.

### TDD step + commit

```bash
go build ./...
go vet ./...

# All tests including the new mount path
go test -race -count=1 ./internal/recording/...
go test -tags=integration -race -count=1 -timeout 5m ./internal/recording/...

# Smoke check cmd/api still boots help
go run ./cmd/api --help 2>&1 | head -3
```

Expected: green, --help exits 0, recording HTTP routes are reachable in a manual smoke test.

```bash
git add internal/recording/module.go
git commit -m "feat(recording): Plan 12.3 Task 5 — Mount HTTP transport via Module.Register

Wires internal/recording/transport/http into Module.Register so the API
process exposes:
  GET  /api/calls/:call_id/recording
  GET  /api/recordings/search
  POST /api/calls/:call_id/recording/verify

ClaimsValidator + RBACChecker are looked up from the locator (auth
module Plan 05). When absent (e.g. minimal dev boot without auth) the
transport mounts without middleware — production always has auth.

Plan 12.3 Task 5."
```

### Acceptance
- `cmd/api` boots cleanly with the new routes registered.
- `curl -i http://localhost:8080/api/recordings/search` returns 401 (auth required) when auth module is wired.
- All integration tests still green.

---

## Self-review

### Spec coverage (against Plan 12 design brief §9, §FR-G, §13.6, §15.5, ADR-005)

| Brief requirement | Plan 12.3 task | Status |
|---|---|---|
| HTTP `GET /api/calls/{id}/recording` (admin/supervisor, audit access, stream-decrypt) | Tasks 4+5 | ✅ |
| `Accept-Ranges: none` v1 trade-off | Task 4 (`recording_handler.streamRecording`) | ✅ |
| HTTP `GET /api/recordings/search` (admin/supervisor, project/operator/period/status filters) | Tasks 1+2+4 | ✅ |
| Cursor pagination | Task 1 (store) + Task 2 (service codec) | ✅ |
| HTTP `POST /api/calls/{id}/recording/verify` (admin only, manual sha256) | Tasks 3+4 | ✅ |
| `recording.accessed` audit | Plan 12.2 Task 4 (already in `OpenAudioStream`) | ✅ already shipped |
| Metrics: `recording_access_total{actor_role}` | Plan 12.2 + Plan 12.3 nit (actor_role label not added — acceptable for v1) | partial — Plan 12.3 follow-up if needed |
| RBAC: admin/supervisor reads, admin only on verify | Task 4 `requireRole` | ✅ |
| Tenant isolation | Task 1 SQL + Task 4 claims.TenantID | ✅ |

### Placeholder scan

- "Plan 12.4" / "Plan 01 (infra)" — explicit forward references with phase number. Not placeholders.
- "Implementer note" call-outs — concrete instructions to confirm a symbol name. Acceptable.
- No "TBD", "fill in later", or open-ended "add validation".

### Type/name consistency

- `store.SearchQ` (Task 1) ↔ `service.Search` builds it from `rapi.SearchQuery` (Task 2). Consistent.
- `cursor` (private struct) used only in `service/search.go`. Consistent.
- `rowToMetadata` (Task 2) is shared between `service.Get` (Plan 12.1) and `service.Search` (Plan 12.3) — single source of truth.
- `RecordingMetadataDTO` (Task 4) ↔ `rapi.RecordingMetadata` — explicit field-by-field projection.
- `requireRole(roles ...authapi.Role)` (Task 4) ↔ same name as `internal/dialer/transport/http/middleware.go::requireRole` — package-private, no collision.

### Carry-forward checklist

- [x] No `init()` MustRegister.
- [x] `*zap.Logger` nil-safe — guards in `streamRecording`.
- [x] Sentinel error aliasing — service re-exports inherited from Plan 12.2.
- [x] Compile-time interface checks — Plan 12.1 already locks in `var _ rapi.RecordingService = (*svc)(nil)`.
- [x] `t.Parallel()` + `t.Cleanup()` + `t.Context()`.
- [x] `goleak.VerifyTestMain` — new package gets its own.
- [x] Modernize: `any`, range over int, `slices` package — used in `requireRole`.
- [x] `pool.WithTenant` (NOT `BypassRLS`).
- [x] testifylint compliance: `require.ErrorIs` etc.
- [x] No `tc := tc` shadows.

### Out of scope (correctly deferred)

- Yandex Cloud KMS / S3 real adapters — Plan 01.
- `actor_role` label on `recording_access_total` — Plan 12.3 follow-up if metric volume warrants.
- Range / partial-content responses — v2 chunked envelope.
- Batch operations (delete-many, verify-many) — not in spec.
- Server-side caching of decrypted plaintext — explicitly forbidden by `Cache-Control: private, no-store`.

**Plan 12.3 verified.**

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-09-12-3-recording-http.md`.**
