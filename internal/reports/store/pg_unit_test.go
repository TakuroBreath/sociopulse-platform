package store

// Pure-helper unit tests for the reports store. No database dependency
// — the testcontainers-backed integration coverage lives in
// pg_pg_test.go behind `//go:build integration`. Internal test package
// so the unexported helpers (encodeCursor / decodeCursor / clampLimit /
// buildListQuery) are directly accessible.

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
)

// TestEncodeCursor_RoundTripsViaDecode verifies that an instant + id encoded
// through encodeCursor round-trips through decodeCursor without loss. The
// cursor format is base64(unix_seconds:id) — the unix half loses sub-second
// precision, but that is acceptable because the consumer always pairs
// created_at >= cursor.created_at with a tie-break on id.
func TestEncodeCursor_RoundTripsViaDecode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ts   time.Time
		id   string
	}{
		{
			name: "typical row",
			ts:   time.Date(2026, 5, 14, 12, 34, 56, 0, time.UTC),
			id:   "job-abc-123",
		},
		{
			name: "ULID-like id",
			ts:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			id:   "01HX0Y4MXKAB12345CDEFG67HJ",
		},
		{
			name: "epoch zero",
			ts:   time.Unix(0, 0).UTC(),
			id:   "first",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cursor := encodeCursor(tc.ts, tc.id)
			require.NotEmpty(t, cursor, "encodeCursor must return non-empty string")

			gotTS, gotID, err := decodeCursor(cursor)
			require.NoError(t, err, "decodeCursor must accept encodeCursor output")

			// Unix-seconds precision: the decoded instant equals the input
			// truncated to one-second granularity.
			require.Equal(t, tc.ts.Unix(), gotTS.Unix(), "unix seconds must match")
			require.Equal(t, tc.id, gotID, "id must round-trip exactly")
		})
	}
}

// TestDecodeCursor_RejectsMalformed asserts every malformed-input failure
// mode is surfaced as a typed error (not a panic). Empty-string input is
// handled separately from "decoded but malformed" — empty is the
// no-cursor convention in ListJobsFilter.
func TestDecodeCursor_RejectsMalformed(t *testing.T) {
	t.Parallel()

	// rawB64 base64-url-encodes a payload so we can feed deliberately
	// malformed payloads to decodeCursor without going through
	// encodeCursor (which only produces valid shapes).
	rawB64 := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(s))
	}

	cases := []struct {
		name      string
		input     string
		wantNoErr bool // empty input must return ts=zero, id="", no err
	}{
		{name: "empty string returns no-cursor sentinel", input: "", wantNoErr: true},
		{name: "non-base64 garbage", input: "!!!@@@notb64@@@"},
		{name: "valid base64 but missing separator", input: rawB64("nosep")},
		{name: "valid base64 but non-numeric unix half", input: rawB64("abc:def")},
		{name: "valid base64 but empty id half", input: rawB64("1234567890:")},
		{name: "valid base64 but empty unix half", input: rawB64(":only-id")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts, id, err := decodeCursor(tc.input)
			if tc.wantNoErr {
				require.NoError(t, err)
				require.True(t, ts.IsZero(), "empty cursor must return zero time")
				require.Empty(t, id, "empty cursor must return empty id")
				return
			}
			require.Error(t, err, "malformed cursor must surface an error")
		})
	}
}

// TestClampLimit_Defaults covers the bounds enforcement: 0 / negative →
// 100 default, > 500 → 500 cap, in-range values pass through unchanged.
func TestClampLimit_Defaults(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input int
		want  int
	}{
		{name: "zero defaults to 100", input: 0, want: 100},
		{name: "negative defaults to 100", input: -5, want: 100},
		{name: "one is the floor", input: 1, want: 1},
		{name: "in-range passes through", input: 250, want: 250},
		{name: "500 is the cap", input: 500, want: 500},
		{name: "above cap clamps to 500", input: 99999, want: 500},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, clampLimit(tc.input))
		})
	}
}

// TestBuildListQuery_AssemblesPredicates documents the SQL assembly rules
// indirectly — we assert presence/absence of fragments rather than exact
// string match (which would be brittle to whitespace changes). The
// integration test in pg_pg_test.go covers end-to-end correctness.
func TestBuildListQuery_AssemblesPredicates(t *testing.T) {
	t.Parallel()

	state := reportsapi.JobQueued
	kind := reportsapi.KindOperatorEfficiency
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)

	t.Run("no filters", func(t *testing.T) {
		t.Parallel()
		sql, args := buildListQuery(reportsapi.ListJobsFilter{Limit: 50})

		require.Contains(t, sql, "FROM reports_jobs", "must select from reports_jobs")
		require.Contains(t, sql, "ORDER BY created_at DESC, id DESC", "stable keyset order")
		require.Contains(t, sql, "LIMIT", "must apply LIMIT clause")
		require.NotContains(t, sql, "AND state =", "no state filter")
		require.NotContains(t, sql, "AND kind =", "no kind filter")
		require.NotContains(t, sql, "AND created_at >=", "no from filter")
		require.NotContains(t, sql, "AND created_at <", "no to filter")
		// limit arg is at the end
		require.Equal(t, 50, args[len(args)-1])
	})

	t.Run("all filters", func(t *testing.T) {
		t.Parallel()
		sql, args := buildListQuery(reportsapi.ListJobsFilter{
			State: &state,
			Kind:  &kind,
			From:  &from,
			To:    &to,
			Limit: 25,
		})

		require.Contains(t, sql, "AND state =")
		require.Contains(t, sql, "AND kind =")
		require.Contains(t, sql, "AND created_at >=")
		require.Contains(t, sql, "AND created_at <")
		require.Contains(t, args, string(state))
		require.Contains(t, args, string(kind))
		require.Contains(t, args, from)
		require.Contains(t, args, to)
	})

	t.Run("limit clamped on zero", func(t *testing.T) {
		t.Parallel()
		_, args := buildListQuery(reportsapi.ListJobsFilter{Limit: 0})
		// Last arg is the LIMIT value — must be the 100 default.
		require.Equal(t, 100, args[len(args)-1])
	})

	t.Run("cursor adds keyset predicate", func(t *testing.T) {
		t.Parallel()
		// A valid cursor is required for the predicate to be emitted.
		cursor := encodeCursor(time.Now().UTC(), "some-id")
		sql, _ := buildListQuery(reportsapi.ListJobsFilter{Cursor: cursor, Limit: 10})
		require.Contains(t, sql, "(created_at, id) <", "keyset predicate must be applied")
	})

	t.Run("invalid cursor is silently skipped", func(t *testing.T) {
		t.Parallel()
		// Malformed cursors must not leak into the SQL — the consumer
		// gets the first page back. (We chose to be permissive rather
		// than 400; the cursor is opaque to clients.)
		sql, _ := buildListQuery(reportsapi.ListJobsFilter{Cursor: "!!!bad!!!", Limit: 10})
		require.NotContains(t, sql, "(created_at, id) <")
	})
}

// TestDecodeCursor_AcceptsUUIDAsID is a defence-in-depth check: real
// jobIDs are short tokens, but if a future caller swaps in UUIDs we
// still want round-trip to work.
func TestDecodeCursor_AcceptsUUIDAsID(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV7()).String()
	ts := time.Now().UTC()

	cursor := encodeCursor(ts, id)
	gotTS, gotID, err := decodeCursor(cursor)
	require.NoError(t, err)
	require.Equal(t, ts.Unix(), gotTS.Unix())
	require.Equal(t, id, gotID)
}
