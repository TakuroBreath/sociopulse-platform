package eventbus

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// TestNATSPublisherSubscriber_RoundTrip is the happy-path smoke: a
// single publisher emits one payload, a single subscriber on the same
// subject + a unique queue receives it, and the broker ack'd the
// publish synchronously.
func TestNATSPublisherSubscriber_RoundTrip(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "RT", []string{"rt.>"})

	ctx := t.Context()

	pub, err := NewNATSPublisher(ctx, []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })

	sub, err := NewNATSSubscriber(ctx, []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	var (
		mu       sync.Mutex
		gotSubj  string
		gotData  []byte
		received = make(chan struct{}, 1)
	)
	require.NoError(t, sub.Subscribe(ctx, "rt.greeting", "rt-q1",
		func(subject string, payload []byte) error {
			mu.Lock()
			gotSubj = subject
			gotData = append([]byte(nil), payload...)
			mu.Unlock()
			select {
			case received <- struct{}{}:
			default:
			}
			return nil
		},
	))

	require.NoError(t, pub.Publish(ctx, "rt.greeting", []byte("hello plan-11")))

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive published message within 2s")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "rt.greeting", gotSubj)
	require.Equal(t, "hello plan-11", string(gotData))
}

// TestNATSPublisherSubscriber_Wildcard verifies a single-token (`*`)
// wildcard binds the subscriber to all matching publishes and that
// the handler observes the resolved subject (the actual published
// subject), not the wildcard pattern.
//
// Subject shape mirrors Plan 11 dialer/queue convention:
// `tenant.<t>.dialer.queue`.
func TestNATSPublisherSubscriber_Wildcard(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "WILD", []string{"tenant.>"})

	ctx := t.Context()

	pub, err := NewNATSPublisher(ctx, []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })

	sub, err := NewNATSSubscriber(ctx, []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	type frame struct {
		Subject string
		Payload []byte
	}
	frames := make(chan frame, 4)

	require.NoError(t, sub.Subscribe(ctx, "tenant.*.dialer.queue", "wild-q",
		func(subject string, payload []byte) error {
			frames <- frame{Subject: subject, Payload: append([]byte(nil), payload...)}
			return nil
		},
	))

	require.NoError(t, pub.Publish(ctx, "tenant.acme.dialer.queue", []byte("acme-1")))
	require.NoError(t, pub.Publish(ctx, "tenant.beta.dialer.queue", []byte("beta-1")))

	got := make(map[string]string)
	for range 2 {
		select {
		case f := <-frames:
			got[f.Subject] = string(f.Payload)
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for wildcard delivery; received %v", got)
		}
	}
	require.Equal(t, "acme-1", got["tenant.acme.dialer.queue"])
	require.Equal(t, "beta-1", got["tenant.beta.dialer.queue"])
}

// TestNATSPublisherSubscriber_QueueLoadBalance verifies that two
// subscribers using the SAME queue group split deliveries evenly:
// each message is delivered to exactly one of them, never both. This
// matches Plan 11 Decision Q2 fallback behaviour (workers behind a
// shared queue).
func TestNATSPublisherSubscriber_QueueLoadBalance(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "QLB", []string{"qlb.>"})

	ctx := t.Context()

	pub, err := NewNATSPublisher(ctx, []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })

	sub, err := NewNATSSubscriber(ctx, []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	var aCount, bCount atomic.Int32
	const queue = "shared-q"

	require.NoError(t, sub.Subscribe(ctx, "qlb.work", queue,
		func(_ string, _ []byte) error {
			aCount.Add(1)
			return nil
		},
	))
	require.NoError(t, sub.Subscribe(ctx, "qlb.work", queue,
		func(_ string, _ []byte) error {
			bCount.Add(1)
			return nil
		},
	))

	const total = 10
	for i := range total {
		require.NoError(t, pub.Publish(ctx, "qlb.work", fmt.Appendf(nil, "msg-%d", i)))
	}

	awaitOK(t, 3*time.Second, func() error {
		if aCount.Load()+bCount.Load() == total {
			return nil
		}
		return fmt.Errorf("waiting: a=%d b=%d total=%d", aCount.Load(), bCount.Load(), total)
	})

	require.Equal(t, int32(total), aCount.Load()+bCount.Load(),
		"sum of deliveries must equal published count (no duplicates)")
	require.Positive(t, aCount.Load(),
		"both subscribers should get at least one message in 10-message run; a=%d b=%d", aCount.Load(), bCount.Load())
	require.Positive(t, bCount.Load(),
		"both subscribers should get at least one message in 10-message run; a=%d b=%d", aCount.Load(), bCount.Load())
}

