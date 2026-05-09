// trunks_replicator_test.go — behaviour tests for *TrunksReplicator.
//
// Test discipline:
//
//   - The replicator owns no goroutines; goleak in main_test.go guards
//     against a regression that adds one.
//   - All tests run in parallel and share package-level fakes only via
//     local construction so race tests are clean.
//   - We exercise the three required cases from Plan 11.1 Task 2 Step 2.2
//     (fan-out, lister-error, no-tenants), the constructor invariants
//     (nil hub / nil lister panic), the metrics observation paths, and
//     the JSON-payload pass-through.
package events_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/events"
)

// fakeTenantLister is a minimal in-test stub satisfying events.TenantLister.
type fakeTenantLister struct {
	mu      sync.Mutex
	tenants []string
	err     error
	calls   int
}

func (f *fakeTenantLister) ListActiveTenantIDs(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]string, len(f.tenants))
	copy(out, f.tenants)
	return out, nil
}

func (f *fakeTenantLister) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Compile-time guarantees the fake satisfies the public port.
var _ events.TenantLister = (*fakeTenantLister)(nil)

// --- Tests ------------------------------------------------------------

func TestTrunksReplicator_FansOutToEveryActiveTenant(t *testing.T) {
	t.Parallel()

	hub := &hubBroadcastRecorder{returnVal: 2}
	lister := &fakeTenantLister{
		tenants: []string{"tenant-A", "tenant-B", "tenant-C"},
	}
	replicator := events.NewTrunksReplicator(hub, lister, zap.NewNop(), nil)

	payload := []byte(`{"node":"fs1","ok":true}`)
	require.NoError(t, replicator.Dispatch(context.Background(), payload))

	calls := hub.Calls()
	require.Len(t, calls, 3)

	tenants := make([]string, 0, len(calls))
	for _, c := range calls {
		require.Equal(t, rtapi.TopicTrunksHealth, c.topic)
		require.JSONEq(t, string(payload), string(c.payload))
		require.NotEmpty(t, c.filter.TenantID, "TenantID must be set on every fan-out call")
		require.Empty(t, c.filter.UserID)
		require.Empty(t, c.filter.ProjectID)
		require.Empty(t, c.filter.CallID)
		tenants = append(tenants, c.filter.TenantID)
	}
	require.ElementsMatch(t, []string{"tenant-A", "tenant-B", "tenant-C"}, tenants)
}

func TestTrunksReplicator_TenantListerErrorIsLogged(t *testing.T) {
	t.Parallel()

	hub := &hubBroadcastRecorder{}
	lister := &fakeTenantLister{err: errors.New("db down")}
	logCore, logs := observer.New(zap.WarnLevel)
	replicator := events.NewTrunksReplicator(hub, lister, zap.New(logCore), nil)

	// Tenant lister failure must NOT propagate to the bus (would
	// trigger NATS redelivery loop). Just log + skip.
	err := replicator.Dispatch(context.Background(), []byte(`{}`))
	require.NoError(t, err)
	require.Empty(t, hub.Calls())
	require.Equal(t, 1, logs.FilterMessageSnippet("tenant lister failed").Len(),
		"WARN log must be emitted with the snippet 'tenant lister failed'")
}

func TestTrunksReplicator_NoActiveTenantsIsNoop(t *testing.T) {
	t.Parallel()

	hub := &hubBroadcastRecorder{}
	lister := &fakeTenantLister{tenants: []string{}}
	replicator := events.NewTrunksReplicator(hub, lister, zap.NewNop(), nil)

	require.NoError(t, replicator.Dispatch(context.Background(), []byte(`{}`)))
	require.Empty(t, hub.Calls())
	// The lister was still consulted exactly once — the replicator must
	// NOT short-circuit before calling out to its dependency.
	require.Equal(t, 1, lister.Calls())
}

func TestTrunksReplicator_ConstructorPanicsOnNilHub(t *testing.T) {
	t.Parallel()

	defer func() {
		require.NotNil(t, recover(), "nil hub must panic at construction")
	}()
	lister := &fakeTenantLister{}
	_ = events.NewTrunksReplicator(nil, lister, nil, nil)
}

func TestTrunksReplicator_ConstructorPanicsOnNilLister(t *testing.T) {
	t.Parallel()

	defer func() {
		require.NotNil(t, recover(), "nil lister must panic at construction")
	}()
	hub := &hubBroadcastRecorder{}
	_ = events.NewTrunksReplicator(hub, nil, nil, nil)
}

func TestTrunksReplicator_NilLogger_DefaultsToNop(t *testing.T) {
	t.Parallel()

	hub := &hubBroadcastRecorder{}
	lister := &fakeTenantLister{tenants: []string{"tenant-A"}}
	// Pass nil logger; the constructor must default to zap.NewNop so
	// Dispatch does not panic when emitting log lines.
	replicator := events.NewTrunksReplicator(hub, lister, nil, nil)

	require.NoError(t, replicator.Dispatch(context.Background(), []byte(`{}`)))
	require.Len(t, hub.Calls(), 1)
}

