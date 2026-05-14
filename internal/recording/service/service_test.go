//go:build integration

package service_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/zap/zaptest"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/crypto"
	"github.com/sociopulse/platform/internal/recording/metrics"
	"github.com/sociopulse/platform/internal/recording/service"
	"github.com/sociopulse/platform/internal/recording/storage"
	"github.com/sociopulse/platform/internal/recording/store"
	"github.com/sociopulse/platform/pkg/encryption"
	"github.com/sociopulse/platform/pkg/postgres"
)

func TestService_Commit_Fresh(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)

	svc := buildService(t, pool)

	out, err := svc.Commit(t.Context(), newCommitInput(t, tenantID, callID))
	require.NoError(t, err)
	require.False(t, out.IdempotentReplay)
	require.NotEqual(t, uuid.Nil, out.RecordingID)

	requireExactlyOneOutboxRow(t, pool, tenantID, callID)
	requireExactlyOneAuditRow(t, pool, tenantID, out.RecordingID, "recording.committed")
}

func TestService_Commit_Idempotent(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)

	svc := buildService(t, pool)
	in := newCommitInput(t, tenantID, callID)

	first, err := svc.Commit(t.Context(), in)
	require.NoError(t, err)
	require.False(t, first.IdempotentReplay)

	second, err := svc.Commit(t.Context(), in)
	require.NoError(t, err)
	require.True(t, second.IdempotentReplay)
	require.Equal(t, first.RecordingID, second.RecordingID)

	// Side-effects emitted exactly ONCE despite two Commits.
	requireExactlyOneOutboxRow(t, pool, tenantID, callID)
	requireExactlyOneAuditRow(t, pool, tenantID, first.RecordingID, "recording.committed")
}

func TestService_Commit_InvalidInput(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)
	svc := buildService(t, pool)

	cases := []struct {
		name string
		mut  func(*rapi.CommitInput)
	}{
		{"missing_tenant", func(i *rapi.CommitInput) { i.TenantID = uuid.Nil }},
		{"missing_call", func(i *rapi.CommitInput) { i.CallID = uuid.Nil }},
		{"sha256_short", func(i *rapi.CommitInput) { i.SHA256Hex = "abcd" }},
		{"sha256_long", func(i *rapi.CommitInput) {
			i.SHA256Hex = "f1e2d3c4b5a697887766554433221100ffeeddccbbaa99887766554433221100EE"
		}},
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
			in := newCommitInput(t, tenantID, callID)
			tc.mut(&in)
			_, err := svc.Commit(t.Context(), in)
			require.True(t, errors.Is(err, rapi.ErrInvalidInput),
				"expected ErrInvalidInput, got %v", err)
		})
	}
}

func TestService_Commit_CallNotFound(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)

	svc := buildService(t, pool)
	in := newCommitInput(t, tenantID, uuid.Must(uuid.NewV7())) // call never seeded
	_, err := svc.Commit(t.Context(), in)
	require.True(t, errors.Is(err, rapi.ErrCallNotFound),
		"expected ErrCallNotFound, got %v", err)
}

func TestService_Get_Found(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)
	svc := buildService(t, pool)

	in := newCommitInput(t, tenantID, callID)
	commitOut, err := svc.Commit(t.Context(), in)
	require.NoError(t, err)

	md, err := svc.Get(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.Equal(t, commitOut.RecordingID, md.RecordingID)
	require.Equal(t, in.SHA256Hex, md.SHA256Hex)
	require.Equal(t, in.BytesSize, md.BytesSize)
}

func TestService_Get_NotFound(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	svc := buildService(t, pool)

	_, err := svc.Get(t.Context(), tenantID, uuid.Must(uuid.NewV7()))
	require.True(t, errors.Is(err, rapi.ErrNotFound),
		"expected api.ErrNotFound, got %v", err)
}

// The two NotImplemented tests verify only the marker-string contract on
// the deferred service methods — they don't touch storage, so we skip the
// 90-second Postgres container bring-up by passing nil Pool/Store. The svc
// returns the placeholder error before reaching any DB call.

