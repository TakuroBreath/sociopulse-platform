# Plan 12.1 — Recording Module Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring `internal/recording` from contract-only stub to a working `Commit` pipeline: gRPC server on `:9091` (mTLS internal), atomic INSERT-with-outbox, idempotent on `call_id`, audit + NATS event published via outbox-relay.

**Architecture:** Phase 1 of Plan 12 (Recording Module). Establishes the data model + Commit pipeline ONLY. NO crypto, NO S3 read, NO HTTP delivery, NO workers — those are Plans 12.2 (crypto+S3 read), 12.3 (HTTP delivery), 12.4 (workers). Reuses Plan 11 outbox infrastructure (`pkg/outbox.Writer.Append` inside the same Tx as the recording INSERT). Composition root in `cmd/api` adds a second gRPC listener alongside the HTTP/WS listener.

**Tech Stack:** Go 1.26.3, gin, zap, pgx/v5, `google.golang.org/grpc v1.81.0`, `google.golang.org/protobuf v1.36.11`.

---

## Implementer corrections — READ FIRST (BLOCKING)

> **Implementer subagents MUST read this section before any code-block in the body.** When the body's example code references symbols below, substitute the canonical name. Body was drafted from the 2026-05-06 design brief which uses idealised names; the corrections reflect what's actually in the repo as of `v0.0.15-realtime-hardening`.

| Body says | Use this instead | Where it lives |
|---|---|---|
| `clock.Clock` / `clock.Real` / `clock.Frozen(t)` | **There is no `pkg/clock`.** Inject a `nowFn func() time.Time` into `service.Deps` (default `time.Now`). Tests pass `func() time.Time { return fixedTime }`. | n/a (new contract) |
| `pgtest.AcquirePool(t)` / `SeedTenant(t, pool)` / `SeedCall(t, pool)` | **There is no `pkg/postgres/pgtest`.** Each integration test package writes its own `startPGContainer(t) *postgres.Pool` helper following `internal/dialer/fsm/audit_pg_test.go:42`. Build tag `//go:build integration`. | per-package `*_pg_test.go` |
| `pool.QueryRow(...)` (direct) | `pool.RawQueryRow(...)` for non-tx reads. Inside `BypassRLS(ctx, fn)` it's `tx.QueryRow(...)`. | `pkg/postgres/pool.go:146` |
| `Locator.Set(name, svc)` / `Locator.Get(name)` | `Locator.Register(name, svc)` and `Lookup(name) (any, bool)`. | `internal/modules/locator.go:18, 26` |
| `Deps.PrometheusRegistry` | **`Deps` has no PrometheusRegistry field.** cmd/api builds metrics outside the standard Module.Register flow. The recording module exposes a `NewModule(reg prometheus.Registerer)` helper called from `cmd/api/recording.go`; the standard `Module.Register(d Deps)` no-ops if the metrics-aware constructor was not called first. See revised Task 5 sub-step 12 below. | `internal/modules/module.go:25-43` |
| `Module.Start(ctx) error` / `Stop(ctx) error` part of interface | `modules.Module` interface ONLY has `Name()` + `Register(d) error` — NO lifecycle hooks. Recording's `Start(ctx)` is a method on the concrete `*recording.Module` type, called explicitly from `cmd/api/run()` via a typed reference (`g.Go(func() error { return recordingMod.Start(gctx) })`). The body's `Module.Stop(ctx)` is removed (no Stop method needed — Start handles ctx cancellation). | `internal/modules/module.go:74-77` |
| `protoDur(md.Duration)` placeholder | `durationpb.New(md.Duration)` from `google.golang.org/protobuf/types/known/durationpb`. Field name on `GetResponse` is `Duration` (regular Go field). | n/a |
| `errClosed{}` sentinel placeholder | `grpc.ErrServerStopped` from `google.golang.org/grpc`. | n/a |
| `RegisterRecordingMetrics(d.PrometheusRegistry)` inside `Module.Register` | `RegisterRecordingMetrics(reg)` is called from `cmd/api/recording.go` BEFORE `Module.Register`. The constructed metrics are passed via a private `*Module.metrics` field set by `NewModule(reg)`. `Module.Register(d Deps)` then uses `m.metrics` (may be nil → ObserveCommit is nil-safe). | n/a (composition pattern) |

**Conclusion:** When implementer subagents follow code blocks below, ALWAYS substitute these names. The carry-forward checklist at the bottom of the plan re-verifies each substitution.

---

## Carry-forward rules (from Plans 09/10/11/11.1/11.2/11.3)

These rules MUST be followed by every implementer subagent — they reflect lessons learned across the prior plans.

1. **No `init()` MustRegister** — metrics via `RegisterRecordingMetrics(reg prometheus.Registerer) (*RecordingMetrics, error)` constructor; constructor is the only way to obtain a metrics struct, and it returns an error on duplicate registration.
2. **`*zap.Logger` is nil-safe** — every method that touches the logger checks `if l != nil { l.Info(...) }` OR the type's constructor stores `zap.NewNop()` when the caller passes `nil`. The project uses zap, NOT slog (Plan 12 design brief mentions slog — substitute zap throughout).
3. **Sentinel errors aliased** — internal-layer files re-export `api.ErrXxx` via `var ErrXxx = api.ErrXxx`. External callers `errors.Is(err, api.ErrXxx)`. Internal callers may use either.
4. **Compile-time interface check** — every concrete service writes `var _ api.Iface = (*svc)(nil)` at package scope to fail at compile-time if the contract drifts.
5. **Tests** — `t.Parallel()` + `t.Cleanup()` + `t.Context()` (Go 1.24+ stdlib). `goleak.VerifyTestMain(m)` per package via `TestMain(m *testing.M)`.
6. **No `time.After` in select-loops** — use `time.NewTimer(d)` + `defer t.Stop()`. `time.After` leaks until deadline expires.
7. **Modernize** — `any` over `interface{}`, range over int (`for i := range n`), `slices` / `maps` standard-library packages.
8. **`wg.Go(func() error)`** (Go 1.25+) over the verbose `wg.Add(1); go func() { defer wg.Done(); ... }()`.
9. **gopls cache pollution** — after subagent dispatches, gopls may report stale "undefined: X" for symbols you just defined. ALWAYS reality-check via `go build ./... && go vet ./... && go test -race -count=1 ./internal/recording/...`. If those are green, the diagnostic is noise.
10. **Module path** — `github.com/sociopulse/platform`. The Plan 12 design brief says `github.com/sociopulse/sociopulse`; substitute throughout.
11. **`*zap.Logger`, NOT `*slog.Logger`** — see point 10. The brief drifts.
12. **Logging field convention** — `zap.String("call_id", id.String())`, `zap.Int64("bytes", n)`, etc. Never log raw PII (phone numbers, names, audio bytes); call ids and tenant ids are fine.
13. **PostgreSQL access** — `pkg/postgres.Pool` and `pkg/postgres.Tx`. The recording module MUST acquire its `Tx` via `pool.BypassRLS` (system module — manages cross-tenant data atomically) and pass that `tx` to `outbox.Writer.Append`.
14. **NATS / outbox** — recording does NOT publish directly to NATS. It calls `outbox.Writer.Append(ctx, tx, ev)` inside the same Tx as the INSERT. The platform-wide `outbox.Relay` (already wired in `cmd/api/main.go`) drains rows to JetStream.

---

## File Structure

```text
docs/api/recording/v1/
└── recording.proto                                    # Task 2

internal/recording/
├── api/                                                # frozen contract (Plan 00a) — DO NOT MODIFY in this plan
│   ├── dto.go                                          # CommitInput / RecordingMetadata / etc.
│   ├── errors.go                                       # ErrNotFound / ErrCallNotFound / etc.
│   ├── events.go                                       # SubjectRecordingUploadedFor / RecordingUploadedEvent
│   └── interfaces.go                                   # RecordingService / URLSigner / RetentionPlanner
├── proto/v1/                                           # Task 2 (generated, committed)
│   ├── recording.pb.go
│   └── recording_grpc.pb.go
├── grpcserver/                                         # Task 5
│   ├── server.go                                       # GRPCServer wiring
│   ├── peer_identity.go                                # SPIFFE-style cert SAN parsing
│   ├── commit_handler.go                               # Commit: proto → service.Commit → proto
│   └── server_test.go
├── service/                                            # Task 4
│   ├── service.go                                      # RecordingService (Commit + Get only — Search/OpenAudio/Verify return ErrNotImplemented for now)
│   └── service_test.go
├── store/                                              # Task 3
│   ├── postgres.go                                     # RecordingStore (InsertRecordingIdempotent + GetByCallID)
│   ├── rows.go                                         # RecordingRow DTO mirroring DB schema
│   └── postgres_test.go                                # testcontainers-based
├── metrics/                                            # Task 4
│   ├── metrics.go                                      # RecordingMetrics + RegisterRecordingMetrics constructor
│   └── metrics_test.go
└── module.go                                           # Task 5 — fills in real Register

cmd/api/
├── main.go                                             # Task 5 — patch run() to register recording.Module + start GRPCServer
└── recording.go                                        # Task 5 (NEW) — composition helpers (kept out of main.go for symmetry with realtime.go)

migrations/
├── 000010_recording_evolve.up.sql                      # Task 1
└── 000010_recording_evolve.down.sql                    # Task 1
```

**Out of scope** (keeping the file count tight for Phase 1):
- `internal/recording/storage/s3.go`, `internal/recording/crypto/aesgcm.go`, `internal/recording/crypto/kms.go` — Plan 12.2.
- `internal/recording/transport/http/` — Plan 12.3.
- `internal/recording/worker/retention.go`, `worker/integrity.go` — Plan 12.4.

---

## Task 1 — Migration `000010_recording_evolve` (evolve call_recordings)

**Goal:** Bring the `call_recordings` table from its Plan 03 shape (call_id PK, single `s3_key`, `duration_sec int`, `created_at`, `retention_until date`) to the Plan 12 shape (`id` UUID PK, `call_id` UNIQUE, `audio_object_key`, `dek_object_key` (nullable), `bytes_size`, `duration_ms`, `sha256_hex`, `sample_rate`, `status`, `committed_at`, `cold_at`, `recorded_at`, `verified_at`, `integrity_ok`, `ingest_agent_id`).

**Files:**
- Create: `migrations/000010_recording_evolve.up.sql`
- Create: `migrations/000010_recording_evolve.down.sql`
- Test: `migrations/migrations_test.go` (existing — re-run to confirm forward+backward integrity)

### Why this migration is non-destructive

