// Package encryption provides AES-256-GCM envelope encryption, DEK
// wrapping, and HMAC-SHA256-based phone-number hashing used across the
// platform. It deliberately does not know about tenants, phones, or
// recordings — those domain types live in internal/<module>/api/.
//
// Concrete crypto wiring (Yandex KMS for KEKs, per-tenant DEKs,
// streaming codec for recording chunks) is filled in by Plan 03 Task 5.
package encryption

// Encrypt encrypts plaintext under the supplied 32-byte AES-256-GCM
// data-encryption key (dek). It returns the ciphertext, a freshly
// generated 12-byte nonce, and any error from the cipher.
//
// The caller is responsible for storing the nonce alongside the
// ciphertext; nonces MUST never be reused with the same DEK.
func Encrypt(plaintext, dek []byte) (ciphertext, nonce []byte, err error) {
	panic("not implemented: see Plan 03 Task 5")
}

// Decrypt reverses Encrypt. It returns the plaintext or an error if
// the ciphertext fails authentication (tampered, wrong key, wrong
// nonce).
func Decrypt(ciphertext, nonce, dek []byte) (plaintext []byte, err error) {
	panic("not implemented: see Plan 03 Task 5")
}
