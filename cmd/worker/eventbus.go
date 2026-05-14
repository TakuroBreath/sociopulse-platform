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
// before falling back to "ingest disabled" mode. Short on purpose —
// cmd/worker's other daemons (dialer retry, recording sweeps) must
// keep booting even when NATS is down. Mirrors cmd/api's choice.
const natsConnectTimeout = time.Second

// openNATS opens publisher + subscriber against cfg.NATS. Best-effort
// (mirrors cmd/api's openNATS): a connection failure is logged at WARN
// by the caller and analytics ingest is skipped. ctx is honoured
// during the dial; timeoutCtx caps the connect at natsConnectTimeout
// so a slow broker doesn't block boot beyond the documented budget.
//
// Returns (publisher, subscriber, error). On error, both publisher and
// subscriber are nil and the caller MUST NOT defer Close on either.
// On success, both are non-nil and the caller defers Close on each
// (subscriber first, then publisher — mirrors cmd/api's order).
//
// cmd/worker today uses the subscriber for analytics ingest; the
// publisher is returned for symmetry with cmd/api and is currently
// unused. Future cross-binary publishers (e.g. retry-pass events)
// can pick it up without re-deriving the helper.
func openNATS(ctx context.Context, cfg config.Config, logger *zap.Logger) (*eventbus.NATSPublisher, *eventbus.NATSSubscriber, error) {
	dialCtx, cancel := context.WithTimeout(ctx, natsConnectTimeout)
	defer cancel()

	pub, err := eventbus.NewNATSPublisher(dialCtx, cfg.NATS.URLs, cfg.NATS.Account,
		eventbus.WithPublisherLogger(logger.Named("publisher")),
		eventbus.WithPublisherName("cmd-worker-publisher"),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("cmd/worker: nats publisher: %w", err)
	}

	sub, err := eventbus.NewNATSSubscriber(dialCtx, cfg.NATS.URLs, cfg.NATS.Account,
		eventbus.WithSubscriberLogger(logger.Named("subscriber")),
		eventbus.WithSubscriberName("cmd-worker-subscriber"),
	)
	if err != nil {
		// Drain the half-open publisher so we don't leak the
		// connection across the fallback path.
		_ = pub.Close()
		return nil, nil, fmt.Errorf("cmd/worker: nats subscriber: %w", err)
	}

	return pub, sub, nil
}

// redactNATSURLs returns a slice of URLs with credential segments
// stripped so boot logs can be safely shipped to a log aggregator.
// Best-effort: an unparseable URL falls back to the original string
// since the connect error already surfaces a parse failure. Mirrors
// cmd/api's redactNATSURLs.
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
		return raw
	}
	if u.User != nil {
		u.User = url.User(strings.Repeat("*", 3))
	}
	return u.String()
}
