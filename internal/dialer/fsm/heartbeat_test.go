package fsm_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/fsm"
)

// fakeOperatorFSM is a minimal api.OperatorFSM stub for the watchdog
// tests. We only need Force; every other method panics so a test that
// accidentally invokes them surfaces immediately.
type fakeOperatorFSM struct {
	mu     sync.Mutex
	forces []forceCall
	err    error
}

type forceCall struct {
	tenantID   uuid.UUID
	operatorID uuid.UUID
	target     api.State
	reason     api.ForceReason
}

func (f *fakeOperatorFSM) Force(_ context.Context, tenantID, operatorID uuid.UUID, target api.State, reason api.ForceReason) (api.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forces = append(f.forces, forceCall{tenantID: tenantID, operatorID: operatorID, target: target, reason: reason})
	if f.err != nil {
		return api.Snapshot{}, f.err
	}
	return api.Snapshot{TenantID: tenantID, OperatorID: operatorID, State: target}, nil
}

func (f *fakeOperatorFSM) snapshotForces() []forceCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]forceCall, len(f.forces))
	copy(out, f.forces)
	return out
}

// All other api.OperatorFSM methods panic in tests — we wire the
// watchdog with this fake and only its Force path is exercised.
func (*fakeOperatorFSM) StartShift(context.Context, api.StartShiftRequest) (api.Snapshot, error) {
	panic("unexpected: heartbeat watchdog must not call StartShift")
}
func (*fakeOperatorFSM) EndShift(context.Context, uuid.UUID, uuid.UUID) (api.Snapshot, error) {
	panic("unexpected: heartbeat watchdog must not call EndShift")
}
func (*fakeOperatorFSM) GoReady(context.Context, uuid.UUID, uuid.UUID) (api.Snapshot, error) {
	panic("unexpected: heartbeat watchdog must not call GoReady")
}
func (*fakeOperatorFSM) GoPause(context.Context, api.GoPauseRequest) (api.Snapshot, error) {
	panic("unexpected: heartbeat watchdog must not call GoPause")
}
func (*fakeOperatorFSM) Resume(context.Context, uuid.UUID, uuid.UUID) (api.Snapshot, error) {
	panic("unexpected: heartbeat watchdog must not call Resume")
}
func (*fakeOperatorFSM) RecordCallStarted(context.Context, api.CallStartedRequest) (api.Snapshot, error) {
	panic("unexpected")
}
func (*fakeOperatorFSM) RecordCallEnded(context.Context, api.CallEndedRequest) (api.Snapshot, error) {
	panic("unexpected")
}
func (*fakeOperatorFSM) SubmitStatus(context.Context, api.SubmitStatusRequest) (api.Snapshot, error) {
	panic("unexpected")
}
func (*fakeOperatorFSM) GoVerify(context.Context, uuid.UUID, uuid.UUID) (api.Snapshot, error) {
	panic("unexpected")
}
func (*fakeOperatorFSM) VerifyDone(context.Context, uuid.UUID, uuid.UUID) (api.Snapshot, error) {
	panic("unexpected")
}
func (*fakeOperatorFSM) GetState(context.Context, uuid.UUID, uuid.UUID) (api.Snapshot, error) {
	panic("unexpected")
}

// newHeartbeatFixture builds a watchdog wired to miniredis + a fake
// FSM. miniredis is sufficient for the watchdog unit tests because
// the SCAN / HGET / EXISTS surface is straightforward; the more
// invasive Lua / TTL behaviour exercised by the FSM lives elsewhere.
type heartbeatFixture struct {
	mr      *miniredis.Miniredis
	rdb     *redis.Client
	fsm     *fakeOperatorFSM
	metrics *fsm.HeartbeatMetrics
	hb      *fsm.Heartbeat
	reg     *prometheus.Registry
}

func newHeartbeatFixture(t *testing.T, opts ...func(*fsm.HeartbeatConfig)) *heartbeatFixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	stub := &fakeOperatorFSM{}
	reg := prometheus.NewRegistry()
	metrics := fsm.RegisterHeartbeatMetrics(reg)

	cfg := fsm.HeartbeatConfig{
		Redis:    rdb,
		FSM:      stub,
		Logger:   zaptest.NewLogger(t),
		Metrics:  metrics,
		Interval: 50 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	hb, err := fsm.NewHeartbeat(cfg)
	require.NoError(t, err)
	return &heartbeatFixture{
		mr:      mr,
		rdb:     rdb,
		fsm:     stub,
		metrics: metrics,
		hb:      hb,
		reg:     reg,
	}
}

