package router_test

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/telephony/api"
	"github.com/sociopulse/platform/internal/telephony/router"
	"github.com/sociopulse/platform/pkg/config"
)

func TestRegisterMetrics_PanicsOnNilRegistry(t *testing.T) {
	t.Parallel()
	require.PanicsWithValue(t,
		"router.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests",
		func() { router.RegisterMetrics(nil) },
	)
}

func TestRegisterMetrics_RegistersCollectorsOnFreshRegistry(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := router.RegisterMetrics(reg)
	require.NotNil(t, m)
	require.NotNil(t, m.SelectsTotal)
	require.NotNil(t, m.SelectDuration)
	require.NotNil(t, m.BackpressureRejects)
	require.NotNil(t, m.Drift)

	// Twice must panic (duplicate registration) — protects boot from a
	// double-wired composition root.
	require.Panics(t, func() { router.RegisterMetrics(reg) })
}

func TestRouter_Select_EmitsOkCounter(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	reg := prometheus.NewRegistry()
	metrics := router.RegisterMetrics(reg)

	trunks := []config.TrunkConfig{
		{ID: "primary", SIPGateway: "gw-primary", CapacityChannels: 60, CostPerMinuteRub: 0.02},
	}
	r, err := router.New(router.Config{
		Pool:            newFakePool("n1"),
		Redis:           rdb,
		Trunks:          trunks,
		BackpressureCap: 5,
		DefaultStrategy: string(api.RouteLeastCost),
		Logger:          zaptest.NewLogger(t),
		Metrics:         metrics,
	})
	require.NoError(t, err)
	t.Cleanup(r.Stop)

	got, err := r.Select(context.Background(), api.SelectRequest{
		TenantID:   uuid.New(),
		OperatorID: uuid.New(),
		Strategy:   api.RouteLeastCost,
	})
	require.NoError(t, err)
	require.Equal(t, "primary", got.TrunkID)

	// SelectsTotal{strategy=least_cost,result=ok} == 1.
	require.InDelta(t, float64(1),
		testutil.ToFloat64(metrics.SelectsTotal.WithLabelValues(string(api.RouteLeastCost), "ok")),
		1e-9,
	)
}

func TestRouter_Select_EmitsNoTrunkCounter(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	reg := prometheus.NewRegistry()
	metrics := router.RegisterMetrics(reg)

	r, err := router.New(router.Config{
		Pool:            newFakePool("n1"),
		Redis:           rdb,
		Trunks:          nil, // empty catalog → no_trunk
		BackpressureCap: 5,
		DefaultStrategy: string(api.RouteLeastCost),
		Logger:          zaptest.NewLogger(t),
		Metrics:         metrics,
	})
	require.NoError(t, err)
	t.Cleanup(r.Stop)

	_, err = r.Select(context.Background(), api.SelectRequest{
		TenantID:   uuid.New(),
		OperatorID: uuid.New(),
		Strategy:   api.RouteLeastCost,
	})
	require.ErrorIs(t, err, router.ErrNoTrunkAvailable)

	require.InDelta(t, float64(1),
		testutil.ToFloat64(metrics.SelectsTotal.WithLabelValues(string(api.RouteLeastCost), "no_trunk")),
		1e-9,
	)
}

func TestRouter_Select_EmitsNoNodeCounter(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	reg := prometheus.NewRegistry()
	metrics := router.RegisterMetrics(reg)

	trunks := []config.TrunkConfig{
		{ID: "primary", SIPGateway: "gw-primary", CapacityChannels: 60, CostPerMinuteRub: 0.02},
	}
	// No healthy nodes — strategy picks a trunk but no node accepts.
	r, err := router.New(router.Config{
		Pool:            newFakePool(),
		Redis:           rdb,
		Trunks:          trunks,
		BackpressureCap: 5,
		DefaultStrategy: string(api.RouteLeastCost),
		Logger:          zaptest.NewLogger(t),
		Metrics:         metrics,
	})
	require.NoError(t, err)
	t.Cleanup(r.Stop)

	_, err = r.Select(context.Background(), api.SelectRequest{
		TenantID:   uuid.New(),
		OperatorID: uuid.New(),
		Strategy:   api.RouteLeastCost,
	})
	require.ErrorIs(t, err, router.ErrNoTrunkAvailable)
	require.InDelta(t, float64(1),
		testutil.ToFloat64(metrics.SelectsTotal.WithLabelValues(string(api.RouteLeastCost), "no_node")),
		1e-9,
	)
}

func TestRouter_Select_EmitsBackpressureRejects(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	reg := prometheus.NewRegistry()
	metrics := router.RegisterMetrics(reg)

	trunks := []config.TrunkConfig{
		{ID: "primary", SIPGateway: "gw-primary", CapacityChannels: 60, CostPerMinuteRub: 0.02},
	}
	r, err := router.New(router.Config{
		Pool:            newFakePool("n1"),
		Redis:           rdb,
		Trunks:          trunks,
		BackpressureCap: 1, // tiny cap so the second Select trips the rejection path
		DefaultStrategy: string(api.RouteLeastCost),
		Logger:          zaptest.NewLogger(t),
		Metrics:         metrics,
	})
	require.NoError(t, err)
	t.Cleanup(r.Stop)
	ctx := context.Background()

	// Fill cap.
	_, err = r.Select(ctx, api.SelectRequest{
		TenantID:   uuid.New(),
		OperatorID: uuid.New(),
		Strategy:   api.RouteLeastCost,
	})
	require.NoError(t, err)

	// Next attempt — backpressure rejects on n1.
	_, err = r.Select(ctx, api.SelectRequest{
		TenantID:   uuid.New(),
		OperatorID: uuid.New(),
		Strategy:   api.RouteLeastCost,
	})
	require.ErrorIs(t, err, router.ErrNoTrunkAvailable)

	require.InDelta(t, float64(1),
		testutil.ToFloat64(metrics.BackpressureRejects.WithLabelValues("n1")),
		1e-9,
	)
}

func TestRouter_Select_DurationHistogramObserves(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	reg := prometheus.NewRegistry()
	metrics := router.RegisterMetrics(reg)

	trunks := []config.TrunkConfig{
		{ID: "primary", SIPGateway: "gw-primary", CapacityChannels: 60, CostPerMinuteRub: 0.02},
	}
	r, err := router.New(router.Config{
		Pool:            newFakePool("n1"),
		Redis:           rdb,
		Trunks:          trunks,
		BackpressureCap: 5,
		DefaultStrategy: string(api.RouteLeastCost),
		Logger:          zaptest.NewLogger(t),
		Metrics:         metrics,
	})
	require.NoError(t, err)
	t.Cleanup(r.Stop)

	_, err = r.Select(context.Background(), api.SelectRequest{
		TenantID:   uuid.New(),
		OperatorID: uuid.New(),
		Strategy:   api.RouteLeastCost,
	})
	require.NoError(t, err)

	// Inspect the histogram via Gather — testutil.CollectAndFormat is the
	// least-fragile way to assert a sample landed.
	got, err := testutil.GatherAndCount(reg, "telephony_router_select_seconds")
	require.NoError(t, err)
	require.Equal(t, 1, got, "exactly one duration sample expected after one Select")
}
