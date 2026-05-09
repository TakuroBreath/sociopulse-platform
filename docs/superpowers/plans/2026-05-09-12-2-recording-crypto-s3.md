# Plan 12.2 — Recording Crypto + S3 Read Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `RecordingService.OpenAudioStream` foundation-phase stub with a real envelope-decrypt pipeline: KMS-unwrap encrypted DEK → S3 GET ciphertext → AES-256-GCM decrypt → return `bytes.Reader` over plaintext audio.

**Architecture:** Phase 2 of 4 of Plan 12 (Recording Module). Establishes the read-path crypto building blocks: `DEKUnwrapper` (KMS port), `ObjectStore` (S3 port), `AudioDecryptor` (AES-GCM wrapper). Both KMS and S3 ports get **Local** in-memory implementations for tests + dev; **Yandex Cloud** implementations are deferred to Plan 01 (infra). v1 buffers the entire decrypted file in RAM (`Accept-Ranges: none` per the design brief trade-off); v2 will introduce a chunked-envelope format with per-chunk auth tags so playback can stream-validate. NO HTTP delivery (Plan 12.3), NO retention/integrity workers (Plan 12.4).

**Tech Stack:** Go 1.26.3, AES-256-GCM via `pkg/encryption.Decrypt` (already in repo), buffer-in-memory decrypt (≤200 MiB per call enforced).

---

## Implementer corrections — READ FIRST

The same set of corrections from Plan 12.1 carries over verbatim. The most relevant for this plan:

| Body says | Use this instead |
|---|---|
| `pgtest.AcquirePool` | Each integration test package writes its own `startPGContainer(t) *postgres.Pool` helper following `internal/recording/store/postgres_pg_test.go` (Plan 12.1 Task 3 left a working template — copy it). |
| `pool.QueryRow` (direct) | `pool.RawQueryRow(...)` for non-tx reads. |
| `pool.BypassRLS` | `pool.WithTenant(ctx, tenantID, fn)` — `tenancy_admin` lacks grants on `calls`/`call_recordings`. |
| `Locator.Set / Get` | `Locator.Register(name, svc)` and `Lookup(name) (any, bool)`. |
| `*slog.Logger` | `*zap.Logger`. |
| Module path `sociopulse/sociopulse` | `github.com/sociopulse/platform`. |

Plan 12.1 Tasks 3+4 are the canonical reference for how recording-module Go code looks today — when in doubt, mirror those files.

---

## Carry-forward rules (from Plans 09/10/11/11.1/11.2/11.3/12.1)

1. **No `init()` MustRegister** — every metrics struct via `RegisterXMetrics(reg) (*M, error)` constructor.
2. **`*zap.Logger` nil-safe** — `zap.NewNop()` fallback when caller passes nil.
3. **Sentinel error aliasing** — `var ErrXxx = api.ErrXxx` re-exports inside the implementation package.
4. **Compile-time interface check** — `var _ Iface = (*Impl)(nil)` at package scope.
5. **Tests** — `t.Parallel()` + `t.Cleanup()` + `t.Context()` (Go 1.24+ stdlib).
6. **`goleak.VerifyTestMain`** per package.
7. **No `time.After` in select-loops** — `time.NewTimer` + `defer t.Stop()`.
8. **Modernize** — `any` over `interface{}`, range over int (`for i := range n`), `slices`/`maps` packages.
9. **`wg.Go(func() error)`** (Go 1.25+) over `wg.Add(1); go func() { defer wg.Done(); ... }()`.
10. **gopls cache pollution** — reality-check via direct `go build && go test -race`.
11. **Module path** `github.com/sociopulse/platform`.
12. **Logger** is zap, NOT slog.
13. **Error fold for sentinel-indistinguishability** — when wrapping a child error so callers cannot probe via `errors.Is`, use `fmt.Errorf("%w: %s", api.ErrXxx, child.Error())` (NOT `%w` for the child).

---

## Scope of Plan 12.2 vs other Plan 12 phases

| Concern | Phase | Status |
|---|---|---|
| Migration 000010 + Proto + RecordingStore + Commit + outbox + gRPC mTLS | **Plan 12.1** | ✅ shipped (`v0.0.16-recording-foundation`) |
| KMS port + S3 port + AES-GCM decrypt + OpenAudioStream real impl | **Plan 12.2** | this plan |
| HTTP delivery (`/api/calls/{id}/recording`, `/api/recordings/search`, `/recording/verify`) | Plan 12.3 | deferred |
| Workers (`retention_pass` daily, `integrity_pass` weekly) | Plan 12.4 | deferred |
| Yandex Cloud KMS / Object Storage real adapters | Plan 01 (infra) | deferred |

**Yandex Cloud SDK is NOT a dependency of Plan 12.2.** The `LocalDEKUnwrapper` and `LocalObjectStore` carry the load until Plan 01 lands the real Yandex SDK adapters.

---

## File Structure

```text
internal/recording/
├── crypto/                                      # NEW — Plan 12.2
│   ├── aesgcm.go                                # AudioDecryptor + size-cap + AAD-bind
│   ├── aesgcm_test.go                           # round-trip + tamper-detect + size-cap
│   ├── kms.go                                   # DEKUnwrapper interface + LocalDEKUnwrapper
│   └── kms_test.go                              # local impl round-trip
├── storage/                                     # NEW — Plan 12.2
│   ├── store.go                                 # ObjectStore interface + LocalObjectStore
│   └── store_test.go                            # local Get/Put/Delete + ErrObjectNotFound
├── service/                                     # MODIFY — replace OpenAudioStream stub
│   ├── service.go                               # add Deps.{Decryptor, ObjectStore}; real OpenAudioStream
│   └── service_test.go                          # add OpenAudioStream tests (in-memory wiring)
├── module.go                                    # MODIFY — Config grows; New takes Local impls
└── ... (api/, store/, grpcserver/, metrics/, proto/v1/ — unchanged from Plan 12.1)

cmd/api/
└── recording.go                                 # MODIFY — wire Local* impls into recording.Config
```

---

## Decision log (recorded for future plans)

### D1: AAD binding strategy

The ingest-uploader (Plan 08 — out of scope here) MUST encrypt audio with AAD = `tenant_id.String()` so the playback path can verify both the DEK and the tenant binding in the AEAD step. Plan 12.2's `AudioDecryptor` passes `tenant_id` as AAD; Plan 08 writes its uploader to match.

**Rationale:** copy-paste defence — encrypted blobs from tenant A cannot be decrypted with tenant A's KEK against tenant B's row, even if an attacker swaps `audio_object_key` in the DB.

