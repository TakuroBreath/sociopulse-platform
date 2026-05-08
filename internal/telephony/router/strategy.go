// Package router implements trunk + FS-node selection for outbound calls.
//
// Two-stage decision:
//
//  1. Strategy.Pick chooses a Trunk among the configured catalog (operator
//     policy: least cost, round-robin, weighted random, or least-cost with a
//     failure-rate veto).
//  2. Router.Select intersects the chosen trunk's NodeAddrs with the pool's
//     HealthyNodes and atomically claims one slot via the Redis backpressure
//     counter (op:active_channels:{node}). The first node that accepts the
//     INCR-with-cap wins; the call URL "sofia/gateway/<gw>/<dest>" is built
//     from the trunk's gateway name and the dialer's destination number.
//
// The strategies live in this file (pure data, no I/O). Backpressure lives
// in backpressure.go (Redis Lua). The composition root in router.go wires
// the two together and implements api.Router.
package router

import (
	"errors"
	"math/rand/v2"
	"sort"
	"sync/atomic"
)

// Trunk is a SIP trunk picked by a Strategy. Fields mirror what the
// composition root reads out of cfg.Telephony.Trunks (TrunkConfig); FailureRate
// is reserved for Plan 13's stats collector and is always 0 in v1.
type Trunk struct {
	// ID identifies the trunk catalog entry. Unique within the router.
	ID string

	// GatewayName is the mod_sofia gateway name on the FS side. The
	// originate URL is built as "sofia/gateway/<GatewayName>/<dest>".
	GatewayName string

	// NodeAddrs lists the FS nodes that have this gateway configured. The
	// router intersects this with the pool's healthy-node set to pick a
	// concrete FS node for dispatch.
	NodeAddrs []string

	// CostPerMin is the trunk's per-minute price (any currency; v1 uses
	// roubles via cfg.Telephony.Trunks.CostPerMinuteRub). Lower is
	// preferred by LeastCost and LeastCostWithFallback.
	CostPerMin float64

	// Weight is the relative weight used by the Weighted strategy. Trunks
	// with weight 0 are skipped by Weighted (totalW excludes them) — this
	// is the documented contract: a trunk with Weight=0 is "active but
	// never picked" by the random strategy.
	Weight int

	// Active is the operator-controlled enable flag. Inactive trunks are
	// invisible to every strategy. The composition root maps
	// `len(NodeAddrs) > 0 && CapacityChannels > 0` onto Active.
	Active bool

	// FailureRate is the rolling failure proportion (0..1) populated by
	// Plan 13's stats collector. LeastCostWithFallback skips trunks whose
	// FailureRate exceeds the configured threshold; in v1 this field is
	// always zero.
	FailureRate float64

	// Priority is reserved for a future "tiered" strategy (try priority=0
	// trunks first, fall back to priority=1). Not consulted in v1.
	Priority int
}

// ErrNoTrunkAvailable is returned by Strategy.Pick (and propagated by
// Router.Select) when no trunk in the supplied list satisfies the strategy's
// active-set predicate. errors.Is-friendly: the same sentinel is also
// declared in internal/telephony/api/errors.go so callers across module
// boundaries can match without importing this package.
var ErrNoTrunkAvailable = errors.New("router: no available trunk")

// Strategy picks a Trunk for a destination phone number. The dest argument is
// reserved for region-aware strategies (e.g. Russian +7-prefix routing); the
// v1 strategies ignore it. Pick MUST NOT mutate the trunks slice — callers
// pass a shared catalog and rely on the snapshot semantics.
type Strategy interface {
	Pick(trunks []Trunk, dest string) (Trunk, error)
}

// LeastCost returns the active trunk with the lowest CostPerMin. Ties are
// broken by the iteration order of the input slice (stable for callers that
// pre-sort by ID). Returns ErrNoTrunkAvailable when no trunk is active.
type LeastCost struct{}

