package rdd

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/sociopulse/platform/pkg/regions"
)

// subscriberDigits is the length of the subscriber suffix that follows
// the three-digit DEF code. RU mobile numbers are E.164 with a fixed
// total length of 11 digits (country code 7 + DEF 3 + subscriber 7).
const subscriberDigits = 7

// errNoEligiblePrefix is returned when [pickPrefixForRegion] cannot find a
// prefix matching the requested ABC/DEF flag for the supplied region.
// Callers bucket the attempt as InvalidHit (the project's quota for the
// region simply has no candidates left) and continue.
var errNoEligiblePrefix = errors.New("rdd/prefixes: no eligible prefix for region/abc combination")

// pickPrefixForRegion returns one DEF prefix from the region. v1 ignores
// the abcFlag because every entry in the embedded YAML is DEF-coded
// (cellular numbering plan), but the parameter is kept on the public
// surface for forward-compatibility with v2 ABC support. The selection
// is uniform — every prefix in the region's pool has equal probability.
func pickPrefixForRegion(rng *rand.Rand, region regions.Region, _ bool) (string, error) {
	if len(region.DEFPrefixes) == 0 {
		return "", fmt.Errorf("region %s: %w", region.Code, errNoEligiblePrefix)
	}
	idx := rng.IntN(len(region.DEFPrefixes))
	return region.DEFPrefixes[idx], nil
}

// rollSubscriber returns a 7-digit random subscriber suffix. The
// returned string contains only the [0-9] characters and never starts
// with the DEF code itself (callers concatenate prefix + subscriber to
// form the complete 10-digit national number).
func rollSubscriber(rng *rand.Rand) string {
	var b strings.Builder
	b.Grow(subscriberDigits)
	for range subscriberDigits {
		// IntN(10) yields [0,9]; rune('0') + IntN(10) is the cheapest
		// digit-to-rune conversion in Go.
		b.WriteRune(rune('0' + rng.IntN(10)))
	}
	return b.String()
}

// composePhone joins the country code, prefix, and subscriber into a
// canonical E.164 string. The country code is hard-coded to "+7" since
// v1 targets the Russian Federation only; multi-country support would
// take this from the [regions.Region] in v2.
func composePhone(prefix, subscriber string) string {
	return "+7" + prefix + subscriber
}

// validE164RU re-checks the composed phone against the canonical RU
// E.164 shape: leading "+7", a three-digit DEF code (mobile prefixes are
// 9XX in v1 — we don't enforce the leading 9 here so the embedded YAML
// can declare 8XX prefixes when v2 adds landline RDD), and a 7-digit
// subscriber. The check is intentionally cheap — the rng output is
// always digits, so the only failure mode in practice is a
// mis-configured prefix length in the YAML.
func validE164RU(phone string) bool {
	// "+7" + 3 + 7 = 12 runes.
	if len(phone) != 12 {
		return false
	}
	if phone[0] != '+' || phone[1] != '7' {
		return false
	}
	for i := 2; i < len(phone); i++ {
		if phone[i] < '0' || phone[i] > '9' {
			return false
		}
	}
	return true
}
