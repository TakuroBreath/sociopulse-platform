package eventbus

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// errClosed is returned by Publish/Subscribe after the owning struct
// has been Close'd. Callers can errors.Is against ErrClosed to
// distinguish lifecycle errors from transport errors.
var errClosed = errors.New("eventbus: closed")

// ErrClosed is the sentinel exposed to callers that need to branch on
// "the bus is shutting down" vs a transient publish/subscribe error.
var ErrClosed = errClosed

// NATSPublisher is the JetStream-backed implementation of Publisher.
// One *nats.Conn is shared across all Publish calls; nats.go is
// goroutine-safe so concurrent Publish from multiple modules is fine.
type NATSPublisher struct {
	nc     *nats.Conn
	js     nats.JetStreamContext
	logger *zap.Logger
	m      *Metrics

	closeMu sync.Mutex
	closed  bool
}

// var compile-time interface check; mirrors the one in main_test.go so
// the production source rejects accidental signature drift even when
// tests aren't built.
var _ Publisher = (*NATSPublisher)(nil)

// PublisherOption tweaks NATSPublisher construction without bloating
// the constructor signature. Functional-options pattern (Plan 09/10
// carry-forward).
type PublisherOption func(*publisherOptions)

type publisherOptions struct {
	logger  *zap.Logger
	metrics *Metrics
	name    string
}

// WithPublisherLogger plugs a zap logger into the publisher. Default
// is zap.NewNop() so callers that don't care never see log noise.
func WithPublisherLogger(l *zap.Logger) PublisherOption {
	return func(o *publisherOptions) { o.logger = l }
}

// WithPublisherMetrics attaches a previously-registered *Metrics
// bundle. nil-tolerated in observe* helpers, so passing nil is
// equivalent to not calling this at all.
func WithPublisherMetrics(m *Metrics) PublisherOption {
	return func(o *publisherOptions) { o.metrics = m }
}

// WithPublisherName sets the connection Name reported to nats-server
// (visible in `nats server report connections`). Defaults to
// "sociopulse-publisher".
func WithPublisherName(name string) PublisherOption {
	return func(o *publisherOptions) { o.name = name }
}

// NewNATSPublisher constructs a Publisher backed by a NATS JetStream
// connection. urls is a list of cluster endpoints (any one works for
// bootstrap; the client discovers the rest via INFO frames). account
// is the user name passed via nats.UserInfo — empty string disables
// credentialed auth.
//
// ctx is honoured during the initial connect (cancellation aborts the
// dial). Once the connection is established the lifetime is governed
// by Close().
func NewNATSPublisher(ctx context.Context, urls []string, account string, opts ...PublisherOption) (*NATSPublisher, error) {
	o := publisherOptions{
		logger: zap.NewNop(),
		name:   "sociopulse-publisher",
	}
	for _, opt := range opts {
		opt(&o)
	}

	nc, err := dialNATS(ctx, urls, account, o.name)
	if err != nil {
		return nil, fmt.Errorf("pkg/eventbus: connect: %w", err)
	}
	js, err := nc.JetStream()
	if err != nil {
		_ = nc.Drain()
		return nil, fmt.Errorf("pkg/eventbus: jetstream: %w", err)
	}

	return &NATSPublisher{
		nc:     nc,
		js:     js,
		logger: o.logger,
		m:      o.metrics,
	}, nil
}

// Publish satisfies the Publisher interface. It performs a synchronous
// JetStream publish — the broker ACKs (or NAKs) before this returns.
// ctx cancellation aborts the in-flight request.
func (p *NATSPublisher) Publish(ctx context.Context, subject string, payload []byte) error {
	if p == nil {
		return fmt.Errorf("pkg/eventbus: publish: %w", errClosed)
	}
	p.closeMu.Lock()
	closed := p.closed
	p.closeMu.Unlock()
	if closed {
		return fmt.Errorf("pkg/eventbus: publish: %w", errClosed)
	}

	start := time.Now()
	msg := &nats.Msg{Subject: subject, Data: payload}
	_, err := p.js.PublishMsg(msg, nats.Context(ctx))
	dur := time.Since(start)
	if err != nil {
		p.m.observePublish("error", dur)
		// Logged at debug to avoid leaking subject names containing
		// PII (tenant IDs, dialer-op IDs) into info-level logs.
		p.logger.Debug("eventbus: publish failed",
			zap.String("subject", subject),
			zap.Int("payload_bytes", len(payload)),
			zap.Error(err),
		)
		return fmt.Errorf("pkg/eventbus: publish %q: %w", subject, err)
	}
	p.m.observePublish("ok", dur)
	return nil
}