// TestService_OpenAudioStream_NotWired covers the same scenario the old
// foundation-phase TestService_OpenAudioStream_NotImplemented test did —
// service constructed without KMS / Objects ports, expected to fail fast
// without touching DB or storage. The stub branch's marker text changed
// from "not implemented in foundation phase" to "not wired" once the real
// implementation landed in Plan 12.2 Task 4.
func TestService_OpenAudioStream_NotWired(t *testing.T) {
	t.Parallel()
	svc := newStubService(t)

	_, err := svc.OpenAudioStream(t.Context(), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), nil)
	require.True(t, errors.Is(err, rapi.ErrInvalidInput))
	require.Contains(t, err.Error(), "not wired")
}

// newStubService builds a minimum-viable RecordingService for tests that
// only assert on the deferred-method placeholders. No DB, no metrics, no
// outbox — the stubs return before any field is dereferenced.
func newStubService(t *testing.T) rapi.RecordingService {
	t.Helper()
	return service.New(service.Deps{})
}

// ────────── OpenAudioStream tests (Plan 12.2 Task 4) ──────────

func TestService_OpenAudioStream_HappyPath(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)

	svc, _, objects, kek := buildServiceWithCrypto(t, pool)
	audio := []byte("hello recording audio bytes")
	recordingID, plaintext := commitRecordingWithEncrypted(t, svc, objects, kek, tenantID, callID, audio)

	stream, err := svc.OpenAudioStream(t.Context(), tenantID, callID, nil)
	require.NoError(t, err)
	require.NotNil(t, stream.Reader)
	t.Cleanup(func() { _ = stream.Reader.Close() })

	got, err := io.ReadAll(stream.Reader)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)

	require.Equal(t, "audio/ogg", stream.ContentType)
	require.Equal(t, int64(len(plaintext)), stream.ContentLength)

	// Audit row must exist for recording.accessed.
	requireExactlyOneAuditRow(t, pool, tenantID, recordingID, "recording.accessed")
}

func TestService_OpenAudioStream_NotFound(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	svc, _, _, _ := buildServiceWithCrypto(t, pool)

	_, err := svc.OpenAudioStream(t.Context(), tenantID, uuid.Must(uuid.NewV7()), nil)
	require.True(t, errors.Is(err, rapi.ErrNotFound),
		"expected ErrNotFound, got %v", err)
}

func TestService_OpenAudioStream_AlreadyDeleted(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)
	svc, _, objects, kek := buildServiceWithCrypto(t, pool)
	commitRecordingWithEncrypted(t, svc, objects, kek, tenantID, callID, []byte("audio"))

	// Manually update status='deleted' via WithTenant tx so the RLS-protected
	// row is reachable from the test caller.
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`UPDATE call_recordings SET status='deleted' WHERE call_id = $1`, callID)
		return err
	}))

	_, err := svc.OpenAudioStream(t.Context(), tenantID, callID, nil)
	require.True(t, errors.Is(err, rapi.ErrAlreadyDeleted),
		"expected ErrAlreadyDeleted, got %v", err)
}

func TestService_OpenAudioStream_KMSWrongAAD(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)
	svc, _, objects, kek := buildServiceWithCrypto(t, pool)

	// Encrypt the DEK with WRONG aad to simulate a corrupted record.
	// At Open time the service's AAD = BuildAAD(tenant, "recording.dek",
	// callID) won't match, so the LocalDEKUnwrapper returns
	// ErrDecryptFailed and the service maps that into "kms_error" + a
	// wrapped error containing "kms".
	aad := []byte("wrong-aad-not-tenant-id")
	dek := make([]byte, 32)
	_, err := rand.Read(dek)
	require.NoError(t, err)
	encryptedDEK, err := encryption.Encrypt(kek, dek, aad)
	require.NoError(t, err)

	// Commit with the wrongly-AAD'd encryptedDEK — Commit accepts it
	// (it doesn't unwrap). OpenAudioStream will fail at the KMS step.
	in := newCommitInput(t, tenantID, callID)
	in.S3Bucket = "rec-bucket-test"
	in.AudioObjectKey = "k.opus.enc"
	in.KMSKeyID = "kek-test"
	in.EncryptedDEK = encryptedDEK
	in.BytesSize = 1024 // doesn't matter — we'll fail before reading
	_, err = svc.Commit(t.Context(), in)
	require.NoError(t, err)
	objects.PutBytes(in.S3Bucket, in.AudioObjectKey, make([]byte, 1024))

	_, err = svc.OpenAudioStream(t.Context(), tenantID, callID, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, crypto.ErrDecryptFailed,
		"wrong-AAD MUST surface as crypto.ErrDecryptFailed; got %v", err)
}