// seedOperator writes the canonical op:<t>:user:<o> hash with state.
// Optionally writes a presence:<t>:user:<o> key with the supplied TTL.
func (f *heartbeatFixture) seedOperator(t *testing.T, tenantID, operatorID uuid.UUID, state api.State, presence time.Duration) {
	t.Helper()
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()
	require.NoError(t, f.rdb.HSet(t.Context(), key, map[string]any{
		"state":            string(state),
		"state_entered_at": time.Now().UTC().Format(time.RFC3339Nano),
		"heartbeat_at":     time.Now().UTC().Format(time.RFC3339Nano),
		"tenant_id":        tenantID.String(),
		"version":          "1",
	}).Err())
	if presence > 0 {
		require.NoError(t, fsm.RefreshPresence(t.Context(), f.rdb, tenantID, operatorID, presence))
	}
}

// TestHeartbeatNewValidatesDeps asserts the constructor rejects
// missing required fields.
func TestHeartbeatNewValidatesDeps(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	stub := &fakeOperatorFSM{}

	t.Run("missing redis", func(t *testing.T) {
		t.Parallel()
		_, err := fsm.NewHeartbeat(fsm.HeartbeatConfig{FSM: stub})
		require.Error(t, err)
	})
	t.Run("missing fsm", func(t *testing.T) {
		t.Parallel()
		_, err := fsm.NewHeartbeat(fsm.HeartbeatConfig{Redis: rdb})
		require.Error(t, err)
	})
	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		hb, err := fsm.NewHeartbeat(fsm.HeartbeatConfig{Redis: rdb, FSM: stub})
		require.NoError(t, err)
		require.NotNil(t, hb)
	})
}

// TestHeartbeatForcesOfflineOnExpiredPresence is the key behavioural
// test: an operator with state=ready and a missing presence key gets
// a Force(target=offline, reason=heartbeat_lost).
func TestHeartbeatForcesOfflineOnExpiredPresence(t *testing.T) {
	t.Parallel()

	f := newHeartbeatFixture(t)
	tenantID := uuid.New()
	operatorID := uuid.New()
	// Seed without a presence key — the watchdog must Force.
	f.seedOperator(t, tenantID, operatorID, api.StateReady, 0)

	f.hb.Sweep(t.Context())

	calls := f.fsm.snapshotForces()
	require.Len(t, calls, 1)
	assert.Equal(t, tenantID, calls[0].tenantID)
	assert.Equal(t, operatorID, calls[0].operatorID)
	assert.Equal(t, api.StateOffline, calls[0].target)
	assert.Equal(t, api.ForceReasonHeartbeatLost, calls[0].reason)

	assert.InDelta(t, float64(1), testutil.ToFloat64(f.metrics.Forced), 0.0001)
}

// TestHeartbeatSkipsAlreadyOfflineOperators ensures the watchdog
// doesn't keep poking offline operators (no-op replays would still
// charge metrics + audit rows in production).
func TestHeartbeatSkipsAlreadyOfflineOperators(t *testing.T) {
	t.Parallel()

	f := newHeartbeatFixture(t)
	tenantID := uuid.New()
	operatorID := uuid.New()
	f.seedOperator(t, tenantID, operatorID, api.StateOffline, 0)

	f.hb.Sweep(t.Context())

	assert.Empty(t, f.fsm.snapshotForces(), "no Force calls expected for already-offline operators")
	assert.InDelta(t, float64(0), testutil.ToFloat64(f.metrics.Forced), 0.0001)
}

// TestHeartbeatSkipsLivePresence: an operator with a fresh presence
// key is left alone.
func TestHeartbeatSkipsLivePresence(t *testing.T) {
	t.Parallel()

	f := newHeartbeatFixture(t)
	tenantID := uuid.New()
	operatorID := uuid.New()
	f.seedOperator(t, tenantID, operatorID, api.StateReady, 30*time.Second)

	f.hb.Sweep(t.Context())

	assert.Empty(t, f.fsm.snapshotForces(), "no Force calls expected when presence key is alive")
}

