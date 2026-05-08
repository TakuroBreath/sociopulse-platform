package rdd_test

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/queue"
	"github.com/sociopulse/platform/internal/dialer/rdd"
	"github.com/sociopulse/platform/pkg/regions"
)

// fakeCRM is the in-memory CRM seam Generate writes through. Tests
// configure dnc / invalid / duplicate phones to drive the corresponding
// branches; everything else returns a fresh respondent stamped with a
// new UUID.
type fakeCRM struct {
	mu              sync.Mutex
	dncPhones       map[string]bool
	invalidPhones   map[string]bool
	duplicatePhones map[string]bool
	created         []crmapi.CreateRespondentInput
	createErr       error
}

func newFakeCRM() *fakeCRM {
	return &fakeCRM{
		dncPhones:       make(map[string]bool),
		invalidPhones:   make(map[string]bool),
		duplicatePhones: make(map[string]bool),
	}
}

func (f *fakeCRM) Create(_ context.Context, in crmapi.CreateRespondentInput) (*crmapi.Respondent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.dncPhones[in.Phone] {
		return nil, crmapi.ErrPhoneInDNC
	}
	if f.invalidPhones[in.Phone] {
		return nil, crmapi.ErrInvalidPhone
	}
	if f.duplicatePhones[in.Phone] {
		return nil, crmapi.ErrDuplicateRespondent
	}
	f.created = append(f.created, in)
	return &crmapi.Respondent{
		ID:         uuid.New(),
		TenantID:   in.TenantID,
		ProjectID:  in.ProjectID,
		RegionCode: in.RegionCode,
		Source:     in.Source,
		Status:     crmapi.RespPending,
	}, nil
}

func (f *fakeCRM) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created)
}

// fakeClock is the frozen clock the rdd tests advance explicitly.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// fixture wires the Generator with miniredis-backed Redis, a real
// queue.RedisQueue (so the EnqueueRespondent path is end-to-end), a
// fakeCRM, and a frozen clock. The deterministic ChaCha8 seed makes
// the prefix selection reproducible across runs.
type fixture struct {
	mr      *miniredis.Miniredis
	rdb     *redis.Client
	clock   *fakeClock
	crm     *fakeCRM
	queue   *queue.RedisQueue
	regions *regions.Set
	gen     *rdd.Generator
	reg     *prometheus.Registry
	metrics *rdd.Metrics
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWithLimits(t, rdd.Limits{})
}

func newFixtureWithLimits(t *testing.T, limits rdd.Limits) *fixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	clk := &fakeClock{now: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)}
	q, err := queue.New(queue.Config{Redis: rdb, Clock: clk.Now})
	require.NoError(t, err)

	regSet, err := regions.Load()
	require.NoError(t, err)

	crm := newFakeCRM()
	reg := prometheus.NewRegistry()
	m := rdd.RegisterMetrics(reg)

	//nolint:gosec // non-crypto deterministic test seed.
	rng := rand.NewChaCha8([32]byte{
		'r', 'd', 'd', '-', 'g', 'e', 'n', '-', 't', 'e', 's', 't',
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	})

	gen, err := rdd.New(rdd.Config{
		Redis:   rdb,
		Queue:   q,
		Crm:     crm,
		Regions: regSet,
		Logger:  zaptest.NewLogger(t),
		Clock:   clk.Now,
		Rand:    rng,
		Metrics: m,
		Limits:  limits,
	})
	require.NoError(t, err)

	return &fixture{
		mr: mr, rdb: rdb, clock: clk, crm: crm, queue: q,
		regions: regSet, gen: gen, reg: reg, metrics: m,
	}
}

// counterValue gathers the registry and sums the named counter
// matching the supplied label match. Returns 0 on a miss so the test
// can assert "still zero" without a separate has-metric check.
func (f *fixture) counterValue(t *testing.T, name string, labelMatch map[string]string) float64 {
	t.Helper()
	mfs, err := f.reg.Gather()
	require.NoError(t, err)
	var sum float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, met := range mf.GetMetric() {
			if !labelsMatch(met.GetLabel(), labelMatch) {
				continue
			}
			if c := met.GetCounter(); c != nil {
				sum += c.GetValue()
			}
		}
	}
	return sum
}