func TestService_OpenAudioStream_ObjectMissing(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)
	svc, _, objects, kek := buildServiceWithCrypto(t, pool)

	// Commit normally but DO NOT seed objects — the Get will miss.
	// AAD must match what the service will use at Decrypt time:
	// BuildAAD(tenant, "recording.dek", callID).
	dekAAD := encryption.BuildAAD(tenantID, "recording.dek", callID.String())
	dek := make([]byte, 32)
	_, err := rand.Read(dek)
	require.NoError(t, err)
	encryptedDEK, err := encryption.Encrypt(kek, dek, dekAAD)
	require.NoError(t, err)

	in := newCommitInput(t, tenantID, callID)
	in.S3Bucket = "rec-bucket-test"
	in.AudioObjectKey = "missing.opus.enc"
	in.KMSKeyID = "kek-test"
	in.EncryptedDEK = encryptedDEK
	in.BytesSize = 1024
	_, err = svc.Commit(t.Context(), in)
	require.NoError(t, err)
	_ = objects // unused — intentionally not seeded

	_, err = svc.OpenAudioStream(t.Context(), tenantID, callID, nil)
	require.True(t, errors.Is(err, rapi.ErrNotFound),
		"ErrObjectNotFound MUST be hidden behind ErrNotFound; got %v", err)
}

// TestRecording_DEKCiphertextSwap_AcrossCalls_SameTenant_Rejected is the
// service-layer demonstration that Plan 13.2.5 Task 6 closes the intra-
// tenant DEK swap defect on the recording flow. Two calls share a tenant;
// each Commit registers a recording whose EncryptedDEK was sealed under
// AAD = BuildAAD(tenant, "recording.dek", callID). An attacker copies
// call A's EncryptedDEK bytes into call B's row, then asks the service
// to OpenAudioStream(B). The service's DEK unwrap step uses callID = B
// in the AAD, but the ciphertext was sealed with callID = A — the AEAD
// auth tag check MUST fail rather than silently surfacing call A's DEK
// (which would in turn try to decrypt B's audio under the wrong key and
// either return garbage or panic the decryptor).
//
// Without the fix this test does NOT fail at the AEAD layer: the broken
// code passed AAD = []byte(tenantID.String()) for both Encrypt and
// Decrypt, so cross-call DEK swap within the same tenant succeeded all
// the way through KMS unwrap and was only caught (if at all) by the
// audio AES-GCM step — and only because the audio AAD bound to the
// per-call DEK plaintext, not the call identity. The fix binds callID
// into the DEK envelope itself.
func TestRecording_DEKCiphertextSwap_AcrossCalls_SameTenant_Rejected(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callA := seedCall(t, pool)
	callB := seedCallForTenant(t, pool, tenantID)

	svc, _, objects, kek := buildServiceWithCrypto(t, pool)

	// Both recordings: same tenant, distinct calls, each DEK sealed under
	// its own (tenant, "recording.dek", callID) AAD via the fixture.
	commitRecordingWithEncrypted(t, svc, objects, kek, tenantID, callA, []byte("call-A audio"))
	commitRecordingWithEncrypted(t, svc, objects, kek, tenantID, callB, []byte("call-B audio"))

	// Snapshot call A's stored ciphertext bytes BEFORE we mutate row B,
	// so the swap is reproducible regardless of pgx column ordering.
	var dekA []byte
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		return tx.QueryRow(t.Context(),
			`SELECT encrypted_dek FROM call_recordings WHERE call_id = $1`, callA,
		).Scan(&dekA)
	}))
	require.NotEmpty(t, dekA, "prerequisite: call A's EncryptedDEK is populated")

	// Snapshot call B's original DEK so we can assert the swap actually
	// replaced distinct ciphertext bytes (defensive against an accidental
	// fixture that yields identical envelopes).
	var dekBOrig []byte
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		return tx.QueryRow(t.Context(),
			`SELECT encrypted_dek FROM call_recordings WHERE call_id = $1`, callB,
		).Scan(&dekBOrig)
	}))
	require.NotEqual(t, dekA, dekBOrig,
		"prerequisite: each call's wrapped DEK must be distinct (BuildAAD bound to callID)")

	// Attacker: replace row B's EncryptedDEK with row A's bytes.
	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`UPDATE call_recordings SET encrypted_dek = $1 WHERE call_id = $2`,
			dekA, callB,
		)
		return err
	}))

	// Attempt to open call B's audio: the unwrap path uses BuildAAD with
	// callID = B but the ciphertext was sealed with callID = A. AEAD MUST
	// fail at the KMS/DEK unwrap layer rather than letting playback proceed.
	_, err := svc.OpenAudioStream(t.Context(), tenantID, callB, nil)
	require.Error(t, err, "intra-tenant DEK swap MUST be rejected — got nil error")
	require.ErrorIs(t, err, crypto.ErrDecryptFailed,
		"swap rejection MUST surface as crypto.ErrDecryptFailed; got %v", err)
}

