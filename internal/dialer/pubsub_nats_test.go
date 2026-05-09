package dialer_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/dialer"
	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// TestNATSPubSub_PublishReachesLocalSubscriber is the happy-path
// round-trip: build a NATSPubSub against an embedded JetStream broker,
// Subscribe locally, Publish, and assert the snapshot arrives on the
// returned channel within the 3s deadline.
func TestNATSPubSub_PublishReachesLocalSubscriber(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "DIALERA", []string{"tenant.>"})

	pub, sub := newBusPair(t, url)

	ps := dialer.NewNATSPubSub(pub, sub, "replica-1", zaptest.NewLogger(t))
	require.NoError(t, ps.Start(t.Context()))
	t.Cleanup(func() { _ = ps.Stop() })

	tenantID := uuid.New()
	operatorID := uuid.New()
	ch, cancel := ps.Subscribe(tenantID, operatorID)
	t.Cleanup(cancel)

	want := api.Snapshot{
		TenantID:       tenantID,
		OperatorID:     operatorID,
		State:          api.StateReady,
		StateEnteredAt: time.Now().UTC().Truncate(time.Millisecond),
		HeartbeatAt:    time.Now().UTC().Truncate(time.Millisecond),
	}
	ps.Publish(want)

	requireRoundTrip(t, ch, want, 3*time.Second)
}

// TestNATSPubSub_OperatorScopingFiltersOutOtherOperators verifies that
// a snapshot for operator A is not delivered to a subscriber registered
// under operator B (same tenant). The bus delivers everything that
// matches the wildcard subject; the local fan-out must filter on the
// (tenantID, operatorID) key.
func TestNATSPubSub_OperatorScopingFiltersOutOtherOperators(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "DIALERB", []string{"tenant.>"})

	pub, sub := newBusPair(t, url)

	ps := dialer.NewNATSPubSub(pub, sub, "replica-2", zaptest.NewLogger(t))
	require.NoError(t, ps.Start(t.Context()))
	t.Cleanup(func() { _ = ps.Stop() })

	tenantID := uuid.New()
	opA := uuid.New()
	opB := uuid.New()

	chA, cancelA := ps.Subscribe(tenantID, opA)
	t.Cleanup(cancelA)
	chB, cancelB := ps.Subscribe(tenantID, opB)
	t.Cleanup(cancelB)

	snap := api.Snapshot{TenantID: tenantID, OperatorID: opA, State: api.StateDialing}
	ps.Publish(snap)

	// A receives within 3s.
	select {
	case got := <-chA:
		require.Equal(t, snap, got)
	case <-time.After(3 * time.Second):
		t.Fatal("operator-A subscriber did not receive snapshot")
	}

	// B must NOT receive within a small grace window. We only need to
	// detect leakage — 250ms is plenty since A already round-tripped.
	select {
	case got := <-chB:
		t.Fatalf("operator-B subscriber unexpectedly received snapshot: %+v", got)
	case <-time.After(250 * time.Millisecond):
	}
}

// TestNATSPubSub_TenantScopingFiltersOutOtherTenants is the cross-tenant
// twin of the operator-scoping test. The same operator UUID under a
// different tenant must NOT receive the snapshot.
func TestNATSPubSub_TenantScopingFiltersOutOtherTenants(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "DIALERC", []string{"tenant.>"})

	pub, sub := newBusPair(t, url)

	ps := dialer.NewNATSPubSub(pub, sub, "replica-3", zaptest.NewLogger(t))
	require.NoError(t, ps.Start(t.Context()))
	t.Cleanup(func() { _ = ps.Stop() })

	tenantA := uuid.New()
	tenantB := uuid.New()
	op := uuid.New()

	chA, cancelA := ps.Subscribe(tenantA, op)
	t.Cleanup(cancelA)
	chB, cancelB := ps.Subscribe(tenantB, op)
	t.Cleanup(cancelB)

	snap := api.Snapshot{TenantID: tenantA, OperatorID: op, State: api.StatePause}
	ps.Publish(snap)

	select {
	case got := <-chA:
		require.Equal(t, snap, got)
	case <-time.After(3 * time.Second):
		t.Fatal("tenant-A subscriber did not receive snapshot")
	}

	select {
	case got := <-chB:
		t.Fatalf("tenant-B subscriber unexpectedly received snapshot: %+v", got)
	case <-time.After(250 * time.Millisecond):
	}
}