func labelsMatch(have []*dto.LabelPair, want map[string]string) bool {
	if want == nil {
		return true
	}
	got := make(map[string]string, len(have))
	for _, lp := range have {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// TestNew_RequiredDeps — every required field surfaces a clean error
// when missing.
func TestNew_RequiredDeps(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	regSet, err := regions.Load()
	require.NoError(t, err)
	q, err := queue.New(queue.Config{Redis: rdb})
	require.NoError(t, err)
	crm := newFakeCRM()

	cases := []struct {
		name string
		cfg  rdd.Config
		want string
	}{
		{"missing redis", rdd.Config{Queue: q, Crm: crm, Regions: regSet}, "Redis"},
		{"missing queue", rdd.Config{Redis: rdb, Crm: crm, Regions: regSet}, "Queue"},
		{"missing crm", rdd.Config{Redis: rdb, Queue: q, Regions: regSet}, "Crm"},
		{"missing regions", rdd.Config{Redis: rdb, Queue: q, Crm: crm}, "Regions"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := rdd.New(c.cfg)
			require.Error(t, err)
			require.Contains(t, err.Error(), c.want)
		})
	}
}

// TestGenerate_HappyPath_AllOK — small N, ample bucket capacity,
// every iteration produces a fresh respondent and the queue grows by
// exactly N.
func TestGenerate_HappyPath_AllOK(t *testing.T) {
	t.Parallel()
	f := newFixtureWithLimits(t, rdd.Limits{PerTenantPerSec: 1000})
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	res, err := f.gen.Generate(ctx, api.GenerateRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		N:         50,
		Quotas:    map[string]int{"RU-MOW": 1, "RU-SPE": 1},
		ABCRatio:  0,
	})
	require.NoError(t, err)
	require.Equal(t, 50, res.Generated)
	require.Equal(t, 50, f.crm.createdCount())

	// Per-region split is non-empty for both regions (sample size 50 is
	// big enough that both quotas fire at least once with high
	// probability).
	require.NotZero(t, res.ByRegion["RU-MOW"])
	require.NotZero(t, res.ByRegion["RU-SPE"])
	require.Equal(t, res.Generated, res.ByRegion["RU-MOW"]+res.ByRegion["RU-SPE"])

	// Queue depth matches.
	n, err := f.queue.Size(ctx, tenantID, projectID)
	require.NoError(t, err)
	require.Equal(t, int64(50), n)

	// Metrics reflect every successful iteration.
	require.InDelta(t, 50.0, f.counterValue(t, "dialer_rdd_generated_total", map[string]string{"result": "ok"}), 0.001)
}

// TestGenerate_RejectsNilUUIDs — defence-in-depth on the boundary.
func TestGenerate_RejectsNilUUIDs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	_, err := f.gen.Generate(ctx, api.GenerateRequest{
		TenantID:  uuid.Nil,
		ProjectID: uuid.New(),
		N:         5,
		Quotas:    map[string]int{"RU-MOW": 1},
	})
	require.Error(t, err)

	_, err = f.gen.Generate(ctx, api.GenerateRequest{
		TenantID:  uuid.New(),
		ProjectID: uuid.Nil,
		N:         5,
		Quotas:    map[string]int{"RU-MOW": 1},
	})
	require.Error(t, err)
}

// TestGenerate_RejectsEmptyQuotas — at least one positive quota must
// be supplied or Generate refuses to produce phones (no candidate set).
func TestGenerate_RejectsEmptyQuotas(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.gen.Generate(context.Background(), api.GenerateRequest{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		N:         5,
		Quotas:    map[string]int{},
	})
	require.Error(t, err)
}

// TestGenerate_ZeroN — early-return with a clean zero-result.
func TestGenerate_ZeroN(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	res, err := f.gen.Generate(context.Background(), api.GenerateRequest{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		N:         0,
		Quotas:    map[string]int{"RU-MOW": 1},
	})
	require.NoError(t, err)
	require.Equal(t, 0, res.Generated)
	require.False(t, res.Throttled)
}

