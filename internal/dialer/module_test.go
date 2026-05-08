package dialer_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/dialer"
	"github.com/sociopulse/platform/internal/modules"
	"github.com/sociopulse/platform/internal/telephony"
	"github.com/sociopulse/platform/pkg/postgres"
)

// TestModuleNameStable: the locator key prefix is owned here; a
// regression that renames the module also renames every locator
// lookup.
func TestModuleNameStable(t *testing.T) {
	t.Parallel()
	m := &dialer.Module{}
	assert.Equal(t, "dialer", m.Name())
}

// TestModuleRegisterRequireDeps documents which fields Register
// rejects as missing.
func TestModuleRegisterRequireDeps(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	pool := &postgres.Pool{} // non-nil but uninitialised — Register accepts it; only Pool's runtime methods would fail
	logger := zaptest.NewLogger(t)
	loc := modules.NewMapLocator()
	ctx := t.Context()

	t.Run("missing logger", func(t *testing.T) {
		t.Parallel()
		m := &dialer.Module{}
		//nolint:contextcheck // Deps.Ctx is the Module API; not a func arg
		err := m.Register(modules.Deps{
			Ctx: ctx, Pool: pool, Redis: rdb, Locator: loc,
		})
		require.ErrorContains(t, err, "Logger")
	})
	t.Run("missing pool", func(t *testing.T) {
		t.Parallel()
		m := &dialer.Module{}
		//nolint:contextcheck // Deps.Ctx is the Module API; not a func arg
		err := m.Register(modules.Deps{
			Ctx: ctx, Logger: logger, Redis: rdb, Locator: loc,
		})
		require.ErrorContains(t, err, "Pool")
	})
	t.Run("missing redis", func(t *testing.T) {
		t.Parallel()
		m := &dialer.Module{}
		//nolint:contextcheck // Deps.Ctx is the Module API; not a func arg
		err := m.Register(modules.Deps{
			Ctx: ctx, Logger: logger, Pool: pool, Locator: loc,
		})
		require.ErrorContains(t, err, "Redis")
	})
	t.Run("missing locator", func(t *testing.T) {
		t.Parallel()
		m := &dialer.Module{}
		//nolint:contextcheck // Deps.Ctx is the Module API; not a func arg
		err := m.Register(modules.Deps{
			Ctx: ctx, Logger: logger, Pool: pool, Redis: rdb,
		})
		require.ErrorContains(t, err, "Locator")
	})
}

// TestModuleRegisterHappyPathRegistersLocatorEntries verifies the
// composition root wires every dialer service the locator key
// promises. We use miniredis so the FSM/queue/heartbeat have a real
// Redis to talk to (the constructors all accept *redis.Client).
//
// Pool is left as the zero-value postgres.Pool — Register only stores
// the pointer; nothing in Register itself dereferences PG (the FSM /
// retry orchestrator only touch the pool when their hot paths run).
func TestModuleRegisterHappyPathRegistersLocatorEntries(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	pool := &postgres.Pool{}
	logger := zaptest.NewLogger(t)
	loc := modules.NewMapLocator()

	// Register the telephony stub publisher first so the dialer's
	// router construction succeeds (matches cmd/api boot order).
	require.NoError(t, telephony.Module{}.Register(modules.Deps{
		Logger:  logger,
		Locator: loc,
	}))

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	m := &dialer.Module{}
	err := m.Register(modules.Deps{
		Ctx:     ctx,
		Logger:  logger,
		Pool:    pool,
		Redis:   rdb,
		Locator: loc,
	})
	require.NoError(t, err)

	// Stop must be safe to call; idempotent across multiple invocations.
	t.Cleanup(func() {
		require.NoError(t, m.Stop())
		require.NoError(t, m.Stop())
	})

	// Locator entries — every dialer service we promise.
	for _, key := range []string{
		dialer.LocatorOperatorFSM,
		dialer.LocatorCallQueue,
		dialer.LocatorRouter,
		dialer.LocatorLineCapacityTracker,
		dialer.LocatorWorkingHoursChecker,
		dialer.LocatorRetryOrchestrator,
		dialer.LocatorSnapshotPubSub,
	} {
		_, ok := loc.Lookup(key)
		assert.Truef(t, ok, "locator missing %s", key)
	}
}

// TestModuleStopIsIdempotent: Stop on a never-Registered module is a
// no-op; multiple Stops are safe.
func TestModuleStopIsIdempotent(t *testing.T) {
	t.Parallel()
	m := &dialer.Module{}
	require.NoError(t, m.Stop())
	require.NoError(t, m.Stop())
}

// TestModuleRegisterRejectsClusterClient verifies the type-assert
// error path: a UniversalClient that's not a *redis.Client (e.g. a
// ClusterClient) is rejected at Register time so the Lua scripts in
// the FSM don't blow up later.
func TestModuleRegisterRejectsClusterClient(t *testing.T) {
	t.Parallel()
	logger := zaptest.NewLogger(t)
	loc := modules.NewMapLocator()
	pool := &postgres.Pool{}

	cluster := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs: []string{"127.0.0.1:1", "127.0.0.1:2"},
	})
	t.Cleanup(func() { _ = cluster.Close() })

	m := &dialer.Module{}
	err := m.Register(modules.Deps{
		Ctx:     t.Context(),
		Logger:  logger,
		Pool:    pool,
		Redis:   cluster,
		Locator: loc,
	})
	require.ErrorContains(t, err, "*redis.Client")
}

// TestModuleHeartbeatStopsBeforeStopReturns ensures Stop drains the
// heartbeat goroutine — a stuck watchdog would otherwise be caught
// by goleak in TestMain.
func TestModuleHeartbeatStopsBeforeStopReturns(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	pool := &postgres.Pool{}
	logger := zaptest.NewLogger(t)
	loc := modules.NewMapLocator()

	require.NoError(t, telephony.Module{}.Register(modules.Deps{
		Logger:  logger,
		Locator: loc,
	}))

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	m := &dialer.Module{}
	require.NoError(t, m.Register(modules.Deps{
		Ctx:     ctx,
		Logger:  logger,
		Pool:    pool,
		Redis:   rdb,
		Locator: loc,
	}))

	// Stop must return promptly even though the watchdog's interval
	// is the production default (30s) — we cancel its ctx and wait.
	stopDone := make(chan struct{})
	go func() {
		_ = m.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Module.Stop blocked > 2s")
	}
}