### D2: Buffer-in-RAM for v1

`OpenAudioStream` reads the entire ciphertext from S3 into memory, decrypts with `pkg/encryption.Decrypt` (full-buffer AEAD), then returns a `bytes.Reader` to the HTTP layer (Plan 12.3). 200 MiB cap on `bytes_size` enforced before the GET — anything larger returns `ErrAudioTooLargeForV1` (a new sentinel) and the HTTP layer maps it to 413.

**Rationale:** Plan 12 design brief §Outputs — "decrypt-all-в-RAM-then-slice" is the simpler v1 trade-off; chunked envelope is a v2 change requiring a coordinated update to the FreeSWITCH-side encrypt pipeline.

### D3: KMS port abstraction

`DEKUnwrapper.DecryptDEK(ctx, kmsKeyID, encryptedDEK, aad) ([]byte, error)` is the recording-specific port. **It is NOT `tenancy.api.KMSResolver`** — that interface is for in-app cached AES-GCM (small payloads like phones) keyed by tenant. The recording flow needs RAW KMS Symmetric Decrypt: the ingest-uploader called `KMS.SymmetricEncrypt(kek_id, aad=tenant_id, plaintext=dek)` and produced opaque KMS ciphertext bytes; we need the inverse with the same KMS-internal mechanics. A separate port keeps the contracts clean and lets the Yandex adapter (Plan 01) call `kms.SymmetricCryptoService.Decrypt(...)` directly.

### D4: Stub vs Local

Following the Plan 04 pattern (BucketProvisioner): default-build `Local*` implementations live alongside the interface; build-tag `-tags=yandex_kms` / `-tags=yandex_s3` will gate the Yandex Cloud SDK adapters when Plan 01 ships them. Plan 12.2 ships ONLY the Local impls.

---

## Task 1 — `internal/recording/crypto/aesgcm.go` — AudioDecryptor

**Goal:** Buffer-in-RAM AES-256-GCM decrypt with size cap + AAD binding. Thin wrapper over `pkg/encryption.Decrypt`.

**Files:**
- Create: `internal/recording/crypto/aesgcm.go`
- Create: `internal/recording/crypto/aesgcm_test.go`
- Create: `internal/recording/crypto/main_test.go` (goleak)

### Constants

```go
// MaxAudioPlaintextBytes caps the plaintext size of a single decrypt call.
// 200 MiB matches the Plan 12 design brief's §Outputs trade-off — at higher
// sizes we'd OOM under concurrent audits. v2 chunked-envelope format
// (deferred) lifts this cap.
const MaxAudioPlaintextBytes = 200 * 1024 * 1024
```

### Sentinels

```go
var (
    // ErrAudioTooLargeForV1 is returned by Decrypt when ciphertext.Size
    // exceeds MaxAudioPlaintextBytes. Plan 12.3 maps this to HTTP 413
    // Payload Too Large.
    ErrAudioTooLargeForV1 = errors.New("recording.crypto: audio exceeds v1 in-memory cap")

    // ErrAuth wraps any AEAD authentication failure. The HTTP layer
    // maps this to 502 Bad Gateway (corrupted ciphertext) — never expose
    // tampering details to the client.
    ErrAuth = errors.New("recording.crypto: authentication failed")
)
```

### API

```go
// AudioDecryptor decrypts a complete AES-256-GCM ciphertext into plaintext
// bytes. Reads the entire ciphertext from r before validating — a partial
// validation would risk delivering tampered plaintext to a client. Size is
// the expected ciphertext length (bytes_size from call_recordings); the
// implementation enforces MaxAudioPlaintextBytes against this BEFORE
// reading.
type AudioDecryptor interface {
    Decrypt(ctx context.Context, key []byte, ciphertext io.Reader, size int64, aad []byte) ([]byte, error)
}

// AESGCMDecryptor is the default AudioDecryptor backed by pkg/encryption.
// Stateless; safe to share across goroutines.
type AESGCMDecryptor struct{}

func NewAESGCMDecryptor() *AESGCMDecryptor { return &AESGCMDecryptor{} }
```

### Implementation sketch

```go
func (d *AESGCMDecryptor) Decrypt(ctx context.Context, key []byte, ciphertext io.Reader, size int64, aad []byte) ([]byte, error) {
    if size <= 0 {
        return nil, fmt.Errorf("recording.crypto: invalid size %d", size)
    }
    if size > MaxAudioPlaintextBytes {
        return nil, fmt.Errorf("%w: size=%d cap=%d", ErrAudioTooLargeForV1, size, MaxAudioPlaintextBytes)
    }
    // pre-allocate to avoid append churn; honours ctx via io.Copy through
    // an io.LimitReader wrapper so a slow S3 stream doesn't pin RAM forever.
    buf := bytes.NewBuffer(make([]byte, 0, size))
    if _, err := io.CopyN(buf, &ctxReader{ctx: ctx, r: ciphertext}, size); err != nil {
        return nil, fmt.Errorf("recording.crypto: read ciphertext: %w", err)
    }
    plain, err := encryption.Decrypt(key, buf.Bytes(), aad)
    if err != nil {
        if errors.Is(err, encryption.ErrAuth) {
            return nil, fmt.Errorf("%w: %s", ErrAuth, err.Error())  // fold details
        }
        return nil, fmt.Errorf("recording.crypto: decrypt: %w", err)
    }
    return plain, nil
}

// ctxReader wraps r so that Read returns ctx.Err() on cancellation. Cheap;
// only checks ctx between reads, not mid-syscall.
type ctxReader struct {
    ctx context.Context
    r   io.Reader
}

func (cr *ctxReader) Read(p []byte) (int, error) {
    if err := cr.ctx.Err(); err != nil {
        return 0, err
    }
    return cr.r.Read(p)
}
```

### Tests

- `TestAESGCMDecryptor_RoundTrip` — encrypt with `pkg/encryption.Encrypt(key, audio, aad)`, decrypt via `AESGCMDecryptor.Decrypt(key, bytes.NewReader(ciphertext), int64(len(ciphertext)), aad)` — assert plaintext matches.
- `TestAESGCMDecryptor_AADMismatchYieldsErrAuth` — encrypt with `aad=[]byte("tenant-A")`, decrypt with `aad=[]byte("tenant-B")` — assert `errors.Is(err, ErrAuth)`.
- `TestAESGCMDecryptor_TamperedCiphertextYieldsErrAuth` — flip a byte before decrypt — assert `errors.Is(err, ErrAuth)`.
- `TestAESGCMDecryptor_SizeCapRejected` — pass `size=MaxAudioPlaintextBytes+1` — assert `errors.Is(err, ErrAudioTooLargeForV1)`.
- `TestAESGCMDecryptor_CtxCancelled` — pre-cancel ctx, pass slow reader — assert ctx.Err() bubbles up. Use `iotest.OneByteReader` over a 1 MiB buffer; cancel ctx after first byte.