// TestHeartbeatExpiredPresenceForcesAfterTTLElapsed: write a presence
// key with a short TTL, advance miniredis' clock past the expiry, and
// verify the watchdog forces.
func TestHeartbeatExpiredPresenceForcesAfterTTLElapsed(t *testing.T) {
	t.Parallel()

	f := newHeartbeatFixture(t)
	tenantID := uuid.New()
	operatorID := uuid.New()
	f.seedOperator(t, tenantID, operatorID, api.StateReady, 1*time.Second)

	// Advance miniredis past the presence TTL so EXISTS returns 0.
	f.mr.FastForward(2 * time.Second)

	f.hb.Sweep(t.Context())

	calls := f.fsm.snapshotForces()
	require.Len(t, calls, 1)
	assert.Equal(t, api.StateOffline, calls[0].target)
}

// TestHeartbeatSweepsMultipleOperators: seed several operators with
// different presence states; the sweep forces only those without a
// presence key.
func TestHeartbeatSweepsMultipleOperators(t *testing.T) {
	t.Parallel()

	f := newHeartbeatFixture(t)
	tenantA := uuid.New()
	op1 := uuid.New()
	op2 := uuid.New()
	op3 := uuid.New()

	f.seedOperator(t, tenantA, op1, api.StateReady, 0)              // expired → force
	f.seedOperator(t, tenantA, op2, api.StateReady, 30*time.Second) // alive → keep
	f.seedOperator(t, tenantA, op3, api.StatePause, 0)              // expired → force
	f.seedOperator(t, tenantA, uuid.New(), api.StateOffline, 0)     // offline → keep

	f.hb.Sweep(t.Context())

	calls := f.fsm.snapshotForces()
	require.Len(t, calls, 2)

	got := map[uuid.UUID]bool{}
	for _, c := range calls {
		got[c.operatorID] = true
		assert.Equal(t, api.StateOffline, c.target)
	}
	assert.True(t, got[op1])
	assert.True(t, got[op3])

	assert.InDelta(t, float64(2), testutil.ToFloat64(f.metrics.Forced), 0.0001)
}

// TestHeartbeatRunStopsOnContextCancel verifies Run exits within one
// interval of context cancellation. goleak in main_test.go catches
// any stuck goroutine.
func TestHeartbeatRunStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	f := newHeartbeatFixture(t)
	ctx, cancel := context.WithCancel(t.Context())

	var done sync.WaitGroup
	done.Add(1)
	var runErr atomic.Value
	go func() {
		defer done.Done()
		err := f.hb.Run(ctx)
		if err != nil {
			runErr.Store(err)
		}
	}()

	// Let one sweep happen, then cancel.
	time.Sleep(80 * time.Millisecond)
	cancel()

	completed := make(chan struct{})
	go func() {
		done.Wait()
		close(completed)
	}()
	select {
	case <-completed:
		// Run returns ctx.Err() (Canceled) — that's the expected
		// shutdown signal.
		err, _ := runErr.Load().(error)
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Heartbeat.Run did not stop within 2s of cancel")
	}
}

