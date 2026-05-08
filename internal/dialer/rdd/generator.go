// Package rdd implements [api.RDDGenerator] — the Random-Digit-Dialing
// respondent generator for the dialer module.
//
// Generation flow (per call to [Generator.Generate]):
//
//  1. For each requested respondent up to req.N:
//     a. Consume a token from the per-tenant leaky bucket. When the
//     bucket is dry the iteration aborts and the run is reported
//     with Throttled=true.
//     b. Pick a region weighted by req.Quotas (uniform within the
//     weight). Pick a DEF prefix from the region.
//     c. Roll a 7-digit subscriber suffix; compose the E.164.
//     d. Reject phones that fail [validE164RU] (defence in depth).
//     e. Reject phones the project's Bloom filter has seen — the
//     Redis SET is consulted only on a Bloom hit, since Bloom has
//     zero false negatives and the SET round-trip dominates the
//     per-iteration cost.
//     f. Persist the respondent via [crmService.Create]. The CRM
//     service runs the DNC check inside Create — when it returns
//     [crmapi.ErrPhoneInDNC] the iteration buckets as DNCHit and
//     moves on. Other errors propagate to the caller.
//     g. Enqueue the respondent into the dialer call queue. A
//     duplicate-in-queue (ok=false) is treated as success here —
//     the rare race with another generator session is benign.
//     h. Mark the phone in BOTH the Bloom filter and the Redis SET.
//
//  2. Return [api.GenerateResult] aggregating per-region counts plus
//     duplicate / DNC / invalid / throttled tallies.
//
// The generator is safe for concurrent calls — every dependency uses
// its own concurrency primitives and the [Generator] itself holds only
// configuration.
package rdd

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/pkg/regions"
)

// Default tunables. Surface mirrors Plan 10 §"Generator's constructor
// signature".
const (
	defaultPerTenantPerSec = 10
	defaultBloomCapacity   = 100_000
	defaultBloomFPRate     = 0.01
	defaultSetTTL          = 30 * 24 * time.Hour
)

// CRM is the minimal slice of [crmapi.RespondentService] the generator
// needs. Defined here so tests can supply a tiny in-memory fake without
// dragging in the full crm/api surface, and so the depguard module-
// boundaries rule (which forbids cross-module imports of internal/crm/
// service) stays satisfied.
type CRM interface {
	Create(ctx context.Context, in crmapi.CreateRespondentInput) (*crmapi.Respondent, error)
}

// Limits bundles the tunable knobs that govern dedup capacity and the
// per-tenant rate cap.
type Limits struct {
	PerTenantPerSec int           // default 10
	BloomCapacity   uint          // default 100_000
	BloomFPRate     float64       // default 0.01
	SetTTL          time.Duration // default 30*24h
}

// Config bundles dependencies + Limits for a [Generator]. Required
// fields (Redis, Queue, Crm, Regions) are documented per-field; nil-
// tolerated fields fall back to safe defaults.
type Config struct {
	// Redis is the connection used for the leaky bucket + dedup SET.
	// Required.
	Redis *redis.Client

	// Queue is the call queue used to enqueue generated respondents.
	// Required.
	Queue api.CallQueue

	// Crm is the small CRM slice the generator depends on.
	// Production wires *crm.Service via the modules.Locator;
	// tests pass a fake. Required.
	Crm CRM

	// Regions is the loaded regions snapshot. Required — the
	// generator uses it to resolve region codes to DEF prefixes.
	Regions *regions.Set

	// Logger receives per-method diagnostics. nil → zap.NewNop().
	Logger *zap.Logger

	// Clock returns the current time. nil → time.Now. Tests pass a
	// frozen clock so the leaky bucket and the duration histogram
	// produce deterministic readings.
	Clock func() time.Time

	// Rand is the seeded ChaCha8 source. nil → seeded from the
	// current clock. Tests pass a deterministic rand.NewChaCha8 with a
	// fixed seed so prefix selection is reproducible.
	Rand *rand.ChaCha8

	// Metrics is the per-package collector group. nil → no metrics
	// (the generator is fully functional without it).
	Metrics *Metrics

	// Limits tunes dedup capacity and the per-tenant rate. Zero
	// fields fall back to defaults documented on the type.
	Limits Limits
}

