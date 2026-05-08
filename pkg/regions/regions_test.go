package regions_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/regions"
)

// TestLoad_EmbeddedYAMLParses verifies the canonical happy path: the
// snapshot loads, has at least the documented core regions, and every
// row carries a non-empty code + timezone.
func TestLoad_EmbeddedYAMLParses(t *testing.T) {
	t.Parallel()
	set, err := regions.Load()
	require.NoError(t, err)
	require.NotNil(t, set)
	require.GreaterOrEqual(t, set.Len(), 15, "regions snapshot must contain at least 15 high-population subjects")

	for _, r := range set.ListAll() {
		require.NotEmpty(t, r.Code, "every region must declare a code")
		require.NotEmpty(t, r.Timezone, "every region must declare a timezone")
		require.NotEmpty(t, r.NameRU, "every region must declare a Russian name")
		require.NotEmpty(t, r.NameEN, "every region must declare an English name")
	}
}

// TestLoad_KeyRegionsPresent locks in the subset that downstream code
// (RDD, working-hours, reports) hard-codes against. If the YAML rotates
// these out the contract breaks for every consumer.
func TestLoad_KeyRegionsPresent(t *testing.T) {
	t.Parallel()
	set, err := regions.Load()
	require.NoError(t, err)

	want := map[string]string{
		// code        : expected IANA timezone
		"RU-MOW": "Europe/Moscow",
		"RU-SPE": "Europe/Moscow",
		"RU-MO":  "Europe/Moscow",
		"RU-LO":  "Europe/Moscow",
		"RU-KDA": "Europe/Moscow",
		"RU-TA":  "Europe/Moscow",
		"RU-BA":  "Asia/Yekaterinburg",
		"RU-SVE": "Asia/Yekaterinburg",
		"RU-NVS": "Asia/Novosibirsk",
		"RU-KYA": "Asia/Krasnoyarsk",
		"RU-PRI": "Asia/Vladivostok",
		"RU-KAM": "Asia/Kamchatka",
	}
	for code, wantTZ := range want {
		r, ok := set.RegionForCode(code)
		require.True(t, ok, "missing region %s", code)
		require.Equal(t, wantTZ, r.Timezone, "region %s timezone drift", code)
		require.NotEmpty(t, r.DEFPrefixes, "region %s must declare at least one DEF prefix", code)
	}
}

// TestRegionForCode_Miss returns (zero, false) for an unknown code; no
// allocation, no error.
func TestRegionForCode_Miss(t *testing.T) {
	t.Parallel()
	set, err := regions.Load()
	require.NoError(t, err)

	r, ok := set.RegionForCode("RU-XXX")
	require.False(t, ok)
	require.Empty(t, r.Code)
}

// TestTimezoneForRegion_LoadsBundledTZData ensures every embedded
// region's timezone parses cleanly. The blank import of time/tzdata
// guarantees Asia/Kamchatka loads on FROM-scratch images.
func TestTimezoneForRegion_LoadsBundledTZData(t *testing.T) {
	t.Parallel()
	set, err := regions.Load()
	require.NoError(t, err)

	for _, r := range set.ListAll() {
		t.Run(r.Code, func(t *testing.T) {
			t.Parallel()
			loc, err := set.TimezoneForRegion(r.Code)
			require.NoError(t, err)
			require.NotNil(t, loc)
			// Sanity: pin a known instant and confirm tz offset is a
			// multiple of an hour. Asia/Kamchatka is +12, Europe/Moscow
			// is +3, etc.
			t0 := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
			_, offset := t0.In(loc).Zone()
			require.Zero(t, offset%3600, "tz offset for %s (%s) must be hour-aligned, got %d", r.Code, r.Timezone, offset)
		})
	}
}

// TestTimezoneForRegion_UnknownReturnsSentinel surfaces ErrUnknownRegion
// — the consumer-facing contract for "unknown code".
func TestTimezoneForRegion_UnknownReturnsSentinel(t *testing.T) {
	t.Parallel()
	set, err := regions.Load()
	require.NoError(t, err)

	_, err = set.TimezoneForRegion("RU-NOPE")
	require.ErrorIs(t, err, regions.ErrUnknownRegion)
}

// TestTimezoneForRegion_CachesResult — the second call for the same
// code must return the SAME *time.Location (pointer-identity), proving
// the cache hit path engages.
func TestTimezoneForRegion_CachesResult(t *testing.T) {
	t.Parallel()
	set, err := regions.Load()
	require.NoError(t, err)

	loc1, err := set.TimezoneForRegion("RU-MOW")
	require.NoError(t, err)
	loc2, err := set.TimezoneForRegion("RU-MOW")
	require.NoError(t, err)
	require.Same(t, loc1, loc2, "TimezoneForRegion must memoise *time.Location for hot-path callers")
}

// TestListAll_ReturnsCopy guards against external mutation of the
// internal slice.
func TestListAll_ReturnsCopy(t *testing.T) {
	t.Parallel()
	set, err := regions.Load()
	require.NoError(t, err)

	all := set.ListAll()
	require.NotEmpty(t, all)
	// Mutate the returned slice; a second ListAll must still see the
	// pristine data.
	all[0] = regions.Region{Code: "MUTATED"}
	fresh := set.ListAll()
	require.NotEqual(t, "MUTATED", fresh[0].Code, "ListAll must return a defensive copy")
}

// TestNilSet_GracefulZero — calling methods on a nil *Set is a wiring
// error but must not panic; the package returns the zero-value
// indication so a misconfigured caller surfaces a sensible error
// (rather than a SIGSEGV in production).
func TestNilSet_GracefulZero(t *testing.T) {
	t.Parallel()
	var s *regions.Set
	require.Zero(t, s.Len())
	require.Empty(t, s.ListAll())
	_, ok := s.RegionForCode("RU-MOW")
	require.False(t, ok)
	_, err := s.TimezoneForRegion("RU-MOW")
	require.ErrorIs(t, err, regions.ErrUnknownRegion)
}
