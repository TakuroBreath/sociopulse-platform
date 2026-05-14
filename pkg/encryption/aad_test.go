package encryption_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/encryption"
)

// TestBuildAAD_Deterministic asserts that BuildAAD is a pure function:
// identical inputs MUST produce byte-identical output across invocations.
// Without this, AAD-bound ciphertexts written by one process couldn't be
// decrypted by another.
func TestBuildAAD_Deterministic(t *testing.T) {
	t.Parallel()

	tenant := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	scope := "auth.user.phone"
	rowID := "00000000-0000-0000-0000-000000000001"

	first := encryption.BuildAAD(tenant, scope, rowID)
	second := encryption.BuildAAD(tenant, scope, rowID)

	require.Equal(t, first, second,
		"BuildAAD must be deterministic — same (tenant, scope, rowID) must yield identical bytes")
}

// TestBuildAAD_DifferentInputsDifferOutputs is a coarse smoke check that
// the inputs participate in the output. A buggy implementation that
// returned a constant byte slice would pass TestBuildAAD_Deterministic
// but would not bind anything at the AEAD layer.
func TestBuildAAD_DifferentInputsDifferOutputs(t *testing.T) {
	t.Parallel()

	base := encryption.BuildAAD(uuid.New(), "auth.user.phone", "row-1")

	cases := []struct {
		name  string
		other []byte
	}{
		{
			name:  "different tenant",
			other: encryption.BuildAAD(uuid.New(), "auth.user.phone", "row-1"),
		},
		{
			name:  "different scope",
			other: encryption.BuildAAD(uuid.Nil, "auth.user.phone", "row-1"),
		},
		{
			name:  "different rowID",
			other: encryption.BuildAAD(uuid.Nil, "auth.user.phone", "row-2"),
		},
	}
	for _, tc := range cases {
		require.NotEqual(t, base, tc.other,
			"BuildAAD must differ for case %q — inputs do not bind into AAD", tc.name)
	}
}

// TestBuildAAD_NoCollision_BetweenScopes is the key length-prefix test:
// without length prefixes, the two parameter sets below would yield the
// same concatenated byte sequence. Length prefixes prevent the swap.
//
// With unprefixed concat:
//
//	("t", "auth.user.phone", "id")    → "tauth.user.phoneid"
//	("t", "auth.user",       "phone.id") → "tauth.userphone.id"
//
// These DO differ — but a sharper case is:
//
//	("t", "auth", ".user.phone.id") → "tauth.user.phone.id"
//	("t", "auth.user.phone", ".id") → "tauth.user.phone.id"
//
// Both unprefixed concats would equal "tauth.user.phone.id". Length
// prefixes (varint per field) defeat this attack.
func TestBuildAAD_NoCollision_BetweenScopes(t *testing.T) {
	t.Parallel()

	tenant := uuid.MustParse("11111111-2222-3333-4444-555555555555")

	a := encryption.BuildAAD(tenant, "auth", ".user.phone.id")
	b := encryption.BuildAAD(tenant, "auth.user.phone", ".id")
	c := encryption.BuildAAD(tenant, "auth.user", "phone.id")

	require.NotEqual(t, a, b, "length-prefix must defeat (scope, rowID) field-boundary ambiguity")
	require.NotEqual(t, a, c, "length-prefix must defeat (scope, rowID) field-boundary ambiguity")
	require.NotEqual(t, b, c, "length-prefix must defeat (scope, rowID) field-boundary ambiguity")
}

// TestBuildAAD_NoCollision_BetweenRowIDs exercises the same invariant
// with rowID variations. Trivially different rowIDs MUST produce
// different AAD.
func TestBuildAAD_NoCollision_BetweenRowIDs(t *testing.T) {
	t.Parallel()

	tenant := uuid.New()
	scope := "crm.respondent.phone"

	for _, ids := range [][]string{
		{"a", "b"},
		{uuid.NewString(), uuid.NewString()},
		{"00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002"},
	} {
		x := encryption.BuildAAD(tenant, scope, ids[0])
		y := encryption.BuildAAD(tenant, scope, ids[1])
		require.NotEqual(t, x, y,
			"distinct rowIDs %q vs %q must yield distinct AAD", ids[0], ids[1])
	}
}

// TestBuildAAD_NoCollision_BetweenTenants asserts that two tenants
// holding the same (scope, rowID) tuple get distinct AAD envelopes —
// the cross-tenant swap attack vector.
func TestBuildAAD_NoCollision_BetweenTenants(t *testing.T) {
	t.Parallel()

	scope := "auth.totp.secret"
	rowID := "00000000-0000-0000-0000-000000000007"

	a := encryption.BuildAAD(uuid.MustParse("11111111-1111-1111-1111-111111111111"), scope, rowID)
	b := encryption.BuildAAD(uuid.MustParse("22222222-2222-2222-2222-222222222222"), scope, rowID)

	require.NotEqual(t, a, b, "same (scope, rowID) under different tenants must produce different AAD")
}

// TestBuildAAD_DocumentedFormat asserts the exact byte layout for a
// fixed input. This catches accidental format drift — any later
// "harmless" refactor that changes the encoding would silently brick
// previously-written v2 ciphertexts.
//
// Encoding: <uvarint(len(tenantStr))><tenantStr><uvarint(len(scope))><scope><uvarint(len(rowID))><rowID>
//
// For a UUID tenant (36 chars), uvarint(36) = 0x24 (single byte since 36 < 128).
func TestBuildAAD_DocumentedFormat(t *testing.T) {
	t.Parallel()

	tenant := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	scope := "auth.user.phone"
	rowID := "abc"

	got := encryption.BuildAAD(tenant, scope, rowID)

	// Build the expected envelope byte-by-byte.
	var want bytes.Buffer
	tenantStr := tenant.String()
	want.WriteByte(byte(len(tenantStr))) // 36 < 128 so single-byte uvarint
	want.WriteString(tenantStr)
	want.WriteByte(byte(len(scope))) // 15 < 128
	want.WriteString(scope)
	want.WriteByte(byte(len(rowID))) // 3 < 128
	want.WriteString(rowID)

	require.Equal(t, want.Bytes(), got,
		"BuildAAD byte layout drifted — see package doc for canonical encoding")

	// Cross-check that the first byte parses as uvarint=36 (sanity).
	v, n := binary.Uvarint(got)
	require.Equal(t, uint64(len(tenantStr)), v, "first uvarint must be len(tenantStr)")
	require.Equal(t, 1, n, "uvarint(36) is a single byte")
}

// TestBuildAAD_EmptyFields exercises the corner case where scope or
// rowID is the empty string. The length prefix preserves unambiguity
// (a 0-length field is encoded as a single 0x00 byte).
func TestBuildAAD_EmptyFields(t *testing.T) {
	t.Parallel()

	tenant := uuid.New()

	// Empty scope, non-empty rowID.
	a := encryption.BuildAAD(tenant, "", "row-1")
	// Empty rowID, non-empty scope of identical bytes.
	b := encryption.BuildAAD(tenant, "row-1", "")

	require.NotEqual(t, a, b,
		"empty scope vs empty rowID with the same shadow string must yield different AAD")
}
