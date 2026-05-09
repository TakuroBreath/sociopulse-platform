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
//
// ErrObjectNotFound is HIDDEN behind ErrNotFound (mirrors OpenAudioStream)
// — the storage shape doesn't leak to API consumers.
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
