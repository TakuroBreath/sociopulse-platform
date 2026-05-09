package crypto

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/sociopulse/platform/pkg/encryption"
)

// DEKUnwrapper unwraps an encrypted DEK using the per-tenant KEK at the
// supplied key id. Implementations MUST bind the supplied AAD into the
// authentication step so a swapped (kms_key_id, encrypted_dek) tuple
// fails fast — a defence-in-depth above the row-level RLS check.
//
// Error contract:
//
//	ErrUnknownKey    — kmsKeyID does not exist or is in the wrong scope.
//	ErrDecryptFailed — KMS rejected the ciphertext (corrupted / wrong AAD / wrong key).
//	ErrUnauthorised  — the caller lacks permission to invoke decrypt (Yandex IAM).
//
// The Local implementation maps every authentication-class failure to
// ErrDecryptFailed; the Yandex implementation will surface IAM errors
// as ErrUnauthorised.
//
// DEKUnwrapper is intentionally separate from `tenancy.api.KMSResolver`:
// that interface caches per-tenant DEKs in-process for short PII (phone
// numbers); the recording flow needs raw KMS Symmetric Decrypt over an
// opaque ciphertext blob produced by the ingest-uploader, with no caching
// (the DEK is single-use per recording).
type DEKUnwrapper interface {
	DecryptDEK(ctx context.Context, kmsKeyID string, encryptedDEK []byte, aad []byte) ([]byte, error)
}

// Sentinels.
var (
	// ErrUnknownKey is returned when the supplied kmsKeyID is not registered
	// with the unwrapper. Plan 12.3 maps this to HTTP 500 with a generic
	// message — operators investigate via the audit log.
	ErrUnknownKey = errors.New("recording.kms: unknown KMS key")

	// ErrDecryptFailed is returned when KMS (or, for the Local impl, the
	// underlying AES-GCM check) rejects the ciphertext. The HTTP layer
	// maps this to 502 with a generic message — like ErrAuth in aesgcm.go,
	// the underlying details MUST NOT leak to the client.
	ErrDecryptFailed = errors.New("recording.kms: decrypt failed")

	// ErrUnauthorised is reserved for the Yandex implementation's IAM
	// failure path. The Local impl never returns it.
	ErrUnauthorised = errors.New("recording.kms: unauthorised")
)

// LocalDEKUnwrapper is the in-process implementation used by tests and
// local dev. The "encrypted_dek" payload is the OUTPUT of pkg/encryption.Encrypt
// against a fixed in-memory KEK keyed by kmsKeyID. Production deployments
// MUST use the Yandex-backed implementation (Plan 01).
//
// Construction:
//
//	keks := map[string][]byte{"kek-tenant-A": <32 random bytes>}
//	u := NewLocalDEKUnwrapper(keks)
//	plaintext, _ := u.DecryptDEK(ctx, "kek-tenant-A", encryptedDEK, aad)
//
// The constructor defensively clones the input map so caller mutations
// after construction do not affect the unwrapper's behaviour.
type LocalDEKUnwrapper struct {
	keks map[string][]byte
}

// NewLocalDEKUnwrapper returns a stateless unwrapper backed by the
// supplied map of (kms_key_id → 32-byte KEK plaintext). The map is
// cloned; mutations after construction have no effect.
func NewLocalDEKUnwrapper(keks map[string][]byte) *LocalDEKUnwrapper {
	cp := make(map[string][]byte, len(keks))
	for k, v := range keks {
		cp[k] = bytes.Clone(v)
	}
	return &LocalDEKUnwrapper{keks: cp}
}

// Compile-time interface check.
var _ DEKUnwrapper = (*LocalDEKUnwrapper)(nil)

// DecryptDEK satisfies DEKUnwrapper.
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
		// Surface ONLY the recording-module sentinel — the underlying
		// `cipher: message authentication failed` text would leak
		// tampering details. Server logs that capture zap.Error(err)
		// from the caller see the original error pointer; this scrub
		// applies to the error.Error() string that may reach an HTTP
		// 502 body via Plan 12.3.
		_ = err // intentionally not wrapped; mirrors AESGCMDecryptor scrub
		return nil, fmt.Errorf("%w", ErrDecryptFailed)
	}
	return plain, nil
}
