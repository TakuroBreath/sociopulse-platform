//go:build integration

// tracker_integration_test.go drives the dialer capacity Tracker
// against a REAL Plan 09 *router.Backpressure backed by Redis 7.4. The
// unit tests in tracker_test.go exercise the round-robin walk and
// metric paths against an in-memory fakeBackpressure; this file proves
// the production wiring (real Redis Lua INCR-with-cap) honours the
// same contract the dialer expects.
//
// Build tag `integration` keeps the testcontainer overhead out of the
// default test run; CI invokes `go test -tags=integration ./...` for
// the integration target.
package capacity_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/capacity"
	telephonypool "github.com/sociopulse/platform/internal/telephony/pool"
	telephonyrouter "github.com/sociopulse/platform/internal/telephony/router"
)

// Compile-time wiring assertions. The dialer capacity package
// declares small interfaces (capacity.Pool, capacity.Bp) so unit tests
// don't drag the full telephony surface in. Production composition
// passes the concrete *telephonypool.ESLPool and *telephonyrouter.
// Backpressure types — these assertions guarantee that contract is
// honoured. Asserted in the integration test (rather than a
// production-side file) so the dialer/capacity prod build stays free
// of any telephony-tree dependency.
var (
	_ capacity.Pool = (*telephonypool.ESLPool)(nil)
	_ capacity.Bp   = (*telephonyrouter.Backpressure)(nil)
)

// TestMain runs goleak.VerifyTestMain across the integration suite so
// any goroutine spawned by go-redis or testcontainers is detected at
// exit.
//
// NOTE: tracker_test.go also declares TestMain; Go disallows two
// TestMain in one package. The build tag `integration` on this file
// ensures tracker_test.go's TestMain runs in the unit build and this
// one runs in the integration build.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// startRedis boots Redis 7.4 in a container and returns its host:port.
// Cleanup is registered via t.Cleanup; Terminate runs against
// context.Background so a test cancelled mid-flight still reaps the
// container.
func startRedis(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image:        "redis:7.4-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })

	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "6379/tcp")
	require.NoError(t, err)
	return host + ":" + port.Port()
}

// staticPool is a non-mutating capacity.Pool implementation for
// integration tests. The healthy-node set is fixed for the test's
// lifetime; production *pool.ESLPool drives the dynamic case.
type staticPool struct{ nodes []string }

func (s *staticPool) HealthyNodes() []string {
	out := make([]string, len(s.nodes))
	copy(out, s.nodes)
	return out
}

// integrationFixture wires real Redis + real *router.Backpressure +
// dialer capacity.Tracker.
type integrationFixture struct {
	rdb     *redis.Client
	bp      *telephonyrouter.Backpressure
	tracker *capacity.Tracker
	pool    *staticPool
	cap     int
}

func newIntegrationFixture(t *testing.T, nodes []string, cap int) *integrationFixture {
	t.Helper()
	addr := startRedis(t)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })

	bp := telephonyrouter.NewBackpressure(rdb, cap)
	pool := &staticPool{nodes: nodes}
	tracker, err := capacity.New(capacity.Config{
		Pool:         pool,
		Backpressure: bp,
		Logger:       zaptest.NewLogger(t),
		Metrics:      capacity.RegisterMetrics(prometheus.NewRegistry()),
	})
	require.NoError(t, err)
	return &integrationFixture{
		rdb:     rdb,
		bp:      bp,
		tracker: tracker,
		pool:    pool,
		cap:     cap,
	}
}

