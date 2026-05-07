package api

import (
	"context"

	"github.com/google/uuid"
)

// PhoneHasher computes a deterministic, per-tenant-salted hash of E.164 phone
// numbers for indexed lookup (respondents.phone_hash, users.login_phone_hash).
//
// Algorithm: HMAC-SHA256(pepper=tenants.phone_hash_pepper, msg=normalised_e164).
// 32 bytes output, stored as bytea.
//
// The pepper is loaded once per process from the database (cached in memory,
// invalidated only on tenant suspension or pepper-rotation).
type PhoneHasher interface {
	// Hash returns the 32-byte HMAC-SHA256 of the canonicalised phone.
	// The phone is canonicalised: digits-only, leading-+, E.164 (e.g. "+79991234567").
	Hash(ctx context.Context, tenantID uuid.UUID, phone string) ([]byte, error)

	// Normalise strips formatting characters and validates the result is E.164.
	// Returns ErrInvalidArgument on garbage input.
	Normalise(phone string) (string, error)
}