// Close drains the publisher and closes the underlying connection.
// Safe to call multiple times — subsequent calls are no-ops.
//
// Drain returns immediately and runs the actual drain in a background
// goroutine. To honour the goleak guarantee we wait for the
// connection to enter the CLOSED state before returning.
func (p *NATSPublisher) Close() error {
	if p == nil {
		return nil
	}
	p.closeMu.Lock()
	if p.closed {
		p.closeMu.Unlock()
		return nil
	}
	p.closed = true
	p.closeMu.Unlock()

	if err := p.nc.Drain(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
		return fmt.Errorf("pkg/eventbus: publisher drain: %w", err)
	}
	waitForClosed(p.nc, drainTimeout)
	return nil
}

// NATSSubscriber is the JetStream-backed implementation of Subscriber.
// It owns one *nats.Conn and a slice of registered *nats.Subscription
// values; Close unsubscribes them all and drains the connection.
type NATSSubscriber struct {
	nc     *nats.Conn
	js     nats.JetStreamContext
	logger *zap.Logger
	m      *Metrics

	// Constructor-time settings; immutable after construction.
	maxAckPending int
	nakDelay      time.Duration

	mu     sync.Mutex
	subs   []*nats.Subscription
	closed bool
}

var _ Subscriber = (*NATSSubscriber)(nil)

// SubscriberOption tweaks NATSSubscriber construction.
type SubscriberOption func(*subscriberOptions)

type subscriberOptions struct {
	logger        *zap.Logger
	metrics       *Metrics
	name          string
	maxAckPending int
	nakDelay      time.Duration
}

// WithSubscriberLogger plugs a zap logger into the subscriber.
func WithSubscriberLogger(l *zap.Logger) SubscriberOption {
	return func(o *subscriberOptions) { o.logger = l }
}

// WithSubscriberMetrics attaches a Metrics bundle.
func WithSubscriberMetrics(m *Metrics) SubscriberOption {
	return func(o *subscriberOptions) { o.metrics = m }
}

// WithSubscriberName sets the nats-server-visible connection name.
func WithSubscriberName(name string) SubscriberOption {
	return func(o *subscriberOptions) { o.name = name }
}

// WithSubscriberMaxAckPending overrides the default 1024 from
// Plan 11 Decision Q2. Lower values throttle in-flight messages per
// consumer; higher values allow burstier delivery.
func WithSubscriberMaxAckPending(n int) SubscriberOption {
	return func(o *subscriberOptions) {
		if n > 0 {
			o.maxAckPending = n
		}
	}
}

// WithSubscriberNakDelay sets the redelivery delay used when the user
// handler returns an error. Default is 250ms; setting to 0 means
// "redeliver immediately" (Nak() with no delay).
func WithSubscriberNakDelay(d time.Duration) SubscriberOption {
	return func(o *subscriberOptions) {
		if d >= 0 {
			o.nakDelay = d
		}
	}
}

// NewNATSSubscriber constructs a Subscriber backed by a NATS JetStream
// connection. Same auth + connect semantics as NewNATSPublisher.
func NewNATSSubscriber(ctx context.Context, urls []string, account string, opts ...SubscriberOption) (*NATSSubscriber, error) {
	o := subscriberOptions{
		logger:        zap.NewNop(),
		name:          "sociopulse-subscriber",
		maxAckPending: 1024, // Plan 11 Decision Q2
		nakDelay:      250 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(&o)
	}

	nc, err := dialNATS(ctx, urls, account, o.name)
	if err != nil {
		return nil, fmt.Errorf("pkg/eventbus: connect: %w", err)
	}
	js, err := nc.JetStream()
	if err != nil {
		_ = nc.Drain()
		return nil, fmt.Errorf("pkg/eventbus: jetstream: %w", err)
	}

	return &NATSSubscriber{
		nc:            nc,
		js:            js,
		logger:        o.logger,
		m:             o.metrics,
		maxAckPending: o.maxAckPending,
		nakDelay:      o.nakDelay,
	}, nil
}