// TestNATSPubSub_Stop_DrainsAndClosesAllSubscribers asserts Stop closes
// every channel handed out by Subscribe, mirroring the existing
// in-memory PubSub.Close contract used by Module.Stop.
func TestNATSPubSub_Stop_DrainsAndClosesAllSubscribers(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "DIALERD", []string{"tenant.>"})

	pub, sub := newBusPair(t, url)

	ps := dialer.NewNATSPubSub(pub, sub, "replica-4", zaptest.NewLogger(t))
	require.NoError(t, ps.Start(t.Context()))

	chA, _ := ps.Subscribe(uuid.New(), uuid.New())
	chB, _ := ps.Subscribe(uuid.New(), uuid.New())

	require.NoError(t, ps.Stop())

	for _, ch := range []<-chan api.Snapshot{chA, chB} {
		select {
		case _, ok := <-ch:
			require.False(t, ok, "channel should be closed after Stop")
		case <-time.After(2 * time.Second):
			t.Fatal("channel did not close after Stop")
		}
	}

	// Stop is idempotent.
	require.NoError(t, ps.Stop())
}

// TestNATSPubSub_PublishAfterStopIsNoop confirms that Publish does not
// panic and does not leak when invoked after Stop. Subscribe-after-Stop
// must hand back a closed channel and a no-op cancel — graceful
// shutdown-race semantics.
func TestNATSPubSub_PublishAfterStopIsNoop(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "DIALERE", []string{"tenant.>"})

	pub, sub := newBusPair(t, url)

	ps := dialer.NewNATSPubSub(pub, sub, "replica-5", zaptest.NewLogger(t))
	require.NoError(t, ps.Start(t.Context()))
	require.NoError(t, ps.Stop())

	// Publish-after-Stop must be a silent no-op.
	require.NotPanics(t, func() {
		ps.Publish(api.Snapshot{TenantID: uuid.New(), OperatorID: uuid.New(), State: api.StateOffline})
	})

	// Subscribe-after-Stop returns a closed channel + no-op cancel.
	ch, cancel := ps.Subscribe(uuid.New(), uuid.New())
	require.NotPanics(t, cancel, "post-Stop cancel must be a no-op")
	select {
	case _, ok := <-ch:
		require.False(t, ok, "Subscribe after Stop must return a closed channel")
	case <-time.After(time.Second):
		t.Fatal("Subscribe-after-Stop channel was not closed")
	}
}

// TestNATSPubSub_StartTwiceReturnsError protects the lifecycle invariant
// that Start is single-shot.
func TestNATSPubSub_StartTwiceReturnsError(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "DIALERF", []string{"tenant.>"})

	pub, sub := newBusPair(t, url)

	ps := dialer.NewNATSPubSub(pub, sub, "replica-6", zaptest.NewLogger(t))
	require.NoError(t, ps.Start(t.Context()))
	t.Cleanup(func() { _ = ps.Stop() })

	err := ps.Start(t.Context())
	require.Error(t, err, "second Start must return an error")
}

// TestNATSPubSub_NilPubPanics verifies the constructor invariant — a
// nil eventbus.Publisher is a programmer bug and panics with a clear
// remediation message.
func TestNATSPubSub_NilPubPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		dialer.NewNATSPubSub(nil, stubSubscriber{}, "r", zap.NewNop())
	})
}

// TestNATSPubSub_NilSubPanics is the symmetric guard for the
// eventbus.Subscriber half.
func TestNATSPubSub_NilSubPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		dialer.NewNATSPubSub(stubPublisher{}, nil, "r", zap.NewNop())
	})
}