// TestGenerate_ThrottledSurfacesSentinel — when the leaky bucket
// throttles every iteration, Generate returns api.ErrThrottled (and
// the result struct flags Throttled=true).
func TestGenerate_ThrottledSurfacesSentinel(t *testing.T) {
	t.Parallel()
	// Capacity 1 means we can squeeze exactly one through; with N=5
	// the first iteration consumes the only token, the next four
	// throttle. That still produces Generated=1 ≠ 0 so we instead
	// flush the bucket via a cooperative pre-call.
	f := newFixtureWithLimits(t, rdd.Limits{PerTenantPerSec: 1})
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	// Drain the bucket BEFORE the test call.
	_, err := f.gen.Generate(ctx, api.GenerateRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		N:         1,
		Quotas:    map[string]int{"RU-MOW": 1},
	})
	require.NoError(t, err)

	// Now the bucket is empty (refill rate 1/s, clock frozen). N=1
	// should hit the throttle path immediately and surface
	// api.ErrThrottled.
	res, err := f.gen.Generate(ctx, api.GenerateRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		N:         3,
		Quotas:    map[string]int{"RU-MOW": 1},
	})
	require.ErrorIs(t, err, api.ErrThrottled)
	require.True(t, res.Throttled)
	require.Equal(t, 0, res.Generated)
}

// TestGenerate_DNCBucketed — phones the CRM service rejects with
// ErrPhoneInDNC bucket as DNCHit; the iteration continues.
func TestGenerate_DNCBucketed(t *testing.T) {
	t.Parallel()
	f := newFixtureWithLimits(t, rdd.Limits{PerTenantPerSec: 1000})
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	// Mark every phone we'll ever see as DNC by returning the sentinel
	// for any incoming Create.
	f.crm.mu.Lock()
	f.crm.createErr = crmapi.ErrPhoneInDNC
	f.crm.mu.Unlock()

	res, err := f.gen.Generate(ctx, api.GenerateRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		N:         10,
		Quotas:    map[string]int{"RU-MOW": 1},
	})
	require.NoError(t, err)
	require.Equal(t, 0, res.Generated)
	require.Equal(t, 10, res.DNCHit)
	require.False(t, res.Throttled)
	require.InDelta(t, 10.0, f.counterValue(t, "dialer_rdd_generated_total", map[string]string{"result": "dnc"}), 0.001)
}

// TestGenerate_DuplicateBucketedFromCRM — when CRM returns
// ErrDuplicateRespondent, the iteration buckets as DuplicatesHit.
func TestGenerate_DuplicateBucketedFromCRM(t *testing.T) {
	t.Parallel()
	f := newFixtureWithLimits(t, rdd.Limits{PerTenantPerSec: 1000})
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	f.crm.mu.Lock()
	f.crm.createErr = crmapi.ErrDuplicateRespondent
	f.crm.mu.Unlock()

	res, err := f.gen.Generate(ctx, api.GenerateRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		N:         5,
		Quotas:    map[string]int{"RU-MOW": 1},
	})
	require.NoError(t, err)
	require.Equal(t, 0, res.Generated)
	require.Equal(t, 5, res.DuplicatesHit)
}

// TestGenerate_InvalidPhoneFromCRM — when CRM returns
// ErrInvalidPhone, the iteration buckets as InvalidHit.
func TestGenerate_InvalidPhoneFromCRM(t *testing.T) {
	t.Parallel()
	f := newFixtureWithLimits(t, rdd.Limits{PerTenantPerSec: 1000})
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	f.crm.mu.Lock()
	f.crm.createErr = crmapi.ErrInvalidPhone
	f.crm.mu.Unlock()

	res, err := f.gen.Generate(ctx, api.GenerateRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		N:         5,
		Quotas:    map[string]int{"RU-MOW": 1},
	})
	require.NoError(t, err)
	require.Equal(t, 0, res.Generated)
	require.Equal(t, 5, res.InvalidHit)
}

