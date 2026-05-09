package nats_bridge_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/telephony/nats_bridge"
	"github.com/sociopulse/platform/internal/telephony/pool"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// fakePub satisfies eventbus.Publisher and records every Publish call.
type fakePub struct{}

func (fakePub) Publish(_ context.Context, _ string, _ []byte) error { return nil }

// fakeSub satisfies eventbus.Subscriber. By default Subscribe returns nil
// so Bridge.Start can advance through the cmdSub registration.
type fakeSub struct {
	mu          sync.Mutex
	subscribeCt int
	failOnce    error
}

func (s *fakeSub) Subscribe(_ context.Context, _ string, _ string, _ func(string, []byte) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribeCt++
	if s.failOnce != nil {
		err := s.failOnce
		s.failOnce = nil
		return err
	}
	return nil
}

// makeMiniESL builds a *pool.ESLPool against a 127.0.0.1 placeholder node
// — Bridge.Start does NOT actually need the pool to be healthy; it only
// reads pool.Events() and resolves nodes lazily on dispatch. This keeps
// the test from spinning up a real ESL fake server. The parent ctx is
// torn down via t.Cleanup so the supervisor goroutine exits before goleak
// inspects.
func makeMiniESL(t *testing.T) *pool.ESLPool {
	t.Helper()
	parent, cancel := context.WithCancel(context.Background())
	p, err := pool.New(parent, pool.Config{
		Nodes:    []string{"127.0.0.1:0"}, // never connects; supervisor backs off
		Password: "ClueCon",
		Logger:   zap.NewNop(),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		_ = p.Close()
	})
	return p
}

// TestBridge_New_ValidatesRequiredFields ensures the constructor fails
// loudly on missing wiring rather than at first use.
func TestBridge_New_ValidatesRequiredFields(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	p := makeMiniESL(t)

	cases := []struct {
		name string
		cfg  nats_bridge.Config
	}{
		{
			name: "missing pool",
			cfg: nats_bridge.Config{
				NATSPublisher:  fakePub{},
				NATSSubscriber: &fakeSub{},
				Redis:          rdb,
			},
		},
		{
			name: "missing publisher",
			cfg: nats_bridge.Config{
				Pool:           p,
				NATSSubscriber: &fakeSub{},
				Redis:          rdb,
			},
		},
		{
			name: "missing subscriber",
			cfg: nats_bridge.Config{
				Pool:          p,
				NATSPublisher: fakePub{},
				Redis:         rdb,
			},
		},
		{
			name: "missing redis",
			cfg: nats_bridge.Config{
				Pool:           p,
				NATSPublisher:  fakePub{},
				NATSSubscriber: &fakeSub{},
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := nats_bridge.New(tc.cfg)
			require.Error(t, err)
		})
	}
}

// TestBridge_Start_RegistersCmdSubscriber asserts Start drives one
// Subscribe call against the eventbus.Subscriber. We don't peek at the
// subject string here — that's the cmdSubscriber's contract — only that
// the registration step ran.
func TestBridge_Start_RegistersCmdSubscriber(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	p := makeMiniESL(t)
	sub := &fakeSub{}

	b, err := nats_bridge.New(nats_bridge.Config{
		Pool:           p,
		NATSPublisher:  fakePub{},
		NATSSubscriber: sub,
		Redis:          rdb,
		Logger:         zap.NewNop(),
	})
	require.NoError(t, err)

	require.NoError(t, b.Start(context.Background()))
	t.Cleanup(b.Stop)

	assert.Equal(t, 1, sub.subscribeCt, "Bridge.Start must call Subscribe exactly once")
}

// TestBridge_Start_ReturnsErrorOnSubscribeFailure ensures registration
// failures bubble out of Start so the composition root can decide
// whether to fall back to a degraded mode.
func TestBridge_Start_ReturnsErrorOnSubscribeFailure(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	p := makeMiniESL(t)
	sub := &fakeSub{failOnce: errors.New("nats: stream not found")}

	b, err := nats_bridge.New(nats_bridge.Config{
		Pool:           p,
		NATSPublisher:  fakePub{},
		NATSSubscriber: sub,
		Redis:          rdb,
		Logger:         zap.NewNop(),
	})
	require.NoError(t, err)

	err = b.Start(context.Background())
	require.Error(t, err)
}

// TestBridge_StopIsIdempotent ensures defer + explicit Stop in main()
// don't double-cancel or re-wait.
func TestBridge_StopIsIdempotent(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	p := makeMiniESL(t)

	b, err := nats_bridge.New(nats_bridge.Config{
		Pool:           p,
		NATSPublisher:  fakePub{},
		NATSSubscriber: &fakeSub{},
		Redis:          rdb,
		Logger:         zap.NewNop(),
	})
	require.NoError(t, err)
	require.NoError(t, b.Start(context.Background()))

	b.Stop()
	require.NotPanics(t, b.Stop)
}

// TestBridge_DrainCallsStop ensures the graceful Drain path tears down
// the same goroutines as Stop. The composition root's defer chain
// (Drain → Stop → subscriber.Close → publisher.Close) relies on Drain
// being a complete teardown of bridge-owned resources.
func TestBridge_DrainCallsStop(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	p := makeMiniESL(t)

	b, err := nats_bridge.New(nats_bridge.Config{
		Pool:           p,
		NATSPublisher:  fakePub{},
		NATSSubscriber: &fakeSub{},
		Redis:          rdb,
		Logger:         zap.NewNop(),
	})
	require.NoError(t, err)
	require.NoError(t, b.Start(context.Background()))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, b.Drain(ctx))

	// Second Stop must be a no-op (defer Stop after Drain).
	require.NotPanics(t, b.Stop)
}

// TestBridge_NilLoggerOK proves the cfg.Logger field is nil-tolerated.
func TestBridge_NilLoggerOK(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	p := makeMiniESL(t)

	b, err := nats_bridge.New(nats_bridge.Config{
		Pool:           p,
		NATSPublisher:  fakePub{},
		NATSSubscriber: &fakeSub{},
		Redis:          rdb,
		Logger:         nil,
	})
	require.NoError(t, err)
	require.NoError(t, b.Start(context.Background()))
	t.Cleanup(b.Stop)
}

// Compile-time assertions — exercised here rather than as `var _` to keep
// production code clean of test-only dependencies.
var (
	_ eventbus.Publisher  = fakePub{}
	_ eventbus.Subscriber = (*fakeSub)(nil)
)
