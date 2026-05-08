package service_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/internal/crm/service"
)

// TestNormalizeRussianPhone_HappyPath exercises the canonical inputs the
// import / single-add flows actually receive: the same number written six
// different ways must collapse to the same E.164 representation.
//
// Russian numbering plan: +7 followed by 10 digits. A leading `8` is the
// "domestic long-distance" prefix and is normalised to the country-code
// `7`. Whitespace, parentheses, and dashes are commonly inserted by
// operators copying from spreadsheets — the parser strips them.
func TestNormalizeRussianPhone_HappyPath(t *testing.T) {
	t.Parallel()

	const wantE164 = "+79161234567"

	cases := []struct {
		name string
		in   string
	}{
		{"already canonical E.164", "+79161234567"},
		{"missing plus", "79161234567"},
		{"domestic 8 prefix", "89161234567"},
		{"8 with parens dashes spaces", "8 (916) 123-45-67"},
		{"+7 with extra whitespace", " +7  916  123 45 67 "},
		{"+7 with em-dashes", "+7—916—123—45—67"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := service.NormalizeRussianPhone(tc.in)
			require.NoError(t, err)
			require.Equal(t, wantE164, got.E164)
			require.Equal(t, "RU", got.Region)
		})
	}
}

// TestNormalizeRussianPhone_MoscowLandline keeps the parser
// permissive: a Moscow АВС-code `4951234567` parses and validates for
// region "RU". Mobile-only enforcement is deferred to v2 — for v1 we
// accept landlines so legacy operator imports don't get bulk-rejected.
func TestNormalizeRussianPhone_MoscowLandline(t *testing.T) {
	t.Parallel()

	got, err := service.NormalizeRussianPhone("+74951234567")
	require.NoError(t, err)
	require.Equal(t, "+74951234567", got.E164)
	require.Equal(t, "RU", got.Region)
}

// TestNormalizeRussianPhone_NBSP exercises the Unicode no-break space
// (U+00A0) commonly produced by Excel cell formatters. The parser must
// strip it before handing the string to libphonenumber, otherwise the
// parse fails with "not a number".
func TestNormalizeRussianPhone_NBSP(t *testing.T) {
	t.Parallel()

	// "+7 916 123 45 67"
	in := "+7 916 123 45 67"
	got, err := service.NormalizeRussianPhone(in)
	require.NoError(t, err)
	require.Equal(t, "+79161234567", got.E164)
}

// TestNormalizeRussianPhone_RoundtripStability — feeding back the
// canonical E.164 form must yield the same canonical form. The
// invariant matters for hash-based deduplication: importing a phone
// twice (once raw, once already-normalised) must map to the same hash.
func TestNormalizeRussianPhone_RoundtripStability(t *testing.T) {
	t.Parallel()

	first, err := service.NormalizeRussianPhone("8 (916) 123-45-67")
	require.NoError(t, err)
	second, err := service.NormalizeRussianPhone(first.E164)
	require.NoError(t, err)
	require.Equal(t, first.E164, second.E164)
	require.Equal(t, first.Region, second.Region)
}

// TestNormalizeRussianPhone_Rejects exercises every garbage path the
// parser must reject with api.ErrInvalidPhone. The errors.Is check
// discriminates the canonical sentinel from other (e.g. wrapped) errors
// so callers can branch reliably.
func TestNormalizeRussianPhone_Rejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
	}{
		{"empty string", ""},
		{"whitespace only", "    "},
		{"single letter", "x"},
		{"only four digits", "1234"},
		{"twenty digits", strings.Repeat("9", 20)},
		{"US number, valid E.164 but not RU", "+14155551234"},
		{"leading zero region", "+70916123456"},
		{"gibberish letters in middle", "+7916abc4567"},
		{"only the plus sign", "+"},
		{"only spaces and dashes", "  - --  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := service.NormalizeRussianPhone(tc.in)
			require.Error(t, err)
			require.ErrorIs(t, err, crmapi.ErrInvalidPhone, "expected ErrInvalidPhone")
		})
	}
}

// TestNormalizeRussianPhone_NormalizedPhoneFields ensures the returned
// struct carries the documented fields (E164 + Region) so downstream
// callers can pattern-match on Region without re-deriving it from E164.
func TestNormalizeRussianPhone_NormalizedPhoneFields(t *testing.T) {
	t.Parallel()

	got, err := service.NormalizeRussianPhone("+79161234567")
	require.NoError(t, err)
	require.Equal(t, "+79161234567", got.E164)
	require.Equal(t, "RU", got.Region)
}
