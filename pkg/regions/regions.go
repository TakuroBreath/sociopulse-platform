// Package regions exposes the canonical Russian Federation subject
// reference data shared across modules. The dialer (Plan 10 — RDD,
// working-hours), reports (Plan 13 — region grouping in dashboards), and
// any future module that needs the (code, name, timezone, mobile-prefix)
// tuple read from the same in-memory snapshot loaded from the embedded
// regions.yaml at process start.
//
// Data freshness: the YAML is checked into the repository and refreshed
// quarterly from Минцифры РФ open registry. RDD and working-hours code
// loads the snapshot once via Load and treats the returned [Set] as
// immutable for the lifetime of the process.
//
// tzdata is bundled into the binary via a blank import below — without
// it [TimezoneForRegion] fails on FROM-scratch images that lack the OS
// tzdata package. The cost is ~450 KiB; well worth the deployment
// portability.
package regions

import (
	"embed"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	// Bundle the IANA time-zone database so [time.LoadLocation] works on
	// minimal images (FROM scratch / distroless). Plan 09 telephony
	// bridge would otherwise need an alpine-tzdata layer; Plan 10
	// dialer needs every Asia/* and Europe/* zone for working-hours
	// computation. The blank-import is the canonical Go path
	// (https://pkg.go.dev/time/tzdata).
	_ "time/tzdata"

	"gopkg.in/yaml.v3"
)

//go:embed configs/regions.yaml
var regionsFS embed.FS

// Region is one row in the canonical RU subject reference. Fields are
// chosen for downstream utility:
//
//   - Code is the ISO 3166-2:RU identifier (RU-MOW, RU-SPE, ...) used
//     as the canonical reference key in DB rows, queue items, and
//     reports.
//   - NameRU / NameEN feed the operator UI and English export reports.
//   - Timezone drives working-hours enforcement (Plan 10 Task 7) — the
//     dialer enforces "no calls outside 9-21 local time" by loading
//     this string into [time.LoadLocation].
//   - ABCFlag distinguishes ABC (legacy landline) from DEF (mobile)
//     numbering plans. RDD (Plan 10 Task 4) uses it to honour the
//     project's АВС/DEF ratio quota.
//   - DEFPrefixes is the list of three-digit mobile codes the RDD
//     generator rolls a 7-digit subscriber suffix against.
type Region struct {
	Code        string
	NameRU      string
	NameEN      string
	Timezone    string
	ABCFlag     bool
	DEFPrefixes []string
}

// Set is the in-memory snapshot of the embedded regions.yaml. Methods
// are read-only; the zero value is unusable. Construct a Set via [Load].
//
// Load is process-scoped — call it once in cmd/api / cmd/worker boot
// and inject the *Set into modules that need it. Tests call [Load]
// directly and freely; the embedded YAML lives in the binary so no
// filesystem I/O is needed.
type Set struct {
	all    []Region
	byCode map[string]Region

	// locCache memoises [TimezoneForRegion] results. time.LoadLocation
	// re-parses the tzdata blob on every call; the cache amortises that
	// cost across hot-path RDD generation. Concurrent reads via
	// sync.Map (locked Read-Mostly) — RDD calls TimezoneForRegion once
	// per generated phone number and we want the lookup ~ free.
	locCache sync.Map // string code → *time.Location
}

// yamlSchema is the on-disk shape of regions.yaml. Kept private so
// callers consume the public Region type only.
type yamlSchema struct {
	Regions []yamlRow `yaml:"regions"`
}

type yamlRow struct {
	Code        string   `yaml:"code"`
	NameRU      string   `yaml:"name_ru"`
	NameEN      string   `yaml:"name_en"`
	Timezone    string   `yaml:"timezone"`
	ABCFlag     bool     `yaml:"abc_flag"`
	DEFPrefixes []string `yaml:"def_prefixes"`
}

// ErrUnknownRegion is returned by [Set.RegionForCode] / [Set.TimezoneForRegion]
// when the requested code is not present in the embedded snapshot.
// Consumers use errors.Is to discriminate.
var ErrUnknownRegion = errors.New("regions: unknown region code")

// Load parses the embedded regions.yaml and returns a ready-to-use
// [Set]. Returns an error when the embedded YAML is malformed (the
// embedded file is repo-controlled, so this is a build-time concern;
// the constructor still surfaces a clean error so misconfiguration is
// visible at boot rather than at first lookup).
func Load() (*Set, error) {
	raw, err := regionsFS.ReadFile("configs/regions.yaml")
	if err != nil {
		return nil, fmt.Errorf("regions.Load: read embedded yaml: %w", err)
	}
	return loadFromBytes(raw)
}

