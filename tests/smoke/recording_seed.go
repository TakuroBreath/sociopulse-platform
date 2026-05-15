//go:build smoke

package smoke

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/recording/storage"
	"github.com/sociopulse/platform/pkg/encryption"
)

// smokeKEKHex is the deterministic 32-byte KEK the smoke harness uses
// for every recording fixture. The value is "abcd" repeated 16 times
// (= 32 bytes after hex decode); it is published into the smoke
// config as recording.local_keks["smoke-kek-default"], so cmd/api's
// in-process LocalDEKUnwrapper recognises the same id.
//
// Hard-coded constant rather than crypto/rand: scenario 5 wants a
// deterministic round-trip the test can reason about; production uses
// real KMS-issued KEKs that the smoke harness never touches.
const smokeKEKHex = "abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"

// smokeRecordingPlaintext is the deterministic "audio" blob the
// fixture encrypts. Real Opus would be header + frames; here we just
// need bytes that round-trip cleanly through AES-GCM. ~4 KiB is enough
// to exercise the storage layer without ballooning test memory.
//
// Pattern is the 16-byte string "OpusFakeAudio16!" repeated 256 times
// for an exact 4096-byte blob — the repeating prefix makes a hex-dump
// diagnosable when a test fails. Length math: 16 × 256 = 4096.
var smokeRecordingPlaintext = bytes.Repeat([]byte("OpusFakeAudio16!"), 256)

// smokeRecordingDEK is the deterministic 32-byte AES-256 DEK every
// fixture uses. We bake it into a constant so the round-trip test in
// harness_test.go can reproduce it without re-running the wrap
// (otherwise we'd need to expose the unwrapped DEK across the API
// boundary and leak crypto material into the surface area).
//
// 0xCC × 32 — chosen so the bytes are visually distinct from the KEK
// (0xab/0xcd × 16) in any hex-dump diagnostic.
var smokeRecordingDEK = bytes.Repeat([]byte{0xCC}, encryption.KeyLen)

// RecordingFixture bundles every byte / hex string a recording-stream
// scenario needs to seed the call_recordings row + the LocalObjectStore
// blob. Constructed by BuildRecordingFixture; consumed by SeedRecording
// (which writes the row + Puts the ciphertext) AND by the scenario's
// HTTP-level assertions (sha256 surfaced on the search response, etc.).
//
// Field ownership:
//   - Ciphertext is the AES-GCM ciphertext (nonce || cipher || tag) of
//     the Opus blob. Stored verbatim in the LocalObjectStore under
//     (Bucket, Key); SHA256Hex is sha256(Ciphertext) per ADR-0005's
//     ciphertext chain-of-custody contract.
//   - WrappedDEKHex is the AES-GCM ciphertext of the DEK under the smoke
//     KEK + recording.dek AAD. Hex-encoded so it round-trips cleanly
//     through SQL (encrypted_dek is bytea — the seed inserter
//     hex-decodes before INSERT).
//   - KMSKeyID is the deterministic "smoke-kek-default" identifier the
//     smoke config registers with LocalDEKUnwrapper.
//   - Plaintext is exposed so the round-trip test can assert the
//     decrypt result equals the original blob.
//   - Bucket / Key are the (bucket, key) pair the recording handler
//     looks up in ObjectStore. Set by BuildRecordingFixture to
//     deterministic values keyed off (tenantID, callID).
type RecordingFixture struct {
	Ciphertext    []byte
	SHA256Hex     string
	WrappedDEKHex string
	KMSKeyID      string
	Plaintext     []byte
	Bucket        string
	Key           string
}

// BuildRecordingFixture produces a deterministic, valid recording
// fixture for tenantID + callID:
//
//  1. Encrypt smokeRecordingPlaintext with smokeRecordingDEK + the
//     audio AAD (BuildAAD(tenantID, "recording.audio", callID)) →
//     ciphertext.
//  2. Encrypt smokeRecordingDEK with the smoke KEK + the DEK AAD
//     (BuildAAD(tenantID, "recording.dek", callID)) → wrappedDEK.
//  3. Compute sha256(ciphertext) → hex.
//  4. Build (bucket, key) pair = ("smoke-recordings-<tenantSuffix>",
//     "<callID>.opus.enc").
//
// Returns the populated fixture; the caller decides whether to feed it
// into SeedRecording (most scenarios) or read it directly (the harness
// self-test asserts AAD shape via decrypt round-trip).
//
// stack is currently unused but accepted to keep the helper signature
// stable across future scenarios that may want to verify the smoke KEK
// is registered in the cmd/api process (looking it up via the override
// seam from the caller side).
func BuildRecordingFixture(t *testing.T, _ *Stack, tenantID, callID uuid.UUID) RecordingFixture {
	t.Helper()

	kek, err := hex.DecodeString(smokeKEKHex)
	require.NoError(t, err, "smoke fixture: decode smoke KEK hex")
	require.Len(t, kek, encryption.KeyLen, "smoke fixture: KEK length")

	// AAD shape MUST match internal/recording/service/service.go's
	// aadScopeRecording* constants. A drift in either scope ('recording.dek'
	// or 'recording.audio') silently breaks the round-trip; the harness
	// self-test pins this contract.
	dekAAD := encryption.BuildAAD(tenantID, "recording.dek", callID.String())
	audioAAD := encryption.BuildAAD(tenantID, "recording.audio", callID.String())

	wrappedDEK, err := encryption.Encrypt(kek, smokeRecordingDEK, dekAAD)
	require.NoError(t, err, "smoke fixture: wrap DEK")

	// bytes.Clone defends against a future caller mutating Plaintext —
	// the global slice would otherwise carry the mutation across
	// fixtures.
	plaintext := bytes.Clone(smokeRecordingPlaintext)
	ciphertext, err := encryption.Encrypt(smokeRecordingDEK, plaintext, audioAAD)
	require.NoError(t, err, "smoke fixture: encrypt audio")

	sum := sha256.Sum256(ciphertext)

	// Bucket / key shape mirrors production-ish (single bucket per
	// tenant) without requiring real S3 conventions. Suffix from the
	// tenantID's first 8 hex chars keeps the bucket name short + unique.
	bucket := "smoke-recordings-" + tenantID.String()[:8]
	key := callID.String() + ".opus.enc"

	return RecordingFixture{
		Ciphertext:    ciphertext,
		SHA256Hex:     hex.EncodeToString(sum[:]),
		WrappedDEKHex: hex.EncodeToString(wrappedDEK),
		KMSKeyID:      "smoke-kek-default",
		Plaintext:     plaintext,
		Bucket:        bucket,
		Key:           key,
	}
}