// TestCommit_OutboxPayload_HasAllAnalyticsFields is the Plan 13.2 Task 1
// guard for the extended RecordingUploadedEvent payload. The CH
// events_recording_uploaded table (migrations/clickhouse/000003_*) needs
// project_id / fs_node / s3_key / encryption_key_alias / event_id /
// duration_sec — fields that did NOT exist in the original Plan 12.1
// payload. The analytics ingester (Plan 13.2 Task 2) cannot fabricate
// them, so the producer must include them.
//
// Backwards-compat: existing fields (recording_id / call_id / tenant_id /
// bytes_size / duration_ms / sha256 / status / committed_at) stay where
// they were. This test asserts ALL fields — both the existing ones (so
// drift surfaces here) and the new ones (so missing-emit surfaces here).
func TestCommit_OutboxPayload_HasAllAnalyticsFields(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)

	svc := buildService(t, pool)

	in := newCommitInput(t, tenantID, callID)
	out, err := svc.Commit(t.Context(), in)
	require.NoError(t, err)
	require.False(t, out.IdempotentReplay)

	// Read the outbox row written by Commit.
	subject := rapi.SubjectRecordingUploadedFor(tenantID)
	var payload []byte
	require.NoError(t, pool.RawQueryRow(context.Background(),
		`SELECT payload FROM event_outbox
		 WHERE tenant_id = $1 AND subject = $2 AND aggregate_id = $3`,
		tenantID, subject, callID,
	).Scan(&payload))

	var ev rapi.RecordingUploadedEvent
	require.NoError(t, json.Unmarshal(payload, &ev))

	// Existing fields — present before Plan 13.2.
	require.Equal(t, out.RecordingID, ev.RecordingID)
	require.Equal(t, callID, ev.CallID)
	require.Equal(t, tenantID, ev.TenantID)
	require.Equal(t, in.BytesSize, ev.BytesSize)
	require.Equal(t, in.Duration.Milliseconds(), ev.DurationMS)
	require.Equal(t, in.SHA256Hex, ev.SHA256Hex)
	require.Equal(t, "stored", ev.Status)
	require.NotZero(t, ev.CommittedAt)

	// Plan 13.2 additive fields — MUST be non-zero / non-empty for an
	// analytics-bound row.
	require.NotEqual(t, uuid.Nil, ev.ProjectID, "project_id must be sourced (joined from calls)")
	require.NotEmpty(t, ev.S3Key, "s3_key must be populated for CH ingest")
	require.NotEmpty(t, ev.EncryptionKeyAlias, "encryption_key_alias must be populated")
	require.NotEqual(t, uuid.Nil, ev.EventID, "event_id must be a fresh uuid for dedup")
	require.Equal(t, int32(in.Duration.Seconds()), ev.DurationSec,
		"duration_sec must equal floor(duration_ms / 1000)")
	// FSNode mirrors calls.freeswitch_node. In this test fixture the
	// calls row is seeded with NULL freeswitch_node, so an empty string
	// is the expected v1 fidelity caveat (analytics ingester treats ""
	// as "unknown"). Make the invariant load-bearing in CI.
	require.Empty(t, ev.FSNode)
}