// TestHeartbeatRunFiresFirstSweepImmediately verifies the boot-time
// sweep so the watchdog isn't idle for an entire interval after start.
func TestHeartbeatRunFiresFirstSweepImmediately(t *testing.T) {
	t.Parallel()

	// Use a long interval so any non-zero forces must come from the
	// boot-time sweep.
	f := newHeartbeatFixture(t, func(c *fsm.HeartbeatConfig) {
		c.Interval = 30 * time.Second
	})
	tenantID := uuid.New()
	operatorID := uuid.New()
	f.seedOperator(t, tenantID, operatorID, api.StateReady, 0)

	ctx, cancel := context.WithTimeout(t.Context(), 1*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.hb.Run(ctx)
	}()

	// Poll until the force lands or the deadline fires.
	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(f.fsm.snapshotForces()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	assert.NotEmpty(t, f.fsm.snapshotForces(), "expected Run to fire a sweep before the first ticker")
}

// TestHeartbeatIgnoresUnrecognisedKeys ensures the watchdog leaves
// unrelated keys (queue ZSETs, presence keys themselves, future
// schemas) alone.
func TestHeartbeatIgnoresUnrecognisedKeys(t *testing.T) {
	t.Parallel()

	f := newHeartbeatFixture(t)
	require.NoError(t, f.rdb.Set(t.Context(), "qd:abc", "1", 0).Err())
	require.NoError(t, f.rdb.Set(t.Context(), "presence:not-uuid:user:not-uuid", "1", time.Minute).Err())
	require.NoError(t, f.rdb.HSet(t.Context(), "op:not-uuid:user:not-uuid", "state", "ready").Err())

	f.hb.Sweep(t.Context())

	assert.Empty(t, f.fsm.snapshotForces())
}

// TestRefreshPresenceSetsKey: helper round-trip — RefreshPresence
// writes the key with the requested TTL and the watchdog observes it.
func TestRefreshPresenceSetsKey(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tenantID := uuid.New()
	operatorID := uuid.New()
	require.NoError(t, fsm.RefreshPresence(t.Context(), rdb, tenantID, operatorID, 5*time.Second))

	exists, err := rdb.Exists(t.Context(), fsm.PresenceKey(tenantID, operatorID)).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), exists)
}

// TestRefreshPresenceRequiresClient: nil rdb returns an error.
func TestRefreshPresenceRequiresClient(t *testing.T) {
	t.Parallel()
	err := fsm.RefreshPresence(t.Context(), nil, uuid.New(), uuid.New(), time.Minute)
	require.Error(t, err)
}

// TestRefreshPresenceDefaultTTL: a zero TTL falls back to the package
// default (30s) — verified by reading TTL back via the Redis client.
func TestRefreshPresenceDefaultTTL(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tenantID := uuid.New()
	operatorID := uuid.New()
	require.NoError(t, fsm.RefreshPresence(t.Context(), rdb, tenantID, operatorID, 0))

	ttl, err := rdb.TTL(t.Context(), fsm.PresenceKey(tenantID, operatorID)).Result()
	require.NoError(t, err)
	// miniredis honours TTL; expect the default 30s.
	assert.InDelta(t, 30*time.Second, ttl, float64(time.Second), "expected default presence TTL ≈ 30s, got %s", ttl)
}

// TestHeartbeatSkipsCorruptStateRow: a hash with a non-recognised
// state value is logged + skipped. We use HSET to write a garbage
// value; the watchdog must NOT Force on it (don't trust corrupt rows).
func TestHeartbeatSkipsCorruptStateRow(t *testing.T) {
	t.Parallel()

	f := newHeartbeatFixture(t)
	tenantID := uuid.New()
	operatorID := uuid.New()
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()
	require.NoError(t, f.rdb.HSet(t.Context(), key, "state", "garbage").Err())

	f.hb.Sweep(t.Context())
	assert.Empty(t, f.fsm.snapshotForces(), "no Force calls on corrupt state rows")
}

// TestHeartbeatSkipsHashWithoutStateField: a hash that exists but
// lacks the state field is silently skipped — caller may have added
// a different schema.
func TestHeartbeatSkipsHashWithoutStateField(t *testing.T) {
	t.Parallel()

	f := newHeartbeatFixture(t)
	tenantID := uuid.New()
	operatorID := uuid.New()
	key := "op:" + tenantID.String() + ":user:" + operatorID.String()
	require.NoError(t, f.rdb.HSet(t.Context(), key, "other_field", "value").Err())

	f.hb.Sweep(t.Context())
	assert.Empty(t, f.fsm.snapshotForces())
}

// TestHeartbeatSweepBailsOnContextCancel: a cancelled ctx during a
// sweep terminates promptly without forcing further operators.
func TestHeartbeatSweepBailsOnContextCancel(t *testing.T) {
	t.Parallel()

	f := newHeartbeatFixture(t)
	for range 5 {
		f.seedOperator(t, uuid.New(), uuid.New(), api.StateReady, 0)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel before the sweep starts
	f.hb.Sweep(ctx)

	// With a pre-cancelled ctx the sweep returns immediately; no
	// forces should have landed.
	assert.Empty(t, f.fsm.snapshotForces())
}