// SeedRecording inserts a call_recordings row referencing fixture's
// (bucket, key) and Puts the ciphertext into objects so the recording
// handler can later GET it. The objects argument MUST be the same
// LocalObjectStore the cmd/api process is using — scenarios obtain it
// via the build-tagged GetSmokeRecordingPorts() shim from the cmd/api
// package and pass the .Objects field here. Without the shared
// instance, cmd/api builds a fresh empty store at boot and the
// scenario fails with ErrObjectNotFound.
//
// Required cmd/api state before this is called:
//   - SeedTenantAndAdmin must have minted the tenant (so kms_kek_id
//     matches the LocalDEKUnwrapper's registered keys).
//   - The seed for callID must already exist in calls (via
//     SeedCall) — the FK on call_recordings.call_id requires it.
//
// The row's NOT NULL columns are populated to satisfy migration
// 000010_recording_evolve.up.sql. Hex-decoding of WrappedDEKHex
// happens here so callers don't need to remember the bytea conversion.
//
// Cleanup deletes both the call_recordings row AND the LocalObjectStore
// blob at test end so a sibling smoke test starts clean.
func SeedRecording(
	t *testing.T,
	stack *Stack,
	objects storage.ObjectStore,
	tenantID, callID uuid.UUID,
	fixture RecordingFixture,
) uuid.UUID {
	t.Helper()
	require.NotNil(t, objects, "smoke seed: objects ObjectStore must be the cmd/api shared instance")
	ctx := t.Context()

	conn, err := pgx.Connect(ctx, stack.PostgresDSN)
	require.NoError(t, err, "smoke seed: connect to %s", stack.PostgresDSN)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	wrappedDEK, err := hex.DecodeString(fixture.WrappedDEKHex)
	require.NoError(t, err, "smoke seed: decode wrapped DEK hex")

	now := time.Now().UTC()
	deleteAt := now.Add(30 * 24 * time.Hour)
	coldAt := now.Add(7 * 24 * time.Hour)

	recordingID := uuid.New()
	const insertSQL = `
		INSERT INTO call_recordings (
			id, call_id, tenant_id,
			s3_bucket, audio_object_key,
			kms_key_id, encrypted_dek,
			bytes_size, duration_ms, sha256_hex,
			codec, sample_rate, status,
			committed_at, delete_at, cold_at, recorded_at, ingest_agent_id
		) VALUES (
			$1, $2, $3,
			$4, $5,
			$6, $7,
			$8, $9, $10,
			$11, $12, $13,
			$14, $15, $16, $17, $18
		)`
	_, err = conn.Exec(ctx, insertSQL,
		recordingID, callID, tenantID,
		fixture.Bucket, fixture.Key,
		fixture.KMSKeyID, wrappedDEK,
		int64(len(fixture.Ciphertext)), int64(len(fixture.Plaintext))*8, fixture.SHA256Hex, // duration_ms — synthetic but non-zero
		"opus", int32(48000), "stored",
		now, deleteAt, coldAt, now, "smoke-test-ingest",
	)
	require.NoError(t, err, "smoke seed: insert call_recordings row")

	// Put the ciphertext into the SHARED ObjectStore so cmd/api's
	// recording handler reads back the same bytes. Content-Type is
	// stubbed; LocalObjectStore ignores it.
	err = objects.Put(ctx, fixture.Bucket, fixture.Key, fixture.Ciphertext, "audio/ogg")
	require.NoError(t, err, "smoke seed: put ciphertext into LocalObjectStore")

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = conn.Exec(bg, `DELETE FROM call_recordings WHERE id = $1`, recordingID)
		// LocalObjectStore.Delete is idempotent on missing keys.
		_ = objects.Delete(bg, fixture.Bucket, fixture.Key)
	})

	return recordingID
}