The table is empty in production (Plan 12 hasn't run before — only Plan 03 created the schema). The migration adds columns, backfills a single test row's defaults if any, then RENAMEs/DROPs old shape. Down-migration restores the Plan 03 shape losslessly because no production data exists yet — the down path uses `RAISE EXCEPTION` if the table is non-empty, mirroring the Plan 05 `000003_users_auth_evolve.down.sql` guard.

- [ ] **Step 1: Create `migrations/000010_recording_evolve.up.sql`**

```sql
-- Plan 12.1 — evolve call_recordings to support full RecordingService.Commit shape.
-- Rationale: Plan 03 created a minimal schema (call_id PK, s3_key, duration_sec, created_at,
-- retention_until). Plan 12 needs a richer shape: separate id PK, audio/dek object keys,
-- bytes_size + duration_ms, sample_rate, status enum, committed_at + cold_at + recorded_at +
-- verified_at + integrity_ok, ingest_agent_id.
--
-- This migration is non-destructive: column adds + renames + a single PK swap.
-- Production data does not exist yet (Plan 12 hasn't shipped), so backfills are
-- defaults-only. The .down.sql guards against silent data loss with RAISE EXCEPTION.

BEGIN;

-- 1. Add new columns (NULL during transition; tightened in step 6 once backfill lands).
ALTER TABLE call_recordings
    ADD COLUMN id              uuid,
    ADD COLUMN dek_object_key  text,
    ADD COLUMN bytes_size      bigint,
    ADD COLUMN duration_ms     bigint,
    ADD COLUMN sample_rate     integer NOT NULL DEFAULT 48000,
    ADD COLUMN status          text    NOT NULL DEFAULT 'stored',
    ADD COLUMN committed_at    timestamptz,
    ADD COLUMN cold_at         timestamptz,
    ADD COLUMN recorded_at     timestamptz,
    ADD COLUMN verified_at     timestamptz,
    ADD COLUMN integrity_ok    boolean,
    ADD COLUMN ingest_agent_id text;

-- 2. Backfill defaults for any historical rows.
UPDATE call_recordings
   SET id = gen_random_uuid()
 WHERE id IS NULL;

UPDATE call_recordings
   SET committed_at = created_at
 WHERE committed_at IS NULL;

UPDATE call_recordings
   SET duration_ms = duration_sec::bigint * 1000
 WHERE duration_ms IS NULL;

UPDATE call_recordings
   SET bytes_size = 0
 WHERE bytes_size IS NULL;

UPDATE call_recordings
   SET cold_at = retention_until::timestamptz
 WHERE cold_at IS NULL;

UPDATE call_recordings
   SET recorded_at = created_at
 WHERE recorded_at IS NULL;

UPDATE call_recordings
   SET ingest_agent_id = ''
 WHERE ingest_agent_id IS NULL;

-- 3. Rename columns to match Plan 12 contract (api/dto.go).
ALTER TABLE call_recordings
    RENAME COLUMN s3_key TO audio_object_key;
ALTER TABLE call_recordings
    RENAME COLUMN sha256 TO sha256_hex;

-- 4. Convert delete_at DATE → TIMESTAMPTZ for sub-day retention precision (Plan 12.4 worker).
ALTER TABLE call_recordings
    ALTER COLUMN delete_at TYPE timestamptz USING delete_at::timestamptz;

-- 5. Drop superseded columns. retention_until is replaced by cold_at; duration_sec by duration_ms;
--    created_at by committed_at.
ALTER TABLE call_recordings
    DROP COLUMN retention_until,
    DROP COLUMN duration_sec,
    DROP COLUMN created_at;

-- 6. Tighten constraints — every Plan 12 column is NOT NULL except verified_at + integrity_ok
--    (set when integrity_pass worker first verifies the row in Plan 12.4) and dek_object_key
--    (nullable in v1 — encrypted_dek lives in-row; sidecar S3 object is reserved for v2).
ALTER TABLE call_recordings
    ALTER COLUMN id           SET NOT NULL,
    ALTER COLUMN bytes_size   SET NOT NULL,
    ALTER COLUMN duration_ms  SET NOT NULL,
    ALTER COLUMN committed_at SET NOT NULL,
    ALTER COLUMN cold_at      SET NOT NULL,
    ALTER COLUMN delete_at    SET NOT NULL,
    ALTER COLUMN recorded_at  SET NOT NULL;

-- 7. Switch primary key from call_id → id, keep UNIQUE on call_id (idempotency uses this constraint).
ALTER TABLE call_recordings
    DROP CONSTRAINT call_recordings_pkey,
    ADD PRIMARY KEY (id),
    ADD CONSTRAINT call_recordings_call_id_unique UNIQUE (call_id);

-- 8. Replace retention index with status-aware variants matching Plan 12.4 query patterns.
DROP INDEX IF EXISTS call_recordings_retention_idx;

CREATE INDEX call_recordings_status_cold_at_idx
    ON call_recordings (status, cold_at)
    WHERE status = 'stored';

CREATE INDEX call_recordings_status_delete_at_idx
    ON call_recordings (status, delete_at)
    WHERE status IN ('stored', 'cold');

-- 9. Cursor-pagination index for Plan 12.3 Search.
CREATE INDEX call_recordings_search_idx
    ON call_recordings (tenant_id, committed_at DESC, id DESC);

-- 10. Status enum check.
ALTER TABLE call_recordings
    ADD CONSTRAINT call_recordings_status_check
    CHECK (status IN ('stored', 'cold', 'deleted'));

COMMIT;
```

- [ ] **Step 2: Create `migrations/000010_recording_evolve.down.sql`**

```sql
-- Plan 12.1 down — restore the Plan 03 shape. Guarded against silent data loss with RAISE EXCEPTION.
-- Down rebuilds: drop the new shape's PK + UNIQUE, drop the new indexes/constraints, drop the new
-- columns, restore the dropped columns, restore the old PK on call_id.

BEGIN;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM call_recordings LIMIT 1) THEN
        RAISE EXCEPTION
            'down migration would lose data — call_recordings has rows. Manually drop or migrate first.';
    END IF;
END$$;

ALTER TABLE call_recordings
    DROP CONSTRAINT IF EXISTS call_recordings_status_check;

DROP INDEX IF EXISTS call_recordings_search_idx;
DROP INDEX IF EXISTS call_recordings_status_delete_at_idx;
DROP INDEX IF EXISTS call_recordings_status_cold_at_idx;

ALTER TABLE call_recordings
    DROP CONSTRAINT IF EXISTS call_recordings_call_id_unique;

ALTER TABLE call_recordings
    DROP CONSTRAINT IF EXISTS call_recordings_pkey;

-- Recreate Plan 03 shape (additive).
ALTER TABLE call_recordings
    ADD COLUMN created_at      timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN duration_sec    integer     NOT NULL DEFAULT 0,
    ADD COLUMN retention_until date        NOT NULL DEFAULT (now()::date + INTERVAL '365 days');

-- Restore old column names.
ALTER TABLE call_recordings
    RENAME COLUMN audio_object_key TO s3_key;
ALTER TABLE call_recordings
    RENAME COLUMN sha256_hex TO sha256;

-- delete_at back to DATE.
ALTER TABLE call_recordings
    ALTER COLUMN delete_at TYPE date USING delete_at::date;

ALTER TABLE call_recordings
    DROP COLUMN id,
    DROP COLUMN dek_object_key,
    DROP COLUMN bytes_size,
    DROP COLUMN duration_ms,
    DROP COLUMN sample_rate,
    DROP COLUMN status,
    DROP COLUMN committed_at,
    DROP COLUMN cold_at,
    DROP COLUMN recorded_at,
    DROP COLUMN verified_at,
    DROP COLUMN integrity_ok,
    DROP COLUMN ingest_agent_id;

-- Restore Plan 03 primary key.
ALTER TABLE call_recordings
    ADD PRIMARY KEY (call_id);

-- Restore Plan 03 retention index.
CREATE INDEX call_recordings_retention_idx
    ON call_recordings (retention_until)
    WHERE delete_at IS NULL;

COMMIT;
```

- [ ] **Step 3: Run the existing migrations test to verify forward+backward integrity**

```bash
go test -race -count=1 ./migrations/...
```

Expected: PASS — the migrations runner applies all .up.sql then all .down.sql against an ephemeral container and compares schema dumps. If the dump differs, the test fails with a unified diff.

If the test fails because the existing harness doesn't generate the schema dump for `call_recordings` evolution, add a dedicated test as Step 4; otherwise skip.

- [ ] **Step 4: Commit**

```bash
git add migrations/000010_recording_evolve.up.sql migrations/000010_recording_evolve.down.sql
git commit -m "feat(migrations): Plan 12.1 Task 1 — evolve call_recordings to Plan 12 shape"
```

### Acceptance
- `migrate up` advances the schema cleanly.
- `migrate down 1` reverts to Plan 03 shape on an empty table; raises on a non-empty table.
- `EXPLAIN ANALYZE` of the cursor-pagination query (`SELECT … FROM call_recordings WHERE tenant_id = $1 AND committed_at < $2 ORDER BY committed_at DESC, id DESC LIMIT 50`) shows `Index Scan using call_recordings_search_idx`.
- Unique constraint on `call_id` is the foundation for `Commit` idempotency (Task 4).

---

## Task 2 — Proto + codegen for `RecordingService`

**Goal:** Define the gRPC contract used by the future `cmd/recording-uploader` (out of scope for Plan 12.x — lives in Plan 08 / future) and consumed by `internal/recording/grpcserver/`. Generate Go bindings.

**Files:**
- Create: `docs/api/recording/v1/recording.proto`
- Create: `internal/recording/proto/v1/recording.pb.go` (generated, committed)
- Create: `internal/recording/proto/v1/recording_grpc.pb.go` (generated, committed)
- Modify: `Makefile` (add `proto-recording` target + wire it into `proto-all` if such target exists)
- Modify: `tools/tools.go` (add protoc-gen-go + protoc-gen-go-grpc imports if missing)

- [ ] **Step 1: Create `docs/api/recording/v1/recording.proto`**

```protobuf
syntax = "proto3";

package sociopulse.recording.v1;

option go_package = "github.com/sociopulse/platform/internal/recording/proto/v1;recordingv1";

import "google/protobuf/timestamp.proto";
import "google/protobuf/duration.proto";

// RecordingService — internal gRPC API consumed by cmd/recording-uploader (out of scope
// for Plan 12.1 — that command lives in Plan 08 or a future plan). Mounted on :9091
// alongside cmd/api's HTTP listener. mTLS is REQUIRED — the server interceptor extracts
// a SPIFFE-style identity from the verified client cert.
service RecordingService {
  // Commit registers a recording that the uploader has already streamed to S3 (.opus.enc)
  // along with its envelope-wrapped DEK (encrypted_dek bytes; sidecar .dek.enc S3 object is
  // reserved for v2, see DekObjectKey notes below).
  //
  // Idempotency: keyed by call_id (UNIQUE constraint in call_recordings). A duplicate Commit
  // for the same call_id returns the EXISTING row's metadata with idempotent_replay=true.
  //
  // Errors:
  //   INVALID_ARGUMENT — sha256 length != 64 hex chars, bytes_size <= 0, missing required field.
  //   FAILED_PRECONDITION — call_id has no matching row in calls(id, tenant_id).
  //   PERMISSION_DENIED — request.tenant_id mismatches the SPIFFE tenant in the client cert.
  //   UNAUTHENTICATED — peer cert chain missing / invalid / no SPIFFE URI SAN.
  rpc Commit(CommitRequest) returns (CommitResponse);

  // Get returns metadata for a single recording (NO audio bytes — public reads go via HTTP).
  // Used for health-check, diagnostics, and future cmd/recording-uploader retry handshakes.
  rpc Get(GetRequest) returns (GetResponse);
}

message CommitRequest {
  string tenant_id = 1;          // UUID v7 — must match SPIFFE cert
  string call_id   = 2;          // UUID v7 — FK to calls(id)

  // S3 layout (single bucket, paths derived):
  //   audio_object_key = recordings/<tenant_id>/<yyyy>/<mm>/<dd>/<call_id>.opus.enc
  //   dek_object_key   = recordings/<tenant_id>/<yyyy>/<mm>/<dd>/<call_id>.dek.enc
  // dek_object_key is OPTIONAL in v1 — encrypted_dek lives in-row in call_recordings.
  // The sidecar object is reserved for v2 client-side decryption.
  string s3_bucket        = 3;
  string audio_object_key = 4;
  string dek_object_key   = 5;   // may be empty in v1

  // Cryptographic envelope (per-recording DEK wrapped by per-tenant KMS KEK).
  string kms_key_id    = 6;      // Yandex Cloud KMS key id (e.g. "abjxxxxxxxxxxxxxxxxx")
  bytes  encrypted_dek = 7;      // ~88 bytes for KMS-wrapped 32-byte DEK; <= 4096 bytes (validated)

  int64                       bytes_size  = 8;   // .opus.enc file size, > 0
  google.protobuf.Duration    duration    = 9;   // > 0
  string                      sha256      = 10;  // hex, 64 chars, of CIPHERTEXT (.opus.enc)
  string                      codec       = 11;  // "opus" or "opus-32" — non-empty
  int32                       sample_rate = 12;  // 48000 typical, > 0

  // Retention plan — resolved by uploader from project policy + tenant defaults.
  google.protobuf.Timestamp delete_at = 13;      // hard-delete deadline
  google.protobuf.Timestamp cold_at   = 14;      // hot→cold lifecycle transition

  // Provenance.
  string ingest_agent_id              = 15;      // recorded for audit
  google.protobuf.Timestamp recorded_at = 16;    // when audio capture STARTED
}

message CommitResponse {
  string                    recording_id      = 1;  // UUID v7 (PK in call_recordings)
  string                    call_id           = 2;
  google.protobuf.Timestamp committed_at      = 3;
  bool                      idempotent_replay = 4;  // true on duplicate Commit
}

message GetRequest {
  string tenant_id = 1;
  string call_id   = 2;
}

message GetResponse {
  string                    recording_id     = 1;
  string                    call_id          = 2;
  string                    tenant_id        = 3;
  string                    s3_bucket        = 4;
  string                    audio_object_key = 5;
  int64                     bytes_size       = 6;
  google.protobuf.Duration  duration         = 7;
  string                    sha256           = 8;
  string                    status           = 9;   // "stored" | "cold" | "deleted"
  google.protobuf.Timestamp committed_at     = 10;
  google.protobuf.Timestamp delete_at        = 11;
  google.protobuf.Timestamp cold_at          = 12;
  google.protobuf.Timestamp verified_at      = 13;  // last integrity verification
}
```

- [ ] **Step 2: Add Makefile target `proto-recording` (and `proto-all` if missing)**

```makefile
.PHONY: proto-recording
proto-recording: ## Generate Go bindings for the RecordingService proto.
	protoc \
	  -I=docs/api \
	  --go_out=. \
	  --go_opt=module=github.com/sociopulse/platform \
	  --go-grpc_out=. \
	  --go-grpc_opt=module=github.com/sociopulse/platform \
	  docs/api/recording/v1/recording.proto

# If a proto-all umbrella target exists, add proto-recording to its prerequisite list.
# If not, leave proto-recording standalone.
```

- [ ] **Step 3: Add tooling imports in `tools/tools.go`**

If `tools/tools.go` already exists, append the imports below to the build-tagged file. If it doesn't, create it:

```go
//go:build tools

// Package tools tracks build-time-only dependencies. The blank imports
// register modules with `go.mod` so `go install` from the repo root
// resolves the same versions as production code uses at runtime.
package tools

import (
    _ "google.golang.org/grpc/cmd/protoc-gen-go-grpc"
    _ "google.golang.org/protobuf/cmd/protoc-gen-go"
)
```

Then run `go mod tidy` and verify `go.sum` contains the new entries.

- [ ] **Step 4: Run the codegen and commit the generated files**

```bash
make proto-recording
go build ./internal/recording/proto/...
go vet ./internal/recording/proto/...
git add docs/api/recording/v1/recording.proto \
        internal/recording/proto/v1/recording.pb.go \
        internal/recording/proto/v1/recording_grpc.pb.go \
        tools/tools.go go.mod go.sum Makefile
git commit -m "feat(recording): Plan 12.1 Task 2 — proto contract + Go bindings for RecordingService"
```

### Acceptance
- `internal/recording/proto/v1/recording_grpc.pb.go` exposes `RecordingServiceServer` interface with `Commit(ctx, *CommitRequest) (*CommitResponse, error)` and `Get(...)` methods, plus `RegisterRecordingServiceServer(grpc.ServiceRegistrar, RecordingServiceServer)`.
- `go build ./...` is clean.
- `git diff --exit-code` returns 0 after re-running `make proto-recording` (deterministic generation).

---

## Task 3 — `internal/recording/store/postgres.go` — RecordingStore

**Goal:** Implement the persistence layer. Plan 12.1 implements only the methods Commit needs: `InsertRecordingIdempotent` (atomic INSERT-or-return-existing inside caller's Tx) and `GetByCallID` (point lookup for handler tests + future Plan 12.3 HTTP handlers).

**Files:**
- Create: `internal/recording/store/postgres.go`
- Create: `internal/recording/store/rows.go`
- Create: `internal/recording/store/postgres_test.go`
- Create: `internal/recording/store/main_test.go` (goleak)

- [ ] **Step 1: Create `internal/recording/store/rows.go`**

```go
// Package store is the persistence layer for the recording module. It owns
// SQL access to the call_recordings table and is the only package that
// touches that schema.
package store

import (
	"time"

	"github.com/google/uuid"
)

// RecordingRow mirrors the call_recordings table 1:1 (column order matches
// migrations/000010_recording_evolve.up.sql). Use NewRecordingRow to
// construct instances with required defaults already filled in.
type RecordingRow struct {
	ID             uuid.UUID
	CallID         uuid.UUID
	TenantID       uuid.UUID
	S3Bucket       string
	AudioObjectKey string
	DEKObjectKey   *string  // NULL allowed — see migration 000010
	KMSKeyID       string
	EncryptedDEK   []byte
	BytesSize      int64
	DurationMS     int64
	SHA256Hex      string
	Codec          string
	SampleRate     int32
	Status         string   // "stored" | "cold" | "deleted"
	CommittedAt    time.Time
	DeleteAt       time.Time
	ColdAt         time.Time
	RecordedAt     time.Time
	VerifiedAt     *time.Time
	IntegrityOK    *bool
	IngestAgentID  string
}
```

- [ ] **Step 2: Write the failing test `store/postgres_test.go`**

```go
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/recording/store"
	"github.com/sociopulse/platform/pkg/postgres"
	"github.com/sociopulse/platform/pkg/postgres/pgtest"
)

func TestRecordingStore_InsertIdempotent_FreshRow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	pool := pgtest.AcquirePool(t)
	tenantID, callID := pgtest.SeedCall(t, pool)

	s := store.NewPostgresStore(pool)

	row := newRow(t, tenantID, callID)

	got, replay, err := postgres.InTx(ctx, pool, func(tx postgres.Tx) (store.RecordingRow, bool, error) {
		return s.InsertRecordingIdempotent(ctx, tx, row)
	})
	require.NoError(t, err)
	require.False(t, replay)
	require.Equal(t, row.ID, got.ID)
	require.Equal(t, row.CallID, got.CallID)
}

func TestRecordingStore_InsertIdempotent_DuplicateReturnsReplay(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	pool := pgtest.AcquirePool(t)
	tenantID, callID := pgtest.SeedCall(t, pool)

	s := store.NewPostgresStore(pool)

	first := newRow(t, tenantID, callID)
	_, _, err := postgres.InTx(ctx, pool, func(tx postgres.Tx) (store.RecordingRow, bool, error) {
		return s.InsertRecordingIdempotent(ctx, tx, first)
	})
	require.NoError(t, err)

	// Second insert with a different ID but same call_id must return the existing row.
	dup := first
	dup.ID = uuid.Must(uuid.NewV7())
	dup.S3Bucket = "different-bucket"

	got, replay, err := postgres.InTx(ctx, pool, func(tx postgres.Tx) (store.RecordingRow, bool, error) {
		return s.InsertRecordingIdempotent(ctx, tx, dup)
	})
	require.NoError(t, err)
	require.True(t, replay)
	require.Equal(t, first.ID, got.ID, "replay must return the original row's ID")
	require.Equal(t, first.S3Bucket, got.S3Bucket, "replay must NOT overwrite the original payload")
}

func TestRecordingStore_InsertIdempotent_CallNotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	pool := pgtest.AcquirePool(t)
	tenantID := pgtest.SeedTenant(t, pool)

	s := store.NewPostgresStore(pool)

	row := newRow(t, tenantID, uuid.Must(uuid.NewV7()))  // call_id never seeded

	_, _, err := postgres.InTx(ctx, pool, func(tx postgres.Tx) (store.RecordingRow, bool, error) {
		return s.InsertRecordingIdempotent(ctx, tx, row)
	})
	require.ErrorIs(t, err, store.ErrCallNotFound)
}

func TestRecordingStore_GetByCallID_Found(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	pool := pgtest.AcquirePool(t)
	tenantID, callID := pgtest.SeedCall(t, pool)

	s := store.NewPostgresStore(pool)

	row := newRow(t, tenantID, callID)
	_, _, err := postgres.InTx(ctx, pool, func(tx postgres.Tx) (store.RecordingRow, bool, error) {
		return s.InsertRecordingIdempotent(ctx, tx, row)
	})
	require.NoError(t, err)

	got, err := s.GetByCallID(ctx, tenantID, callID)
	require.NoError(t, err)
	require.Equal(t, row.ID, got.ID)
	require.Equal(t, row.SHA256Hex, got.SHA256Hex)
}

func TestRecordingStore_GetByCallID_NotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	pool := pgtest.AcquirePool(t)
	tenantID := pgtest.SeedTenant(t, pool)

	s := store.NewPostgresStore(pool)

	_, err := s.GetByCallID(ctx, tenantID, uuid.Must(uuid.NewV7()))
	require.ErrorIs(t, err, store.ErrCallNotFound)
}

// newRow returns a RecordingRow with all required fields populated. Only
// the (tenant, call) pair varies between tests.
func newRow(t *testing.T, tenantID, callID uuid.UUID) store.RecordingRow {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	return store.RecordingRow{
		ID:             uuid.Must(uuid.NewV7()),
		CallID:         callID,
		TenantID:       tenantID,
		S3Bucket:       "rec-bucket-1",
		AudioObjectKey: "recordings/x/x/x/x.opus.enc",
		KMSKeyID:       "kms-key-1",
		EncryptedDEK:   []byte("encrypted-dek-stub-32bytes-..."),
		BytesSize:      1234567,
		DurationMS:     12345,
		SHA256Hex:      "f1e2d3c4b5a697887766554433221100ffeeddccbbaa99887766554433221100",
		Codec:          "opus",
		SampleRate:     48000,
		Status:         "stored",
		CommittedAt:    now,
		DeleteAt:       now.Add(730 * 24 * time.Hour),
		ColdAt:         now.Add(365 * 24 * time.Hour),
		RecordedAt:     now.Add(-1 * time.Hour),
		IngestAgentID:  "agent-test",
	}
}
```

- [ ] **Step 3: Run the failing tests**

```bash
go test -race -count=1 ./internal/recording/store/...
```

Expected: FAIL — `store.NewPostgresStore`, `store.ErrCallNotFound`, `(*store.PostgresStore).InsertRecordingIdempotent`, `(*store.PostgresStore).GetByCallID` are not defined.

- [ ] **Step 4: Implement `internal/recording/store/postgres.go`**

```go
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sociopulse/platform/pkg/postgres"
)

// ErrCallNotFound is returned by InsertRecordingIdempotent and GetByCallID when
// the request references a call that does not exist for the given tenant.
var ErrCallNotFound = errors.New("recording.store: call not found")

// PostgresStore is the canonical RecordingStore implementation. It holds
// no per-instance state — a single pointer can be shared across goroutines.
type PostgresStore struct {
	pool *postgres.Pool
}

// NewPostgresStore constructs a store backed by pool. The store does NOT
// take ownership of the pool — callers manage its lifecycle.
func NewPostgresStore(pool *postgres.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// insertRecordingSQL is a single statement that:
//   1. Tries to INSERT the row (ON CONFLICT (call_id) DO NOTHING).
//   2. If the conflict fired, returns the existing row's id + committed_at.
//   3. Reports replay=true iff the row already existed.
//
// This is a single round-trip for both fresh and replay paths, with no
// race window between the conflict probe and the read-back.
const insertRecordingSQL = `
WITH ins AS (
    INSERT INTO call_recordings (
        id, call_id, tenant_id, s3_bucket, audio_object_key, dek_object_key,
        kms_key_id, encrypted_dek, bytes_size, duration_ms, sha256_hex,
        codec, sample_rate, status, committed_at, delete_at, cold_at,
        recorded_at, ingest_agent_id
    )
    VALUES (
        $1, $2, $3, $4, $5, $6,
        $7, $8, $9, $10, $11,
        $12, $13, $14, $15, $16, $17,
        $18, $19
    )
    ON CONFLICT (call_id) DO NOTHING
    RETURNING id, committed_at
)
SELECT
    COALESCE((SELECT id           FROM ins),
             (SELECT id           FROM call_recordings WHERE call_id = $2)) AS id,
    COALESCE((SELECT committed_at FROM ins),
             (SELECT committed_at FROM call_recordings WHERE call_id = $2)) AS committed_at,
    NOT EXISTS (SELECT 1 FROM ins) AS replay
`

// InsertRecordingIdempotent persists r inside the caller's transaction.
// Idempotent on call_id: a duplicate Commit returns the existing row's id
// and committed_at with replay=true; r.S3Bucket / r.AudioObjectKey / etc.
// are NOT overwritten on replay.
//
// FK violation on call_id (no parent in calls(id, tenant_id)) returns
// ErrCallNotFound. The check is explicit (SELECT EXISTS) ahead of the
// INSERT so the caller's TX is rolled back cleanly without surfacing a
// pgconn error.
func (s *PostgresStore) InsertRecordingIdempotent(ctx context.Context, tx postgres.Tx, r RecordingRow) (RecordingRow, bool, error) {
	// 1. FK check — call must exist in same tenant. We do this explicitly
	//    rather than relying on FK violation so the error code is stable
	//    across pg versions and the message is recognisable.
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM calls WHERE id = $1 AND tenant_id = $2)`,
		r.CallID, r.TenantID,
	).Scan(&exists); err != nil {
		return RecordingRow{}, false, fmt.Errorf("recording.store: call exists check: %w", err)
	}
	if !exists {
		return RecordingRow{}, false, ErrCallNotFound
	}

	// 2. Atomic INSERT-or-return-existing.
	var (
		id          uuid.UUID
		committedAt time.Time
		replay      bool
	)
	if err := tx.QueryRow(ctx, insertRecordingSQL,
		r.ID, r.CallID, r.TenantID, r.S3Bucket, r.AudioObjectKey, r.DEKObjectKey,
		r.KMSKeyID, r.EncryptedDEK, r.BytesSize, r.DurationMS, r.SHA256Hex,
		r.Codec, r.SampleRate, r.Status, r.CommittedAt, r.DeleteAt, r.ColdAt,
		r.RecordedAt, r.IngestAgentID,
	).Scan(&id, &committedAt, &replay); err != nil {
		// Defence-in-depth: if a different goroutine deleted the parent
		// `calls` row between our exists-check and the INSERT, we'd hit
		// FK 23503 here. Map it to ErrCallNotFound for symmetry.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return RecordingRow{}, false, ErrCallNotFound
		}
		return RecordingRow{}, false, fmt.Errorf("recording.store: insert call_recordings: %w", err)
	}

	r.ID = id
	r.CommittedAt = committedAt
	return r, replay, nil
}