// Generator implements [api.RDDGenerator]. Stateless beyond the
// configured dependencies — concurrent Generate calls share the same
// Bloom-filter map and leaky-bucket store but do not share per-call
// state.
type Generator struct {
	rdb     *redis.Client
	queue   api.CallQueue
	crm     CRM
	regions *regions.Set
	log     *zap.Logger
	clock   func() time.Time
	rng     *rand.Rand
	rngMu   sync.Mutex // protects rng across concurrent Generate calls
	metrics *Metrics

	bucket *LeakBucket
	dedup  *Dedup
}

// Compile-time interface check. Surfaces api.RDDGenerator signature
// drift the moment it happens (Plan 09 lessons §8).
var _ api.RDDGenerator = (*Generator)(nil)

// New constructs a Generator. Returns an error when a required
// dependency is missing; nil-tolerated fields are filled with defaults
// so callers can pass a minimal Config.
func New(cfg Config) (*Generator, error) {
	if cfg.Redis == nil {
		return nil, errors.New("rdd.New: Redis is required")
	}
	if cfg.Queue == nil {
		return nil, errors.New("rdd.New: Queue is required")
	}
	if cfg.Crm == nil {
		return nil, errors.New("rdd.New: Crm is required")
	}
	if cfg.Regions == nil {
		return nil, errors.New("rdd.New: Regions is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	limits := cfg.Limits
	if limits.PerTenantPerSec <= 0 {
		limits.PerTenantPerSec = defaultPerTenantPerSec
	}
	if limits.BloomCapacity == 0 {
		limits.BloomCapacity = defaultBloomCapacity
	}
	if limits.BloomFPRate <= 0 {
		limits.BloomFPRate = defaultBloomFPRate
	}
	if limits.SetTTL <= 0 {
		limits.SetTTL = defaultSetTTL
	}

	// Rand: when nil, seed ChaCha8 from the clock. The two-uint64
	// seed is intentionally low-quality (the wall clock plus a
	// constant) — the depguard rule + golangci-lint enforce that we
	// never use math/rand v1, but math/rand/v2 sources do not need
	// crypto-grade entropy for RDD prefix selection.
	rngSrc := cfg.Rand
	if rngSrc == nil {
		nowMS := clock().UnixNano()
		//nolint:gosec // non-crypto seed for prefix selection; depguard already
		// bans math/rand v1, math/rand/v2 needs no crypto entropy here.
		rngSrc = rand.NewChaCha8([32]byte{
			byte(nowMS), byte(nowMS >> 8), byte(nowMS >> 16), byte(nowMS >> 24),
			byte(nowMS >> 32), byte(nowMS >> 40), byte(nowMS >> 48), byte(nowMS >> 56),
		})
	}
	//nolint:gosec // non-crypto: math/rand/v2 is the project standard for jitter / prefix picking.
	rng := rand.New(rngSrc)

	return &Generator{
		rdb:     cfg.Redis,
		queue:   cfg.Queue,
		crm:     cfg.Crm,
		regions: cfg.Regions,
		log:     logger,
		clock:   clock,
		rng:     rng,
		metrics: cfg.Metrics,
		bucket:  newLeakBucket(cfg.Redis, limits.PerTenantPerSec, time.Hour, clock),
		dedup:   newDedup(cfg.Redis, limits.BloomCapacity, limits.BloomFPRate, limits.SetTTL),
	}, nil
}

// Generate implements [api.RDDGenerator]. Synthesises up to req.N
// respondents under the supplied region quotas + ABC ratio. Returns
// the aggregated [api.GenerateResult] plus an error only on
// configuration / unrecoverable transport failures — DNC hits, Bloom
// duplicates, and rate-limit throttles flow into the result struct
// rather than as errors.
//
// When the leaky bucket throttles the very first iteration AND
// req.N > 0 (i.e. nothing was generated), Generate returns
// [api.ErrThrottled] alongside the partial result so callers can
// errors.Is the throttle case without parsing the result struct.
func (g *Generator) Generate(ctx context.Context, req api.GenerateRequest) (api.GenerateResult, error) {
	if req.N <= 0 {
		return api.GenerateResult{ByRegion: map[string]int{}}, nil
	}
	if req.TenantID == uuid.Nil || req.ProjectID == uuid.Nil {
		return api.GenerateResult{}, fmt.Errorf("rdd.Generate: tenant/project must be non-nil: %w", crmapi.ErrInvalidArgument)
	}
	if len(req.Quotas) == 0 {
		return api.GenerateResult{}, fmt.Errorf("rdd.Generate: at least one region quota required: %w", crmapi.ErrInvalidArgument)
	}

	start := g.clock()
	defer func() {
		g.metrics.observeDuration(g.clock().Sub(start).Seconds())
	}()

	weighted := buildRegionPicker(req.Quotas)
	if len(weighted.codes) == 0 {
		return api.GenerateResult{}, fmt.Errorf("rdd.Generate: no positive-weight regions: %w", crmapi.ErrInvalidArgument)
	}

	out := api.GenerateResult{ByRegion: make(map[string]int, len(req.Quotas))}

	for range req.N {
		if err := ctx.Err(); err != nil {
			return out, fmt.Errorf("rdd.Generate: %w", err)
		}
		ok, err := g.bucket.Allow(ctx, req.TenantID)
		if err != nil {
			return out, err
		}
		if !ok {
			out.Throttled = true
			g.metrics.observe(resultThrottled)
			break
		}
		bucketed, err := g.attempt(ctx, req, weighted, &out)
		if err != nil {
			return out, err
		}
		_ = bucketed // result already accumulated by attempt
	}

	if out.Generated == 0 && out.Throttled {
		// The leaky bucket throttled on the very first iteration. Surface
		// ErrThrottled per the Plan 10 contract so callers do not have to
		// pattern-match on the result struct.
		return out, fmt.Errorf("rdd.Generate: %w", api.ErrThrottled)
	}
	return out, nil
}

// attempt runs ONE respondent-generation attempt. Returns nil error on
// any expected outcome (ok / duplicate / dnc / invalid); only Redis or
// CRM transport failures bubble up. The result struct is mutated in
// place so the caller does not have to re-aggregate.
func (g *Generator) attempt(
	ctx context.Context,
	req api.GenerateRequest,
	weighted *regionPicker,
	out *api.GenerateResult,
) (string, error) {
	// Roll the random outputs we need under one lock acquisition so a
	// concurrent Generate call cannot corrupt the ChaCha8 state. The
	// helpers below run on plain values once the lock is released.
	regionCode, prefix, subscriber, ok := g.rollAttempt(weighted, req.ABCRatio)
	if !ok {
		out.InvalidHit++
		g.metrics.observe(resultInvalid)
		return resultInvalid, nil
	}
	phone := composePhone(prefix, subscriber)
	if !validE164RU(phone) {
		out.InvalidHit++
		g.metrics.observe(resultInvalid)
		return resultInvalid, nil
	}
	seen, err := g.dedup.Seen(ctx, req.TenantID, req.ProjectID, phone)
	if err != nil {
		return "", err
	}
	if seen {
		out.DuplicatesHit++
		g.metrics.observe(resultDuplicate)
		return resultDuplicate, nil
	}

	resp, err := g.crm.Create(ctx, crmapi.CreateRespondentInput{
		TenantID:   req.TenantID,
		ProjectID:  req.ProjectID,
		Phone:      phone,
		RegionCode: regionCode,
		Source:     crmapi.SourceRDD,
	})
	switch {
	case errors.Is(err, crmapi.ErrPhoneInDNC):
		out.DNCHit++
		g.metrics.observe(resultDNC)
		// Mark in dedup so we don't roll the same phone again — DNC
		// status survives the 30-day SET TTL window deliberately. A
		// dedup-write failure here is non-fatal: the phone may roll
		// again on a later run and harmlessly hit DNC again.
		if mErr := g.dedup.Mark(ctx, req.TenantID, req.ProjectID, phone); mErr != nil {
			g.log.Warn("rdd.Generate: dedup mark failed (dnc path)", zap.Error(mErr))
		}
		return resultDNC, nil
	case errors.Is(err, crmapi.ErrInvalidPhone):
		out.InvalidHit++
		g.metrics.observe(resultInvalid)
		return resultInvalid, nil
	case errors.Is(err, crmapi.ErrDuplicateRespondent):
		// (project_id, phone_hash) collision — another generator round
		// (or an import) already inserted this phone. Bucket as
		// duplicate and mark in dedup for forward-correctness.
		out.DuplicatesHit++
		g.metrics.observe(resultDuplicate)
		if mErr := g.dedup.Mark(ctx, req.TenantID, req.ProjectID, phone); mErr != nil {
			g.log.Warn("rdd.Generate: dedup mark failed (duplicate path)", zap.Error(mErr))
		}
		return resultDuplicate, nil
	case err != nil:
		return "", fmt.Errorf("rdd.Generate: crm.Create: %w", err)
	}

	respondentID := resp.ID
	if respondentID == uuid.Nil {
		// Defensive — the crm contract returns the inserted respondent;
		// a Nil ID indicates a fake/test bug. Fall back to a freshly-
		// minted UUID so the queue insert proceeds.
		respondentID = uuid.New()
	}

	if _, err := g.queue.EnqueueRespondent(ctx, api.EnqueueRequest{
		TenantID:     req.TenantID,
		ProjectID:    req.ProjectID,
		RespondentID: respondentID,
		Phone:        phone,
		Region:       regionCode,
		Priority:     0,
	}); err != nil {
		// Enqueue failure is fatal — without the queue insert the
		// respondent will never be dialled. Surface upward so the
		// caller can retry the run / alert.
		return "", fmt.Errorf("rdd.Generate: enqueue: %w", err)
	}

	if err := g.dedup.Mark(ctx, req.TenantID, req.ProjectID, phone); err != nil {
		// Dedup write failed; log but proceed — the in-process Bloom
		// filter has already absorbed the phone, so subsequent
		// iterations within this run still skip it. Cross-process
		// dedup degrades to "phone may roll again on a later run"
		// which is the same as a Bloom false positive on a live cache
		// — no correctness loss, just a wasted attempt.
		g.log.Warn("rdd.Generate: dedup mark failed", zap.Error(err))
	}

	out.Generated++
	out.ByRegion[regionCode]++
	g.metrics.observe(resultOK)
	return resultOK, nil
}

// rollAttempt produces every random draw needed for one generation
// attempt under a single lock acquisition. Returns
// (regionCode, prefix, subscriber, ok). ok=false signals the picked
// region has no eligible prefix and the caller should bucket the
// attempt as InvalidHit.
//
// Holding the lock across all three rolls keeps the ChaCha8 state
// race-free without forcing the surrounding I/O (Bloom check, Redis
// SET, CRM Create, queue enqueue) to serialise. Per-attempt lock hold
// is microseconds; contention under realistic concurrent loads is
// negligible.
func (g *Generator) rollAttempt(weighted *regionPicker, abcRatio float64) (string, string, string, bool) {
	g.rngMu.Lock()
	defer g.rngMu.Unlock()
	regionCode := weighted.pick(g.rng)
	region, ok := g.regions.RegionForCode(regionCode)
	if !ok {
		return regionCode, "", "", false
	}
	isABC := g.rng.Float64() < abcRatio
	prefix, err := pickPrefixForRegion(g.rng, region, isABC)
	if err != nil {
		return regionCode, "", "", false
	}
	subscriber := rollSubscriber(g.rng)
	return regionCode, prefix, subscriber, true
}

// regionPicker is the precomputed weighted picker for the supplied
// quotas. Conceptually a CDF: codes carries the region codes in
// declaration order; cumulative carries the running sum of weights so
// pick() can binary-search a uniform sample into the right band.
//
// Quota weights of 0 or negative are filtered out at build time —
// callers that pass {region: 0} are effectively asking us to skip that
// region entirely.
type regionPicker struct {
	codes      []string
	cumulative []int
	total      int
}

// buildRegionPicker constructs a regionPicker. Empty input yields an
// empty picker; pick() on an empty picker panics — callers must check
// len(codes)>0 before invoking.
func buildRegionPicker(quotas map[string]int) *regionPicker {
	// Stable iteration order matters for tests + reproducibility:
	// sort by code so pick() is deterministic given a deterministic
	// rng seed.
	codes := make([]string, 0, len(quotas))
	for c := range quotas {
		codes = append(codes, c)
	}
	sort.Strings(codes)

	p := &regionPicker{}
	for _, c := range codes {
		w := quotas[c]
		if w <= 0 {
			continue
		}
		p.total += w
		p.codes = append(p.codes, c)
		p.cumulative = append(p.cumulative, p.total)
	}
	return p
}

// pick returns one region code drawn uniformly from the weighted CDF.
// The caller MUST check len(p.codes)>0 before invoking.
func (p *regionPicker) pick(rng *rand.Rand) string {
	r := rng.IntN(p.total)
	idx := sort.SearchInts(p.cumulative, r+1)
	if idx >= len(p.codes) {
		idx = len(p.codes) - 1
	}
	return p.codes[idx]
}
