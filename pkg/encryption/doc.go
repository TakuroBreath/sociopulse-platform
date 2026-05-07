// Package encryption implements the application-layer crypto primitives
// СоциоПульс uses to protect PII at rest in PostgreSQL.
//
// Two pieces:
//
//   - AES-256-GCM Encrypt/Decrypt with a 12-byte nonce prefixed to the
//     ciphertext. Encrypt accepts an optional `additionalData` argument
//     bound to the ciphertext via GCM's AEAD construction; Decrypt fails
//     with ErrAuth if the data was tampered with or the AAD differs.
//
//   - PhoneHasher: HMAC-SHA256 over a normalized phone number, using a
//     per-tenant 32-byte pepper. The hash is deterministic (so lookups
//     work) but cross-tenant comparisons are impossible because each
//     tenant's pepper is unique.
//
// Keys come from elsewhere: the spec resolves DEKs from Yandex KMS
// (per-tenant KEK). This package consumes raw key material; KMS plumbing
// lives in pkg/kms (added later).
//
// Nonce uniqueness invariant: 12 random bytes from crypto/rand are
// collision-safe up to ~2^48 messages per key (NIST SP 800-38D §8.3).
// This is far above any plausible per-DEK ciphertext volume in
// СоциоПульс — DEKs are per-recording (Plan 03 §6.2).
package encryption