### TDD steps

- [ ] **Step 1: Write failing test (round-trip happy path)**

```go
package crypto_test

import (
    "bytes"
    "crypto/rand"
    "errors"
    "testing"

    "github.com/stretchr/testify/require"

    "github.com/sociopulse/platform/internal/recording/crypto"
    "github.com/sociopulse/platform/pkg/encryption"
)

func TestAESGCMDecryptor_RoundTrip(t *testing.T) {
    t.Parallel()
    ctx := t.Context()

    key := make([]byte, 32)
    _, _ = rand.Read(key)
    plaintext := []byte("hello recording audio")
    aad := []byte("tenant-A")

    ct, err := encryption.Encrypt(key, plaintext, aad)
    require.NoError(t, err)

    d := crypto.NewAESGCMDecryptor()
    got, err := d.Decrypt(ctx, key, bytes.NewReader(ct), int64(len(ct)), aad)
    require.NoError(t, err)
    require.Equal(t, plaintext, got)
}
```

- [ ] **Step 2: Run test — should FAIL** (`crypto.NewAESGCMDecryptor`, `crypto.AESGCMDecryptor.Decrypt` undefined).

```bash
go test -race -count=1 ./internal/recording/crypto/...
```

Expected: FAIL with "undefined" diagnostics.

- [ ] **Step 3: Implement minimal `aesgcm.go`** (use the implementation sketch above).

- [ ] **Step 4: Add the four other tests** (AAD-mismatch, tampered, size-cap, ctx-cancel) and confirm all five pass.

```bash
go test -race -count=1 ./internal/recording/crypto/...
```

Expected: PASS — 5/5.

- [ ] **Step 5: Add `internal/recording/crypto/main_test.go` with goleak**

```go
package crypto_test

import (
    "testing"

    "go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }
```

- [ ] **Step 6: Commit**

```bash
git add internal/recording/crypto/
git commit -m "feat(recording/crypto): Plan 12.2 Task 1 — AudioDecryptor (AES-256-GCM, in-RAM, size-capped)"
```

### Acceptance
- 5/5 tests green under `-race -count=1`.
- `errors.Is` works for both `ErrAuth` and `ErrAudioTooLargeForV1`.
- 200 MiB cap is enforced BEFORE allocation (peek `size` first).
- Context cancellation aborts mid-stream within one read iteration.
- `var _ AudioDecryptor = (*AESGCMDecryptor)(nil)` compile-time check.

---

## Task 2 — `internal/recording/crypto/kms.go` — DEKUnwrapper

**Goal:** Define the `DEKUnwrapper` port and ship a `LocalDEKUnwrapper` implementation suitable for tests + dev. Yandex SDK is deferred to Plan 01.

**Files:**
- Create: `internal/recording/crypto/kms.go`
- Create: `internal/recording/crypto/kms_test.go`

### API

```go
// DEKUnwrapper unwraps an encrypted DEK using the per-tenant KEK at the
// supplied key id. Implementations MUST bind the supplied AAD into the
// authentication step so a swapped (kms_key_id, encrypted_dek) tuple
// fails fast — a defence-in-depth above the row-level RLS check.
//
// The error contract:
//   ErrUnknownKey       — kmsKeyID does not exist or is in the wrong scope.
//   ErrDecryptFailed    — KMS rejected the ciphertext (corrupted / wrong AAD / wrong key).
//   ErrUnauthorised     — the caller lacks permission to invoke decrypt (Yandex IAM).
//
// The Local implementation maps every failure to ErrDecryptFailed; the
// Yandex implementation will map IAM errors to ErrUnauthorised.
type DEKUnwrapper interface {
    DecryptDEK(ctx context.Context, kmsKeyID string, encryptedDEK []byte, aad []byte) ([]byte, error)
}
```

### Sentinels

```go
var (
    ErrUnknownKey    = errors.New("recording.kms: unknown KMS key")
    ErrDecryptFailed = errors.New("recording.kms: decrypt failed")
    ErrUnauthorised  = errors.New("recording.kms: unauthorised")
)
```

### LocalDEKUnwrapper

```go
// LocalDEKUnwrapper is the in-process implementation used by tests and
// local dev. The "encrypted_dek" payload is the OUTPUT of pkg/encryption.Encrypt
// against a fixed in-memory KEK keyed by kmsKeyID. Production deployments
// MUST use the Yandex-backed implementation (Plan 01).
//
// Construction:
//   keks := map[string][]byte{"kek-tenant-A": <32 random bytes>}
//   u := NewLocalDEKUnwrapper(keks)
//   plaintext, _ := u.DecryptDEK(ctx, "kek-tenant-A", encryptedDEK, aad)
type LocalDEKUnwrapper struct {
    keks map[string][]byte // kmsKeyID → 32-byte KEK plaintext
}

func NewLocalDEKUnwrapper(keks map[string][]byte) *LocalDEKUnwrapper {
    cp := make(map[string][]byte, len(keks))
    for k, v := range keks {
        cp[k] = bytes.Clone(v)
    }
    return &LocalDEKUnwrapper{keks: cp}
}

// var _ DEKUnwrapper = (*LocalDEKUnwrapper)(nil)
func (u *LocalDEKUnwrapper) DecryptDEK(ctx context.Context, kmsKeyID string, encryptedDEK []byte, aad []byte) ([]byte, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }
    kek, ok := u.keks[kmsKeyID]
    if !ok {
        return nil, fmt.Errorf("%w: %s", ErrUnknownKey, kmsKeyID)
    }
    plain, err := encryption.Decrypt(kek, encryptedDEK, aad)
    if err != nil {
        return nil, fmt.Errorf("%w: %s", ErrDecryptFailed, err.Error())
    }
    return plain, nil
}
```

### Tests

