package outbox_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/pkg/outbox"
)

// upstreamFake is a tiny eventbus.Publisher stand-in. The real fakePublisher
// in relay_test.go lives behind the integration tag, so we redefine a
// minimal one here for the unit tests.
type upstreamFake struct {
	calls      atomic.Int64
	failNTimes atomic.Int64
}

func (u *upstreamFake) Publish(_ context.Context, _ string, _ []byte) error {
	u.calls.Add(1)
	if remaining := u.failNTimes.Load(); remaining > 0 {
		u.failNTimes.Store(remaining - 1)
		return errUpstreamBoom
	}
	return nil
}

var errUpstreamBoom = errors.New("upstream boom")

// TestPublisherAdapter_PassesThroughOnSuccess covers the happy path:
// upstream returns nil → adapter returns nil with one upstream call.
func TestPublisherAdapter_PassesThroughOnSuccess(t *testing.T) {
	t.Parallel()

	up := &upstreamFake{}
	adapter := outbox.NewPublisherAdapter(up)

	err := adapter.Publish(context.Background(), outbox.Event{
		Subject: "test.subj",
		Payload: []byte(`{}`),
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, up.calls.Load())
}

// TestPublisherAdapter_RetriesUntilSuccess exercises the exponential
// backoff path: upstream fails twice then succeeds. Adapter must keep
// retrying and ultimately return nil.
func TestPublisherAdapter_RetriesUntilSuccess(t *testing.T) {
	t.Parallel()

	up := &upstreamFake{}
	up.failNTimes.Store(2)
	adapter := outbox.NewPublisherAdapter(up)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := adapter.Publish(ctx, outbox.Event{
		Subject: "test.subj",
		Payload: []byte(`{}`),
	})
	require.NoError(t, err)
	require.EqualValues(t, 3, up.calls.Load())
}

// TestPublisherAdapter_ReturnsErrPublisherFailedAfterMaxRetries asserts
// the sentinel error chain when retries are exhausted.
func TestPublisherAdapter_ReturnsErrPublisherFailedAfterMaxRetries(t *testing.T) {
	t.Parallel()

	up := &upstreamFake{}
	up.failNTimes.Store(1000) // far more than the adapter's max attempts
	adapter := outbox.NewPublisherAdapter(up)

	// Tight ctx so the test is fast even if backoff sleeps.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := adapter.Publish(ctx, outbox.Event{
		Subject: "test.subj",
		Payload: []byte(`{}`),
	})
	require.Error(t, err)
	require.ErrorIs(t, err, outbox.ErrPublisherFailed)
	require.ErrorIs(t, err, errUpstreamBoom, "should wrap the last upstream error")
}

// TestPublisherAdapter_RespectsContextCancellation: an already-cancelled
// context returns immediately without calling upstream.
func TestPublisherAdapter_RespectsContextCancellation(t *testing.T) {
	t.Parallel()

	up := &upstreamFake{}
	up.failNTimes.Store(1000)
	adapter := outbox.NewPublisherAdapter(up)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := adapter.Publish(ctx, outbox.Event{
		Subject: "test.subj",
		Payload: []byte(`{}`),
	})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}
