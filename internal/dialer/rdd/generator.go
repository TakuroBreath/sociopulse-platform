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
//     f. MARK the phone in dedup (Bloom + Redis SET) BEFORE persist —
//     so a concurrent Generate goroutine for the same tenant cannot
//     double-roll the same phone on a Bloom miss. If a later step
//     fails, we deliberately leave the mark in place: the phone is
//     out of rotation for the rest of this run, and the 30-day SET
//     TTL means a future Generate run sees it on the Bloom and
//     skips harmlessly.
//     g. Persist the respondent via [crmService.Create]. The CRM
//     service runs the DNC check inside Create — when it returns
//     [crmapi.ErrPhoneInDNC] the iteration buckets as DNCHit and
//     moves on. Other expected errors (invalid, duplicate) bucket
//     into their respective tallies. Transport errors propagate.
//     h. Enqueue the respondent into the dialer call queue. On
//     enqueue failure AFTER a successful CRM persist, the
//     respondent exists in Postgres but not in the queue — Generate
//     surfaces the error so the operator can re-enqueue / alert.
//
//  2. Return [api.GenerateResult] aggregating per-region counts plus
//     duplicate / DNC / invalid / throttled tallies.
//
// Failure-recovery contract:
//   - Dedup-mark failure (before CRM): non-fatal; log and continue. The
//     in-process Bloom may have absorbed the phone but the durable SET
//     write failed. Subsequent rolls in the same run still skip the
//     phone via the Bloom; cross-process dedup degrades to "phone may
//     re-roll in a future run" which is the same as a Bloom FP — no
//     correctness loss, just a wasted attempt.
//   - CRM-create failure (after mark): bucket the expected sentinels
//     (DNC, invalid, duplicate) and return cleanly. The mark stays —
//     the phone is out of rotation for this run. On transient errors
//     (DB down) the operator retries Generate; the mark hits Bloom and
//     a fresh phone is rolled. The 30-day TTL bounds the cost.
//   - Enqueue failure (after mark + CRM persist): respondent is in DB
//     but not in queue. Return error so the caller can re-enqueue via
//     the admin tool. Future work: emit a `dialer.rdd.enqueue_failed`
//     outbox event so a reaper picks the orphan up automatically.
//
// The generator is safe for concurrent calls — every dependency uses
// its own concurrency primitives and the [Generator] itself holds only
// configuration.
package rdd

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"sync"
	"sync/atomic"
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

	// Rand: when nil, seed ChaCha8 from crypto/rand so two Generators
	// booted in the same nanosecond do not produce identical sequences.
	// Tests pass a deterministic seed; production goes through
	// newChaCha8Seeded which fills all 32 bytes.
	rngSrc := cfg.Rand
	if rngSrc == nil {
		rngSrc = newChaCha8Seeded(logger)
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
	// Empty Quotas is an explicit error rather than a uniform-random
	// fallback. Plan 10 §"Generator's RPC contract" calls the Quotas
	// map the region-by-quota selector — there is no semantic
	// "uniform across all known regions" mode because (a) the embedded
	// regions snapshot includes regions the operator may not be
	// licensed for, and (b) RU DEF prefix density varies by region
	// (e.g. Moscow has many more 9XX prefixes than KAM), so a
	// uniform-by-region distribution would still produce a wildly
	// non-uniform phone-number distribution. Callers must specify at
	// least one positive-weight region.
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
		// attempt accumulates into out and emits its own metrics; the
		// returned label is unused at this layer so we discard it.
		if _, err := g.attempt(ctx, req, weighted, &out); err != nil {
			return out, err
		}
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
// any expected outcome (ok / duplicate / dnc / invalid); Redis transport
// failures and post-persist enqueue failures bubble up. The result
// struct is mutated in place so the caller does not have to re-aggregate.
//
// Ordering is deliberate: dedup.Mark runs BEFORE crm.Create so that two
// concurrent Generate goroutines that both miss the Bloom for the same
// phone don't both write to the CRM. The CRM unique-index is the
// cross-process safety net (loser gets ErrDuplicateRespondent), but
// marking first eliminates the redundant DB round-trip on the loser.
//
// On a CRM error path (DNC / invalid / duplicate / transport) we
// deliberately keep the mark — the phone is taken out of rotation for
// the remainder of the run. On a transport error a future operator
// retry will Bloom-hit the phone and roll a fresh one; the 30-day SET
// TTL bounds the cost.
//
// On enqueue failure AFTER a successful CRM persist, the respondent is
// in Postgres but never made it into the queue. We log loudly and
// surface the error to fail the entire batch — the operator must
// re-enqueue manually (or via a future reaper that consumes
// `dialer.rdd.enqueue_failed` outbox events).
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
	// Note: two parallel Generate goroutines for the same tenant rolling
	// the same phone both miss the Bloom and reach crm.Create. The
	// crm.RespondentService unique-index is the cross-goroutine safety
	// net (returns ErrDuplicateRespondent on the loser); the Bloom is a
	// per-process probabilistic pre-filter, not a serialisation point.
	seen, err := g.dedup.Seen(ctx, req.TenantID, req.ProjectID, phone)
	if err != nil {
		return "", err
	}
	if seen {
		out.DuplicatesHit++
		g.metrics.observe(resultDuplicate)
		return resultDuplicate, nil
	}

	// MARK FIRST — take the phone out of rotation for the rest of this
	// run before any DB round-trip. A failure here is non-fatal: the
	// in-process Bloom may have absorbed the phone (fast-path skip on
	// later iterations in the same run); cross-process dedup degrades
	// to "phone may roll again on a later run" which is identical to a
	// live Bloom false positive — no correctness loss.
	if mErr := g.dedup.Mark(ctx, req.TenantID, req.ProjectID, phone); mErr != nil {
		g.log.Warn("rdd.Generate: dedup mark failed (pre-persist)", zap.Error(mErr))
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
		// DNC sentinel — phone stays marked so we don't re-roll it for
		// the rest of this run. The 30-day TTL covers cross-run
		// recurrence; a stale DNC entry harmlessly re-hits the sentinel.
		out.DNCHit++
		g.metrics.observe(resultDNC)
		return resultDNC, nil
	case errors.Is(err, crmapi.ErrInvalidPhone):
		// Invalid sentinel — keep the mark so we don't waste cycles
		// rolling the same losing prefix+subscriber on the next iter.
		out.InvalidHit++
		g.metrics.observe(resultInvalid)
		return resultInvalid, nil
	case errors.Is(err, crmapi.ErrDuplicateRespondent):
		// (project_id, phone_hash) collision — another generator round
		// (or an import) already inserted this phone. Bucket as
		// duplicate; the mark is already in place from the pre-persist
		// step.
		out.DuplicatesHit++
		g.metrics.observe(resultDuplicate)
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
		// Enqueue failed AFTER a successful CRM persist. The respondent
		// is in Postgres but never made it into the queue — the worst
		// failure-mode in the pipeline. Log loudly with the IDs an
		// operator needs to re-enqueue, then return the error so the
		// caller fails the batch.
		g.log.Error("rdd.Generate: enqueue failed post-persist; respondent orphaned in DB",
			zap.String("tenant_id", req.TenantID.String()),
			zap.String("project_id", req.ProjectID.String()),
			zap.String("respondent_id", respondentID.String()),
			zap.String("phone", phone),
			zap.String("region", regionCode),
			zap.Error(err),
		)
		g.metrics.observe(resultEnqueueFailed)
		return "", fmt.Errorf("rdd.Generate: enqueue post-persist: %w", err)
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
// pre: p.total > 0; caller must check len(p.codes) > 0 before calling
// pick. An empty picker triggers IntN(0) which panics — that's the
// fail-fast behaviour we want, so this method intentionally has no
// guard.
func (p *regionPicker) pick(rng *rand.Rand) string {
	r := rng.IntN(p.total)
	idx := sort.SearchInts(p.cumulative, r+1)
	if idx >= len(p.codes) {
		idx = len(p.codes) - 1
	}
	return p.codes[idx]
}

// seedCounter is a process-wide fallback counter consumed only when
// crypto/rand.Read fails (extremely rare on Linux/macOS). Using it
// guarantees that two consecutive calls in the same nanosecond don't
// collapse to identical seeds even on the fallback path.
var seedCounter uint64

// newChaCha8Seeded returns a freshly-seeded *rand.ChaCha8 source. Every
// one of the 32 seed bytes is filled from crypto/rand so two Generators
// booted in the same nanosecond produce different sequences.
//
// On the (extremely unlikely on Linux/macOS) case where crypto/rand
// fails, we fall back to a derived seed of UnixNano + an atomic counter
// + the PID. This still avoids the all-zero-tail problem and gives
// distinct streams for distinct processes / consecutive calls — but
// callers that depend on cryptographic-quality entropy should treat a
// crypto/rand failure as a deployment-environment bug.
func newChaCha8Seeded(logger *zap.Logger) *rand.ChaCha8 {
	var seed [32]byte
	if _, err := cryptorand.Read(seed[:]); err != nil {
		// Production deployments MUST have working crypto/rand —
		// surface the deviation so the operator notices.
		if logger != nil {
			logger.Warn("rdd.New: crypto/rand.Read failed; falling back to derived seed",
				zap.Error(err))
		}
		binary.LittleEndian.PutUint64(seed[0:8], uint64(time.Now().UnixNano())) //nolint:gosec // UnixNano is positive in practice; cast widens.
		binary.LittleEndian.PutUint64(seed[8:16], atomic.AddUint64(&seedCounter, 1))
		binary.LittleEndian.PutUint64(seed[16:24], uint64(os.Getpid())) //nolint:gosec // PID always non-negative; widening cast.
		// bytes 24..31 stay zero — better than nothing, and the derived
		// upper 24 bytes still distinguish concurrent fallbacks.
	}
	//nolint:gosec // ChaCha8 is the project's standard non-crypto rand source.
	return rand.NewChaCha8(seed)
}