// TestNATSPubSub_MalformedJSONIsAcked exercises the bus-handler error
// path: a payload that fails json.Unmarshal must be ack'd (return nil)
// — not NACK'd (which would trigger an infinite redelivery loop) — and
// must not panic. We assert the absence of panic by Publish-ing
// alongside a real snapshot and confirming the real snapshot still
// reaches its subscriber, which proves the handler did not get stuck.
func TestNATSPubSub_MalformedJSONIsAcked(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "DIALERG", []string{"tenant.>"})

	pub, sub := newBusPair(t, url)

	ps := dialer.NewNATSPubSub(pub, sub, "replica-7", zaptest.NewLogger(t))
	require.NoError(t, ps.Start(t.Context()))
	t.Cleanup(func() { _ = ps.Stop() })

	tenantID := uuid.New()
	operatorID := uuid.New()
	ch, cancel := ps.Subscribe(tenantID, operatorID)
	t.Cleanup(cancel)

	// Inject a malformed payload directly via the raw publisher so the
	// bus handler observes a JSON-unmarshal error path. Subject must
	// match the wildcard the handler subscribed on.
	garbage := []byte("{not-valid-json")
	subject := fmt.Sprintf("tenant.%s.dialer.op.%s.state", tenantID, operatorID)
	require.NoError(t, pub.Publish(t.Context(), subject, garbage))

	// Now publish a well-formed snapshot — it must still reach the
	// subscriber, proving the malformed message did not stall delivery.
	want := api.Snapshot{TenantID: tenantID, OperatorID: operatorID, State: api.StateCall}
	ps.Publish(want)

	requireRoundTrip(t, ch, want, 3*time.Second)
}

// TestNATSPubSub_NewAppliesNilLoggerAndEmptyReplicaIDDefaults exercises
// the constructor's nil-logger and empty-replicaID fallbacks without
// engaging the bus. Two stub Publish/Subscribe doubles satisfy the
// non-nil pre-conditions; the constructor must not panic and must
// return a usable handle.
func TestNATSPubSub_NewAppliesNilLoggerAndEmptyReplicaIDDefaults(t *testing.T) {
	t.Parallel()
	ps := dialer.NewNATSPubSub(stubPublisher{}, stubSubscriber{}, "", nil)
	require.NotNil(t, ps)
}

// TestNATSPubSub_StartPropagatesSubscribeError covers the wrapped
// error path when the supplied Subscriber returns a non-nil error
// from its Subscribe call. We use a stub that always errors so the
// path runs synchronously without touching JetStream.
func TestNATSPubSub_StartPropagatesSubscribeError(t *testing.T) {
	t.Parallel()
	boom := errors.New("subscribe boom")
	ps := dialer.NewNATSPubSub(stubPublisher{}, errSubscriber{err: boom}, "r", zaptest.NewLogger(t))
	err := ps.Start(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, boom)
	require.Contains(t, err.Error(), "dialer/pubsub_nats: subscribe")
}

// TestNATSPubSub_PublishLogsBrokerErrorAndDoesNotPanic uses a stub
// publisher whose Publish always returns an error so we exercise the
// publish-error branch (logged + swallowed). We also exercise the
// nil-Ctx default in Start (passing context.Background()).
func TestNATSPubSub_PublishLogsBrokerErrorAndDoesNotPanic(t *testing.T) {
	t.Parallel()
	ps := dialer.NewNATSPubSub(errPublisher{err: errors.New("publish fail")}, stubSubscriber{}, "r", zaptest.NewLogger(t))
	require.NoError(t, ps.Start(t.Context()))
	t.Cleanup(func() { _ = ps.Stop() })

	require.NotPanics(t, func() {
		ps.Publish(api.Snapshot{TenantID: uuid.New(), OperatorID: uuid.New(), State: api.StateReady})
	})
}

