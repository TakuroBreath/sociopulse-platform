package regions

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// loadFromBytes is exercised here (package-internal test) so the
// error paths in [Load] are reachable without round-tripping through
// the embedded regions.yaml.

func TestLoadFromBytes_MalformedYAML(t *testing.T) {
	t.Parallel()
	_, err := loadFromBytes([]byte("regions: : :"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse")
}

func TestLoadFromBytes_NoRegions(t *testing.T) {
	t.Parallel()
	_, err := loadFromBytes([]byte("regions: []"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty")
}

func TestLoadFromBytes_EmptyCode(t *testing.T) {
	t.Parallel()
	yamlIn := []byte(`regions:
  - code: ""
    name_ru: "тест"
    name_en: "test"
    timezone: "Europe/Moscow"
    abc_flag: false
    def_prefixes: ["916"]
`)
	_, err := loadFromBytes(yamlIn)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty code")
}

func TestLoadFromBytes_EmptyTimezone(t *testing.T) {
	t.Parallel()
	yamlIn := []byte(`regions:
  - code: "RU-XX"
    name_ru: "тест"
    name_en: "test"
    timezone: ""
    abc_flag: false
    def_prefixes: ["916"]
`)
	_, err := loadFromBytes(yamlIn)
	require.Error(t, err)
	require.Contains(t, err.Error(), "timezone")
}

func TestLoadFromBytes_DuplicateCode(t *testing.T) {
	t.Parallel()
	yamlIn := []byte(`regions:
  - code: "RU-MOW"
    name_ru: "Москва"
    name_en: "Moscow"
    timezone: "Europe/Moscow"
    abc_flag: false
    def_prefixes: ["916"]
  - code: "RU-MOW"
    name_ru: "Москва duplicate"
    name_en: "Moscow duplicate"
    timezone: "Europe/Moscow"
    abc_flag: false
    def_prefixes: ["917"]
`)
	_, err := loadFromBytes(yamlIn)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate")
}

func TestLoadFromBytes_BadTimezone(t *testing.T) {
	t.Parallel()
	yamlIn := []byte(`regions:
  - code: "RU-XX"
    name_ru: "тест"
    name_en: "test"
    timezone: "Definitely/Not/A/Real/TZ"
    abc_flag: false
    def_prefixes: ["916"]
`)
	_, err := loadFromBytes(yamlIn)
	require.Error(t, err)
}

// TestTimezoneForRegion_BadTimezoneAfterLoad — manually construct a
// Set with a corrupted timezone string and confirm the second-call
// path (cache miss + load failure) surfaces a wrapped error. Stretches
// the cache-load branch that happy-path tests cannot reach.
func TestTimezoneForRegion_BadTimezoneAfterLoad(t *testing.T) {
	t.Parallel()
	s := &Set{
		all: []Region{{Code: "RU-FAKE", Timezone: "Definitely/Not/A/Real/TZ"}},
		byCode: map[string]Region{
			"RU-FAKE": {Code: "RU-FAKE", Timezone: "Definitely/Not/A/Real/TZ"},
		},
	}
	_, err := s.TimezoneForRegion("RU-FAKE")
	require.Error(t, err)
}
