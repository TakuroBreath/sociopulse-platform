package encryption

// PhoneHasher produces a deterministic, salted hash of a phone number
// suitable for indexing. The salt is per-tenant so two tenants holding
// the same phone produce different hashes — preventing cross-tenant
// correlation by inspection of the raw column.
//
// Implementations MUST use HMAC-SHA256 over a normalized E.164 phone
// (see NormalizePhone) with a tenant-bound salt of at least 32 bytes.
type PhoneHasher interface {
	// Hash returns the lowercase hex digest of HMAC-SHA256(salt, phone).
	Hash(phone string) (string, error)
}

// NewHMACPhoneHasher constructs a PhoneHasher. salt is the per-tenant
// secret; it must be at least 32 bytes. Concrete implementation lands
// in Plan 03 Task 5.
func NewHMACPhoneHasher(salt []byte) (PhoneHasher, error) {
	panic("not implemented: see Plan 03 Task 5")
}