func TestTrunksReplicator_DispatchAlwaysReturnsNil(t *testing.T) {
	t.Parallel()

	// Even when the lister errors, Dispatch must return nil so the bus
	// acks the message (propagating an error would trigger NATS
	// redelivery against a permanently-broken catalog).
	hub := &hubBroadcastRecorder{}
	lister := &fakeTenantLister{err: errors.New("permanent failure")}
	replicator := events.NewTrunksReplicator(hub, lister, zap.NewNop(), nil)

	for range 5 {
		require.NoError(t, replicator.Dispatch(context.Background(), []byte(`{}`)))
	}
	require.Empty(t, hub.Calls())
	require.Equal(t, 5, lister.Calls(),
		"each Dispatch must call the lister; replicator must NOT cache")
}

func TestTrunksReplicator_MetricsObservedOnFanout(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	metrics := events.RegisterMetrics(reg)

	hub := &hubBroadcastRecorder{returnVal: 4}
	lister := &fakeTenantLister{tenants: []string{"tenant-A", "tenant-B"}}
	replicator := events.NewTrunksReplicator(hub, lister, zap.NewNop(), metrics)

	require.NoError(t, replicator.Dispatch(context.Background(), []byte(`{}`)))

	// Two messages observed (one per tenant), each with fan-out 4.
	require.InDelta(t, 2.0, counterValue(t, reg,
		"realtime_dispatcher_messages_total",
		map[string]string{"topic": "trunks.health"}), 0.001)

	count, sum := histogramSnapshot(t, reg, "realtime_dispatcher_fanout_size")
	require.Equal(t, uint64(2), count)
	require.InDelta(t, 8.0, sum, 0.001)
}

func TestTrunksReplicator_MetricsObservedOnListerFailure(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	metrics := events.RegisterMetrics(reg)

	hub := &hubBroadcastRecorder{}
	lister := &fakeTenantLister{err: errors.New("db down")}
	replicator := events.NewTrunksReplicator(hub, lister, zap.NewNop(), metrics)

	require.NoError(t, replicator.Dispatch(context.Background(), []byte(`{}`)))

	require.InDelta(t, 1.0, counterValue(t, reg,
		"realtime_dispatcher_dispatch_failures_total",
		map[string]string{"topic": "trunks.health", "reason": "tenant_lister_failed"}), 0.001)
}

func TestTrunksReplicator_NilMetricsIsTolerated(t *testing.T) {
	t.Parallel()

	hub := &hubBroadcastRecorder{returnVal: 1}
	lister := &fakeTenantLister{tenants: []string{"tenant-A"}}
	// nil *Metrics — every observe* call short-circuits.
	replicator := events.NewTrunksReplicator(hub, lister, zap.NewNop(), nil)

	require.NoError(t, replicator.Dispatch(context.Background(), []byte(`{}`)))
	require.Len(t, hub.Calls(), 1)
}

func TestTrunksReplicator_PayloadIsPassedThroughUntouched(t *testing.T) {
	t.Parallel()

	hub := &hubBroadcastRecorder{}
	lister := &fakeTenantLister{tenants: []string{"tenant-A"}}
	replicator := events.NewTrunksReplicator(hub, lister, zap.NewNop(), nil)

	// Crafted JSON with whitespace + key-order distinctions so we
	// can prove the replicator forwards the payload byte-for-byte
	// rather than re-marshalling (which would normalise whitespace
	// + key order and break the strict byte equality below).
	original := []byte(`{ "node":"fs-1",  "trunks":[{"id":"t1","ok":true}] }`)
	require.NoError(t, replicator.Dispatch(context.Background(), original))

	calls := hub.Calls()
	require.Len(t, calls, 1)

	// Strict byte equality — testifylint's encoded-compare rule
	// would normally suggest JSONEq for two JSON values, but here
	// we WANT byte-for-byte equality because the contract is "no
	// re-marshalling".
	require.Equal(t, original, []byte(calls[0].payload)) //nolint:testifylint // byte equality intentional — proves no re-marshal
}

func TestTrunksReplicator_ConcurrentDispatchRaceClean(t *testing.T) {
	t.Parallel()

	hub := &hubBroadcastRecorder{}
	lister := &fakeTenantLister{tenants: []string{"tenant-A", "tenant-B"}}
	replicator := events.NewTrunksReplicator(hub, lister, zap.NewNop(), nil)

	const goroutines = 8
	const messagesEach = 25

	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range messagesEach {
				require.NoError(t, replicator.Dispatch(context.Background(), []byte(`{}`)))
			}
		})
	}
	wg.Wait()

	require.Len(t, hub.Calls(), goroutines*messagesEach*2,
		"every Dispatch must produce one Broadcast per active tenant")
}