// Subscribe registers handler on subject within the named queue group
// as a JetStream push consumer. Per Plan 11 Decision Q2 the consumer
// uses MaxAckPending=1024, AckExplicit, DeliverNew, and a durable
// name derived from (queue, subject) so re-subscribing the same
// (queue, subject) pair after a restart resumes the same consumer.
//
// The user handler runs on a goroutine owned by the nats.go push
// dispatcher. Returning nil triggers Ack; returning an error triggers
// NakWithDelay (configurable via WithSubscriberNakDelay) so the
// broker schedules a redelivery.
func (s *NATSSubscriber) Subscribe(ctx context.Context, subject, queue string, handler func(subject string, payload []byte) error) error {
	if s == nil {
		return fmt.Errorf("pkg/eventbus: subscribe: %w", errClosed)
	}
	if handler == nil {
		return fmt.Errorf("pkg/eventbus: subscribe: handler must be non-nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("pkg/eventbus: subscribe: %w", err)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("pkg/eventbus: subscribe: %w", errClosed)
	}
	s.mu.Unlock()

	durable := durableNameFor(queue, subject)
	msgHandler := func(m *nats.Msg) {
		err := handler(m.Subject, m.Data)
		if err != nil {
			s.m.observeSubscribeMessage("nak")
			s.m.observeRedelivery()
			s.logger.Debug("eventbus: handler returned error, NAKing",
				zap.String("subject", m.Subject),
				zap.Error(err),
			)
			if nakErr := m.NakWithDelay(s.nakDelay); nakErr != nil {
				s.logger.Debug("eventbus: nak failed",
					zap.String("subject", m.Subject),
					zap.Error(nakErr),
				)
			}
			return
		}
		s.m.observeSubscribeMessage("ack")
		if ackErr := m.Ack(); ackErr != nil {
			s.logger.Debug("eventbus: ack failed",
				zap.String("subject", m.Subject),
				zap.Error(ackErr),
			)
		}
	}

	subOpts := []nats.SubOpt{
		nats.Durable(durable),
		nats.AckExplicit(),
		nats.DeliverNew(),
		nats.MaxAckPending(s.maxAckPending),
		nats.ManualAck(),
	}

	sub, err := s.js.QueueSubscribe(subject, queue, msgHandler, subOpts...)
	if err != nil {
		return fmt.Errorf("pkg/eventbus: subscribe %q (queue %q): %w", subject, queue, err)
	}

	s.mu.Lock()
	if s.closed {
		// Race: Close fired between the closed-check and AddSubscription.
		// Unsubscribe ourselves so we don't leak the consumer.
		s.mu.Unlock()
		_ = sub.Unsubscribe()
		return fmt.Errorf("pkg/eventbus: subscribe: %w", errClosed)
	}
	s.subs = append(s.subs, sub)
	s.mu.Unlock()

	return nil
}

// Close stops all consumers and closes the underlying connection.
// Idempotent; subsequent calls are no-ops.
//
// We rely on nc.Drain() to drain every subscription registered on
// the connection — calling sub.Drain() per-subscription first is
// redundant. If the conn was already closed externally, Drain
// returns ErrConnectionClosed and we treat it as success.
func (s *NATSSubscriber) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.subs = nil
	s.mu.Unlock()

	if err := s.nc.Drain(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
		return fmt.Errorf("pkg/eventbus: subscriber drain: %w", err)
	}
	waitForClosed(s.nc, drainTimeout)
	return nil
}