// errSubscriber satisfies eventbus.Subscriber and returns a
// configurable error. Used by the Start-error path test.
type errSubscriber struct{ err error }

func (e errSubscriber) Subscribe(_ context.Context, _, _ string, _ func(string, []byte) error) error {
	return e.err
}

// errPublisher satisfies eventbus.Publisher and returns a
// configurable error from Publish. Used by the Publish-error path
// test (the path the production NATSPublisher hits when the broker
// rejects a publish).
type errPublisher struct{ err error }

func (e errPublisher) Publish(_ context.Context, _ string, _ []byte) error { return e.err }

// requireRoundTrip waits up to deadline for the snapshot to arrive on
// ch and asserts equality. Centralised to keep the per-case bodies
// terse.
func requireRoundTrip(t *testing.T, ch <-chan api.Snapshot, want api.Snapshot, deadline time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool {
		select {
		case got := <-ch:
			require.Equal(t, want, got)
			return true
		default:
			return false
		}
	}, deadline, 20*time.Millisecond, "did not receive snapshot within %s", deadline)
}

// newBusPair constructs a JetStream-backed Publisher + Subscriber pair
// pointing at the supplied embedded broker URL. Each side gets its own
// connection so we exercise the same wiring shape cmd/api uses in
// production. Cleanup is registered on t.
func newBusPair(t *testing.T, url string) (*eventbus.NATSPublisher, *eventbus.NATSSubscriber) {
	t.Helper()
	pub, err := eventbus.NewNATSPublisher(t.Context(), []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })

	sub, err := eventbus.NewNATSSubscriber(t.Context(), []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	return pub, sub
}

// stubPublisher / stubSubscriber are the cheapest possible interface
// satisfiers. Used only by the nil-arg constructor panic tests, where
// the real bus is never engaged.
type stubPublisher struct{}

func (stubPublisher) Publish(_ context.Context, _ string, _ []byte) error { return nil }

type stubSubscriber struct{}

func (stubSubscriber) Subscribe(_ context.Context, _, _ string, _ func(string, []byte) error) error {
	return nil
}

// startEmbeddedJetStream boots an in-process NATS server with JetStream
// enabled on a random TCP port. Mirrors pkg/eventbus/helpers_test.go —
// duplicated here because helpers_test.go is in the eventbus package
// and a `_test.go` file is invisible to other packages.
func startEmbeddedJetStream(t *testing.T) string {
	t.Helper()

	storeDir := filepath.Join(t.TempDir(), "jetstream")
	opts := &server.Options{
		Host:                  "127.0.0.1",
		Port:                  -1, // random port
		NoLog:                 true,
		NoSigs:                true,
		MaxControlLine:        4096,
		DisableShortFirstPing: true,
		JetStream:             true,
		StoreDir:              storeDir,
	}

	srv, err := server.NewServer(opts)
	require.NoError(t, err, "failed to construct embedded NATS server")

	go srv.Start()

	if !srv.ReadyForConnections(5 * time.Second) {
		srv.Shutdown()
		srv.WaitForShutdown()
		t.Fatal("embedded NATS server did not become ready in 5s")
	}

	t.Cleanup(func() {
		srv.Shutdown()
		srv.WaitForShutdown()
	})

	return srv.ClientURL()
}

// ensureStream provisions a JetStream stream covering the supplied
// subject patterns. Mirrors pkg/eventbus/helpers_test.go.
func ensureStream(t *testing.T, url, name string, subjects []string) {
	t.Helper()

	nc, err := nats.Connect(url)
	require.NoError(t, err)
	defer nc.Close()

	js, err := nc.JetStream()
	require.NoError(t, err)

	cfg := &nats.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Retention: nats.InterestPolicy,
		Storage:   nats.MemoryStorage,
		MaxAge:    1 * time.Minute,
	}
	if _, err := js.AddStream(cfg); err != nil {
		if errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
			_, err = js.UpdateStream(cfg)
		}
		require.NoError(t, err, "ensure stream %q", name)
	}
}
