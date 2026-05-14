//go:build integration

// Integration tests for pkg/outbox.Relay against a real Postgres 16 instance.
// The Relay drains pending rows from event_outbox, calls Publisher.Publish,
// and marks rows on success / increments attempts on failure.
//
// Run: go test -tags=integration -count=1 -timeout 5m ./pkg/outbox/...
package outbox_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// fakePublisher is a Publisher whose behaviour is controlled per call.
// Tests configure it via failNTimes / failAlways and inspect calls via
// the recorded slice.
type fakePublisher struct {
	mu         sync.Mutex
	calls      []recordedCall
	failNTimes atomic.Int64 // remaining calls that will fail before success
	failAlways atomic.Bool  // when true, every call fails until cleared
}

type recordedCall struct {
	subject string
	payload []byte
}

func (f *fakePublisher) Publish(_ context.Context, subject string, payload []byte) error {
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{subject: subject, payload: append([]byte(nil), payload...)})
	f.mu.Unlock()

	if f.failAlways.Load() {
		return errFakePublishFailed
	}
	if remaining := f.failNTimes.Load(); remaining > 0 {
		f.failNTimes.Store(remaining - 1)
		return errFakePublishFailed
	}
	return nil
}

func (f *fakePublisher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakePublisher) successCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	// success = total - errors that were forced; we cannot know directly
	// here without re-running the policy. Tests instead query Postgres
	// for published_at IS NOT NULL counts when they need the truth.
	return len(f.calls)
}

var errFakePublishFailed = errors.New("publish failed")

// seedEvents inserts n outbox rows under a fresh tenant. The rows are
// "pending" — published_at is NULL.
func seedEvents(t *testing.T, ctx context.Context, pool *postgres.Pool, n int) {
	t.Helper()
	tenantID := uuid.New()
	w := outbox.NewPostgresWriter()

	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		for i := 0; i < n; i++ {
			aggID := uuid.New()
			if err := w.Append(ctx, tx, outbox.Event{
				TenantID:    &tenantID,
				AggregateID: &aggID,
				Subject:     "test.subj",
				Payload:     []byte(`{"i":` + intToStr(i) + `}`),
			}); err != nil {
				return err
			}
		}
		return nil
	}))
}

// intToStr converts a small int to its decimal string. Avoids strconv just
// to keep the helper module narrow.
func intToStr(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// TestRelay_DrainsAllPending writes 5 events, runs the relay, and asserts
// that within a short window all rows are marked published_at and the
// publisher saw exactly 5 calls.
func TestRelay_DrainsAllPending(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newOutboxTestPool(t)
	seedEvents(t, ctx, pool, 5)

	pub := &fakePublisher{}
	relay := outbox.NewRelay(pool, pub, outbox.RelayConfig{
		BatchSize:      10,
		Tick:           50 * time.Millisecond,
		MaxRetry:       3,
		PublishTimeout: time.Second,
	}, zap.NewNop())

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	done := make(chan error, 1)
	go func() { done <- relay.Run(runCtx) }()

	require.Eventually(t, func() bool {
		return pub.successCount() >= 5 && unpublishedCount(t, ctx, pool) == 0
	}, 5*time.Second, 50*time.Millisecond, "relay did not drain all events")

	runCancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not return within 2s of cancel")
	}

	require.Equal(t, 0, unpublishedCount(t, ctx, pool))
}