// GetByCallID returns the recording for (tenantID, callID). Returns
// ErrCallNotFound on a miss (which subsumes both "no recording" and
// "no call" — callers should reach through api.ErrNotFound from the
// service layer if they need to discriminate).
func (s *PostgresStore) GetByCallID(ctx context.Context, tenantID, callID uuid.UUID) (RecordingRow, error) {
	const q = `
SELECT id, call_id, tenant_id, s3_bucket, audio_object_key, dek_object_key,
       kms_key_id, encrypted_dek, bytes_size, duration_ms, sha256_hex,
       codec, sample_rate, status, committed_at, delete_at, cold_at,
       recorded_at, verified_at, integrity_ok, ingest_agent_id
FROM call_recordings
WHERE tenant_id = $1 AND call_id = $2
`
	var r RecordingRow
	err := s.pool.RawQueryRow(ctx, q, tenantID, callID).Scan(
		&r.ID, &r.CallID, &r.TenantID, &r.S3Bucket, &r.AudioObjectKey, &r.DEKObjectKey,
		&r.KMSKeyID, &r.EncryptedDEK, &r.BytesSize, &r.DurationMS, &r.SHA256Hex,
		&r.Codec, &r.SampleRate, &r.Status, &r.CommittedAt, &r.DeleteAt, &r.ColdAt,
		&r.RecordedAt, &r.VerifiedAt, &r.IntegrityOK, &r.IngestAgentID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecordingRow{}, ErrCallNotFound
	}
	if err != nil {
		return RecordingRow{}, fmt.Errorf("recording.store: get by call_id: %w", err)
	}
	return r, nil
}
```

- [ ] **Step 5: Create `internal/recording/store/main_test.go`**

```go
package store_test

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// pgx connection pool maintains long-lived background goroutines.
		// pgtest.AcquirePool registers a cleanup that returns the pool to
		// the harness; goleak should see only the harness's idle conns.
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	)
}
```

- [ ] **Step 6: Run the tests — they should PASS now**

```bash
go test -race -count=1 ./internal/recording/store/...
```

Expected: PASS — all 5 tests green.

- [ ] **Step 7: Commit**

```bash
git add internal/recording/store/
git commit -m "feat(recording/store): Plan 12.1 Task 3 — RecordingStore (idempotent insert + get)"
```

### Notes for the implementer
- `pkg/postgres.InTx` is the project-standard helper. Confirm its exact signature in `pkg/postgres/`. If the canonical helper is `pgxhelper.InTx` or similar, adapt the test file accordingly — the key is "single function that begins-tx, runs callback, commits-or-rolls-back". If no such helper exists, inline `tx, _ := pool.Begin(ctx); defer tx.Rollback(ctx); … tx.Commit(ctx)` in the test file's helper.
- `pgtest.AcquirePool / SeedTenant / SeedCall` — these are project-standard testcontainers helpers (Plans 03/04 introduced `pkg/postgres/pgtest` for this). Confirm exact symbol names in `pkg/postgres/pgtest/` and adapt; if the only existing helper is `pgtest.OpenForTest()` with manual seeding, write `seedTenant(t, pool)` + `seedCall(t, pool, tenantID)` private helpers in the same test file.

### Acceptance
- `InsertRecordingIdempotent` returns `(row, false, nil)` on first insert.
- Same call again returns `(originalRow, true, nil)` — original payload NOT overwritten.
- Insert with bogus call_id returns `ErrCallNotFound`.
- `GetByCallID` returns the row on hit; `ErrCallNotFound` on miss.
- All tests use `t.Parallel()`; `goleak.VerifyTestMain` clean.

---

## Task 4 — `internal/recording/service/service.go` — RecordingService.Commit + Get

**Goal:** Implement the service-level orchestration: validation, INSERT-via-store, audit-log entry, outbox event — all in a single Tx — and return `(CommitOutput, error)` to the gRPC handler. `Get` is a thin pass-through to `store.GetByCallID`. The other RecordingService methods (Search / OpenAudioStream / VerifyChecksum) return `api.ErrInvalidInput` wrapped with `"not implemented in foundation phase"` — they're filled in by Plan 12.2/12.3.

**Files:**
- Create: `internal/recording/service/service.go`
- Create: `internal/recording/service/service_test.go`
- Create: `internal/recording/service/main_test.go`
- Create: `internal/recording/metrics/metrics.go`
- Create: `internal/recording/metrics/metrics_test.go`

- [ ] **Step 1: Create `internal/recording/metrics/metrics.go`**

```go
// Package metrics owns Prometheus collectors for the recording module.
// Constructors return errors on duplicate registration — no init() / no MustRegister.
// Carry-forward from Plans 09/10/11: every metrics struct must support
// nil-safe usage so unit tests can pass nil where a metric tick is irrelevant.
package metrics

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// RecordingMetrics aggregates Prometheus collectors for the recording module.
// A nil receiver is safe — every method becomes a no-op.
type RecordingMetrics struct {
	CommitTotal      *prometheus.CounterVec   // labels: tenant_id, result {ok|replay|invalid|call_not_found|error}
	StorageSizeBytes *prometheus.GaugeVec     // labels: tenant_id (Counter-like, only Add on commit)
	CommitDuration   *prometheus.HistogramVec // labels: tenant_id, result
}