- `TestLocalDEKUnwrapper_RoundTrip` — encrypt 32 random bytes via `pkg/encryption.Encrypt(kek, dek, aad)`, decrypt via `LocalDEKUnwrapper.DecryptDEK(kmsKeyID, encryptedDEK, aad)` — assert plaintext matches.
- `TestLocalDEKUnwrapper_UnknownKey` — empty keks map, any kmsKeyID — assert `errors.Is(err, ErrUnknownKey)`.
- `TestLocalDEKUnwrapper_AADMismatch` — encrypt with `aad="A"`, decrypt with `aad="B"` — assert `errors.Is(err, ErrDecryptFailed)`.
- `TestLocalDEKUnwrapper_CtxCancelled` — pre-cancelled ctx — assert `errors.Is(err, context.Canceled)`.
- `TestNewLocalDEKUnwrapper_ClonesInputMap` — pass a map, mutate it, then call DecryptDEK — assert behaviour unchanged. Defence against accidentally-shared backing maps.

### TDD steps

- [ ] **Step 1: Write failing test (round-trip happy path)** — see test list above.
- [ ] **Step 2: Run — FAIL.**
- [ ] **Step 3: Implement** — see code sketch.
- [ ] **Step 4: Add the four other tests + confirm all 5 pass.**
- [ ] **Step 5: Commit.**

```bash
git add internal/recording/crypto/kms.go internal/recording/crypto/kms_test.go
git commit -m "feat(recording/crypto): Plan 12.2 Task 2 — DEKUnwrapper port + LocalDEKUnwrapper"
```

### Acceptance
- 5/5 tests green.
- Sentinels match `errors.Is` for `ErrUnknownKey`, `ErrDecryptFailed`.
- Compile-time `var _ DEKUnwrapper = (*LocalDEKUnwrapper)(nil)`.
- `NewLocalDEKUnwrapper` defensively clones the input map (regression test locked in).

---

## Task 3 — `internal/recording/storage/store.go` — ObjectStore

**Goal:** Define the `ObjectStore` port (Get + Delete are the only methods Plan 12.x needs) and ship a `LocalObjectStore` implementation. Yandex S3 adapter deferred to Plan 01.

**Files:**
- Create: `internal/recording/storage/store.go`
- Create: `internal/recording/storage/store_test.go`
- Create: `internal/recording/storage/main_test.go` (goleak)

### API

```go
// ObjectStore is the recording module's view of object storage. Plan 12.2
// uses Get for stream playback and Delete for the retention worker
// (deferred — Plan 12.4). Production runs against Yandex Object Storage
// (S3-compatible); the Local implementation lives in this package for
// tests and dev environments without cloud credentials.
//
// All methods are tenant-agnostic — the bucket name embeds the tenant
// segregation by design (see migration 000010 + BucketProvisioner). The
// caller is responsible for passing the correct (bucket, key) pair from
// the call_recordings row.
type ObjectStore interface {
    // Get streams the object as ciphertext. The returned ReadCloser MUST
    // be closed by the caller. The size argument is informational; it
    // matches call_recordings.bytes_size and the implementation MAY use
    // it to short-circuit a HEAD before the GET.
    //
    // Errors:
    //   ErrObjectNotFound — object does not exist or has been deleted.
    //   wrapped network/IO errors — propagated as-is for retry classification.
    Get(ctx context.Context, bucket, key string) (io.ReadCloser, error)

    // Delete removes the object. Idempotent — calling Delete on a missing
    // object returns nil (matches S3 semantics).
    Delete(ctx context.Context, bucket, key string) error
}

// ErrObjectNotFound is returned by Get when the object is missing.
var ErrObjectNotFound = errors.New("recording.storage: object not found")
```

### LocalObjectStore

```go
// LocalObjectStore is an in-memory ObjectStore for tests and local dev.
// Concurrent-safe via sync.RWMutex. Bucket isolation is preserved — each
// (bucket, key) tuple is a separate slot.
//
// Test seam: PutBytes(bucket, key, payload) seeds the store. Production
// uses the Yandex S3 implementation (Plan 01); the recording flow's
// "Put" path is owned by the ingest-uploader (Plan 08) which uses the
// Yandex SDK directly.
type LocalObjectStore struct {
    mu      sync.RWMutex
    objects map[string]map[string][]byte // bucket → key → bytes
}

func NewLocalObjectStore() *LocalObjectStore {
    return &LocalObjectStore{objects: make(map[string]map[string][]byte)}
}

// var _ ObjectStore = (*LocalObjectStore)(nil)
func (s *LocalObjectStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }
    s.mu.RLock()
    defer s.mu.RUnlock()
    b, ok := s.objects[bucket]
    if !ok {
        return nil, fmt.Errorf("%w: bucket=%s key=%s", ErrObjectNotFound, bucket, key)
    }
    payload, ok := b[key]
    if !ok {
        return nil, fmt.Errorf("%w: bucket=%s key=%s", ErrObjectNotFound, bucket, key)
    }
    // Clone to keep caller-mutations from corrupting the store.
    return io.NopCloser(bytes.NewReader(bytes.Clone(payload))), nil
}

func (s *LocalObjectStore) Delete(ctx context.Context, bucket, key string) error {
    if err := ctx.Err(); err != nil {
        return err
    }
    s.mu.Lock()
    defer s.mu.Unlock()
    if b, ok := s.objects[bucket]; ok {
        delete(b, key)
    }
    return nil
}

// PutBytes seeds the store with a single object — TEST USE ONLY.
func (s *LocalObjectStore) PutBytes(bucket, key string, payload []byte) {
    s.mu.Lock()
    defer s.mu.Unlock()
    if _, ok := s.objects[bucket]; !ok {
        s.objects[bucket] = make(map[string][]byte)
    }
    s.objects[bucket][key] = bytes.Clone(payload)
}
```

### Tests

- `TestLocalObjectStore_PutThenGetReturnsBytes` — PutBytes(b, k, payload), Get → ReadAll equals payload.
- `TestLocalObjectStore_GetMissingReturnsNotFound` — Get on empty store — assert `errors.Is(err, ErrObjectNotFound)`.
- `TestLocalObjectStore_DeleteMissingIsNoOp` — Delete on empty store returns nil.
- `TestLocalObjectStore_DeleteThenGetReturnsNotFound` — Put, Delete, Get — assert `ErrObjectNotFound`.
- `TestLocalObjectStore_GetIsolatedFromCallerMutations` — Put `["a","b","c"]`, Get → mutate slice → re-Get → assert second read still `["a","b","c"]` (clone safety).
- `TestLocalObjectStore_ConcurrentPutAndGet` — 10 goroutines × 1000 iterations PutBytes + Get on disjoint keys, no race or panic. Use `t.Parallel()` + `wg.Go`.

