package main

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/internal/dialer/retry"
)

// passthroughDecryptor is the v1 retry.Decryptor used by cmd/worker.
// It returns the ciphertext bytes verbatim; in dev/integration
// environments the respondents.phone_encrypted column is stored as
// plaintext so the orchestrator's enqueue path keeps working without
// a KMS round-trip.
//
// Plan 12 wires the production KMS-backed decryptor (a thin adapter
// over tenancy.KMSResolver). Until then, an integration environment
// that requires real envelope encryption must run the dialer through
// cmd/api (which has the full module composition root with KMS
// adapter wired) instead of the worker.
type passthroughDecryptor struct{}

// Compile-time check that passthroughDecryptor satisfies retry.Decryptor.
var _ retry.Decryptor = passthroughDecryptor{}

// Decrypt satisfies retry.Decryptor. Empty input is rejected so a
// truly broken encryption path surfaces as a per-row warn rather than
// a silent zero-byte phone in the dialer queue. The respondentID
// argument is unused by the passthrough impl (it has no AAD to
// reproduce) but the interface requires it (Plan 13.2.5 Task 6).
func (passthroughDecryptor) Decrypt(_ context.Context, _, _ uuid.UUID, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 {
		return nil, errors.New("cmd/worker: empty ciphertext (passthrough decryptor)")
	}
	out := make([]byte, len(ciphertext))
	copy(out, ciphertext)
	return out, nil
}