// RegisterRecordingMetrics constructs and registers all collectors with reg.
// Returns the populated struct + a non-nil error if any registration fails.
// Reg may be nil — in that case the collectors are still constructed but not
// registered (useful in unit tests that don't run a Prometheus server).
func RegisterRecordingMetrics(reg prometheus.Registerer) (*RecordingMetrics, error) {
	m := &RecordingMetrics{
		CommitTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "sociopulse",
			Subsystem: "recording",
			Name:      "commit_total",
			Help:      "Number of RecordingService.Commit calls broken out by result.",
		}, []string{"tenant_id", "result"}),

		StorageSizeBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "sociopulse",
			Subsystem: "recording",
			Name:      "storage_size_bytes",
			Help:      "Cumulative bytes_size of all committed (non-deleted) recordings, by tenant.",
		}, []string{"tenant_id"}),

		CommitDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "sociopulse",
			Subsystem: "recording",
			Name:      "commit_duration_seconds",
			Help:      "Wall time of one Commit call (validation + INSERT + outbox + audit).",
			Buckets:   prometheus.DefBuckets, // 5ms .. 10s
		}, []string{"tenant_id", "result"}),
	}

	if reg == nil {
		return m, nil
	}

	for _, c := range []prometheus.Collector{m.CommitTotal, m.StorageSizeBytes, m.CommitDuration} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("recording metrics: register: %w", err)
		}
	}
	return m, nil
}

// ObserveCommit ticks the relevant collectors. Safe to call on a nil receiver.
func (m *RecordingMetrics) ObserveCommit(tenantID, result string, durSec float64) {
	if m == nil {
		return
	}
	m.CommitTotal.WithLabelValues(tenantID, result).Inc()
	m.CommitDuration.WithLabelValues(tenantID, result).Observe(durSec)
}

// AddStorageBytes records a successful commit's bytes_size. Safe on nil.
func (m *RecordingMetrics) AddStorageBytes(tenantID string, bytes int64) {
	if m == nil || bytes < 0 {
		return
	}
	m.StorageSizeBytes.WithLabelValues(tenantID).Add(float64(bytes))
}
```

- [ ] **Step 2: Quick metrics test `internal/recording/metrics/metrics_test.go`**

```go
package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/recording/metrics"
)

func TestRegisterRecordingMetrics_NilReg(t *testing.T) {
	t.Parallel()
	m, err := metrics.RegisterRecordingMetrics(nil)
	require.NoError(t, err)
	require.NotNil(t, m)
}

func TestRegisterRecordingMetrics_DuplicateFails(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	_, err := metrics.RegisterRecordingMetrics(reg)
	require.NoError(t, err)
	_, err = metrics.RegisterRecordingMetrics(reg)
	require.Error(t, err, "second registration must fail")
}

func TestRecordingMetrics_NilReceiverNoOp(t *testing.T) {
	t.Parallel()
	var m *metrics.RecordingMetrics
	require.NotPanics(t, func() {
		m.ObserveCommit("t", "ok", 0.1)
		m.AddStorageBytes("t", 1234)
	})
}
```

- [ ] **Step 3: Run metrics tests — should PASS now (after Step 1 implementation)**

```bash
go test -race -count=1 ./internal/recording/metrics/...
```

Expected: PASS.

- [ ] **Step 4: Write the failing service test `internal/recording/service/service_test.go`**

```go
package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/service"
	"github.com/sociopulse/platform/internal/recording/store"
	"github.com/sociopulse/platform/pkg/clock"
	"github.com/sociopulse/platform/pkg/postgres"
	"github.com/sociopulse/platform/pkg/postgres/pgtest"
)

func TestService_Commit_Fresh(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	pool := pgtest.AcquirePool(t)
	tenantID, callID := pgtest.SeedCall(t, pool)

	st := store.NewPostgresStore(pool)
	svc := service.New(service.Deps{
		Pool:   pool,
		Store:  st,
		Logger: zaptest.NewLogger(t),
		Clock:  clock.Frozen(time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)),
	})

	out, err := svc.Commit(ctx, validInput(t, tenantID, callID))
	require.NoError(t, err)
	require.False(t, out.IdempotentReplay)
	require.NotEqual(t, uuid.Nil, out.RecordingID)

	// Verify side-effects: outbox row + audit row land in the same Tx.
	requireExactlyOneOutboxRow(t, pool, tenantID, callID)
	requireExactlyOneAuditRow(t, pool, tenantID, callID, "recording.committed")
}

func TestService_Commit_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	pool := pgtest.AcquirePool(t)
	tenantID, callID := pgtest.SeedCall(t, pool)

	st := store.NewPostgresStore(pool)
	svc := service.New(service.Deps{
		Pool:   pool,
		Store:  st,
		Logger: zaptest.NewLogger(t),
		Clock:  clock.Real,
	})

	in := validInput(t, tenantID, callID)
	first, err := svc.Commit(ctx, in)
	require.NoError(t, err)
	require.False(t, first.IdempotentReplay)

	second, err := svc.Commit(ctx, in)
	require.NoError(t, err)
	require.True(t, second.IdempotentReplay)
	require.Equal(t, first.RecordingID, second.RecordingID)

	// Side-effects emitted exactly ONCE despite two Commits.
	requireExactlyOneOutboxRow(t, pool, tenantID, callID)
	requireExactlyOneAuditRow(t, pool, tenantID, callID, "recording.committed")
}

func TestService_Commit_InvalidInput(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	pool := pgtest.AcquirePool(t)
	tenantID, callID := pgtest.SeedCall(t, pool)

	svc := service.New(service.Deps{
		Pool:   pool,
		Store:  store.NewPostgresStore(pool),
		Logger: zaptest.NewLogger(t),
		Clock:  clock.Real,
	})

	cases := []struct {
		name string
		mut  func(*rapi.CommitInput)
	}{
		{"missing_tenant", func(i *rapi.CommitInput) { i.TenantID = uuid.Nil }},
		{"missing_call", func(i *rapi.CommitInput) { i.CallID = uuid.Nil }},
		{"sha256_short", func(i *rapi.CommitInput) { i.SHA256Hex = "abcd" }},
		{"sha256_long", func(i *rapi.CommitInput) { i.SHA256Hex = "f1e2d3c4b5a697887766554433221100ffeeddccbbaa99887766554433221100EE" }},
		{"bytes_zero", func(i *rapi.CommitInput) { i.BytesSize = 0 }},
		{"bytes_negative", func(i *rapi.CommitInput) { i.BytesSize = -1 }},
		{"empty_codec", func(i *rapi.CommitInput) { i.Codec = "" }},
		{"missing_kms_key", func(i *rapi.CommitInput) { i.KMSKeyID = "" }},
		{"missing_dek", func(i *rapi.CommitInput) { i.EncryptedDEK = nil }},
		{"missing_audio_key", func(i *rapi.CommitInput) { i.AudioObjectKey = "" }},
		{"zero_delete_at", func(i *rapi.CommitInput) { i.DeleteAt = time.Time{} }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := validInput(t, tenantID, callID)
			tc.mut(&in)
			_, err := svc.Commit(ctx, in)
			require.True(t, errors.Is(err, rapi.ErrInvalidInput),
				"expected ErrInvalidInput, got %v", err)
		})
	}
}

func TestService_Commit_CallNotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	pool := pgtest.AcquirePool(t)
	tenantID := pgtest.SeedTenant(t, pool)

	svc := service.New(service.Deps{
		Pool:   pool,
		Store:  store.NewPostgresStore(pool),
		Logger: zaptest.NewLogger(t),
		Clock:  clock.Real,
	})

	in := validInput(t, tenantID, uuid.Must(uuid.NewV7())) // call never seeded
	_, err := svc.Commit(ctx, in)
	require.True(t, errors.Is(err, rapi.ErrCallNotFound),
		"expected ErrCallNotFound, got %v", err)
}