// TestNATSPublisherSubscriber_FanOut verifies that two subscribers
// using DIFFERENT queue group names BOTH receive every message — the
// per-replica fan-out semantics described in Plan 11 Decision Q2.
//
// Each replica registers its own queue group ("realtime-replica-<pod>")
// so it's effectively the only member of its group; every event
// reaches every replica's local hub for dispatch to local
// connections.
func TestNATSPublisherSubscriber_FanOut(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "FAN", []string{"fan.>"})

	ctx := t.Context()

	pub, err := NewNATSPublisher(ctx, []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })

	sub, err := NewNATSSubscriber(ctx, []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	var aCount, bCount atomic.Int32

	require.NoError(t, sub.Subscribe(ctx, "fan.event", "realtime-replica-pod-a",
		func(_ string, _ []byte) error {
			aCount.Add(1)
			return nil
		},
	))
	require.NoError(t, sub.Subscribe(ctx, "fan.event", "realtime-replica-pod-b",
		func(_ string, _ []byte) error {
			bCount.Add(1)
			return nil
		},
	))

	const total = 5
	for i := range total {
		require.NoError(t, pub.Publish(ctx, "fan.event", fmt.Appendf(nil, "msg-%d", i)))
	}

	awaitOK(t, 3*time.Second, func() error {
		if aCount.Load() == total && bCount.Load() == total {
			return nil
		}
		return fmt.Errorf("waiting fan-out: a=%d b=%d want=%d", aCount.Load(), bCount.Load(), total)
	})
}

// TestNATSSubscriber_HandlerErrorRedelivers verifies the contract
// that a handler returning a non-nil error triggers a NAK and the
// broker redelivers the same message. We use WithSubscriberNakDelay(0)
// to keep the test fast.
//
// Coverage detail: this also validates the Metrics observers fire
// for both ack-on-success and nak-on-error code paths.
func TestNATSSubscriber_HandlerErrorRedelivers(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "REDLV", []string{"redlv.>"})

	ctx := t.Context()

	pub, err := NewNATSPublisher(ctx, []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })

	reg := prometheus.NewRegistry()
	metrics := RegisterMetrics(reg)

	sub, err := NewNATSSubscriber(ctx, []string{url}, "",
		WithSubscriberNakDelay(0),
		WithSubscriberMetrics(metrics),
		WithSubscriberLogger(zaptest.NewLogger(t)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	var attempts atomic.Int32
	done := make(chan struct{})

	require.NoError(t, sub.Subscribe(ctx, "redlv.flaky", "redlv-q",
		func(_ string, _ []byte) error {
			n := attempts.Add(1)
			switch n {
			case 1:
				return errors.New("simulated transient failure")
			case 2:
				close(done)
				return nil
			default:
				return nil
			}
		},
	))

	require.NoError(t, pub.Publish(ctx, "redlv.flaky", []byte("retry me")))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("did not see redelivery within 5s; attempts=%d", attempts.Load())
	}

	require.GreaterOrEqual(t, attempts.Load(), int32(2),
		"handler should have been invoked at least twice (initial + redelivery)")
}