### TDD steps

- [ ] **Step 1: Write the 6 failing tests.**
- [ ] **Step 2: Run — FAIL** (`storage.NewLocalObjectStore` undefined).
- [ ] **Step 3: Implement `store.go`.**
- [ ] **Step 4: Run — PASS 6/6.**
- [ ] **Step 5: Add `main_test.go` with goleak.**
- [ ] **Step 6: Commit.**

```bash
git add internal/recording/storage/
git commit -m "feat(recording/storage): Plan 12.2 Task 3 — ObjectStore port + LocalObjectStore"
```

### Acceptance
- All 6 tests green under `-race -count=1`.
- `var _ ObjectStore = (*LocalObjectStore)(nil)`.
- Caller mutations to the returned bytes do NOT corrupt the store (clone safety locked in).
- `goleak` clean.

---

## Task 4 — Replace `RecordingService.OpenAudioStream` stub with real impl

**Goal:** Wire `DEKUnwrapper` + `ObjectStore` + `AudioDecryptor` together inside `(*svc).OpenAudioStream`. Replace the foundation-phase stub.

**Files:**
- Modify: `internal/recording/service/service.go` — add fields to `Deps` and `svc`; replace `OpenAudioStream`.
- Modify: `internal/recording/service/service_test.go` — add OpenAudioStream tests using in-memory wiring (NO new Postgres container — exercise only the read path with a test stub for the store layer).

### Service changes

#### `service.Deps` grows

```go
type Deps struct {
    Pool      *postgres.Pool
    Store     *store.PostgresStore
    Outbox    outbox.Writer
    Logger    *zap.Logger
    Metrics   *metrics.RecordingMetrics
    // NEW (Plan 12.2):
    Decryptor crypto.AudioDecryptor
    KMS       crypto.DEKUnwrapper
    Objects   storage.ObjectStore
}
```

#### `svc` grows

```go
type svc struct {
    pool      *postgres.Pool
    store     *store.PostgresStore
    outbox    outbox.Writer
    logger    *zap.Logger
    metrics   *metrics.RecordingMetrics
    decryptor crypto.AudioDecryptor
    kms       crypto.DEKUnwrapper
    objects   storage.ObjectStore
}
```

#### `New` populates the new fields with sensible defaults

```go
func New(d Deps) rapi.RecordingService {
    if d.Logger == nil {
        d.Logger = zap.NewNop()
    }
    if d.Outbox == nil {
        d.Outbox = outbox.NewPostgresWriter()
    }
    if d.Decryptor == nil {
        d.Decryptor = crypto.NewAESGCMDecryptor()
    }
    return &svc{
        pool:      d.Pool,
        store:     d.Store,
        outbox:    d.Outbox,
        logger:    d.Logger,
        metrics:   d.Metrics,
        decryptor: d.Decryptor,
        kms:       d.KMS,
        objects:   d.Objects,
    }
}
```

> **Note:** `d.KMS` and `d.Objects` are NOT defaulted — if either is nil, `OpenAudioStream` returns `ErrInvalidInput` wrapped with marker `"recording crypto/storage not wired"` (similar to the foundation-phase stubs for Search/VerifyChecksum). The Module wires real Local* impls in cmd/api when Plan 12.2 ships, so production never hits this path.

#### `OpenAudioStream` — REAL implementation

```go
// OpenAudioStream returns a streamed, decrypted reader for the audio.
// byteRange is IGNORED in v1 — Accept-Ranges: none is set on the HTTP
// response (Plan 12.3). Future v2 chunked-envelope format will support
// ranges natively.
//
// Pipeline:
//   1. Lookup row by (tenantID, callID).                   → store.GetByCallID
//   2. Bail if status == 'deleted'.
//   3. Unwrap DEK via the KMS port, AAD = tenant_id bytes. → kms.DecryptDEK
//   4. Stream ciphertext from S3 (full-buffer for v1).     → objects.Get
//   5. AES-GCM decrypt with AAD = tenant_id bytes.          → decryptor.Decrypt
//   6. Write recording.accessed audit row.                  → audit_log INSERT
//   7. Return AudioStream with bytes.Reader over plaintext.
//
// Metrics:
//   recording_access_total{tenant_id, result}              — new in this task
//   recording_decrypt_duration_seconds{tenant_id, result}  — new in this task
func (s *svc) OpenAudioStream(ctx context.Context, tenantID, callID uuid.UUID, byteRange *rapi.ByteRange) (rapi.AudioStream, error) {
    if s.kms == nil || s.objects == nil {
        return rapi.AudioStream{}, fmt.Errorf("%w: recording crypto/storage not wired", ErrInvalidInput)
    }

    start := time.Now()
    tenantLabel := tenantID.String()

    row, err := s.store.GetByCallID(ctx, tenantID, callID)
    if errors.Is(err, store.ErrCallNotFound) {
        s.metrics.ObserveAccess(tenantLabel, "not_found", time.Since(start).Seconds())
        return rapi.AudioStream{}, ErrNotFound
    }
    if err != nil {
        s.metrics.ObserveAccess(tenantLabel, "error", time.Since(start).Seconds())
        return rapi.AudioStream{}, fmt.Errorf("recording.open_audio: %w", err)
    }
    if row.Status == "deleted" {
        s.metrics.ObserveAccess(tenantLabel, "deleted", time.Since(start).Seconds())
        return rapi.AudioStream{}, ErrAlreadyDeleted
    }

    aad := []byte(row.TenantID.String())

    dekPlain, err := s.kms.DecryptDEK(ctx, row.KMSKeyID, row.EncryptedDEK, aad)
    if err != nil {
        s.metrics.ObserveAccess(tenantLabel, "kms_error", time.Since(start).Seconds())
        return rapi.AudioStream{}, fmt.Errorf("recording.open_audio.kms: %w", err)
    }
    defer zeroBytes(dekPlain) // best-effort; defer fires after we've used it for decrypt

    rc, err := s.objects.Get(ctx, row.S3Bucket, row.AudioObjectKey)
    if err != nil {
        s.metrics.ObserveAccess(tenantLabel, "object_error", time.Since(start).Seconds())
        if errors.Is(err, storage.ErrObjectNotFound) {
            return rapi.AudioStream{}, ErrNotFound // hide the storage shape from API consumers
        }
        return rapi.AudioStream{}, fmt.Errorf("recording.open_audio.object: %w", err)
    }
    defer rc.Close()

    plain, err := s.decryptor.Decrypt(ctx, dekPlain, rc, row.BytesSize, aad)
    if err != nil {
        s.metrics.ObserveAccess(tenantLabel, "decrypt_error", time.Since(start).Seconds())
        return rapi.AudioStream{}, fmt.Errorf("recording.open_audio.decrypt: %w", err)
    }

    if err := s.writeAccessAudit(ctx, row); err != nil {
        // Audit failure must NOT block playback — log + tick + continue.
        s.logger.Warn("recording access audit failed",
            zap.String("tenant_id", tenantLabel),
            zap.String("call_id", callID.String()),
            zap.Error(err))
        s.metrics.ObserveAccess(tenantLabel, "audit_failed", time.Since(start).Seconds())
    } else {
        s.metrics.ObserveAccess(tenantLabel, "ok", time.Since(start).Seconds())
    }

    return rapi.AudioStream{
        Reader:        io.NopCloser(bytes.NewReader(plain)),
        ContentType:   contentTypeForCodec(row.Codec),
        ContentLength: int64(len(plain)),
        StartOffset:   0,
        EndOffset:     int64(len(plain)) - 1,
    }, nil
}

// contentTypeForCodec returns the canonical MIME for our supported codecs.
// Default ("opus", "opus-32") → audio/ogg (the FreeSWITCH side packages opus
// in an Ogg container). Unknown codecs fall back to application/octet-stream
// so misconfigured rows are detectable in browser DevTools.
func contentTypeForCodec(codec string) string {
    switch codec {
    case "opus", "opus-32":
        return "audio/ogg"
    default:
        return "application/octet-stream"
    }
}

// zeroBytes overwrites the buffer with zeros. Best-effort hygiene against
// the DEK plaintext lingering in heap memory longer than necessary. Go's
// GC may still hold a copy; cryptographic claims here are weak.
func zeroBytes(b []byte) {
    for i := range b {
        b[i] = 0
    }
}

// writeAccessAudit appends an audit_log row recording who fetched what.
// Single-statement INSERT outside a transaction (caller doesn't need
// rollback semantics — playback proceeds even on audit failure).
func (s *svc) writeAccessAudit(ctx context.Context, r store.RecordingRow) error {
    payload, err := json.Marshal(map[string]any{
        "recording_id":     r.ID,
        "call_id":          r.CallID,
        "audio_object_key": r.AudioObjectKey,
        "bytes_size":       r.BytesSize,
        "sha256":           r.SHA256Hex,
    })
    if err != nil {
        return err
    }
    return s.pool.WithTenant(ctx, r.TenantID, func(tx postgres.Tx) error {
        const q = `