// TestGenerate_DedupSkipsKnownPhones — pre-marking a phone in the
// dedup tier causes Generate to bucket it as a duplicate. Achieved by
// running Generate twice with the same deterministic seed: the second
// call rolls the same prefix+subscriber as the first and therefore
// hits the dedup pre-filter for every one of its iterations.
func TestGenerate_DedupSkipsKnownPhones(t *testing.T) {
	t.Parallel()
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	clk := &fakeClock{now: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)}
	q, err := queue.New(queue.Config{Redis: rdb, Clock: clk.Now})
	require.NoError(t, err)
	regSet, err := regions.Load()
	require.NoError(t, err)
	crm := newFakeCRM()

	makeGen := func() *rdd.Generator {
		t.Helper()
		//nolint:gosec // non-crypto deterministic seed.
		rng := rand.NewChaCha8([32]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
		g, err := rdd.New(rdd.Config{
			Redis:   rdb,
			Queue:   q,
			Crm:     crm,
			Regions: regSet,
			Clock:   clk.Now,
			Rand:    rng,
			Limits:  rdd.Limits{PerTenantPerSec: 1000},
		})
		require.NoError(t, err)
		return g
	}

	// First run: 20 fresh phones generated.
	g1 := makeGen()
	res, err := g1.Generate(ctx, api.GenerateRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		N:         20,
		Quotas:    map[string]int{"RU-MOW": 1},
	})
	require.NoError(t, err)
	require.Equal(t, 20, res.Generated)

	// Second run with the same seed: every roll lands on a phone the
	// dedup tier already knows. Generated must be 0; DuplicatesHit
	// should equal N. Regional Bloom may FP on a different phone
	// occasionally, but the deterministic seed guarantees the exact
	// same draws.
	g2 := makeGen()
	res, err = g2.Generate(ctx, api.GenerateRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		N:         20,
		Quotas:    map[string]int{"RU-MOW": 1},
	})
	require.NoError(t, err)
	require.Equal(t, 0, res.Generated)
	require.Equal(t, 20, res.DuplicatesHit)
}

// TestGenerate_ABCRatioPasses — the ABCRatio field flows through the
// path without panicking. v1 has every embedded region as DEF-coded so
// the ratio has no observable side-effect on prefix selection; the
// test still exercises the value to guard against a future regression
// that would propagate ABCRatio into a typed branch.
func TestGenerate_ABCRatioPasses(t *testing.T) {
	t.Parallel()
	f := newFixtureWithLimits(t, rdd.Limits{PerTenantPerSec: 1000})
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	for _, r := range []float64{0, 0.5, 1.0} {
		res, err := f.gen.Generate(ctx, api.GenerateRequest{
			TenantID:  tenantID,
			ProjectID: projectID,
			N:         5,
			Quotas:    map[string]int{"RU-SPE": 1},
			ABCRatio:  r,
		})
		require.NoError(t, err)
		require.Equal(t, 5, res.Generated, "abcRatio=%v should not affect generation rate when every prefix is DEF-coded", r)
	}
}

// TestGenerate_QuotaWeightsRespected — feeding a 9:1 weight produces
// an ~9:1 split over a large sample.
func TestGenerate_QuotaWeightsRespected(t *testing.T) {
	t.Parallel()
	f := newFixtureWithLimits(t, rdd.Limits{PerTenantPerSec: 10000})
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	const total = 1000
	res, err := f.gen.Generate(ctx, api.GenerateRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		N:         total,
		Quotas:    map[string]int{"RU-MOW": 9, "RU-SPE": 1},
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, res.Generated, total*8/10) // expect close to total minus rare collisions

	// Allow a 30% deviation to cover ChaCha sampling variance.
	mowShare := float64(res.ByRegion["RU-MOW"]) / float64(res.Generated)
	require.Greater(t, mowShare, 0.7, "9:1 weight must put majority in MOW (got %.2f)", mowShare)
	require.Less(t, mowShare, 1.0, "9:1 weight must leave some share to SPE (got %.2f)", mowShare)
}

// TestGenerate_PropagatesContextCancel — a cancelled context aborts
// the iteration loop.
func TestGenerate_PropagatesContextCancel(t *testing.T) {
	t.Parallel()
	f := newFixtureWithLimits(t, rdd.Limits{PerTenantPerSec: 10000})
	tenantID, projectID := uuid.New(), uuid.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled context

	_, err := f.gen.Generate(ctx, api.GenerateRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		N:         100,
		Quotas:    map[string]int{"RU-MOW": 1},
	})
	require.ErrorIs(t, err, context.Canceled)
}

