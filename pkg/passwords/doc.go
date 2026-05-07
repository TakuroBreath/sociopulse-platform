// Package passwords provides Argon2id password hashing for СоциоПульс.
//
// Why Argon2id, not bcrypt: Argon2 is the winner of the Password Hashing
// Competition and is RFC 9106. It is memory-hard (resistant to ASIC/GPU
// brute-force) and the "id" variant balances side-channel resistance
// (Argon2i) with brute-force resistance (Argon2d). bcrypt is not
// memory-hard and has a hard 72-byte input limit; we don't use it.
//
// Encoded form follows the PHC string format defined at
// https://github.com/P-H-C/phc-string-format/blob/master/phc-sf-spec.md
//
//	$argon2id$v=19$m=65536,t=3,p=4$<salt-b64>$<hash-b64>
//
// where <salt-b64> and <hash-b64> are unpadded standard base64
// (RawStdEncoding). The "v=19" segment encodes the Argon2 version
// (0x13 == 19); only this version is accepted on Verify and any other
// value yields ErrIncompatibleVersion.
//
// Defaults match §14.2 of the СоциоПульс system-design spec:
//
//	memory      = 64 * 1024 KiB  (64 MiB)
//	iterations  = 3
//	parallelism = 4
//	salt length = 16 bytes
//	key length  = 32 bytes
//
// On a modern x86_64 server these parameters cost ~50–100 ms per call,
// which keeps a single core's login throughput around ~10 RPS — comfortable
// at our scale (Plan 05 spec §13.2). Tune via Params if your target
// hardware differs.
//
// Typical use:
//
//	encoded, err := passwords.Hash(plain, passwords.DefaultParams())
//	ok, err := passwords.Verify(encoded, plain)
//
// or via the Hasher interface for dependency injection in tests:
//
//	h := passwords.Default()
//	encoded, _ := h.Hash(plain)
//	ok, _ := h.Verify(encoded, plain)
package passwords
