package encryption

// WrapDEK wraps a per-tenant data-encryption key under a key-encryption
// key (kek). The wrapped DEK is what we persist alongside the encrypted
// payload; KEKs themselves never leave the KMS.
func WrapDEK(dek, kek []byte) (wrapped []byte, err error) {
	panic("not implemented: see Plan 03 Task 5")
}

// UnwrapDEK reverses WrapDEK. Implementations MUST use a constant-time
// comparison on the authentication tag.
func UnwrapDEK(wrapped, kek []byte) (dek []byte, err error) {
	panic("not implemented: see Plan 03 Task 5")
}