// TestIntegration_AcquireRespectsCap verifies the canonical
// INCR-with-cap semantics through the dialer Tracker: cap=2, three
// concurrent Acquires on a single-node fleet, exactly TWO succeed; the
// third returns api.ErrAllNodesFull. Then Release one slot;
// subsequent Acquire succeeds.
func TestIntegration_AcquireRespectsCap(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	const cap = 2
	const node = "fs1.example.com:8021"
	f := newIntegrationFixture(t, []string{node}, cap)

	ctx := context.Background()

	// Race three Acquire calls; record per-call results.
	type result struct {
		node string
		err  error
	}
	const racers = 3
	results := make([]result, racers)
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := range racers {
		go func() {
			defer wg.Done()
			n, err := f.tracker.Acquire(ctx)
			results[i] = result{node: n, err: err}
		}()
	}
	wg.Wait()

	var ok, full int
	for _, r := range results {
		switch {
		case r.err == nil:
			ok++
			require.Equal(t, node, r.node)
		case r.err != nil:
			require.ErrorIs(t, r.err, api.ErrAllNodesFull)
			full++
		}
	}
	require.Equal(t, cap, ok, "exactly cap=2 Acquires must succeed under contention")
	require.Equal(t, racers-cap, full, "exactly one Acquire must surface ErrAllNodesFull")

	// The Redis counter sits at the cap.
	got, err := f.bp.Get(ctx, node)
	require.NoError(t, err)
	require.Equal(t, cap, got)

	// One more Acquire on this saturated single-node fleet → still full.
	n, err := f.tracker.Acquire(ctx)
	require.ErrorIs(t, err, api.ErrAllNodesFull)
	require.Empty(t, n)

	// Release one slot.
	require.NoError(t, f.tracker.Release(ctx, node))
	got, err = f.bp.Get(ctx, node)
	require.NoError(t, err)
	require.Equal(t, cap-1, got)

	// Now another Acquire succeeds.
	n, err = f.tracker.Acquire(ctx)
	require.NoError(t, err)
	require.Equal(t, node, n)
	got, err = f.bp.Get(ctx, node)
	require.NoError(t, err)
	require.Equal(t, cap, got)
}

// TestIntegration_StatsReflectsRealCounter — Acquire / Release through
// the dialer tracker mutate the SAME op:active_channels:{node} key the
// telephony bridge writes. Stats reads it back and matches.
func TestIntegration_StatsReflectsRealCounter(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	const cap = 5
	f := newIntegrationFixture(t, []string{"fs1", "fs2"}, cap)
	ctx := context.Background()

	// Acquire 3 on fs1, 1 on fs2.
	got, err := f.bp.TryAcquire(ctx, "fs1")
	require.NoError(t, err)
	require.True(t, got)
	got, err = f.bp.TryAcquire(ctx, "fs1")
	require.NoError(t, err)
	require.True(t, got)
	got, err = f.bp.TryAcquire(ctx, "fs1")
	require.NoError(t, err)
	require.True(t, got)
	got, err = f.bp.TryAcquire(ctx, "fs2")
	require.NoError(t, err)
	require.True(t, got)

	stats, err := f.tracker.Stats(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), stats["fs1"])
	require.Equal(t, int64(1), stats["fs2"])

	// Release one on fs1; Stats reflects the decrement.
	require.NoError(t, f.tracker.Release(ctx, "fs1"))
	stats, err = f.tracker.Stats(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), stats["fs1"])
	require.Equal(t, int64(1), stats["fs2"])
}

// TestIntegration_RoundRobinDistributesUnderContention — fleet of two
// nodes, cap=4 each; race 8 Acquires; the round-robin walk
// distributes 4 + 4 (NOT 8 + 0). The exact split varies under racing
// goroutines but the per-node count is bounded by cap, so saturation
// of one node forces the other to absorb the rest.
func TestIntegration_RoundRobinDistributesUnderContention(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	const cap = 4
	f := newIntegrationFixture(t, []string{"a", "b"}, cap)
	ctx := context.Background()

	const racers = 8
	var wg sync.WaitGroup
	wg.Add(racers)
	var okCount int64
	for range racers {
		go func() {
			defer wg.Done()
			_, err := f.tracker.Acquire(ctx)
			if err == nil {
				atomic.AddInt64(&okCount, 1)
			}
		}()
	}
	wg.Wait()

	require.Equal(t, int64(racers), atomic.LoadInt64(&okCount),
		"with total cap = 2*4 = 8 == racers, every Acquire must succeed")

	// The two per-node counters sum to 8 and neither exceeds cap.
	a, err := f.bp.Get(ctx, "a")
	require.NoError(t, err)
	b, err := f.bp.Get(ctx, "b")
	require.NoError(t, err)
	require.LessOrEqual(t, a, cap)
	require.LessOrEqual(t, b, cap)
	require.Equal(t, racers, a+b)
}
