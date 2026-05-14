package encryption

import (
	"encoding/binary"

	"github.com/google/uuid"
)

// BuildAAD returns a canonical additional-authenticated-data envelope
// binding a ciphertext to its (tenant, scope, row) tuple. Used by
// tenancy.KMSResolver.{Encrypt,Decrypt} so an attacker who can swap
// ciphertext blobs between rows, tenants, or columns fails AEAD auth-
// tag verification.
//
// Encoding: <uvarint(len(tenantStr))><tenantStr><uvarint(len(scope))><scope><uvarint(len(rowID))><rowID>
//
// Length prefixes prevent ambiguity attacks where two distinct logical
// tuples would otherwise yield the same byte sequence — e.g. without
// prefixes, ("t", "auth", ".user.phone.id") and ("t", "auth.user.phone",
// ".id") would both serialise to "tauth.user.phone.id".
//
// scope is a short low-cardinality string identifying the column / use:
//
//   - "auth.user.phone"      — UserService.PhoneEncrypted (future)
//   - "auth.totp.secret"     — TOTPState.SecretEncrypted
//   - "crm.respondent.phone" — Respondent.PhoneEncrypted
//   - "recording.dek"        — recording DEK wrapping (Plan 12)
//
// rowID is the stable identifier of the row owning the ciphertext —
// typically the row UUID (rendered as a 36-char string). For ciphertexts
// not yet bound to a row at encrypt time (e.g. an import-time bulk insert
// where row IDs are server-generated), callers MUST mint the ID client-
// side (uuid.New()) before encryption and persist that same ID with the
// row so a later decrypt reproduces the same AAD.
//
// The output is a byte slice; callers pass it verbatim to
// pkg/encryption.Encrypt / Decrypt as additionalData.
func BuildAAD(tenantID uuid.UUID, scope, rowID string) []byte {
	tenant := tenantID.String()
	// Pre-size: 3 uvarints (max 10 bytes each) + the three strings. Caller
	// can rely on the returned slice being a fresh allocation — it is safe
	// to retain.
	out := make([]byte, 0, len(tenant)+len(scope)+len(rowID)+3*binary.MaxVarintLen64)
	out = binary.AppendUvarint(out, uint64(len(tenant)))
	out = append(out, tenant...)
	out = binary.AppendUvarint(out, uint64(len(scope)))
	out = append(out, scope...)
	out = binary.AppendUvarint(out, uint64(len(rowID)))
	out = append(out, rowID...)
	return out
}