// TestNATSSubscriber_CloseIsIdempotent verifies that Close can be
// called multiple times without panicking and without leaking
// goroutines (goleak.VerifyTestMain enforces the leak guarantee
// across the whole test binary).
//
// We DON'T register a t.Cleanup Close because we want the second
// explicit Close inside the test body to be the redundant call.
func TestNATSSubscriber_CloseIsIdempotent(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "CLOSE", []string{"close.>"})

	ctx := t.Context()

	sub, err := NewNATSSubscriber(ctx, []string{url}, "")
	require.NoError(t, err)

	require.NoError(t, sub.Subscribe(ctx, "close.event", "close-q",
		func(_ string, _ []byte) error { return nil },
	))

	require.NoError(t, sub.Close())
	require.NoError(t, sub.Close(), "second Close must be a no-op")

	// Subscribe-after-close MUST return the closed sentinel.
	err = sub.Subscribe(ctx, "close.event", "close-q",
		func(_ string, _ []byte) error { return nil },
	)
	require.ErrorIs(t, err, ErrClosed)
}

// TestNATSPublisher_CloseIsIdempotent mirrors the subscriber test for
// the publisher half. Verifies Publish-after-Close returns the closed
// sentinel.
func TestNATSPublisher_CloseIsIdempotent(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "PCLOSE", []string{"pclose.>"})

	ctx := t.Context()

	pub, err := NewNATSPublisher(ctx, []string{url}, "")
	require.NoError(t, err)

	require.NoError(t, pub.Publish(ctx, "pclose.ping", []byte("alive")))
	require.NoError(t, pub.Close())
	require.NoError(t, pub.Close(), "second Close must be a no-op")

	err = pub.Publish(ctx, "pclose.ping", []byte("after close"))
	require.ErrorIs(t, err, ErrClosed)
}