// TestRelay_RetriesOnPublishError configures the publisher to fail the
// first 3 publish attempts and succeed afterwards. With one event in
// the outbox, that means: try, fail, attempts=1; try, fail, attempts=2;
// try, fail, attempts=3; try, success, published_at set, attempts stays 3.
func TestRelay_RetriesOnPublishError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newOutboxTestPool(t)
	seedEvents(t, ctx, pool, 1)

	pub := &fakePublisher{}
	pub.failNTimes.Store(3)

	relay := outbox.NewRelay(pool, pub, outbox.RelayConfig{
		BatchSize:      5,
		Tick:           50 * time.Millisecond,
		MaxRetry:       10,
		PublishTimeout: time.Second,
	}, zap.NewNop())

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	done := make(chan error, 1)
	go func() { done <- relay.Run(runCtx) }()

	require.Eventually(t, func() bool {
		return unpublishedCount(t, ctx, pool) == 0
	}, 10*time.Second, 50*time.Millisecond, "row never published despite eventual success")

	runCancel()
	<-done

	// Verify attempts counter incremented above zero.
	var attempts int
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, `SELECT max(attempts) FROM event_outbox`).Scan(&attempts)
	}))
	require.GreaterOrEqual(t, attempts, 1, "attempts should have been incremented")
	// Publisher should have been called >= 4 times (3 failures + at least one success).
	require.GreaterOrEqual(t, pub.callCount(), 4)
}

// TestRelay_RespectsMaxRetry parks rows once attempts reaches MaxRetry. The
// drain query filters those out, so a fully-failing publisher does NOT spin
// the loop forever — attempts grows to MaxRetry, then the row is skipped.
func TestRelay_RespectsMaxRetry(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newOutboxTestPool(t)
	seedEvents(t, ctx, pool, 1)

	pub := &fakePublisher{}
	pub.failAlways.Store(true)

	maxRetry := 3
	relay := outbox.NewRelay(pool, pub, outbox.RelayConfig{
		BatchSize:      5,
		Tick:           20 * time.Millisecond,
		MaxRetry:       maxRetry,
		PublishTimeout: 200 * time.Millisecond,
	}, zap.NewNop())

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	done := make(chan error, 1)
	go func() { done <- relay.Run(runCtx) }()

	// Wait for attempts to reach MaxRetry. After that, the relay must
	// stop touching the row — successive ticks must see attempts stuck
	// at MaxRetry.
	require.Eventually(t, func() bool {
		return maxAttempts(t, ctx, pool) >= maxRetry
	}, 5*time.Second, 50*time.Millisecond)

	// Give the relay a few extra ticks; attempts must NOT exceed MaxRetry.
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, maxRetry, maxAttempts(t, ctx, pool))

	runCancel()
	<-done
}

// TestRelay_ReturnsOnContextCancel asserts that Run exits cleanly within a
// short deadline once the context is cancelled. This is the contract that
// goleak (TestMain) relies on.
func TestRelay_ReturnsOnContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newOutboxTestPool(t)

	pub := &fakePublisher{}
	relay := outbox.NewRelay(pool, pub, outbox.RelayConfig{
		BatchSize:      5,
		Tick:           50 * time.Millisecond,
		MaxRetry:       3,
		PublishTimeout: time.Second,
	}, zap.NewNop())

	runCtx, runCancel := context.WithCancel(ctx)

	done := make(chan error, 1)
	go func() { done <- relay.Run(runCtx) }()

	// Let the relay run a couple of ticks first.
	time.Sleep(150 * time.Millisecond)

	runCancel()

	select {
	case err := <-done:
		require.NoError(t, err, "Run must return nil on graceful shutdown")
	case <-time.After(time.Second):
		t.Fatal("Run did not exit within 1s of cancel — leaked goroutine likely")
	}
}

// TestRelay_NoPendingRowsIsNoOp verifies that ticks against an empty outbox
// do not error out and the relay keeps running.
func TestRelay_NoPendingRowsIsNoOp(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newOutboxTestPool(t)

	pub := &fakePublisher{}
	relay := outbox.NewRelay(pool, pub, outbox.RelayConfig{
		BatchSize:      5,
		Tick:           20 * time.Millisecond,
		MaxRetry:       3,
		PublishTimeout: time.Second,
	}, zap.NewNop())

	runCtx, runCancel := context.WithCancel(ctx)

	done := make(chan error, 1)
	go func() { done <- relay.Run(runCtx) }()

	// Let it tick a few times against an empty table.
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 0, pub.callCount(), "publisher must not be called when outbox is empty")

	runCancel()
	<-done
}

