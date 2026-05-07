package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// ErrAuth is returned by Decrypt when the GCM authentication check fails:
// either the ciphertext was tampered with, or the additionalData doesn't
// match what was supplied during Encrypt.
var ErrAuth = errors.New("encryption: authentication failed")

// ErrKeySize is returned by Encrypt/Decrypt when the key is not 32 bytes
// (we lock to AES-256 only — the spec is unambiguous on this).
var ErrKeySize = errors.New("encryption: key must be 32 bytes (AES-256)")

// ErrCiphertextTooShort is returned by Decrypt when the input is shorter
// than nonce + minimum auth tag.
var ErrCiphertextTooShort = errors.New("encryption: ciphertext too short")

// Crypto sizing constants. Documented in code per Plan 03 §6.2.
const (
	// KeyLen is the AES-256 key length in bytes.
	KeyLen = 32
	// NonceLen is the AES-GCM nonce length in bytes (12, per NIST SP 800-38D).
	NonceLen = 12
	// TagLen is the AES-GCM authentication tag length in bytes.
	TagLen = 16
)

// Encrypt performs AES-256-GCM and returns nonce || ciphertext || tag.
//
// additionalData is bound by GCM but not stored in the output. Common
// choices: tenant id, table name, column name — anything that the
// caller can reproduce at Decrypt time and that uniquely identifies the
// row. Pass nil for "no AAD".
//
// The 12-byte nonce is generated fresh from crypto/rand on every call;
// random nonces collide with negligible probability up to ~2^48 messages
// per key (NIST SP 800-38D §8.3), well above per-DEK message volume in
// СоциоПульс (DEKs are per-recording).
func Encrypt(key, plaintext, additionalData []byte) ([]byte, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("%w: got %d", ErrKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("encryption: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encryption: new gcm: %w", err)
	}
	nonce := make([]byte, NonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("encryption: read nonce: %w", err)
	}
	// Seal appends to nonce so the nonce ends up as the prefix.
	out := gcm.Seal(nonce, nonce, plaintext, additionalData)
	return out, nil
}

// Decrypt reverses Encrypt. additionalData must match exactly what was
// passed to Encrypt; mismatch yields ErrAuth (wrapped through GCM's auth
// check). GCM's tag verification is constant-time internally.
func Decrypt(key, ciphertext, additionalData []byte) ([]byte, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("%w: got %d", ErrKeySize, len(key))
	}
	if len(ciphertext) < NonceLen+TagLen {
		return nil, fmt.Errorf("%w: got %d", ErrCiphertextTooShort, len(ciphertext))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("encryption: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encryption: new gcm: %w", err)
	}
	nonce, body := ciphertext[:NonceLen], ciphertext[NonceLen:]
	plaintext, err := gcm.Open(nil, nonce, body, additionalData)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAuth, err)
	}
	return plaintext, nil
}