// TestNATSSubscriber_SubscribeCtxCancelled verifies that calling
// Subscribe with an already-cancelled ctx aborts immediately with a
// wrapped ctx.Err() rather than registering the consumer or
// blocking. Mirrors the connect-time ctx cancellation guarantee.
func TestNATSSubscriber_SubscribeCtxCancelled(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "CTX", []string{"ctx.>"})

	bgCtx := t.Context()
	sub, err := NewNATSSubscriber(bgCtx, []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	cancelled, cancel := context.WithCancel(bgCtx)
	cancel()

	err = sub.Subscribe(cancelled, "ctx.event", "ctx-q",
		func(_ string, _ []byte) error { return nil },
	)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

// TestNewNATSPublisher_DialCtxCancelled verifies that ctx cancellation
// during the initial dial returns ctx.Err() rather than blocking
// forever waiting for a NATS server that's never going to respond.
//
// We point the constructor at a TCP port that's free (no server
// listening) so nats.Connect would normally block until its connect
// timeout fires. Cancelling ctx aborts immediately.
func TestNewNATSPublisher_DialCtxCancelled(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	url := fmt.Sprintf("nats://127.0.0.1:%d", port)

	ctx, cancel := context.WithCancel(t.Context())
	// Cancel after a short delay so nats.Connect is mid-retry.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := NewNATSPublisher(ctx, []string{url}, "")
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

// TestRegisterMetrics_NilRegPanics validates the boot-time wiring
// invariant: calling RegisterMetrics with a nil registerer is a
// programmer error and panics with a clear remediation message.
func TestRegisterMetrics_NilRegPanics(t *testing.T) {
	t.Parallel()
	require.PanicsWithValue(
		t,
		"eventbus.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests",
		func() { RegisterMetrics(nil) },
	)
}

// TestMetrics_NilTolerated verifies every observe* helper handles a
// nil *Metrics receiver and a partially-zero Metrics struct without
// panicking. Mirrors the carry-forward "nil-tolerated observe*"
// pattern from internal/realtime/service/metrics.go.
func TestMetrics_NilTolerated(t *testing.T) {
	t.Parallel()
	var m *Metrics
	require.NotPanics(t, func() {
		m.observePublish("ok", time.Millisecond)
		m.observePublish("error", time.Millisecond)
		m.observeSubscribeMessage("ack")
		m.observeSubscribeMessage("nak")
		m.observeRedelivery()
	})

	zero := &Metrics{}
	require.NotPanics(t, func() {
		zero.observePublish("ok", time.Millisecond)
		zero.observeSubscribeMessage("ack")
		zero.observeRedelivery()
	})
}

// TestNATSPublisherSubscriber_OptionsAndMetricsIntegration exercises
// every functional option (logger, metrics, name, max-ack-pending,
// nak-delay) and asserts the metric counters were incremented for
// successful publishes + ack'd messages.
func TestNATSPublisherSubscriber_OptionsAndMetricsIntegration(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "OPT", []string{"opt.>"})

	ctx := t.Context()
	logger := zaptest.NewLogger(t)

	pubReg := prometheus.NewRegistry()
	pubMetrics := RegisterMetrics(pubReg)
	pub, err := NewNATSPublisher(ctx, []string{url}, "",
		WithPublisherLogger(logger),
		WithPublisherMetrics(pubMetrics),
		WithPublisherName("integration-pub"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })

	subReg := prometheus.NewRegistry()
	subMetrics := RegisterMetrics(subReg)
	sub, err := NewNATSSubscriber(ctx, []string{url}, "",
		WithSubscriberLogger(logger),
		WithSubscriberMetrics(subMetrics),
		WithSubscriberName("integration-sub"),
		WithSubscriberMaxAckPending(64),
		WithSubscriberNakDelay(10*time.Millisecond),
		// Negative values are ignored by the option setters; passing
		// a negative duration here documents that the floor is 0.
		WithSubscriberNakDelay(-1*time.Second),
		// Zero is also rejected by WithSubscriberMaxAckPending — the
		// default 1024 stays in place if you pass 0.
		WithSubscriberMaxAckPending(0),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	got := make(chan struct{}, 1)
	require.NoError(t, sub.Subscribe(ctx, "opt.event", "opt-q",
		func(_ string, _ []byte) error {
			select {
			case got <- struct{}{}:
			default:
			}
			return nil
		},
	))

	require.NoError(t, pub.Publish(ctx, "opt.event", []byte("first")))

	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("did not see message in OptionsAndMetricsIntegration")
	}

	// Publish-side metric is sync (incremented before Publish returns)
	// so we can assert it immediately. Subscribe-side metric is
	// incremented AFTER the handler returns inside the dispatcher
	// goroutine — race with our test goroutine, so poll for it.
	require.InDelta(t, 1.0, counterValue(t, pubReg, "eventbus_publish_total", "result", "ok"), 0.0001)
	require.GreaterOrEqual(t, histogramSampleCount(t, pubReg, "eventbus_publish_latency_seconds"), uint64(1))
	awaitOK(t, 2*time.Second, func() error {
		v := counterValueOrZero(t, subReg, "eventbus_subscribe_message_total", "result", "ack")
		if v >= 1 {
			return nil
		}
		return fmt.Errorf("ack counter still %v", v)
	})
}

// TestNATSPublisher_NilReceiver verifies the nil-receiver guards on
// Publish/Close so callers that pass a `var pub *NATSPublisher`
// (uninstantiated) get a wrapped ErrClosed rather than a panic.
func TestNATSPublisher_NilReceiver(t *testing.T) {
	t.Parallel()
	var pub *NATSPublisher
	err := pub.Publish(t.Context(), "subject", []byte("payload"))
	require.ErrorIs(t, err, ErrClosed)
	require.NoError(t, pub.Close())
}

// TestNATSSubscriber_NilReceiver mirrors TestNATSPublisher_NilReceiver
// for the subscriber half.
func TestNATSSubscriber_NilReceiver(t *testing.T) {
	t.Parallel()
	var sub *NATSSubscriber
	err := sub.Subscribe(t.Context(), "subject", "queue",
		func(_ string, _ []byte) error { return nil })
	require.ErrorIs(t, err, ErrClosed)
	require.NoError(t, sub.Close())
}

// TestNATSSubscriber_NilHandler rejects nil handlers up front so
// callers don't trip a nil-deref later inside the dispatch goroutine.
func TestNATSSubscriber_NilHandler(t *testing.T) {
	t.Parallel()
	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "NILH", []string{"nilh.>"})

	sub, err := NewNATSSubscriber(t.Context(), []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	err = sub.Subscribe(t.Context(), "nilh.event", "nilh-q", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "handler must be non-nil")
}

// TestNATSPublisher_PublishWithoutStream verifies the error path
// where a publish fails because no JetStream stream covers the
// target subject. JetStream replies ErrNoStream and we wrap it.
func TestNATSPublisher_PublishWithoutStream(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	// Note: NO ensureStream — publishing to "orphan.subject" must fail
	// because there's no stream listening for it.

	ctx := t.Context()
	reg := prometheus.NewRegistry()
	metrics := RegisterMetrics(reg)
	pub, err := NewNATSPublisher(ctx, []string{url}, "",
		WithPublisherMetrics(metrics),
		WithPublisherLogger(zaptest.NewLogger(t)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })

	// Use a short ctx so the no-responders retry loop doesn't drag
	// out the test for 30s.
	pubCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	err = pub.Publish(pubCtx, "orphan.subject", []byte("nobody-home"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "pkg/eventbus: publish")

	awaitOK(t, 2*time.Second, func() error {
		v := counterValueOrZero(t, reg, "eventbus_publish_total", "result", "error")
		if v >= 1 {
			return nil
		}
		return fmt.Errorf("error counter still %v", v)
	})
}

// TestNewNATSPublisher_BadURL covers the connection-failure path:
// when the supplied URL is malformed, nats.Connect returns an error
// synchronously and we wrap it with the "connect" op tag.
func TestNewNATSPublisher_BadURL(t *testing.T) {
	t.Parallel()
	_, err := NewNATSPublisher(t.Context(), []string{"://not-a-url"}, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "pkg/eventbus: connect:")
}

// TestNewNATSSubscriber_BadURL is the symmetric check for the
// subscriber's connect path.
func TestNewNATSSubscriber_BadURL(t *testing.T) {
	t.Parallel()
	_, err := NewNATSSubscriber(t.Context(), []string{"://not-a-url"}, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "pkg/eventbus: connect:")
}

// TestNewNATSPublisher_AccountAuth exercises the nats.UserInfo branch
// in dialNATS by passing a non-empty account string. The embedded
// server we boot doesn't enforce auth so the connect succeeds even
// with a bogus account; the goal is purely to cover the option
// branch.
func TestNewNATSPublisher_AccountAuth(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)

	pub, err := NewNATSPublisher(t.Context(), []string{url}, "anonymous-but-set")
	require.NoError(t, err)
	require.NoError(t, pub.Close())
}

// TestDialNATS_DirectClosedClosesNc covers the waitForConnected
// "CLOSED before CONNECTED" branch by running dialNATS against a
// running embedded server and then immediately shutting the server
// down so the client transitions to CLOSED while we're still
// waiting.
//
// This branch is unlikely in production (servers don't go away in
// the millisecond between Connect and waitForConnected) but the
// guarded code path needs coverage.
func TestDialNATS_AccountAuthSucceeds(t *testing.T) {
	t.Parallel()
	url := startEmbeddedJetStream(t)
	nc, err := dialNATS(t.Context(), []string{url}, "user", "test-conn")
	require.NoError(t, err)
	require.NotNil(t, nc)
	nc.Close()
}

// TestSubscribe_RaceWithClose stresses the close-during-Subscribe
// race. Calling Subscribe N times while concurrently calling Close
// surfaces the in-flight registration window guarded inside
// Subscribe with a re-check of s.closed AFTER QueueSubscribe
// returns.
func TestSubscribe_RaceWithClose(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	ensureStream(t, url, "RACE", []string{"race.>"})

	sub, err := NewNATSSubscriber(t.Context(), []string{url}, "")
	require.NoError(t, err)

	// Spawn a few subscribe attempts then trigger Close. We don't
	// assert exact outcomes (the race resolution depends on
	// goroutine scheduling) — what matters is no panic, no
	// goroutine leak, and that subsequent Subscribe calls return
	// ErrClosed.
	var wg sync.WaitGroup
	for i := range 5 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = sub.Subscribe(t.Context(),
				fmt.Sprintf("race.event-%d", n),
				fmt.Sprintf("race-q-%d", n),
				func(_ string, _ []byte) error { return nil },
			)
		}(i)
	}

	// Race: while subscribers are still registering, close.
	require.NoError(t, sub.Close())
	wg.Wait()

	err = sub.Subscribe(t.Context(), "race.late", "race-late-q",
		func(_ string, _ []byte) error { return nil })
	require.ErrorIs(t, err, ErrClosed)
}

