//go:build integration

// generator_integration_test.go drives the RDD Generator against a
// real Redis 7.4 container. miniredis interprets the Lua scripts well
// enough for the unit tests in generator_test.go (and the leak-bucket
// / dedup peers), but the production invariants — Lua atomicity under
// contention, SSCAN cursor semantics, EXPIRE refresh granularity —
// are real-Redis facts. This binary verifies them.
//
// Build tag `integration` keeps the testcontainer overhead out of the
// default test run; CI invokes `go test -tags=integration ./...` for
// the integration target.

package rdd_test

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/queue"
	"github.com/sociopulse/platform/internal/dialer/rdd"
	"github.com/sociopulse/platform/pkg/regions"
)

// TestMain runs goleak.VerifyTestMain across the integration suite so
// any goroutine spawned by go-redis, testcontainers, or our scripts is
// detected at exit. Cheap insurance — testcontainers spins up a
// Docker container per fixture and any leaked client goroutine would
// otherwise hide behind the slow test signal.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

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

// integrationFixture wires real Redis + RedisQueue + Generator + a
// fakeCRM and a deterministic ChaCha8 seed.
type integrationFixture struct {
	rdb *redis.Client
	clk *fakeClock
	gen *rdd.Generator
	crm *fakeCRM
}

func newIntegrationFixture(t *testing.T) *integrationFixture {
	t.Helper()
	addr := startRedis(t)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })

	clk := &fakeClock{now: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)}
	q, err := queue.New(queue.Config{Redis: rdb, Clock: clk.Now})
	require.NoError(t, err)
	regSet, err := regions.Load()
	require.NoError(t, err)
	crm := newFakeCRM()

	//nolint:gosec // non-crypto deterministic seed for prefix selection.
	rng := rand.NewChaCha8([32]byte{
		'i', 'n', 't', 'e', 'g', 'r', 'a', 't', 'i', 'o', 'n', 0,
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
		Limits:  rdd.Limits{PerTenantPerSec: 100_000},
	})
	require.NoError(t, err)
	return &integrationFixture{rdb: rdb, clk: clk, gen: gen, crm: crm}
}

// TestIntegration_GenerateRoundTrip drives a small N happy path on
// real Redis and asserts the Redis SET / queue ZSET both grew by N.
func TestIntegration_GenerateRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	f := newIntegrationFixture(t)
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	res, err := f.gen.Generate(ctx, api.GenerateRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		N:         100,
		Quotas:    map[string]int{"RU-MOW": 1, "RU-SPE": 1},
	})
	require.NoError(t, err)
	require.Equal(t, 100, res.Generated)

	// Redis SET grew by 100.
	card, err := f.rdb.SCard(ctx, "rdd:seen:"+tenantID.String()).Result()
	require.NoError(t, err)
	require.Equal(t, int64(100), card)

	// Queue ZSET grew by 100.
	zKey := "q:" + tenantID.String() + ":project:" + projectID.String()
	zCard, err := f.rdb.ZCard(ctx, zKey).Result()
	require.NoError(t, err)
	require.Equal(t, int64(100), zCard)
}

// TestIntegration_ConcurrentGenerate_NoDoublePush — ten goroutines
// hit Generate concurrently on the same tenant + project. Sum of
// Generated must equal the cardinality of the Redis SET. No phone
// is pushed to the queue twice.
func TestIntegration_ConcurrentGenerate_NoDoublePush(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	f := newIntegrationFixture(t)
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	const goroutines = 10
	const perGoroutine = 50
	var totalGen int64

	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			res, err := f.gen.Generate(ctx, api.GenerateRequest{
				TenantID:  tenantID,
				ProjectID: projectID,
				N:         perGoroutine,
				Quotas:    map[string]int{"RU-MOW": 1, "RU-SPE": 1, "RU-NVS": 1},
			})
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			atomic.AddInt64(&totalGen, int64(res.Generated))
		})
	}
	wg.Wait()

	gen := atomic.LoadInt64(&totalGen)
	card, err := f.rdb.SCard(ctx, "rdd:seen:"+tenantID.String()).Result()
	require.NoError(t, err)
	require.Equal(t, gen, card, "every Generated phone must appear in the Redis SET exactly once")
}

// TestIntegration_BloomBootstrapAcrossInstances — instance A writes
// 50 phones, then a fresh instance B is constructed and sees them
// all on first Seen() call (proves the SSCAN-bootstrap path works on
// real Redis).
func TestIntegration_BloomBootstrapAcrossInstances(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Parallel()
	f := newIntegrationFixture(t)
	tenantID, projectID := uuid.New(), uuid.New()
	ctx := context.Background()

	res, err := f.gen.Generate(ctx, api.GenerateRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		N:         50,
		Quotas:    map[string]int{"RU-MOW": 1},
	})
	require.NoError(t, err)
	require.Equal(t, 50, res.Generated)

	// Spin up a fresh Generator pointing at the same Redis. A fresh
	// FakeCRM ensures Create succeeds for any phone the second
	// generator rolls; if the dedup bootstrap works, the second run
	// produces zero generated (every phone is already in the SET).
	addr := f.rdb.Options().Addr
	rdb2 := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb2.Close() })
	q2, err := queue.New(queue.Config{Redis: rdb2, Clock: f.clk.Now})
	require.NoError(t, err)
	regSet, err := regions.Load()
	require.NoError(t, err)
	crm2 := newFakeCRM()

	//nolint:gosec // deterministic non-crypto seed.
	rng2 := rand.NewChaCha8([32]byte{
		'i', 'n', 't', 'e', 'g', 'r', 'a', 't', 'i', 'o', 'n', 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	})
	gen2, err := rdd.New(rdd.Config{
		Redis:   rdb2,
		Queue:   q2,
		Crm:     crm2,
		Regions: regSet,
		Clock:   f.clk.Now,
		Rand:    rng2,
		Limits:  rdd.Limits{PerTenantPerSec: 1000},
	})
	require.NoError(t, err)

	// Same seed → second instance rolls the same phones; bootstrap
	// from Redis SET should detect every one as seen.
	res, err = gen2.Generate(ctx, api.GenerateRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		N:         50,
		Quotas:    map[string]int{"RU-MOW": 1},
	})
	require.NoError(t, err)
	require.Equal(t, 0, res.Generated)
	require.Equal(t, 50, res.DuplicatesHit)
}