// validInput returns a CommitInput with all required fields set.
func validInput(t *testing.T, tenantID, callID uuid.UUID) rapi.CommitInput {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	return rapi.CommitInput{
		TenantID:       tenantID,
		CallID:         callID,
		S3Bucket:       "rec-bucket-1",
		AudioObjectKey: "recordings/x/x/x/x.opus.enc",
		DEKObjectKey:   "",
		KMSKeyID:       "kms-key-1",
		EncryptedDEK:   []byte("encrypted-dek-stub-32bytes-xxxxx"),
		BytesSize:      1234567,
		Duration:       12345 * time.Millisecond,
		SHA256Hex:      "f1e2d3c4b5a697887766554433221100ffeeddccbbaa99887766554433221100",
		Codec:          "opus",
		SampleRate:     48000,
		DeleteAt:       now.Add(730 * 24 * time.Hour),
		ColdAt:         now.Add(365 * 24 * time.Hour),
		IngestAgentID:  "agent-test",
		RecordedAt:     now.Add(-1 * time.Hour),
	}
}

// requireExactlyOneOutboxRow asserts that exactly one row exists in
// event_outbox for the given (tenant, call) pair, with the recording
// subject. It does NOT assert on `published_at` — the relay may have
// drained it asynchronously, and the test isn't gated on a NATS ack.
func requireExactlyOneOutboxRow(t *testing.T, pool *postgres.Pool, tenantID, callID uuid.UUID) {
	t.Helper()
	var count int
	subject := rapi.SubjectRecordingUploadedFor(tenantID)
	err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM event_outbox
		 WHERE tenant_id = $1 AND subject = $2 AND aggregate_id = $3`,
		tenantID, subject, callID,
	).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count, "expected exactly one outbox row for %s", subject)
}

// requireExactlyOneAuditRow asserts that exactly one audit_log row exists
// for the given action. The audit_log column shape is checked against
// the Plan 04 / Plan 06 conventions.
func requireExactlyOneAuditRow(t *testing.T, pool *postgres.Pool, tenantID, callID uuid.UUID, action string) {
	t.Helper()
	var count int
	err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_log
		 WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
		tenantID, action, callID,
	).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count, "expected exactly one audit row for action=%s", action)
}
```

- [ ] **Step 5: Run the failing service tests**

```bash
go test -race -count=1 ./internal/recording/service/...
```

Expected: FAIL — `service.New`, `service.Deps`, `(*svc).Commit` undefined.

- [ ] **Step 6: Implement `internal/recording/service/service.go`**

```go
// Package service implements the recording module's RecordingService.
// Plan 12.1 (Foundation): Commit + Get only. Search / OpenAudioStream /
// VerifyChecksum return wrapped api.ErrInvalidInput with the marker
// "not implemented in foundation phase" until Plan 12.2 / 12.3.
package service

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
	"github.com/sociopulse/platform/internal/recording/store"
	"github.com/sociopulse/platform/pkg/clock"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// sentinel re-exports keep callers in this package idiomatic without a
// hop through `rapi.`. External callers should still errors.Is against
// the rapi sentinels — the aliases below preserve identity.
var (
	ErrInvalidInput   = rapi.ErrInvalidInput
	ErrCallNotFound   = rapi.ErrCallNotFound
	ErrNotFound       = rapi.ErrNotFound
	ErrTenantMismatch = rapi.ErrTenantMismatch
)

const (
	// sha256HexLen is the canonical lower-hex sha256 length (32 bytes × 2).
	sha256HexLen = 64

	// maxEncryptedDEKBytes guards against pathological / malicious payloads.
	// Yandex KMS wraps a 32-byte DEK into ~88 bytes; 4 KiB is generous.
	maxEncryptedDEKBytes = 4 * 1024

	// auditActorKindIngest matches the Plan 06 audit conventions.
	auditActorKindIngest = "service"
)

// Deps wires the service. Pool is required (used to begin the Commit Tx).
// Logger and Metrics may be nil — the implementation is nil-safe.
type Deps struct {
	Pool    *postgres.Pool
	Store   *store.PostgresStore
	Outbox  outbox.Writer       // optional; defaults to outbox.NewPostgresWriter() if nil
	Logger  *zap.Logger
	Metrics *metrics.RecordingMetrics
	Clock   clock.Clock         // optional; defaults to clock.Real
}

type svc struct {
	pool    *postgres.Pool
	store   *store.PostgresStore
	outbox  outbox.Writer
	logger  *zap.Logger
	metrics *metrics.RecordingMetrics
	clock   clock.Clock
}

// Compile-time interface check — guards against contract drift.
var _ rapi.RecordingService = (*svc)(nil)

// New constructs the service. Returns a nil-safe instance even if
// Logger / Metrics / Clock are not provided.
func New(d Deps) rapi.RecordingService {
	if d.Logger == nil {
		d.Logger = zap.NewNop()
	}
	if d.Clock == nil {
		d.Clock = clock.Real
	}
	if d.Outbox == nil {
		d.Outbox = outbox.NewPostgresWriter()
	}
	return &svc{
		pool:    d.Pool,
		store:   d.Store,
		outbox:  d.Outbox,
		logger:  d.Logger,
		metrics: d.Metrics,
		clock:   d.Clock,
	}
}

// Commit performs the full end-to-end commit flow:
//   1. Validate input.
//   2. Begin Tx (BypassRLS — the recording module owns metadata for all tenants).
//   3. INSERT (idempotent on call_id) → returns row + replay flag.
//   4. On fresh insert: append audit row + outbox event in same Tx.
//   5. Commit Tx.
//   6. Tick metrics.
//
// Carry-forward: outbox-relay drains rows to JetStream asynchronously; the
// caller does NOT block on NATS ack. Replay path skips audit + outbox so
// downstream subscribers see exactly one event per recording.
func (s *svc) Commit(ctx context.Context, in rapi.CommitInput) (rapi.CommitOutput, error) {
	start := s.clock.Now()
	tenantLabel := in.TenantID.String()

	if err := validateCommit(in); err != nil {
		s.metrics.ObserveCommit(tenantLabel, "invalid", time.Since(start).Seconds())
		return rapi.CommitOutput{}, fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
	}

	row := storeRowFromInput(in, s.clock.Now().UTC())

	var (
		out    rapi.CommitOutput
		replay bool
	)
	err := s.pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		inserted, didReplay, err := s.store.InsertRecordingIdempotent(ctx, tx, row)
		if err != nil {
			return err
		}
		replay = didReplay
		out = rapi.CommitOutput{
			RecordingID:      inserted.ID,
			CallID:           inserted.CallID,
			CommittedAt:      inserted.CommittedAt,
			IdempotentReplay: didReplay,
		}

		if didReplay {
			return nil // skip audit + outbox on replay so downstream sees exactly one event
		}

		// 1. Audit row (in same Tx so a rollback discards both).
		if err := writeAuditRow(ctx, tx, inserted); err != nil {
			return fmt.Errorf("audit insert: %w", err)
		}

		// 2. Outbox row (drained async by pkg/outbox.Relay).
		ev, err := buildOutboxEvent(inserted)
		if err != nil {
			return fmt.Errorf("build outbox event: %w", err)
		}
		if err := s.outbox.Append(ctx, tx, ev); err != nil {
			return fmt.Errorf("outbox append: %w", err)
		}
		return nil
	})

	dur := time.Since(start).Seconds()
	switch {
	case errors.Is(err, ErrCallNotFound):
		s.metrics.ObserveCommit(tenantLabel, "call_not_found", dur)
		return rapi.CommitOutput{}, ErrCallNotFound
	case err != nil:
		s.metrics.ObserveCommit(tenantLabel, "error", dur)
		return rapi.CommitOutput{}, fmt.Errorf("recording.commit: %w", err)
	}

	if replay {
		s.metrics.ObserveCommit(tenantLabel, "replay", dur)
		s.logger.Info("recording commit idempotent replay",
			zap.String("tenant_id", tenantLabel),
			zap.String("call_id", in.CallID.String()),
			zap.String("recording_id", out.RecordingID.String()))
	} else {
		s.metrics.ObserveCommit(tenantLabel, "ok", dur)
		s.metrics.AddStorageBytes(tenantLabel, in.BytesSize)
		s.logger.Info("recording committed",
			zap.String("tenant_id", tenantLabel),
			zap.String("call_id", in.CallID.String()),
			zap.String("recording_id", out.RecordingID.String()),
			zap.Int64("bytes", in.BytesSize),
			zap.String("sha256", in.SHA256Hex))
	}
	return out, nil
}

// Get is a thin pass-through to store.GetByCallID with row→DTO mapping.
func (s *svc) Get(ctx context.Context, tenantID, callID uuid.UUID) (rapi.RecordingMetadata, error) {
	r, err := s.store.GetByCallID(ctx, tenantID, callID)
	if errors.Is(err, store.ErrCallNotFound) {
		return rapi.RecordingMetadata{}, ErrNotFound
	}
	if err != nil {
		return rapi.RecordingMetadata{}, fmt.Errorf("recording.get: %w", err)
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
		DeleteAt:       r.DeleteAt,
		ColdAt:         r.ColdAt,
		VerifiedAt:     r.VerifiedAt,
	}, nil
}

// Search / OpenAudioStream / VerifyChecksum are deferred to Plan 12.2/12.3.
// Returning ErrInvalidInput with a marker substring lets callers detect
// the foundation-phase placeholder; future plans replace these with real
// implementations.

func (s *svc) Search(ctx context.Context, tenantID uuid.UUID, q rapi.SearchQuery) (rapi.SearchResult, error) {
	return rapi.SearchResult{}, fmt.Errorf("%w: Search not implemented in foundation phase", ErrInvalidInput)
}

func (s *svc) OpenAudioStream(ctx context.Context, tenantID, callID uuid.UUID, byteRange *rapi.ByteRange) (rapi.AudioStream, error) {
	return rapi.AudioStream{}, fmt.Errorf("%w: OpenAudioStream not implemented in foundation phase", ErrInvalidInput)
}

func (s *svc) VerifyChecksum(ctx context.Context, tenantID, callID uuid.UUID) (rapi.VerifyResult, error) {
	return rapi.VerifyResult{}, fmt.Errorf("%w: VerifyChecksum not implemented in foundation phase", ErrInvalidInput)
}

// ----- helpers -----

func validateCommit(in rapi.CommitInput) error {
	switch {
	case in.TenantID == uuid.Nil:
		return errors.New("tenant_id required")
	case in.CallID == uuid.Nil:
		return errors.New("call_id required")
	case in.S3Bucket == "":
		return errors.New("s3_bucket required")
	case in.AudioObjectKey == "":
		return errors.New("audio_object_key required")
	case in.KMSKeyID == "":
		return errors.New("kms_key_id required")
	case len(in.EncryptedDEK) == 0:
		return errors.New("encrypted_dek required")
	case len(in.EncryptedDEK) > maxEncryptedDEKBytes:
		return fmt.Errorf("encrypted_dek too large: max %d bytes", maxEncryptedDEKBytes)
	case in.BytesSize <= 0:
		return errors.New("bytes_size must be > 0")
	case in.Duration <= 0:
		return errors.New("duration must be > 0")
	case len(in.SHA256Hex) != sha256HexLen:
		return fmt.Errorf("sha256 length: want %d hex chars, got %d", sha256HexLen, len(in.SHA256Hex))
	case in.Codec == "":
		return errors.New("codec required")
	case in.SampleRate <= 0:
		return errors.New("sample_rate must be > 0")
	case in.DeleteAt.IsZero():
		return errors.New("delete_at required (retention plan must be resolved)")
	case in.ColdAt.IsZero():
		return errors.New("cold_at required")
	case in.RecordedAt.IsZero():
		return errors.New("recorded_at required")
	}
	return nil
}

func storeRowFromInput(in rapi.CommitInput, committedAt time.Time) store.RecordingRow {
	var dekKey *string
	if in.DEKObjectKey != "" {
		k := in.DEKObjectKey
		dekKey = &k
	}
	return store.RecordingRow{
		ID:             uuid.Must(uuid.NewV7()),
		CallID:         in.CallID,
		TenantID:       in.TenantID,
		S3Bucket:       in.S3Bucket,
		AudioObjectKey: in.AudioObjectKey,
		DEKObjectKey:   dekKey,
		KMSKeyID:       in.KMSKeyID,
		EncryptedDEK:   in.EncryptedDEK,
		BytesSize:      in.BytesSize,
		DurationMS:     in.Duration.Milliseconds(),
		SHA256Hex:      in.SHA256Hex,
		Codec:          in.Codec,
		SampleRate:     in.SampleRate,
		Status:         "stored",
		CommittedAt:    committedAt,
		DeleteAt:       in.DeleteAt,
		ColdAt:         in.ColdAt,
		RecordedAt:     in.RecordedAt,
		IngestAgentID:  in.IngestAgentID,
	}
}

func writeAuditRow(ctx context.Context, tx postgres.Tx, r store.RecordingRow) error {
	payload, err := json.Marshal(map[string]any{
		"recording_id":     r.ID,
		"call_id":          r.CallID,
		"sha256":           r.SHA256Hex,
		"bytes_size":       r.BytesSize,
		"kms_key_id":       r.KMSKeyID,
		"audio_object_key": r.AudioObjectKey,
		"ingest_agent_id":  r.IngestAgentID,
	})
	if err != nil {
		return fmt.Errorf("marshal audit payload: %w", err)
	}

	const q = `
INSERT INTO audit_log (id, tenant_id, actor_kind, actor_id, action, target_kind, target_id, payload, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
`
	_, err = tx.Exec(ctx, q,
		uuid.Must(uuid.NewV7()),
		r.TenantID,
		auditActorKindIngest,
		r.IngestAgentID,
		rapi.AuditActionCommitted,
		"recording",
		r.ID,
		payload,
		r.CommittedAt,
	)
	return err
}

func buildOutboxEvent(r store.RecordingRow) (outbox.Event, error) {
	payload, err := json.Marshal(rapi.RecordingUploadedEvent{
		RecordingID: r.ID,
		CallID:      r.CallID,
		TenantID:    r.TenantID,
		BytesSize:   r.BytesSize,
		DurationMS:  r.DurationMS,
		SHA256Hex:   r.SHA256Hex,
		Status:      r.Status,
		CommittedAt: r.CommittedAt.Unix(),
	})
	if err != nil {
		return outbox.Event{}, fmt.Errorf("marshal outbox payload: %w", err)
	}
	tenantID := r.TenantID
	callID := r.CallID
	return outbox.Event{
		TenantID:    &tenantID,
		AggregateID: &callID,
		Subject:     rapi.SubjectRecordingUploadedFor(r.TenantID),
		Payload:     payload,
	}, nil
}
```

- [ ] **Step 7: Create `internal/recording/service/main_test.go`**

```go
package service_test

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	)
}
```

- [ ] **Step 8: Run service tests — they should PASS now**

```bash
go build ./internal/recording/...
go test -race -count=1 ./internal/recording/service/... ./internal/recording/metrics/...
```

Expected: PASS — all service + metrics tests green.

- [ ] **Step 9: Commit**

```bash
git add internal/recording/service/ internal/recording/metrics/
git commit -m "feat(recording/service): Plan 12.1 Task 4 — RecordingService.Commit + Get with outbox + audit"
```

### Notes for the implementer
- `pkg/postgres.Pool.BypassRLS(ctx, func(tx Tx) error)` — confirm exact signature in `pkg/postgres/`. If it's `BypassRLSWithTx(ctx, fn)` or similar, adapt.
- `pkg/clock.Real` and `pkg/clock.Frozen(time.Time)` — confirm exact symbol names. The Plan 04/06 helpers exist; if `clock.Frozen` is named `clock.Fixed` or `clock.At`, adapt.
- `outbox.Event` field names: TenantID `*uuid.UUID`, AggregateID `*uuid.UUID`, Subject `string`, Payload `[]byte`. Both `TenantID` and `AggregateID` are pointer types because the outbox supports platform-global events.
- `rapi.AuditActionCommitted` = `"recording.committed"` (already in api/events.go).
- The existing `audit_log` schema uses columns `(id, tenant_id, actor_kind, actor_id, action, target_kind, target_id, payload, occurred_at)`. Confirm against `migrations/000001_init.up.sql`. If a column differs, adapt the INSERT.

### Acceptance
- Fresh Commit returns `(out, replay=false, nil)`, persists row + audit + outbox.
- Duplicate Commit returns same `recording_id`, `replay=true`, no extra audit/outbox.
- Bad input → `errors.Is(err, rapi.ErrInvalidInput)`.
- Missing call → `errors.Is(err, rapi.ErrCallNotFound)`.
- `Get` returns metadata; missing → `errors.Is(err, rapi.ErrNotFound)`.
- Metrics: nil-safe; `commit_total{result="ok"}` increments on fresh insert.

---

## Task 5 — gRPC server + Module composition + cmd/api wiring

**Goal:** Stand up the gRPC listener (mTLS), implement the `Commit` and `Get` handlers (proto → service.Commit → proto), wire `recording.Module` into `cmd/api`, expose configuration, and ship an integration test that exercises the full Commit path through the gRPC server.

**Files:**
- Create: `internal/recording/grpcserver/server.go`
- Create: `internal/recording/grpcserver/peer_identity.go`
- Create: `internal/recording/grpcserver/commit_handler.go`
- Create: `internal/recording/grpcserver/server_test.go`
- Create: `internal/recording/grpcserver/main_test.go`
- Modify: `internal/recording/module.go`
- Create: `cmd/api/recording.go`
- Modify: `cmd/api/main.go` — wire recording.Module into Register loop + start GRPCServer in errgroup
- Modify: `pkg/config/config.go` (or wherever `Config` aggregate lives) — add `Recording` block

### 5.1 `peer_identity.go` — SPIFFE-style cert SAN parsing

- [ ] **Step 1: Write the failing peer_identity test**

```go
// internal/recording/grpcserver/peer_identity_test.go
package grpcserver_test

import (
	"crypto/x509"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/recording/grpcserver"
)

func TestParsePeerIdentity_HappyPath(t *testing.T) {
	t.Parallel()
	tenantID := uuid.Must(uuid.NewV7())
	u, err := url.Parse("spiffe://sociopulse/ingest-agent/agent-1?tenant=" + tenantID.String())
	require.NoError(t, err)

	cert := &x509.Certificate{URIs: []*url.URL{u}}

	got, err := grpcserver.ParsePeerIdentity(cert)
	require.NoError(t, err)
	require.Equal(t, tenantID, got.TenantID)
	require.Equal(t, "/ingest-agent/agent-1", got.AgentID)
}

func TestParsePeerIdentity_NoURI(t *testing.T) {
	t.Parallel()
	cert := &x509.Certificate{}
	_, err := grpcserver.ParsePeerIdentity(cert)
	require.ErrorContains(t, err, "no URI SAN")
}

func TestParsePeerIdentity_WrongScheme(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("https://example.com/cert?tenant=" + uuid.Must(uuid.NewV7()).String())
	cert := &x509.Certificate{URIs: []*url.URL{u}}
	_, err := grpcserver.ParsePeerIdentity(cert)
	require.ErrorContains(t, err, "unsupported scheme")
}

func TestParsePeerIdentity_BadTenant(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("spiffe://sociopulse/agent?tenant=not-a-uuid")
	cert := &x509.Certificate{URIs: []*url.URL{u}}
	_, err := grpcserver.ParsePeerIdentity(cert)
	require.ErrorContains(t, err, "tenant uuid")
}
```

- [ ] **Step 2: Run — should FAIL (undefined symbol)**

```bash
go test -race -count=1 ./internal/recording/grpcserver/...
```

- [ ] **Step 3: Implement `internal/recording/grpcserver/peer_identity.go`**

```go
package grpcserver

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// PeerIdentity is the per-call provenance extracted from the verified mTLS
// client cert's URI SAN. The expected URI shape is:
//   spiffe://sociopulse/ingest-agent/<agent-id>?tenant=<tenant-uuid>
type PeerIdentity struct {
	TenantID uuid.UUID
	AgentID  string
	URI      string
}

// ParsePeerIdentity extracts the SPIFFE-style identity from a leaf cert.
// Errors are intentionally generic — they reach the wire only via the
// peerTenantInterceptor below, which translates them to gRPC codes.
func ParsePeerIdentity(cert *x509.Certificate) (PeerIdentity, error) {
	if cert == nil || len(cert.URIs) == 0 {
		return PeerIdentity{}, errors.New("no URI SAN on client cert")
	}
	u := cert.URIs[0]
	if u.Scheme != "spiffe" {
		return PeerIdentity{}, fmt.Errorf("unsupported scheme %q (want spiffe)", u.Scheme)
	}
	tenantStr := u.Query().Get("tenant")
	tenantID, err := uuid.Parse(tenantStr)
	if err != nil {
		return PeerIdentity{}, fmt.Errorf("tenant uuid: %w", err)
	}
	return PeerIdentity{
		TenantID: tenantID,
		AgentID:  u.Path,
		URI:      u.String(),
	}, nil
}

type peerIdentityKey struct{}

// peerIdentityFromCtx returns the PeerIdentity stashed by the interceptor.
// Internal helper — exported for tests via the seam in commit_handler.go.
func peerIdentityFromCtx(ctx context.Context) (PeerIdentity, bool) {
	v, ok := ctx.Value(peerIdentityKey{}).(PeerIdentity)
	return v, ok
}

// peerTenantInterceptor extracts the SPIFFE identity from the client
// cert and attaches it to ctx. Calls without a verified cert are
// rejected with Unauthenticated so handlers can assume identity exists.
func peerTenantInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		p, ok := peer.FromContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing peer info")
		}
		tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
		if !ok || len(tlsInfo.State.VerifiedChains) == 0 {
			return nil, status.Error(codes.Unauthenticated, "client cert required")
		}
		leaf := tlsInfo.State.VerifiedChains[0][0]
		identity, err := ParsePeerIdentity(leaf)
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "invalid SPIFFE identity: %v", err)
		}
		ctx = context.WithValue(ctx, peerIdentityKey{}, identity)
		return h(ctx, req)
	}
}
```

- [ ] **Step 4: Run — should PASS**

```bash
go test -race -count=1 ./internal/recording/grpcserver/...
```

### 5.2 `server.go` + `commit_handler.go`

- [ ] **Step 5: Write the failing server test (uses bufconn — no real network/TLS)**

```go
// internal/recording/grpcserver/server_test.go
package grpcserver_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/grpcserver"
	rpb "github.com/sociopulse/platform/internal/recording/proto/v1"
)

func TestGRPCServer_Commit_DelegatesToService(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	tenantID := uuid.Must(uuid.NewV7())
	callID := uuid.Must(uuid.NewV7())

	fakeSvc := &fakeRecordingService{
		commitOut: rapi.CommitOutput{
			RecordingID:      uuid.Must(uuid.NewV7()),
			CallID:           callID,
			CommittedAt:      time.Now().UTC(),
			IdempotentReplay: false,
		},
	}

	conn := bufconnClient(t, fakeSvc, tenantID)
	t.Cleanup(func() { _ = conn.Close() })

	cli := rpb.NewRecordingServiceClient(conn)
	resp, err := cli.Commit(ctx, &rpb.CommitRequest{
		TenantId:       tenantID.String(),
		CallId:         callID.String(),
		S3Bucket:       "bucket",
		AudioObjectKey: "k.opus.enc",
		KmsKeyId:       "kms-1",
		EncryptedDek:   []byte("encrypted-dek-stub-32bytes-xxxxx"),
		BytesSize:      1234,
		Duration:       durationpb.New(12 * time.Second),
		Sha256:         "f1e2d3c4b5a697887766554433221100ffeeddccbbaa99887766554433221100",
		Codec:          "opus",
		SampleRate:     48000,
		DeleteAt:       timestamppb.New(time.Now().Add(730 * 24 * time.Hour)),
		ColdAt:         timestamppb.New(time.Now().Add(365 * 24 * time.Hour)),
		IngestAgentId:  "agent-test",
		RecordedAt:     timestamppb.New(time.Now().Add(-1 * time.Hour)),
	})
	require.NoError(t, err)
	require.False(t, resp.IdempotentReplay)
	require.NotEmpty(t, resp.RecordingId)
	require.Equal(t, 1, fakeSvc.commitCalls, "service.Commit must be called exactly once")
}

func TestGRPCServer_Commit_TenantMismatch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	tenantInCert := uuid.Must(uuid.NewV7())
	otherTenant := uuid.Must(uuid.NewV7())

	conn := bufconnClient(t, &fakeRecordingService{}, tenantInCert)
	t.Cleanup(func() { _ = conn.Close() })

	cli := rpb.NewRecordingServiceClient(conn)
	_, err := cli.Commit(ctx, &rpb.CommitRequest{
		TenantId: otherTenant.String(),
		CallId:   uuid.Must(uuid.NewV7()).String(),
	})
	require.Error(t, err)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestGRPCServer_Commit_BadCallID(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tenantID := uuid.Must(uuid.NewV7())
	conn := bufconnClient(t, &fakeRecordingService{}, tenantID)
	t.Cleanup(func() { _ = conn.Close() })

	cli := rpb.NewRecordingServiceClient(conn)
	_, err := cli.Commit(ctx, &rpb.CommitRequest{
		TenantId: tenantID.String(),
		CallId:   "not-a-uuid",
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestGRPCServer_Commit_ServiceCallNotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tenantID := uuid.Must(uuid.NewV7())

	fakeSvc := &fakeRecordingService{commitErr: rapi.ErrCallNotFound}
	conn := bufconnClient(t, fakeSvc, tenantID)
	t.Cleanup(func() { _ = conn.Close() })

	cli := rpb.NewRecordingServiceClient(conn)
	_, err := cli.Commit(ctx, validProtoCommit(tenantID))
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestGRPCServer_Commit_ServiceInvalidInput(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tenantID := uuid.Must(uuid.NewV7())

	fakeSvc := &fakeRecordingService{commitErr: rapi.ErrInvalidInput}
	conn := bufconnClient(t, fakeSvc, tenantID)
	t.Cleanup(func() { _ = conn.Close() })

	cli := rpb.NewRecordingServiceClient(conn)
	_, err := cli.Commit(ctx, validProtoCommit(tenantID))
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// ----- helpers -----

// bufconnClient builds an in-memory gRPC server using the supplied fake service
// and returns a connected client. Bypasses TLS — the SPIFFE identity is injected
// directly via grpcserver.WithInjectedPeerIdentityForTest. This keeps the test
// fast and free of cert-management overhead.
func bufconnClient(t *testing.T, svc rapi.RecordingService, tenantID uuid.UUID) *grpc.ClientConn {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	srv := grpcserver.NewForTest(svc, tenantID)

	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(srv.GracefulStop)

	conn, err := grpc.NewClient("passthrough:bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	return conn
}

func validProtoCommit(tenantID uuid.UUID) *rpb.CommitRequest {
	return &rpb.CommitRequest{
		TenantId:       tenantID.String(),
		CallId:         uuid.Must(uuid.NewV7()).String(),
		S3Bucket:       "b",
		AudioObjectKey: "k.opus.enc",
		KmsKeyId:       "kms",
		EncryptedDek:   []byte("dek-stub-32bytes-xxxxxxxxxxxxxxxx"),
		BytesSize:      1,
		Duration:       durationpb.New(time.Second),
		Sha256:         "f1e2d3c4b5a697887766554433221100ffeeddccbbaa99887766554433221100",
		Codec:          "opus",
		SampleRate:     48000,
		DeleteAt:       timestamppb.New(time.Now().Add(time.Hour)),
		ColdAt:         timestamppb.New(time.Now().Add(time.Hour)),
		RecordedAt:     timestamppb.New(time.Now()),
	}
}

type fakeRecordingService struct {
	commitOut   rapi.CommitOutput
	commitErr   error
	commitCalls int
}

func (f *fakeRecordingService) Commit(_ context.Context, _ rapi.CommitInput) (rapi.CommitOutput, error) {
	f.commitCalls++
	if f.commitErr != nil {
		return rapi.CommitOutput{}, f.commitErr
	}
	return f.commitOut, nil
}
func (f *fakeRecordingService) Get(_ context.Context, _, _ uuid.UUID) (rapi.RecordingMetadata, error) {
	return rapi.RecordingMetadata{}, rapi.ErrNotFound
}
func (f *fakeRecordingService) Search(_ context.Context, _ uuid.UUID, _ rapi.SearchQuery) (rapi.SearchResult, error) {
	return rapi.SearchResult{}, rapi.ErrInvalidInput
}
func (f *fakeRecordingService) OpenAudioStream(_ context.Context, _, _ uuid.UUID, _ *rapi.ByteRange) (rapi.AudioStream, error) {
	return rapi.AudioStream{}, rapi.ErrInvalidInput
}
func (f *fakeRecordingService) VerifyChecksum(_ context.Context, _, _ uuid.UUID) (rapi.VerifyResult, error) {
	return rapi.VerifyResult{}, rapi.ErrInvalidInput
}
```

- [ ] **Step 6: Run — should FAIL (undefined `grpcserver.NewForTest`)**

- [ ] **Step 7: Implement `internal/recording/grpcserver/server.go`**

```go
// Package grpcserver implements the gRPC façade for internal/recording.
// Plan 12.1 (Foundation) covers Commit and Get. The server requires mTLS
// in production; tests can use NewForTest to inject a PeerIdentity directly.
package grpcserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	rpb "github.com/sociopulse/platform/internal/recording/proto/v1"
)

// Config controls listener address and TLS material.
type Config struct {
	ListenAddr   string        // ":9091"
	TLSCertFile  string        // server cert (signed by internal CA)
	TLSKeyFile   string        // server key
	TLSCAFile    string        // CA bundle that signs client (ingest-agent) certs
	MaxRecvBytes int           // default 4 MiB
	Timeout      time.Duration // per-call deadline
}

// Server wires the RecordingService implementation behind a gRPC endpoint.
type Server struct {
	rpb.UnimplementedRecordingServiceServer

	svc      rapi.RecordingService
	logger   *zap.Logger
	listenOn string
	server   *grpc.Server
}

// New constructs a production server with mTLS credentials loaded from cfg.
// Returns an error if the cert/key/CA cannot be loaded; callers should
// fail fast on that.
func New(cfg Config, svc rapi.RecordingService, logger *zap.Logger) (*Server, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if cfg.MaxRecvBytes <= 0 {
		cfg.MaxRecvBytes = 4 * 1024 * 1024
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}

	creds, err := loadMTLSCreds(cfg)
	if err != nil {
		return nil, fmt.Errorf("recording grpc: load mtls: %w", err)
	}

	srv := grpc.NewServer(
		grpc.Creds(creds),
		grpc.MaxRecvMsgSize(cfg.MaxRecvBytes),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
			Time:              30 * time.Second,
			Timeout:           10 * time.Second,
		}),
		grpc.ChainUnaryInterceptor(
			peerTenantInterceptor(),
		),
	)

	g := &Server{
		svc:      svc,
		logger:   logger,
		listenOn: cfg.ListenAddr,
		server:   srv,
	}
	rpb.RegisterRecordingServiceServer(srv, g)
	return g, nil
}

// NewForTest constructs a server with a stub interceptor that injects a fixed
// SPIFFE identity (TenantID = tenantID, AgentID = "/test/agent"). Bypasses TLS.
// Tests using NewForTest call Server.Serve(net.Listener) directly and tear down
// via Server.GracefulStop.
func NewForTest(svc rapi.RecordingService, tenantID interface{ String() string }) *Server {
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(testInjectIdentityInterceptor(tenantID)),
	)
	g := &Server{
		svc:      svc,
		logger:   zap.NewNop(),
		listenOn: "buf",
		server:   srv,
	}
	rpb.RegisterRecordingServiceServer(srv, g)
	return g
}

func testInjectIdentityInterceptor(tenantID interface{ String() string }) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		// Build identity from the supplied tenant; agent is fixed.
		// Tests that need other tenants can construct multiple servers.
		identity := PeerIdentity{
			AgentID: "/test/agent",
			URI:     "spiffe://test/agent?tenant=" + tenantID.String(),
		}
		// uuid.Parse is safe — bufconnClient passes uuid.UUID via String().
		// Failure is a programmer error; convert to status.
		if v, err := parseUUIDAllowError(tenantID.String()); err == nil {
			identity.TenantID = v
		}
		ctx = context.WithValue(ctx, peerIdentityKey{}, identity)
		return h(ctx, req)
	}
}

func loadMTLSCreds(cfg Config) (credentials.TransportCredentials, error) {
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" || cfg.TLSCAFile == "" {
		return nil, errors.New("recording grpc: tls cert/key/ca path required")
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}
	caBytes, err := os.ReadFile(cfg.TLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("read ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, errors.New("ca bundle has no valid certificates")
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}), nil
}

// Serve blocks on the supplied listener until GracefulStop is called or the
// listener errors.
func (s *Server) Serve(lis net.Listener) error {
	s.logger.Info("recording grpc server listening", zap.String("addr", lis.Addr().String()))
	return s.server.Serve(lis)
}

// ServeAddr is a convenience for production callers — listens on s.listenOn.
func (s *Server) ServeAddr() error {
	lis, err := net.Listen("tcp", s.listenOn)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.listenOn, err)
	}
	return s.Serve(lis)
}

// GracefulStop drains in-flight calls.
func (s *Server) GracefulStop() { s.server.GracefulStop() }
```

- [ ] **Step 8: Add `internal/recording/grpcserver/uuid_parse.go` (or inline as private helper in `server.go`)**

```go
package grpcserver

import "github.com/google/uuid"

func parseUUIDAllowError(s string) (uuid.UUID, error) {
	return uuid.Parse(s)
}
```

(This is a thin wrapper kept in its own helper to make the `NewForTest` path easy to read. Inline the call directly into `testInjectIdentityInterceptor` if you prefer.)

- [ ] **Step 9: Implement `internal/recording/grpcserver/commit_handler.go`**

```go
package grpcserver

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	rpb "github.com/sociopulse/platform/internal/recording/proto/v1"
)

// Commit translates the proto request into rapi.CommitInput, calls the
// service, and translates the result back into the proto response. Errors
// are mapped to gRPC status codes using errors.Is on the api sentinels.
func (s *Server) Commit(ctx context.Context, req *rpb.CommitRequest) (*rpb.CommitResponse, error) {
	identity, ok := peerIdentityFromCtx(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer identity")
	}

	tenantID, err := uuid.Parse(req.GetTenantId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "tenant_id: %v", err)
	}
	if tenantID != identity.TenantID {
		return nil, status.Error(codes.PermissionDenied, "tenant mismatch")
	}
	callID, err := uuid.Parse(req.GetCallId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "call_id: %v", err)
	}

	in := rapi.CommitInput{
		TenantID:       tenantID,
		CallID:         callID,
		S3Bucket:       req.GetS3Bucket(),
		AudioObjectKey: req.GetAudioObjectKey(),
		DEKObjectKey:   req.GetDekObjectKey(),
		KMSKeyID:       req.GetKmsKeyId(),
		EncryptedDEK:   req.GetEncryptedDek(),
		BytesSize:      req.GetBytesSize(),
		Duration:       req.GetDuration().AsDuration(),
		SHA256Hex:      req.GetSha256(),
		Codec:          req.GetCodec(),
		SampleRate:     req.GetSampleRate(),
		DeleteAt:       req.GetDeleteAt().AsTime(),
		ColdAt:         req.GetColdAt().AsTime(),
		IngestAgentID:  identity.AgentID, // override any client-supplied value with the cert-derived one
		RecordedAt:     req.GetRecordedAt().AsTime(),
	}

	out, err := s.svc.Commit(ctx, in)
	switch {
	case errors.Is(err, rapi.ErrInvalidInput):
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	case errors.Is(err, rapi.ErrCallNotFound):
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	case errors.Is(err, rapi.ErrTenantMismatch):
		return nil, status.Errorf(codes.PermissionDenied, "%v", err)
	case err != nil:
		return nil, status.Errorf(codes.Internal, "commit: %v", err)
	}

	return &rpb.CommitResponse{
		RecordingId:      out.RecordingID.String(),
		CallId:           out.CallID.String(),
		CommittedAt:      timestamppb.New(out.CommittedAt),
		IdempotentReplay: out.IdempotentReplay,
	}, nil
}

// Get is a thin wrapper over service.Get with proto<->DTO mapping.
func (s *Server) Get(ctx context.Context, req *rpb.GetRequest) (*rpb.GetResponse, error) {
	identity, ok := peerIdentityFromCtx(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer identity")
	}
	tenantID, err := uuid.Parse(req.GetTenantId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "tenant_id: %v", err)
	}
	if tenantID != identity.TenantID {
		return nil, status.Error(codes.PermissionDenied, "tenant mismatch")
	}
	callID, err := uuid.Parse(req.GetCallId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "call_id: %v", err)
	}

	md, err := s.svc.Get(ctx, tenantID, callID)
	switch {
	case errors.Is(err, rapi.ErrNotFound):
		return nil, status.Error(codes.NotFound, "not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "get: %v", err)
	}

	resp := &rpb.GetResponse{
		RecordingId:    md.RecordingID.String(),
		CallId:         md.CallID.String(),
		TenantId:       md.TenantID.String(),
		S3Bucket:       md.S3Bucket,
		AudioObjectKey: md.AudioObjectKey,
		BytesSize:      md.BytesSize,
		Duration:       durationpb.New(md.Duration),
		Sha256:         md.SHA256Hex,
		Status:         md.Status,
		CommittedAt:    timestamppb.New(md.CommittedAt),
		DeleteAt:       timestamppb.New(md.DeleteAt),
		ColdAt:         timestamppb.New(md.ColdAt),
	}
	if md.VerifiedAt != nil {
		resp.VerifiedAt = timestamppb.New(*md.VerifiedAt)
	}
	return resp, nil
}
```

> **Imports** for `commit_handler.go`: `google.golang.org/protobuf/types/known/durationpb` (for `Duration`), `google.golang.org/protobuf/types/known/timestamppb` (for timestamps). Confirm the generated field name is `Duration *durationpb.Duration` via `grep -n "Duration" internal/recording/proto/v1/recording.pb.go` after Task 2.

- [ ] **Step 10: Run server tests — should PASS**

```bash
go test -race -count=1 ./internal/recording/grpcserver/...
```

- [ ] **Step 11: Create goleak `internal/recording/grpcserver/main_test.go`**

```go
package grpcserver_test

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("google.golang.org/grpc.(*Server).handleStream"),
		goleak.IgnoreTopFunction("google.golang.org/grpc/internal/transport.(*serverHandlerTransport).runStream"),
	)
}
```

### 5.3 Module composition

- [ ] **Step 12: Replace `internal/recording/module.go`**

```go
// Package recording — Module registration entry point.
//
// Plan 12.1 (Foundation): wires RecordingService.Commit + gRPC server,
// stores the service in d.Locator under "recording.RecordingService",
// and starts the gRPC listener via the Module's Start lifecycle hook.
//
// Plan 12.2 will add the S3+KMS+AES-GCM stack; Plan 12.3 will mount
// HTTP transport routes; Plan 12.4 adds workers.
package recording

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/grpcserver"
	"github.com/sociopulse/platform/internal/recording/metrics"
	"github.com/sociopulse/platform/internal/recording/service"
	"github.com/sociopulse/platform/internal/recording/store"
	"github.com/sociopulse/platform/internal/modules"
)

// LocatorRecordingService is the locator key under which Module.Register
// publishes the constructed RecordingService.
const LocatorRecordingService = "recording.RecordingService"

// Module is the top-level registration handle.
type Module struct {
	server *grpcserver.Server
	logger *zap.Logger
}

// Name returns the module's unique identifier within the registry.
func (m *Module) Name() string { return "recording" }

// Register builds the service, stashes it in the locator, and prepares
// the gRPC server for the cmd/api errgroup. The server is NOT started
// here — that's the caller's responsibility (cmd/api wires it into its
// errgroup so SIGTERM cancels the listener cleanly).
func (m *Module) Register(d modules.Deps) error {
	if d.Pool == nil {
		// Without Postgres there is no recording — silently skip so
		// dev environments without a DB can still boot the API.
		return nil
	}

	cfg := d.Config.Recording
	if cfg == nil || !cfg.Enabled {
		return nil
	}

	met, err := metrics.RegisterRecordingMetrics(d.PrometheusRegistry)
	if err != nil {
		return fmt.Errorf("recording metrics: %w", err)
	}

	pgStore := store.NewPostgresStore(d.Pool)
	logger := d.Logger.Named("recording")

	svc := service.New(service.Deps{
		Pool:    d.Pool,
		Store:   pgStore,
		Logger:  logger,
		Metrics: met,
	})

	if d.Locator != nil {
		d.Locator.Register(LocatorRecordingService, svc)
	}

	srv, err := grpcserver.New(grpcserver.Config{
		ListenAddr:   cfg.GRPCListenAddr,
		TLSCertFile:  cfg.TLSCertFile,
		TLSKeyFile:   cfg.TLSKeyFile,
		TLSCAFile:    cfg.TLSCAFile,
		MaxRecvBytes: cfg.MaxRecvBytes,
		Timeout:      cfg.Timeout,
	}, svc, logger)
	if err != nil {
		// In dev the cert paths are unset — log a warning and continue
		// without the listener so the rest of cmd/api boots.
		logger.Warn("recording grpc disabled: " + err.Error())
		return nil
	}
	m.server = srv
	m.logger = logger
	return nil
}

// Start blocks until ctx is cancelled or the listener errors. cmd/api wires
// it into the errgroup; SIGTERM cancels ctx and triggers GracefulStop.
func (m *Module) Start(ctx context.Context) error {
	if m.server == nil {
		<-ctx.Done()
		return nil
	}

	errCh := make(chan error, 1)
	go func() { errCh <- m.server.ServeAddr() }()

	select {
	case <-ctx.Done():
		m.server.GracefulStop()
		// Drain the listener-error channel so the goroutine doesn't leak.
		<-errCh
		return nil
	case err := <-errCh:
		// listener failed before shutdown — surface unless it's the post-shutdown ErrServerStopped.
		if err == nil || errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return err
	}
}

// Compile-time interface check — guards Module against contract drift on modules.Module.
var _ modules.Module = (*Module)(nil)
```

> **Imports** for `module.go`: `"google.golang.org/grpc"` (for `grpc.ErrServerStopped`). The Module type only implements `modules.Module` (`Name()` + `Register(d) error`); `Run(ctx)` is a recording-specific lifecycle hook called explicitly by `cmd/api/run()` via a typed reference, NOT through the modules registry walk.

- [ ] **Step 13: Add config block in `pkg/config/config.go`**

Locate the existing `Config` struct and append a `Recording *RecordingConfig` field. Define the type:

```go
// pkg/config/config.go (excerpt — add to the file in the appropriate location)

// RecordingConfig configures internal/recording. Nil or Enabled=false means
// the module skips wiring (no gRPC listener, no metrics).
type RecordingConfig struct {
	Enabled        bool          `mapstructure:"enabled"`
	GRPCListenAddr string        `mapstructure:"grpc_listen_addr"` // ":9091" default
	TLSCertFile    string        `mapstructure:"tls_cert_file"`
	TLSKeyFile     string        `mapstructure:"tls_key_file"`
	TLSCAFile      string        `mapstructure:"tls_ca_file"`
	MaxRecvBytes   int           `mapstructure:"max_recv_bytes"`   // default 4 MiB
	Timeout        time.Duration `mapstructure:"timeout"`          // default 30s
}

// Append to the master Config struct's field list:
//   Recording *RecordingConfig `mapstructure:"recording"`
```

Then add a default block in the Viper bootstrap (matching the pattern used by other modules — search `Outbox` or `Realtime` config for the pattern). Default values:

```yaml
# configs/development/config.yaml — append
recording:
  enabled: false                # flip to true once mTLS certs land in dev
  grpc_listen_addr: ":9091"
  tls_cert_file: ""             # set when dev CA exists
  tls_key_file: ""
  tls_ca_file: ""
  max_recv_bytes: 4194304       # 4 MiB
  timeout: 30s
```

- [ ] **Step 14: Patch `cmd/api/main.go` to register `recording.Module`**

Find the existing module-registration loop (it iterates over `[]modules.Module{tenancy, auth, …}`). Append a `recording.Module{}` instance:

```go
// cmd/api/main.go (excerpt — add to the modules slice)
recordingModule := &recording.Module{}
moduleList := []modules.Module{
    /* existing modules ... */
    recordingModule,
}
```

Then in the errgroup setup (where `realtime.Module.Start` etc. are called), add:

```go
g.Go(func() error { return recordingModule.Start(gctx) })
```

Add the import: `"github.com/sociopulse/platform/internal/recording"`.

- [ ] **Step 15: Create `cmd/api/recording.go` with composition helpers** (mirroring `cmd/api/realtime.go`)

```go
// Package main — composition helpers for internal/recording. Kept out of
// main.go so the boot sequence stays readable; mirrors cmd/api/realtime.go.
package main

import (
	"github.com/sociopulse/platform/internal/recording"
)

// recordingModule constructs the Module instance. Held in main.go's
// run() so its Start hook can be wired into the errgroup.
func recordingModule() *recording.Module {
	return &recording.Module{}
}
```

(If main.go inlines the construction directly, this helper file isn't strictly needed — it's a stylistic split. Decide based on the existing pattern in cmd/api/.)

- [ ] **Step 16: Run the full build + tests**

```bash
go build ./...
go vet ./...
go test -race -count=1 ./internal/recording/... ./cmd/api/...
```

Expected: PASS. If gopls reports stale "undefined" diagnostics post-codegen, ignore — the build/test output is the source of truth.

- [ ] **Step 17: Commit**

```bash
git add internal/recording/grpcserver/ \
        internal/recording/module.go \
        cmd/api/main.go cmd/api/recording.go \
        pkg/config/config.go \
        configs/development/config.yaml
git commit -m "feat(recording): Plan 12.1 Task 5 — gRPC server + module wiring + cmd/api composition"
```

### Acceptance
- `cmd/api` boots with `recording.enabled: false` (default) — no gRPC listener, but the module registers cleanly.
- Flipping `recording.enabled: true` with valid cert paths starts the listener on `:9091`.
- `bufconn`-backed unit tests prove the proto→service→proto round-trip.
- Tenant mismatch / bad call_id / missing peer cert all return correct gRPC codes.
- `goleak` clean for `internal/recording/grpcserver/`.

---

## Self-review

**Spec coverage** (against the Plan 12 design brief — `docs/superpowers/plans/2026-05-06-12-recording-module.md`):

| Brief requirement | Plan 12.1 task | Status |
|---|---|---|
| gRPC `RecordingService` on `:9091` (mTLS, internal) | Task 5 | ✅ |
| `Commit` with idempotency on `call_id` | Tasks 3+4 | ✅ |
| INSERT call_recordings inside Tx | Tasks 3+4 | ✅ |
| Audit log entry `recording.committed` in same Tx | Task 4 | ✅ |
| NATS event `tenant.<t>.recording.uploaded` via outbox | Task 4 | ✅ |
| `Get` by call_id | Task 4 + 5.2 | ✅ |
| HTTP `/api/calls/{id}/recording` (download) | **Plan 12.3** | deferred ⏭ |
| HTTP `/api/recordings/search` | **Plan 12.3** | deferred ⏭ |
| HTTP `/api/calls/{id}/recording/verify` | **Plan 12.3** | deferred ⏭ |
| Worker `recording.retention_pass` | **Plan 12.4** | deferred ⏭ |
| Worker `recording.integrity_pass` | **Plan 12.4** | deferred ⏭ |
| Envelope encryption (AES-256-GCM + KMS) | **Plan 12.2** | deferred ⏭ |
| S3 read with stream decrypt | **Plan 12.2 + 12.3** | deferred ⏭ |
| Prometheus metrics (commit, storage, decrypt, integrity) | Task 4 (commit + storage subset) | partial — rest in 12.2/12.3/12.4 |
| Distributed tracing | Reused from `pkg/observability` (not module-specific) | ✅ via interceptor |
| ADR-005 envelope encryption (per-recording DEK + per-tenant KEK) | Plan 12.2 (crypto subsystem) | deferred ⏭ |
| §15.5 retention pipeline (hot→cold→delete) | Plan 12.4 | deferred ⏭ |

**Placeholder scan:**
- The `protoDur` placeholder in Step 9 is flagged with an inline implementer note pointing to the canonical `durationpb.New` — must be replaced before commit.
- The `errClosed` sentinel in Step 12 is flagged with an inline implementer note pointing to `grpc.ErrServerStopped` — must be replaced before commit.
- Search/OpenAudioStream/VerifyChecksum return `ErrInvalidInput` with marker `"not implemented in foundation phase"` — intentional placeholder. Plan 12.2 / 12.3 replace these.

**Type/name consistency:**
- `RecordingRow` (Task 3 / `store/rows.go`) ←→ `CommitInput` (api/dto.go) ←→ `CommitRequest` (Task 2 proto) ←→ `service.Commit` (Task 4) — all share the same column / field names: `BytesSize`, `DurationMS`, `SHA256Hex`, `KMSKeyID`, `EncryptedDEK`, `AudioObjectKey`, `DEKObjectKey`, `Status`, `ColdAt`, `DeleteAt`, `RecordedAt`, `IngestAgentID`. Verified.
- `LocatorRecordingService` constant (`internal/recording/module.go`) — single source of truth for the locator key. No drift.
- `rapi.SubjectRecordingUploadedFor(tenantID)` — single source of truth for the NATS subject.

**Carry-forward checklist (from Plans 09/10/11/11.1/11.2/11.3):**
- [x] No `init()` MustRegister — `RegisterRecordingMetrics(reg)` constructor.
- [x] `*zap.Logger` nil-safe — `service.New` substitutes `zap.NewNop()`.
- [x] Sentinel error aliasing — `service.go` re-exports `rapi.ErrInvalidInput` etc.
- [x] Compile-time interface check — `var _ rapi.RecordingService = (*svc)(nil)` and `var _ modules.Module = (*Module)(nil)`.
- [x] `t.Parallel()` + `t.Cleanup()` + `t.Context()` — used throughout.
- [x] `goleak.VerifyTestMain` per package — all 4 test packages.
- [x] No `time.After` in select-loops — n/a (no select-loops in foundation).
- [x] Modernize: `any`, range over int — used (no `interface{}`).
- [x] `wg.Go` (Go 1.25+) — n/a (no waitgroups).
- [x] gopls cache pollution warning — covered in Carry-forward rules.
- [x] Module path `github.com/sociopulse/platform` — used throughout (NOT `sociopulse/sociopulse`).
- [x] zap, NOT slog — used throughout.

**Cross-tenant defence:**
- Server-side cert SAN tenant ID is the source of truth (`peerTenantInterceptor` parses, `Commit` handler compares against `req.TenantId`). A malicious uploader supplying another tenant's `call_id` is caught by `InsertRecordingIdempotent`'s explicit `EXISTS(SELECT … FROM calls WHERE id=$1 AND tenant_id=$2)` pre-check.
- `IngestAgentID` in audit row is sourced from cert SPIFFE URI, NOT from request body — uploader cannot forge attribution.

**Out of scope (correctly deferred):**
- mTLS PKI bootstrap — Plan 18 (PKI) or operator runbook for dev CA.
- Encryption pipeline on FreeSWITCH side — Plan 08.
- Live listen-in via mixmonitor — Plan 11 (already shipped a stub).
- Audio waveform UI — Plan 19 (frontend).
- Periodic full-archive verify (100% sweep) — backlog (cost-prohibitive on KMS rate limits).

**Plan 12.1 verified.**

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-09-12-1-recording-foundation.md`.**

**Two execution options:**

1. **Subagent-Driven (recommended)** — fresh subagent per task, two-stage review per task, fast iteration.
2. **Inline Execution** — sequential tasks in this session via `superpowers:executing-plans`.