// ────────── helpers ──────────

func buildService(t *testing.T, pool *postgres.Pool) rapi.RecordingService {
	t.Helper()
	pgStore := store.NewPostgresStore(pool)
	met, err := metrics.RegisterRecordingMetrics(nil) // nil reg — tests don't run a prom server
	require.NoError(t, err)
	return service.New(service.Deps{
		Pool:    pool,
		Store:   pgStore,
		Logger:  zaptest.NewLogger(t),
		Metrics: met,
	})
}

// buildServiceWithCrypto wires the recording service with the
// LocalDEKUnwrapper + LocalObjectStore + AESGCMDecryptor primitives so
// OpenAudioStream can run end-to-end against a real DB. Returns the svc
// plus the live KMS / Objects / KEK so per-test setup can seed wrapped
// DEKs and ciphertext using the SAME KEK.
func buildServiceWithCrypto(t *testing.T, pool *postgres.Pool) (rapi.RecordingService, *crypto.LocalDEKUnwrapper, *storage.LocalObjectStore, []byte) {
	t.Helper()
	pgStore := store.NewPostgresStore(pool)
	met, err := metrics.RegisterRecordingMetrics(nil) // nil reg — tests don't run a prom server
	require.NoError(t, err)

	kek := make([]byte, 32)
	_, err = rand.Read(kek)
	require.NoError(t, err)
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

// commitRecordingWithEncrypted builds an end-to-end seeded recording.
//  1. Generate a random DEK (32 bytes).
//  2. Wrap DEK under the test KEK with AAD = BuildAAD(tenant,
//     "recording.dek", callID). The ingest-uploader (cmd/recording-
//     uploader, a stub until Plan 14 Task 1) must use the same encoding
//     in production so the recording service can unwrap.
//  3. Encrypt the audio payload with the DEK + AAD = BuildAAD(tenant,
//     "recording.audio", callID). Binding callID into the audio AAD
//     blocks an attacker who swaps an entire S3 object between two
//     calls of the same tenant.
//  4. Seed LocalObjectStore with the ciphertext at the row's
//     audio_object_key.
//  5. Call svc.Commit with the wrapped DEK to register the recording.
//
// Returns the recordingID + the original plaintext (for assertion).
func commitRecordingWithEncrypted(
	t *testing.T,
	svc rapi.RecordingService,
	objects *storage.LocalObjectStore,
	kek []byte,
	tenantID, callID uuid.UUID,
	audio []byte,
) (uuid.UUID, []byte) {
	t.Helper()

	dekAAD := encryption.BuildAAD(tenantID, "recording.dek", callID.String())
	audioAAD := encryption.BuildAAD(tenantID, "recording.audio", callID.String())

	// 1+2: generate DEK, wrap under KEK with the DEK-scope AAD.
	dek := make([]byte, 32)
	_, err := rand.Read(dek)
	require.NoError(t, err)
	encryptedDEK, err := encryption.Encrypt(kek, dek, dekAAD)
	require.NoError(t, err)

	// 3: encrypt audio under DEK with the audio-scope AAD.
	ciphertext, err := encryption.Encrypt(dek, audio, audioAAD)
	require.NoError(t, err)

	// 4: seed object store.
	bucket := "rec-bucket-test"
	audioKey := "recordings/x/x/x/" + callID.String() + ".opus.enc"
	objects.PutBytes(bucket, audioKey, ciphertext)

	// 5: commit.
	in := newCommitInput(t, tenantID, callID)
	in.S3Bucket = bucket
	in.AudioObjectKey = audioKey
	in.KMSKeyID = "kek-test"
	in.EncryptedDEK = encryptedDEK
	in.BytesSize = int64(len(ciphertext))

	out, err := svc.Commit(t.Context(), in)
	require.NoError(t, err)
	return out.RecordingID, audio
}

func startPGContainer(t *testing.T) *postgres.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pg, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("sociopulse_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	migrationsURL := repoMigrationsURL(t)
	mig, err := migrate.New(migrationsURL, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = mig.Close() })
	require.NoError(t, mig.Up())

	pool, err := postgres.Open(ctx, postgres.Config{
		DSN:            dsn,
		MaxConns:       8,
		MinConns:       1,
		ConnectTimeout: 10 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func repoMigrationsURL(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repo := filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))
	abs, err := filepath.Abs(filepath.Join(repo, "migrations"))
	require.NoError(t, err)
	_, err = os.Stat(abs)
	require.NoError(t, err, "migrations dir not found at %s", abs)
	return "file://" + abs
}

// seedTenant inserts a fresh tenant row and returns its ID. Uses BypassRLS so
// the test can write to the tenants table directly.
func seedTenant(t *testing.T, pool *postgres.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	require.NoError(t, pool.BypassRLS(t.Context(), func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO tenants (id, org_code, name, status, kms_kek_id, phone_hash_pepper)
			 VALUES ($1, $2, $3, 'active', 'kms-test', '\x00')`,
			id,
			"org-"+id.String()[:8],
			"tenant-"+id.String()[:8],
		)
		return err
	}))
	return id
}

// seedCall inserts a tenant, project, and call — returning both tenantID and callID.
func seedCall(t *testing.T, pool *postgres.Pool) (tenantID, callID uuid.UUID) {
	t.Helper()
	tenantID = seedTenant(t, pool)
	callID = seedCallForTenant(t, pool, tenantID)
	return tenantID, callID
}

// seedCallForTenant inserts a fresh project + call under the supplied
// tenant. Use when a single test needs two distinct calls in the SAME
// tenant (e.g. the DEK-swap-rejection check below).
func seedCallForTenant(t *testing.T, pool *postgres.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	callID := uuid.Must(uuid.NewV7())
	projectID := uuid.Must(uuid.NewV7())

	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		if _, err := tx.Exec(t.Context(),
			`INSERT INTO projects (id, tenant_id, code, name, status)
			 VALUES ($1, $2, $3, 'Test Project', 'active')`,
			// Full project UUID in the code suffix — when a single tenant
			// hosts two calls (intra-tenant swap test), the short 8-char
			// prefix isn't unique across the tightly-clocked UUIDv7s.
			projectID, tenantID, "proj-"+projectID.String(),
		); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(),
			`INSERT INTO calls (id, tenant_id, project_id, started_at, status)
			 VALUES ($1, $2, $3, now(), 'success')`,
			callID, tenantID, projectID,
		)
		return err
	}))
	return callID
}

func newCommitInput(t *testing.T, tenantID, callID uuid.UUID) rapi.CommitInput {
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

func requireExactlyOneOutboxRow(t *testing.T, pool *postgres.Pool, tenantID, callID uuid.UUID) {
	t.Helper()
	var count int
	subject := rapi.SubjectRecordingUploadedFor(tenantID)
	require.NoError(t, pool.RawQueryRow(context.Background(),
		`SELECT COUNT(*) FROM event_outbox
		 WHERE tenant_id = $1 AND subject = $2 AND aggregate_id = $3`,
		tenantID, subject, callID,
	).Scan(&count))
	require.Equal(t, 1, count, "expected exactly one outbox row for %s", subject)
}

func requireExactlyOneAuditRow(t *testing.T, pool *postgres.Pool, tenantID, recordingID uuid.UUID, action string) {
	t.Helper()
	var count int
	// audit_log has FORCE ROW LEVEL SECURITY with a tenant_id = app.tenant_id policy,
	// so we must use WithTenant (which sets app.tenant_id) to query it.
	// target_id is text — pass the recording UUID as a string.
	require.NoError(t, pool.WithTenant(context.Background(), tenantID, func(tx postgres.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM audit_log
			 WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
			tenantID, action, recordingID.String(),
		).Scan(&count)
	}))
	require.Equal(t, 1, count, "expected exactly one audit row for action=%s", action)
}