// loadFromBytes is the parser core used by [Load]. Exposed (package-
// private) so the test suite can drive the failure paths (malformed
// YAML, empty rows, duplicate codes, bad timezones) without round-
// tripping through the real embed.FS.
func loadFromBytes(raw []byte) (*Set, error) {
	var schema yamlSchema
	if err := yaml.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("regions.Load: parse embedded yaml: %w", err)
	}
	if len(schema.Regions) == 0 {
		return nil, errors.New("regions.Load: empty embedded yaml — no regions defined")
	}
	all := make([]Region, 0, len(schema.Regions))
	byCode := make(map[string]Region, len(schema.Regions))
	for i, row := range schema.Regions {
		if row.Code == "" {
			return nil, fmt.Errorf("regions.Load: row %d has empty code", i)
		}
		if row.Timezone == "" {
			return nil, fmt.Errorf("regions.Load: %s has empty timezone", row.Code)
		}
		if _, dup := byCode[row.Code]; dup {
			return nil, fmt.Errorf("regions.Load: duplicate code %q", row.Code)
		}
		// Validate timezone parse at boot — surfaces a typo in the
		// YAML immediately rather than at the first call to
		// TimezoneForRegion months later.
		if _, err := time.LoadLocation(row.Timezone); err != nil {
			return nil, fmt.Errorf("regions.Load: %s: timezone %q: %w", row.Code, row.Timezone, err)
		}
		// Defensive copy of the prefixes slice — yaml.v3 returns a
		// freshly-allocated []string, but we own the data from this
		// point and the caller mustn't be able to mutate it through
		// the Region value.
		prefixes := slices.Clone(row.DEFPrefixes)
		r := Region{
			Code:        row.Code,
			NameRU:      row.NameRU,
			NameEN:      row.NameEN,
			Timezone:    row.Timezone,
			ABCFlag:     row.ABCFlag,
			DEFPrefixes: prefixes,
		}
		all = append(all, r)
		byCode[row.Code] = r
	}
	return &Set{all: all, byCode: byCode}, nil
}

// RegionForCode returns the [Region] whose Code field equals code, or
// (zero, false) when no such region exists in the snapshot.
func (s *Set) RegionForCode(code string) (Region, bool) {
	if s == nil {
		return Region{}, false
	}
	r, ok := s.byCode[code]
	return r, ok
}

// TimezoneForRegion returns the [time.Location] for the named region.
// Returns [ErrUnknownRegion] when the code is not in the snapshot. The
// returned *time.Location is shared across callers and MUST NOT be
// mutated (time.Location has no exported mutators, so the zero risk is
// already enforced by the type system; this comment is documentation).
//
// Subsequent calls for the same code reuse a cached *time.Location to
// avoid re-parsing the embedded tzdata blob; the first call per code
// pays the parse cost once.
func (s *Set) TimezoneForRegion(code string) (*time.Location, error) {
	if s == nil {
		return nil, ErrUnknownRegion
	}
	r, ok := s.byCode[code]
	if !ok {
		return nil, fmt.Errorf("regions.TimezoneForRegion: %q: %w", code, ErrUnknownRegion)
	}
	if v, ok := s.locCache.Load(code); ok {
		loc, ok := v.(*time.Location)
		if !ok {
			// Defensive — only *time.Location is ever stored. Surface a
			// loud failure rather than panic on type assertion.
			return nil, fmt.Errorf("regions.TimezoneForRegion: %q: cache type %T", code, v)
		}
		return loc, nil
	}
	loc, err := time.LoadLocation(r.Timezone)
	if err != nil {
		return nil, fmt.Errorf("regions.TimezoneForRegion: %q: load %q: %w", code, r.Timezone, err)
	}
	// LoadOrStore — another goroutine may have raced us to load the
	// same zone; use whichever value lands first to keep the cache
	// canonical.
	actual, _ := s.locCache.LoadOrStore(code, loc)
	out, ok := actual.(*time.Location)
	if !ok {
		return nil, fmt.Errorf("regions.TimezoneForRegion: %q: cache type %T", code, actual)
	}
	return out, nil
}

// ListAll returns a copy of every [Region] in the snapshot, in
// declaration order from the YAML. The slice is a fresh allocation;
// mutating it does not affect the [Set].
func (s *Set) ListAll() []Region {
	if s == nil {
		return nil
	}
	return slices.Clone(s.all)
}

// Len returns the number of regions in the snapshot. Cheap to call.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.all)
}