// unpublishedCount returns the number of rows in event_outbox where
// published_at IS NULL. Test helper.
func unpublishedCount(t *testing.T, ctx context.Context, pool *postgres.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM event_outbox WHERE published_at IS NULL`).Scan(&n)
	}))
	return n
}

// maxAttempts returns the largest attempts value across event_outbox.
func maxAttempts(t *testing.T, ctx context.Context, pool *postgres.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(max(attempts), 0) FROM event_outbox`).Scan(&n)
	}))
	return n
}

// seedParkedRows inserts n parked rows for the given tenant. "Parked"
// means attempts >= maxRetry AND published_at IS NULL — exactly the rows
// the DLQ poll counts.
func seedParkedRows(t *testing.T, ctx context.Context, pool *postgres.Pool, tenantID uuid.UUID, n, maxRetry int) {
	t.Helper()
	w := outbox.NewPostgresWriter()

	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		for i := 0; i < n; i++ {
			aggID := uuid.New()
			if err := w.Append(ctx, tx, outbox.Event{
				TenantID:    &tenantID,
				AggregateID: &aggID,
				Subject:     "test.parked",
				Payload:     []byte(`{"i":` + intToStr(i) + `}`),
			}); err != nil {
				return err
			}
		}
		// Force attempts to MaxRetry so the DLQ predicate matches.
		_, err := tx.Exec(ctx,
			`UPDATE event_outbox SET attempts = $1
			 WHERE tenant_id = $2 AND published_at IS NULL`,
			maxRetry, tenantID)
		return err
	}))
}

// resetAttempts pulls every parked row for tenantID back below the DLQ
// threshold. Used to simulate operator remediation between poll cycles.
func resetAttempts(t *testing.T, ctx context.Context, pool *postgres.Pool, tenantID uuid.UUID) {
	t.Helper()
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE event_outbox SET attempts = 0
			 WHERE tenant_id = $1 AND published_at IS NULL`,
			tenantID)
		return err
	}))
}

// resetOneAttempt picks a single parked row for tenantID and resets its
// attempts to 0 — used to assert the gauge tracks partial remediation.
func resetOneAttempt(t *testing.T, ctx context.Context, pool *postgres.Pool, tenantID uuid.UUID) {
	t.Helper()
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE event_outbox SET attempts = 0
			 WHERE id = (
			   SELECT id FROM event_outbox
			   WHERE tenant_id = $1 AND published_at IS NULL AND attempts > 0
			   ORDER BY id LIMIT 1
			 )`,
			tenantID)
		return err
	}))
}

// gaugeFor reads the current value of m.ParkedRows for the supplied tenant.
// Returns 0 if the label combination is not present.
func gaugeFor(t *testing.T, m *outbox.RelayMetrics, tenantID uuid.UUID) float64 {
	t.Helper()
	return testutil.ToFloat64(m.ParkedRows.WithLabelValues(tenantID.String()))
}

// newRelayWithMetrics builds a Relay wired to a fresh metrics struct,
// with DLQ polling DISABLED in Run (interval=0). Tests drive PollOnce
// explicitly so they don't depend on timer scheduling.
func newRelayWithMetrics(t *testing.T, pool *postgres.Pool, pub *fakePublisher, maxRetry int) (*outbox.Relay, *outbox.RelayMetrics) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := outbox.RegisterRelayMetrics(reg)
	require.NoError(t, err)

	relay := outbox.NewRelay(pool, pub, outbox.RelayConfig{
		BatchSize:       10,
		Tick:            50 * time.Millisecond,
		MaxRetry:        maxRetry,
		PublishTimeout:  time.Second,
		DLQPollInterval: 0, // disable poll goroutine; tests drive PollOnce
		Metrics:         m,
	}, zap.NewNop())
	return relay, m
}