// Pick implements Strategy.
func (LeastCost) Pick(trunks []Trunk, _ string) (Trunk, error) {
	var best *Trunk
	for i := range trunks {
		t := &trunks[i]
		if !t.Active {
			continue
		}
		if best == nil || t.CostPerMin < best.CostPerMin {
			best = t
		}
	}
	if best == nil {
		return Trunk{}, ErrNoTrunkAvailable
	}
	return *best, nil
}

// LeastCostWithFallback filters out trunks whose FailureRate exceeds
// FailureThreshold, then runs LeastCost on the remainder. If every trunk is
// over the threshold, the strategy still returns the cheapest trunk (failing
// trunks are better than failing the call) — Plan 09 references doc gotcha
// #6: never silently drop a call when there is *some* trunk to try.
type LeastCostWithFallback struct {
	// FailureThreshold is the maximum acceptable FailureRate (inclusive).
	// Default zero means "any failure removes the trunk" — set this
	// explicitly to e.g. 0.5 in production wiring.
	FailureThreshold float64
}

// Pick implements Strategy.
func (s LeastCostWithFallback) Pick(trunks []Trunk, dest string) (Trunk, error) {
	filtered := make([]Trunk, 0, len(trunks))
	for _, t := range trunks {
		if t.Active && t.FailureRate <= s.FailureThreshold {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) == 0 {
		// Fallback path: every trunk is over the threshold. Try them
		// anyway — a degraded trunk is still better than a hard fail.
		return LeastCost{}.Pick(trunks, dest)
	}
	return LeastCost{}.Pick(filtered, dest)
}

// RoundRobin cycles through the active trunks in lexicographic ID order.
// Concurrency-safe: counter is an atomic.Uint64 so 1000 parallel Picks return
// each trunk roughly the same number of times. Requires a pointer receiver
// because the counter is a value field — callers MUST pass `*RoundRobin`,
// not `RoundRobin{}`.
type RoundRobin struct {
	counter atomic.Uint64
}

// Pick implements Strategy.
func (s *RoundRobin) Pick(trunks []Trunk, _ string) (Trunk, error) {
	active := make([]Trunk, 0, len(trunks))
	for _, t := range trunks {
		if t.Active {
			active = append(active, t)
		}
	}
	if len(active) == 0 {
		return Trunk{}, ErrNoTrunkAvailable
	}
	// Sort by ID for deterministic order independent of input slice
	// ordering — tests rely on this, and the production catalog refresh
	// might re-emit trunks in different order between reloads.
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
	// Add returns the new value; subtract 1 to land in [0, n) on the
	// first call (counter starts at 0, Add(1) returns 1).
	n := s.counter.Add(1) - 1
	idx := n % uint64(len(active)) //nolint:gosec // len(active) > 0 guaranteed by the early return above
	return active[idx], nil
}

// Weighted picks an active trunk with probability proportional to its Weight.
// Trunks with Weight <= 0 are excluded from the lottery; if every active
// trunk has Weight 0, ErrNoTrunkAvailable is returned. Uses math/rand/v2
// (per repo policy: math/rand v1 is depguard-banned) — non-cryptographic
// randomness is fine here because the choice is not security-sensitive.
type Weighted struct{}

// Pick implements Strategy.
func (Weighted) Pick(trunks []Trunk, _ string) (Trunk, error) {
	totalW := 0
	for _, t := range trunks {
		if t.Active && t.Weight > 0 {
			totalW += t.Weight
		}
	}
	if totalW == 0 {
		return Trunk{}, ErrNoTrunkAvailable
	}
	r := rand.IntN(totalW) //nolint:gosec // non-security weighted choice; math/rand/v2 is the project-blessed PRNG (.golangci.yml depguard banned-stdlib)
	for _, t := range trunks {
		if !t.Active || t.Weight <= 0 {
			continue
		}
		r -= t.Weight
		if r < 0 {
			return t, nil
		}
	}
	// Unreachable: totalW > 0 and we just walked the same predicate. Kept
	// as a safety net to satisfy the compiler's "all paths return" rule.
	return Trunk{}, ErrNoTrunkAvailable
}
