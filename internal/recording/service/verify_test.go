//go:build integration

package service_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/storage"
	"github.com/sociopulse/platform/pkg/encryption"
	"github.com/sociopulse/platform/pkg/postgres"
)

func TestService_Verify_HappyPath(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)
	svc, _, objects, kek := buildServiceWithCrypto(t, pool)

	audio := []byte("audio bytes for verify happy path")
	_, expectedSHA, ciphertext := commitRecordingWithRealSHA(t, svc, objects, kek, tenantID, callID, audio)

	result, err := svc.VerifyChecksum(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.True(t, result.OK, "expected OK=true on round-trip")
	require.Equal(t, expectedSHA, result.ExpectedSHA)
	require.Equal(t, expectedSHA, result.ActualSHA)
	require.Equal(t, int64(len(ciphertext)), result.BytesScanned)
	require.GreaterOrEqual(t, result.DurationMS, int64(0))
}

func TestService_Verify_TamperedObject(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)
	svc, _, objects, kek := buildServiceWithCrypto(t, pool)

	audio := []byte("verify tampered")
	recordingID, expectedSHA, ciphertext := commitRecordingWithRealSHA(t, svc, objects, kek, tenantID, callID, audio)
	_ = recordingID

	// Replace the seeded object with a tampered copy.
	tampered := make([]byte, len(ciphertext))
	copy(tampered, ciphertext)
	tampered[0] ^= 0xFF
	objects.PutBytes("rec-bucket-test", "recordings/v/"+callID.String()+".opus.enc", tampered)

	result, err := svc.VerifyChecksum(t.Context(), tenantID, callID)
	require.NoError(t, err)
	require.False(t, result.OK, "tampered object must fail verify")
	require.Equal(t, expectedSHA, result.ExpectedSHA)
	require.NotEqual(t, expectedSHA, result.ActualSHA)
}

func TestService_Verify_NotFound(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID := seedTenant(t, pool)
	svc, _, _, _ := buildServiceWithCrypto(t, pool)

	_, err := svc.VerifyChecksum(t.Context(), tenantID, uuid.Must(uuid.NewV7()))
	require.ErrorIs(t, err, rapi.ErrNotFound)
}

func TestService_Verify_AlreadyDeleted(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)
	svc, _, objects, kek := buildServiceWithCrypto(t, pool)
	commitRecordingWithRealSHA(t, svc, objects, kek, tenantID, callID, []byte("audio"))

	require.NoError(t, pool.WithTenant(t.Context(), tenantID, func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(),
			`UPDATE call_recordings SET status='deleted' WHERE call_id=$1`, callID)
		return err
	}))

	_, err := svc.VerifyChecksum(t.Context(), tenantID, callID)
	require.ErrorIs(t, err, rapi.ErrAlreadyDeleted)
}

func TestService_Verify_ObjectMissing(t *testing.T) {
	t.Parallel()
	pool := startPGContainer(t)
	tenantID, callID := seedCall(t, pool)
	svc, _, objects, kek := buildServiceWithCrypto(t, pool)

	// Commit with real sha BUT then DELETE the object so verify can't find it.
	commitRecordingWithRealSHA(t, svc, objects, kek, tenantID, callID, []byte("audio"))
	require.NoError(t, objects.Delete(t.Context(), "rec-bucket-test", "recordings/v/"+callID.String()+".opus.enc"))

	_, err := svc.VerifyChecksum(t.Context(), tenantID, callID)
	require.ErrorIs(t, err, rapi.ErrNotFound)
	require.NotErrorIs(t, err, storage.ErrObjectNotFound,
		"storage shape must not leak — only ErrNotFound is exposed")
}

func TestService_Verify_NotWired(t *testing.T) {
	t.Parallel()
	svc := newStubService(t)
	_, err := svc.VerifyChecksum(t.Context(), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()))
	require.ErrorIs(t, err, rapi.ErrInvalidInput)
	require.Contains(t, err.Error(), "not wired")
}

// commitRecordingWithRealSHA wraps a fresh DEK under the test KEK,
// encrypts audio with the DEK, computes sha256 of the ciphertext, seeds
// the LocalObjectStore, and calls svc.Commit with the matching SHA256Hex.
// Returns (recordingID, sha256-hex of the ciphertext, ciphertext bytes).
func commitRecordingWithRealSHA(
	t *testing.T,
	svc rapi.RecordingService,
	objects *storage.LocalObjectStore,
	kek []byte,
	tenantID, callID uuid.UUID,
	audio []byte,
) (uuid.UUID, string, []byte) {
	t.Helper()
	aad := []byte(tenantID.String())

	dek := make([]byte, 32)
	_, err := rand.Read(dek)
	require.NoError(t, err)

	encryptedDEK, err := encryption.Encrypt(kek, dek, aad)
	require.NoError(t, err)

	ciphertext, err := encryption.Encrypt(dek, audio, aad)
	require.NoError(t, err)

	sum := sha256.Sum256(ciphertext)
	sha := hex.EncodeToString(sum[:])

	bucket := "rec-bucket-test"
	audioKey := "recordings/v/" + callID.String() + ".opus.enc"
	objects.PutBytes(bucket, audioKey, ciphertext)

	in := newCommitInput(t, tenantID, callID)
	in.S3Bucket = bucket
	in.AudioObjectKey = audioKey
	in.KMSKeyID = "kek-test"
	in.EncryptedDEK = encryptedDEK
	in.BytesSize = int64(len(ciphertext))
	in.SHA256Hex = sha

	out, err := svc.Commit(t.Context(), in)
	require.NoError(t, err)
	return out.RecordingID, sha, ciphertext
}
