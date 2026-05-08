package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/pkg/config"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// natsConnectTimeout caps the boot-time wait for a NATS connection
// before falling back to the noop publisher/subscriber. Short on
// purpose — we don't want to block startup on an unreachable broker
// in dev/test. 1s comfortably covers a healthy intra-AZ NATS dial
// (sub-10ms typical) while keeping the dev-loop snappy when nothing
// is listening on :4222. Mirrors the Postgres/Redis ping policy.
const natsConnectTimeout = time.Second

// noopPublisher is the dev/test fallback when a NATS broker is
// unavailable at boot. It logs every publish at debug level so
// developers can watch the outbox relay drain locally without a NATS
// cluster running. Plan 11 Task 4c keeps it as the failure-mode
// fallback (the real NATSPublisher is the production path).
type noopPublisher struct {
	logger *zap.Logger
}

// newNoopPublisher returns a Publisher that succeeds for every Publish
// call and logs the subject at debug level.
func newNoopPublisher(logger *zap.Logger) *noopPublisher {
	return &noopPublisher{logger: logger}
}

// Publish satisfies eventbus.Publisher. It always returns nil.
func (p *noopPublisher) Publish(_ context.Context, subject string, payload []byte) error {
	p.logger.Debug("noopPublisher: publish",
		zap.String("subject", subject),
		zap.Int("payload_bytes", len(payload)),
	)
	return nil
}

// Compile-time check that noopPublisher satisfies eventbus.Publisher so
// future interface changes are caught at build time, not at the call
// site in run().
var _ eventbus.Publisher = (*noopPublisher)(nil)

// noopSubscriber satisfies eventbus.Subscriber for the same dev/test
// boot path. It accepts every Subscribe call and never delivers a
// message — perfect for keeping the realtime module's Register happy
// when no broker is reachable. Plan 11 Task 4c hands this to
// realtime.Module so Hub construction proceeds even with no NATS.
type noopSubscriber struct {
	logger *zap.Logger
}

func newNoopSubscriber(logger *zap.Logger) *noopSubscriber {
	return &noopSubscriber{logger: logger}
}

// Subscribe satisfies eventbus.Subscriber. The handler is silently
// discarded — no broker means no inbound messages.
func (s *noopSubscriber) Subscribe(_ context.Context, subject, queue string, _ func(string, []byte) error) error {
	s.logger.Debug("noopSubscriber: subscribe",
		zap.String("subject", subject),
		zap.String("queue", queue),
	)
	return nil
}

var _ eventbus.Subscriber = (*noopSubscriber)(nil)

// openNATS opens publisher + subscriber against cfg.NATS. Best-effort
// (mirrors openRedis): a connection failure is logged at WARN and the
// caller falls back to the noop pair. ctx is honoured during the
// dial; timeoutCtx caps the connect at natsConnectTimeout so a slow
// broker doesn't block boot beyond the documented budget.
//
// Returns (publisher, subscriber, error). On error, both publisher
// and subscriber are nil and the caller MUST NOT defer Close on
// either. On success, both are non-nil and the caller defers Close on
// each (subscriber first, then publisher per the Plan 11 Task 4c
// shutdown order).
func openNATS(ctx context.Context, cfg config.Config, logger *zap.Logger) (*eventbus.NATSPublisher, *eventbus.NATSSubscriber, error) {
	dialCtx, cancel := context.WithTimeout(ctx, natsConnectTimeout)
	defer cancel()

	pub, err := eventbus.NewNATSPublisher(dialCtx, cfg.NATS.URLs, cfg.NATS.Account,
		eventbus.WithPublisherLogger(logger.Named("publisher")),
		eventbus.WithPublisherName("cmd-api-publisher"),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("nats publisher: %w", err)
	}

	sub, err := eventbus.NewNATSSubscriber(dialCtx, cfg.NATS.URLs, cfg.NATS.Account,
		eventbus.WithSubscriberLogger(logger.Named("subscriber")),
		eventbus.WithSubscriberName("cmd-api-subscriber"),
	)
	if err != nil {
		// Drain the half-open publisher so we don't leak the
		// connection across the fallback path.
		_ = pub.Close()
		return nil, nil, fmt.Errorf("nats subscriber: %w", err)
	}

	return pub, sub, nil
}

// redactNATSURLs returns a slice of URLs with credential segments
// stripped so boot logs can be safely shipped to a log aggregator.
// Best-effort: an unparseable URL falls back to the original string
// since the connect error already surfaces a parse failure. Mirrors
// redactDSN in postgres.go.
func redactNATSURLs(urls []string) []string {
	out := make([]string, len(urls))
	for i, raw := range urls {
		out[i] = redactNATSURL(raw)
	}
	return out
}

func redactNATSURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// Leave the URL alone if parsing fails — the connect error
		// already includes the raw form.
		return raw
	}
	if u.User != nil {
		u.User = url.User(strings.Repeat("*", 3))
	}
	return u.String()
}