// durableNameFor builds a JetStream consumer durable name from a
// (queue, subject) pair. Durable names cannot contain "." or wildcards
// per nats.go validation, so we hash the subject pattern into a
// stable suffix. Keeping the queue prefix in plain text aids
// debugging in `nats consumer ls`.
func durableNameFor(queue, subject string) string {
	clean := strings.ReplaceAll(queue, ".", "_")
	h := fnv.New64a()
	_, _ = h.Write([]byte(subject))
	return fmt.Sprintf("%s-%x", clean, h.Sum64())
}

// drainTimeout caps the amount of time Close will wait for the NATS
// connection to transition to CLOSED. nats.go's Drain runs async; the
// drain itself is bounded by the conn-level DrainTimeout (default
// 30s) but we add a hard ceiling here as defence-in-depth.
const drainTimeout = 35 * time.Second

// waitForClosed blocks until nc.IsClosed() is true or the deadline
// expires. We poll rather than registering a status listener so the
// call is safe regardless of what state the connection was in when
// Close was invoked (listeners would race against the state
// transition).
//
// On deadline expiry we hard-close the connection as a defensive
// measure so the underlying goroutine doesn't leak past the goleak
// check.
func waitForClosed(nc *nats.Conn, deadline time.Duration) {
	if nc == nil || nc.IsClosed() {
		return
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	stop := time.NewTimer(deadline)
	defer stop.Stop()
	for {
		select {
		case <-tick.C:
			if nc.IsClosed() {
				return
			}
		case <-stop.C:
			nc.Close()
			return
		}
	}
}

// dialNATS opens a NATS connection honouring ctx during the dial.
// account, when non-empty, drives nats.UserInfo (single-user auth).
//
// Production hardening: retry on failed connect, unbounded
// reconnects with 2s wait, name set so server-side dashboards can
// identify the role. With RetryOnFailedConnect(true) nats.Connect
// returns optimistically before the TCP handshake completes; we
// therefore poll the connection status (with ctx awareness) until it
// reports CONNECTED so callers see a usable handle on success.
func dialNATS(ctx context.Context, urls []string, account, name string) (*nats.Conn, error) {
	url := strings.Join(urls, ",")
	opts := []nats.Option{
		nats.Name(name),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
		nats.Timeout(5 * time.Second),
	}
	if account != "" {
		opts = append(opts, nats.UserInfo(account, ""))
	}

	type result struct {
		nc  *nats.Conn
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		nc, err := nats.Connect(url, opts...)
		resCh <- result{nc: nc, err: err}
	}()

	var nc *nats.Conn
	select {
	case <-ctx.Done():
		// If the dial completes after ctx cancellation, drain the
		// returned connection in the background so we don't leak it.
		go func() {
			r := <-resCh
			if r.nc != nil {
				r.nc.Close()
			}
		}()
		return nil, ctx.Err()
	case r := <-resCh:
		if r.err != nil {
			return nil, r.err
		}
		nc = r.nc
	}

	// nats.Connect with RetryOnFailedConnect returns before the TCP
	// handshake completes. Wait for CONNECTED, ctx, or repeated
	// failure (the connection moves to CLOSED if all retries exhaust
	// without cancellation, but with MaxReconnects=-1 that doesn't
	// happen — only ctx terminates the wait early).
	if err := waitForConnected(ctx, nc); err != nil {
		nc.Close()
		return nil, err
	}
	return nc, nil
}

// waitForConnected polls nc.Status() until CONNECTED is observed or
// ctx fires. The poll interval is short (10ms) because in the happy
// path the status flips within milliseconds of nats.Connect
// returning; we don't pay for the polling cost in production.
//
// CLOSED is treated as terminal (the connection failed and won't
// recover); all other intermediate states (DISCONNECTED, CONNECTING,
// RECONNECTING, DRAINING_SUBS, DRAINING_PUBS) are non-terminal — we
// keep polling.
func waitForConnected(ctx context.Context, nc *nats.Conn) error {
	if nc.Status() == nats.CONNECTED {
		return nil
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			st := nc.Status()
			if st == nats.CONNECTED {
				return nil
			}
			if st == nats.CLOSED {
				return errors.New("nats: connection closed before reaching CONNECTED")
			}
			// DISCONNECTED, CONNECTING, RECONNECTING,
			// DRAINING_SUBS, DRAINING_PUBS: keep polling.
		}
	}
}