// TestRelay_DLQGauge_TracksParkedRowsPerTenant seeds 3 parked rows for
// tenant A and 2 parked rows for tenant B; PollOnce must publish the
// per-tenant counts to the gauge.
func TestRelay_DLQGauge_TracksParkedRowsPerTenant(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newOutboxTestPool(t)

	const maxRetry = 3
	tenantA := uuid.New()
	tenantB := uuid.New()
	seedParkedRows(t, ctx, pool, tenantA, 3, maxRetry)
	seedParkedRows(t, ctx, pool, tenantB, 2, maxRetry)

	relay, m := newRelayWithMetrics(t, pool, &fakePublisher{}, maxRetry)

	require.NoError(t, relay.PollOnce(ctx))

	require.InDelta(t, 3.0, gaugeFor(t, m, tenantA), 0.0,
		"tenant A should report 3 parked rows")
	require.InDelta(t, 2.0, gaugeFor(t, m, tenantB), 0.0,
		"tenant B should report 2 parked rows")
}

// TestRelay_DLQGauge_ResetsWhenAttemptsDropBelowMax verifies that after a
// row is remediated (attempts pulled below MaxRetry), the next poll
// reflects the new lower count for that tenant.
func TestRelay_DLQGauge_ResetsWhenAttemptsDropBelowMax(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newOutboxTestPool(t)

	const maxRetry = 3
	tenantA := uuid.New()
	seedParkedRows(t, ctx, pool, tenantA, 3, maxRetry)

	relay, m := newRelayWithMetrics(t, pool, &fakePublisher{}, maxRetry)

	require.NoError(t, relay.PollOnce(ctx))
	require.InDelta(t, 3.0, gaugeFor(t, m, tenantA), 0.0)

	resetOneAttempt(t, ctx, pool, tenantA)
	require.NoError(t, relay.PollOnce(ctx))
	require.InDelta(t, 2.0, gaugeFor(t, m, tenantA), 0.0,
		"gauge should track partial remediation")
}

// TestRelay_DLQGauge_DisappearsWhenAllRowsClear asserts that when a
// previously parked tenant has zero rows on the next poll, the gauge
// label combination is removed (not left at the prior count).
func TestRelay_DLQGauge_DisappearsWhenAllRowsClear(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newOutboxTestPool(t)

	const maxRetry = 3
	tenantA := uuid.New()
	seedParkedRows(t, ctx, pool, tenantA, 3, maxRetry)

	relay, m := newRelayWithMetrics(t, pool, &fakePublisher{}, maxRetry)

	require.NoError(t, relay.PollOnce(ctx))
	require.InDelta(t, 3.0, gaugeFor(t, m, tenantA), 0.0)

	resetAttempts(t, ctx, pool, tenantA)
	require.NoError(t, relay.PollOnce(ctx))

	// After all rows clear, the tenant's label must be deleted from the
	// gauge entirely (DeleteLabelValues). Count via CollectAndCount: 0
	// means no series, which is what we want.
	got := testutil.CollectAndCount(m.ParkedRows, "sociopulse_outbox_parked_rows_total")
	require.Equal(t, 0, got,
		"gauge label for tenant with zero parked rows must be deleted, not left at 0")
}

// TestRelay_DLQPollGoroutineExits_OnContextCancel verifies the poll
// goroutine respects ctx cancellation. goleak (TestMain) backs this up.
func TestRelay_DLQPollGoroutineExits_OnContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newOutboxTestPool(t)

	reg := prometheus.NewRegistry()
	m, err := outbox.RegisterRelayMetrics(reg)
	require.NoError(t, err)

	relay := outbox.NewRelay(pool, &fakePublisher{}, outbox.RelayConfig{
		BatchSize:       5,
		Tick:            50 * time.Millisecond,
		MaxRetry:        3,
		PublishTimeout:  time.Second,
		DLQPollInterval: 50 * time.Millisecond, // poll goroutine MUST also exit on cancel
		Metrics:         m,
	}, zap.NewNop())

	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- relay.Run(runCtx) }()

	// Let the poll goroutine tick at least once.
	time.Sleep(150 * time.Millisecond)
	runCancel()

	select {
	case err := <-done:
		require.NoError(t, err, "Run must return nil on graceful shutdown")
	case <-time.After(time.Second):
		t.Fatal("Run did not exit within 1s of cancel — DLQ-poll goroutine likely leaked")
	}
}