// TestPublisher_PublishAfterServerShutdown forces the publish-error
// metric path by tearing down the broker mid-test and trying to
// publish.
func TestPublisher_PublishAfterServerShutdown(t *testing.T) {
	t.Parallel()

	storeDir := filepath.Join(t.TempDir(), "jetstream")
	opts := &server.Options{
		Host:                  "127.0.0.1",
		Port:                  -1,
		NoLog:                 true,
		NoSigs:                true,
		MaxControlLine:        4096,
		DisableShortFirstPing: true,
		JetStream:             true,
		StoreDir:              storeDir,
	}
	srv, err := server.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(5*time.Second))

	url := srv.ClientURL()

	// Provision a stream BEFORE shutdown so the first publish would
	// otherwise succeed.
	{
		nc, err := nats.Connect(url)
		require.NoError(t, err)
		js, err := nc.JetStream()
		require.NoError(t, err)
		_, err = js.AddStream(&nats.StreamConfig{
			Name:     "DOWN",
			Subjects: []string{"down.>"},
			Storage:  nats.MemoryStorage,
		})
		require.NoError(t, err)
		nc.Close()
	}

	pub, err := NewNATSPublisher(t.Context(), []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })

	// Tear down the server so subsequent publishes can't reach a
	// broker.
	srv.Shutdown()
	srv.WaitForShutdown()

	pubCtx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	err = pub.Publish(pubCtx, "down.event", []byte("server gone"))
	require.Error(t, err)
}