INSERT INTO audit_log (tenant_id, action, target_kind, target_id, payload, ts)
VALUES ($1, $2, $3, $4, $5, now())
`
        _, err := tx.Exec(ctx, q,
            r.TenantID, rapi.AuditActionAccessed, "recording", r.ID.String(), payload,
        )
        return err
    })
}
```

#### Metrics extension

`internal/recording/metrics/metrics.go` gains:

```go
// add to RecordingMetrics struct
AccessTotal      *prometheus.CounterVec   // labels: tenant_id, result
AccessDuration   *prometheus.HistogramVec // labels: tenant_id, result

// in RegisterRecordingMetrics constructor:
m.AccessTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
    Namespace: "sociopulse",
    Subsystem: "recording",
    Name:      "access_total",
    Help:      "Number of OpenAudioStream calls broken out by result {ok|not_found|deleted|kms_error|object_error|decrypt_error|audit_failed|error}.",
}, []string{"tenant_id", "result"})
m.AccessDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
    Namespace: "sociopulse",
    Subsystem: "recording",
    Name:      "access_duration_seconds",
    Help:      "Wall time of one OpenAudioStream call (lookup + KMS + S3 + decrypt + audit).",
    Buckets:   prometheus.DefBuckets,
}, []string{"tenant_id", "result"})

// add to the registration loop:
for _, c := range []prometheus.Collector{m.CommitTotal, m.StorageSizeBytes, m.CommitDuration, m.AccessTotal, m.AccessDuration} {
    if err := reg.Register(c); err != nil {
        return nil, fmt.Errorf("recording metrics: register: %w", err)
    }
}
```

`ObserveAccess(tenantID, result, durSec)` method mirrors `ObserveCommit`.

### Tests

Six new tests in `service_test.go`, using a helper `buildServiceWithCrypto` that spins up testcontainers Postgres + wires `LocalDEKUnwrapper` + `LocalObjectStore`. Build tag stays `//go:build integration`.

- `TestService_OpenAudioStream_HappyPath`:
  1. seedCall + commit a recording with `encrypted_dek` = `Encrypt(KEK, randomDEK, aad=tenant_id)` and seed `LocalObjectStore.PutBytes(bucket, key, Encrypt(DEK, "hello audio", aad=tenant_id))`.
  2. call OpenAudioStream → assert ReadAll == "hello audio", ContentType == "audio/ogg", ContentLength == len("hello audio"), audit_log has one `recording.accessed` row.
- `TestService_OpenAudioStream_NotFound` — bogus call_id → `errors.Is(err, rapi.ErrNotFound)`.
- `TestService_OpenAudioStream_AlreadyDeleted` — manually update row.status='deleted', call OpenAudioStream → `errors.Is(err, rapi.ErrAlreadyDeleted)`.
- `TestService_OpenAudioStream_KMSWrongAAD` — seed wrapped DEK with `aad="wrong"`, call OpenAudioStream → wrapped error containing `"kms"` in message; metrics tick on `kms_error`.
- `TestService_OpenAudioStream_ObjectMissing` — commit row but skip `PutBytes` → `errors.Is(err, rapi.ErrNotFound)` (hidden behind ErrNotFound; do NOT leak `ErrObjectNotFound`).
- `TestService_OpenAudioStream_NotWired` — build service via `buildService(t, pool)` (no Local* injection) → `errors.Is(err, rapi.ErrInvalidInput)` and message contains `"not wired"`.

### TDD steps

- [ ] **Step 1: Add metrics first** (small, isolated change):

```go
// in internal/recording/metrics/metrics.go — add the two new collectors per the sketch.
// Update existing metrics_test.go to assert RegisterRecordingMetrics still NoError on
// nil registry and DuplicateFails on second registration.
```

- [ ] **Step 2: Run metrics tests** — confirm 3/3 still pass.

```bash
go test -race -count=1 ./internal/recording/metrics/...
```

