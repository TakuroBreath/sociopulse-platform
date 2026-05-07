// Package passwords provides Argon2id password hashing for СоциоПульс.
//
// # Why Argon2id, not bcrypt or scrypt
//
// Argon2 is the winner of the Password Hashing Competition and is RFC 9106.
// The "id" variant balances side-channel resistance (Argon2i) with
// brute-force resistance (Argon2d). bcrypt is not memory-hard and has a
// hard 72-byte input limit; we don't use it.
//
// # Why these defaults (and not the canonical 64 MiB / t=3 / p=4)
//
// We deliberately ship OWASP's "Argon2id minimum" (Password Storage Cheat
// Sheet, Aug 2024 revision) instead of the spec's max-security profile:
//
//	memory      = 19 * 1024 KiB  (19 MiB)
//	iterations  = 2
//	parallelism = 1
//	salt length = 16 bytes
//	key length  = 32 bytes
//
// Rationale — the dominant risk for our deployment is NOT an offline
// dictionary attack against a leaked DB (we have neither a high-value PII
// blob nor an existing reputation as a target). It is in-process memory
// pressure: every Hash call allocates the full Memory working set, so a
// burst of concurrent logins (organic OR malicious) at 64 MiB each will
// OOM-kill a small pod long before any lockout kicks in. Sizing to 19 MiB
// caps the worst-case resident set and lets BoundedHasher (see
// bounded.go) gate concurrency at a known ceiling.
//
// If you ever change the threat model — onboard a high-value tenant, host
// publicly-accessible PII, or anticipate a determined attacker — bump the
// defaults via the auth.password.{memory,iterations,parallelism} config
// keys; existing hashes keep working because the cost parameters are
// embedded in every PHC string and recovered on Verify.
//
// # PHC encoding
//
// The encoded form follows the PHC string format defined at
// https://github.com/P-H-C/phc-string-format/blob/master/phc-sf-spec.md
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt-b64>$<hash-b64>
//
// where <salt-b64> and <hash-b64> are unpadded standard base64
// (RawStdEncoding). The "v=19" segment encodes the Argon2 version
// (0x13 == 19); only this version is accepted on Verify and any other
// value yields ErrIncompatibleVersion.
//
// # Typical use
//
//	encoded, err := passwords.Hash(plain, passwords.DefaultParams())
//	ok, err := passwords.Verify(encoded, plain)
//
// or via the Hasher interface for dependency injection in tests:
//
//	h := passwords.Default()
//	encoded, _ := h.Hash(plain)
//	ok, _ := h.Verify(encoded, plain)
//
// In production handlers wrap Default with BoundedHasher so a request
// flood cannot exhaust memory:
//
//	h := passwords.NewBoundedHasher(passwords.Default(), runtime.NumCPU())
package passwords