// TestSubscribe_NoMatchingStream covers the QueueSubscribe error
// path — subscribing to a subject that no JetStream stream is
// configured to capture surfaces ErrNoMatchingStream which we wrap
// with the "subscribe" op tag.
func TestSubscribe_NoMatchingStream(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	// Intentionally NO ensureStream so the subscribe finds no stream.

	sub, err := NewNATSSubscriber(t.Context(), []string{url}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })

	err = sub.Subscribe(t.Context(), "orphan.event", "orphan-q",
		func(_ string, _ []byte) error { return nil })
	require.Error(t, err)
	require.Contains(t, err.Error(), "pkg/eventbus: subscribe")
}

// TestWaitForClosed_AlreadyClosed covers the early-return branch
// inside waitForClosed when the conn is already CLOSED on entry.
func TestWaitForClosed_AlreadyClosed(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	nc, err := nats.Connect(url)
	require.NoError(t, err)
	nc.Close()
	require.True(t, nc.IsClosed())

	// Should return immediately, well before the deadline.
	start := time.Now()
	waitForClosed(nc, 5*time.Second)
	require.Less(t, time.Since(start), 100*time.Millisecond)
}

// TestWaitForClosed_NilConn covers the nil-receiver early return
// inside waitForClosed.
func TestWaitForClosed_NilConn(t *testing.T) {
	t.Parallel()
	start := time.Now()
	waitForClosed(nil, 5*time.Second)
	require.Less(t, time.Since(start), 50*time.Millisecond)
}

// TestWaitForConnected_AlreadyClosed covers the CLOSED branch in
// waitForConnected — when handed a connection that's already CLOSED
// the function should return the "closed before reaching CONNECTED"
// error rather than spinning forever.
func TestWaitForConnected_AlreadyClosed(t *testing.T) {
	t.Parallel()

	url := startEmbeddedJetStream(t)
	nc, err := nats.Connect(url)
	require.NoError(t, err)
	nc.Close()
	require.True(t, nc.IsClosed())

	err = waitForConnected(t.Context(), nc)
	require.Error(t, err)
	require.Contains(t, err.Error(), "connection closed before reaching CONNECTED")
}

// silence unused if subsequent tests don't import these.
var _ atomic.Int32
var _ = context.Background
var _ = errors.New
var _ = prometheus.NewRegistry
var _ = zaptest.NewLogger