- [ ] **Step 3: Modify Deps + svc + New** in `service.go` per the sketches above. `OpenAudioStream` still stubbed at this point — only the field plumbing.

- [ ] **Step 4: Run service tests** — should still PASS (Plan 12.1 tests don't touch the new fields).

```bash
go test -tags=integration -race -count=1 ./internal/recording/service/...
```

- [ ] **Step 5: Replace `OpenAudioStream` body** with the real implementation per the sketch.

- [ ] **Step 6: Add the 6 new tests + helper `buildServiceWithCrypto(t, pool)`** to `service_test.go`. Helper sketch:

```go
func buildServiceWithCrypto(t *testing.T, pool *postgres.Pool) (rapi.RecordingService, *crypto.LocalDEKUnwrapper, *storage.LocalObjectStore, []byte) {
    t.Helper()
    pgStore := store.NewPostgresStore(pool)
    met, err := metrics.RegisterRecordingMetrics(nil)
    require.NoError(t, err)

    kek := make([]byte, 32)
    _, _ = rand.Read(kek)
    kms := crypto.NewLocalDEKUnwrapper(map[string][]byte{"kek-test": kek})
    objects := storage.NewLocalObjectStore()

    svc := service.New(service.Deps{
        Pool:      pool,
        Store:     pgStore,
        Logger:    zaptest.NewLogger(t),
        Metrics:   met,
        Decryptor: crypto.NewAESGCMDecryptor(),
        KMS:       kms,
        Objects:   objects,
    })
    return svc, kms, objects, kek
}
```

- [ ] **Step 7: Run all service tests — confirm 9 (Plan 12.1) + 6 (Plan 12.2) = 15 PASS.**

```bash
go test -tags=integration -race -count=1 ./internal/recording/service/...
```

- [ ] **Step 8: Commit**

```bash
git add internal/recording/service/ internal/recording/metrics/
git commit -m "feat(recording/service): Plan 12.2 Task 4 — OpenAudioStream real impl + access metrics"
```

### Acceptance
- All 15 service tests green under `-tags=integration -race -count=1`.
- `recording_access_total` + `recording_access_duration_seconds` registered.
- Audit row written for every successful access (assert via row count).
- ErrObjectNotFound is HIDDEN behind ErrNotFound (no leak of storage shape).
- Audit failure logs WARN + tick metric but does NOT block playback.

---

## Task 5 — Module wiring + cmd/api composition

**Goal:** Update `internal/recording/Module` to accept `DEKUnwrapper` + `ObjectStore` via `Config`; cmd/api wires Local* impls today, leaves a hook for the Yandex impls when Plan 01 lands.

**Files:**
- Modify: `internal/recording/module.go` — `Config` grows with `DEKUnwrapper` + `ObjectStore` fields; `Register` passes them to `service.New`.
- Modify: `cmd/api/recording.go` — construct `crypto.NewLocalDEKUnwrapper(...)` + `storage.NewLocalObjectStore()` and inject into `recording.New(Config{...})`.

### Module changes

```go
// internal/recording/module.go (excerpt)

type Config struct {
    Registerer prometheus.Registerer
    GRPCConfig *grpcserver.Config

    // NEW (Plan 12.2): crypto + storage ports. If either is nil at
    // Register time, Module proceeds with the foundation-phase stub
    // semantics — OpenAudioStream returns ErrInvalidInput "not wired".
    DEKUnwrapper crypto.DEKUnwrapper
    ObjectStore  storage.ObjectStore
}
```

```go
// internal/recording/module.go (Register excerpt)

svc := service.New(service.Deps{
    Pool:      d.Pool,
    Store:     pgStore,
    Logger:    logger,
    Metrics:   met,
    Decryptor: crypto.NewAESGCMDecryptor(),
    KMS:       m.cfg.DEKUnwrapper,
    Objects:   m.cfg.ObjectStore,
})
```

### cmd/api wiring

```go
// cmd/api/recording.go (excerpt)

// recordingPorts builds the Local* DEKUnwrapper + ObjectStore for now.
// Plan 01 (Yandex infra) will add a -tags=yandex_kms / -tags=yandex_s3
// branch that returns the SDK-backed adapters instead.
//
// In v1 the KEK material is sourced from configuration: cfg.Recording.LocalKEKs
// is a map[kmsKeyID]hexEncodedKEK. Production deployments either set this
// to a single platform-wide test KEK (dev) OR leave it empty + supply the
// real Yandex-tagged binary instead.
func recordingPorts(cfg config.RecordingConfig, logger *zap.Logger) (crypto.DEKUnwrapper, storage.ObjectStore, error) {
    keks := make(map[string][]byte, len(cfg.LocalKEKs))
    for keyID, hexKEK := range cfg.LocalKEKs {
        kek, err := hex.DecodeString(hexKEK)
        if err != nil {
            return nil, nil, fmt.Errorf("recording: decode local KEK %q: %w", keyID, err)
        }
        if len(kek) != 32 {
            return nil, nil, fmt.Errorf("recording: local KEK %q must be 32 bytes (got %d)", keyID, len(kek))
        }
        keks[keyID] = kek
    }
    if len(keks) == 0 {
        logger.Warn("recording: no local KEKs configured — OpenAudioStream will fail until Plan 01 wires Yandex KMS")
    }
    return crypto.NewLocalDEKUnwrapper(keks), storage.NewLocalObjectStore(), nil
}
```

```go
// cmd/api/main.go — point of injection (excerpt)

dekUnwrapper, objectStore, err := recordingPorts(cfg.Recording, logger.Named("recording"))
if err != nil {
    return fmt.Errorf("recording ports: %w", err)
}

recordingModule := recording.New(recording.Config{
    Registerer:   metrics.Registry,
    GRPCConfig:   recordingGRPCConfig(cfg.Recording),
    DEKUnwrapper: dekUnwrapper,
    ObjectStore:  objectStore,
})
```

### Config evolution

`pkg/config/recording.go` gains:

```go
// LocalKEKs is a map of kms_key_id → hex-encoded 32-byte KEK plaintext.
// Used by the Local DEKUnwrapper for dev/test environments without
// access to Yandex Cloud KMS. Production deployments either build with
// -tags=yandex_kms (which routes through the Yandex SDK adapter that
// IGNORES this field) OR populate this with a platform-wide test KEK
// for non-prod investigations.
//
// Format example (config.yaml):
//   recording:
//     local_keks:
//       kek-tenant-A: a1b2c3...  # 64 hex chars
LocalKEKs map[string]string `mapstructure:"local_keks"`
```

`configs/development/config.yaml` gets a placeholder entry that's commented out (so no test KEK is committed):

```yaml
recording:
  enabled: false
  # local_keks:
  #   kek-platform-test: "0000000000000000000000000000000000000000000000000000000000000000"
```

### Steps

- [ ] **Step 1: Update `pkg/config/recording.go`** with `LocalKEKs` field. Default empty map.

- [ ] **Step 2: Add `cmd/api/recording.go::recordingPorts`** helper.

- [ ] **Step 3: Patch `cmd/api/main.go`** — call `recordingPorts(...)`, pass result into `recording.New(...)`.

- [ ] **Step 4: Update `internal/recording/module.go`** — `Config` adds two fields; `Register` plumbs them into `service.New`.

- [ ] **Step 5: Smoke build + unit tests + cmd/api smoke run**

```bash
go build ./...
go vet ./...
go test -race -count=1 ./internal/recording/... ./pkg/config/... ./cmd/api/...
go test -tags=integration -race -count=1 ./internal/recording/...
go run ./cmd/api --help  # confirms cmd/api still boots
```

Expected: all green, --help exits 0.

- [ ] **Step 6: Commit**

```bash
git add internal/recording/module.go cmd/api/main.go cmd/api/recording.go pkg/config/recording.go configs/development/config.yaml
git commit -m "feat(recording): Plan 12.2 Task 5 — wire DEKUnwrapper + ObjectStore via Module.Config"
```

### Acceptance
- `cmd/api` boots cleanly with `recording.enabled=false` (default) regardless of `local_keks` content.
- With `recording.enabled=true` + valid mTLS certs + `local_keks` populated, gRPC Commit + OpenAudioStream both work end-to-end against Local* impls.
- `recording.LocalKEKs` is HEX-decoded; non-32-byte KEKs fail boot (defence-in-depth).

---

## Self-review

### Spec coverage (against Plan 12 design brief §9, §FR-G, §13.6, §15.5, ADR-005)

| Brief requirement | Plan 12.2 task | Status |
|---|---|---|
| Envelope encryption: per-recording DEK + per-tenant KEK | Tasks 1+2 — `AudioDecryptor` + `DEKUnwrapper` ports | ✅ |
| AES-256-GCM stream decrypt | Task 1 (buffer-in-RAM v1; chunked v2 deferred) | ✅ v1 |
| KMS Decrypt of wrapped DEK | Task 2 — `LocalDEKUnwrapper`; Yandex adapter Plan 01 | partial — Local only |
| `Accept-Ranges: none` in v1 | Plan 12.3 (HTTP layer); byteRange ignored in service | ✅ contract documented |
| 200 MiB cap on in-RAM decrypt | Task 1 — `MaxAudioPlaintextBytes` enforced | ✅ |
| `recording.accessed` audit on every read | Task 4 — `writeAccessAudit` | ✅ |
| Metrics: `recording_access_total{actor_role}` | Task 4 — `AccessTotal{tenant_id, result}` (actor_role to be added Plan 12.3 once role is known) | partial |
| Metrics: `recording_decrypt_duration_seconds` | Task 4 — `AccessDuration{tenant_id, result}` | ✅ |
| ErrObjectNotFound hidden from API consumers | Task 4 — folded into `ErrNotFound` | ✅ |
| `tenant_id` AAD binding | Tasks 1+2+4 — `aad = []byte(tenantID.String())` everywhere | ✅ |

### Placeholder scan

- "Future v2 chunked-envelope" mentioned twice — both with explicit Plan 12 design brief reference + sentinel name. Not a placeholder.
- `// add to ...` comments in Task 4 metrics extension — those are EDIT instructions for the implementer, paired with concrete code. Acceptable.
- No "TBD", "implement later", "fill in details" anywhere.

### Type/name consistency

- `AudioDecryptor` (Task 1) → consumed by `service.svc.decryptor` (Task 4) → wired by `service.New` → defaulted to `crypto.NewAESGCMDecryptor()`. ✓
- `DEKUnwrapper` (Task 2) → consumed by `service.svc.kms` (Task 4) → wired by `Module.Config.DEKUnwrapper` (Task 5) → constructed by `cmd/api/recording.go::recordingPorts` (Task 5). ✓
- `ObjectStore` (Task 3) → consumed by `service.svc.objects` (Task 4) → wired by `Module.Config.ObjectStore` (Task 5) → constructed by `cmd/api/recording.go::recordingPorts` (Task 5). ✓
- All sentinels prefixed with their package: `crypto.ErrAuth`, `crypto.ErrAudioTooLargeForV1`, `crypto.ErrUnknownKey`, `crypto.ErrDecryptFailed`, `crypto.ErrUnauthorised`, `storage.ErrObjectNotFound`. No collisions.
- `metrics.RecordingMetrics` gains two collectors with consistent label names (`tenant_id, result`).

### Carry-forward checklist

- [x] No `init()` MustRegister — metrics extended via existing constructor.
- [x] `*zap.Logger` nil-safe — `zap.NewNop()` fallback inherited from Plan 12.1.
- [x] Sentinels — every new error matches `errors.Is`.
- [x] Compile-time interface checks — all three new types (`AESGCMDecryptor`, `LocalDEKUnwrapper`, `LocalObjectStore`) checked against their interfaces.
- [x] `t.Parallel()` + `t.Cleanup()` + `t.Context()` — used everywhere.
- [x] `goleak.VerifyTestMain` — added per package.
- [x] No `time.After` in select-loops — n/a.
- [x] Modernize: `any` over `interface{}`, range over int — used.
- [x] `wg.Go` — n/a (no goroutines in service.OpenAudioStream).
- [x] gopls cache pollution — diagnostic warning included as carry-forward rule.
- [x] Module path `github.com/sociopulse/platform`.

### Out of scope (correctly deferred)

- Yandex Cloud KMS real adapter — Plan 01.
- Yandex Object Storage real adapter — Plan 01.
- Chunked-envelope format with per-chunk auth tags — v2.
- HTTP `/api/calls/{id}/recording` route — Plan 12.3.
- `actor_role` label in `recording_access_total` — Plan 12.3 (HTTP knows the JWT role).
- Per-tenant rate limiting on OpenAudioStream — Plan 12.3 (HTTP middleware).
- DEK plaintext zeroing under stricter cryptographic guarantees — defer (current `zeroBytes` is best-effort).

**Plan 12.2 verified.**

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-09-12-2-recording-crypto-s3.md`.**