// TestGenerate_UnknownRegionInQuota — a quota referencing a region
// not in the embedded snapshot buckets every iteration as InvalidHit.
func TestGenerate_UnknownRegionInQuota(t *testing.T) {
	t.Parallel()
	f := newFixtureWithLimits(t, rdd.Limits{PerTenantPerSec: 1000})
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	res, err := f.gen.Generate(ctx, api.GenerateRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		N:         5,
		Quotas:    map[string]int{"RU-NOPE": 1},
	})
	require.NoError(t, err)
	require.Equal(t, 0, res.Generated)
	require.Equal(t, 5, res.InvalidHit)
}

// TestGenerate_NilMetrics — a Generator built without Metrics is
// fully functional. Every observe* path is nil-tolerant.
func TestGenerate_NilMetrics(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	clk := &fakeClock{now: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)}
	q, err := queue.New(queue.Config{Redis: rdb, Clock: clk.Now})
	require.NoError(t, err)
	regSet, err := regions.Load()
	require.NoError(t, err)

	g, err := rdd.New(rdd.Config{
		Redis:   rdb,
		Queue:   q,
		Crm:     newFakeCRM(),
		Regions: regSet,
		Clock:   clk.Now,
		Limits:  rdd.Limits{PerTenantPerSec: 1000},
	})
	require.NoError(t, err)
	res, err := g.Generate(context.Background(), api.GenerateRequest{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		N:         5,
		Quotas:    map[string]int{"RU-MOW": 1},
	})
	require.NoError(t, err)
	require.Equal(t, 5, res.Generated)
}

// TestGenerate_PartialThrottle — when the bucket holds K tokens and
// N > K, the first K iterations succeed and the rest mark Throttled
// (no error since Generated > 0).
func TestGenerate_PartialThrottle(t *testing.T) {
	t.Parallel()
	f := newFixtureWithLimits(t, rdd.Limits{PerTenantPerSec: 3})
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	res, err := f.gen.Generate(ctx, api.GenerateRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		N:         10,
		Quotas:    map[string]int{"RU-MOW": 1},
	})
	require.NoError(t, err, "partial throttle must not surface as an error when Generated > 0")
	require.True(t, res.Throttled)
	require.Equal(t, 3, res.Generated)
}

// TestRegisterMetrics_NilPanics — wiring error: passing nil to
// RegisterMetrics fails fast with a remediation message.
func TestRegisterMetrics_NilPanics(t *testing.T) {
	t.Parallel()
	require.PanicsWithValue(t,
		"rdd.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests",
		func() { rdd.RegisterMetrics(nil) })
}

// BenchmarkRDDGenerate10k drives 10 000 generations against
// miniredis. Skipped under -short (CI uses -short on the unit tier).
//
// The Plan 10 DoD calls for "<1s on a developer laptop" — we actually
// run miniredis here, so this benchmark is closer to a coverage probe
// than a tight latency target. The integration tier (real Redis 7.4)
// is the authoritative number.
func BenchmarkRDDGenerate10k(b *testing.B) {
	if testing.Short() {
		b.Skip("benchmark skipped under -short")
	}
	mr, err := miniredis.Run()
	if err != nil {
		b.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()
	clk := &fakeClock{now: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)}
	q, err := queue.New(queue.Config{Redis: rdb, Clock: clk.Now})
	if err != nil {
		b.Fatal(err)
	}
	regSet, err := regions.Load()
	if err != nil {
		b.Fatal(err)
	}
	gen, err := rdd.New(rdd.Config{
		Redis:   rdb,
		Queue:   q,
		Crm:     newFakeCRM(),
		Regions: regSet,
		Clock:   clk.Now,
		Limits:  rdd.Limits{PerTenantPerSec: 1_000_000},
	})
	if err != nil {
		b.Fatal(err)
	}
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		_, err := gen.Generate(ctx, api.GenerateRequest{
			TenantID:  tenantID,
			ProjectID: projectID,
			N:         10_000,
			Quotas:    map[string]int{"RU-MOW": 5, "RU-SPE": 3, "RU-NVS": 2},
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
