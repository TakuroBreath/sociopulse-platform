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
// before the composition root falls back to skipping Bridge.Start. Short
// on purpose — we don't want to block startup on an unreachable broker
// in dev/test. Mirrors cmd/api/eventbus.go's identical constant.
const natsConnectTimeout = time.Second

// openNATS opens publisher + subscriber against cfg.NATS. Best-effort
// (mirrors cmd/api/eventbus.go): a connection failure is logged at WARN
// by the caller and Bridge.Start is skipped. Returns (publisher,
// subscriber, error). On error both are nil; the caller MUST NOT defer
// Close on either. On success both are non-nil and the caller defers
// Close in LIFO order so the bridge tears down before the connections.
func openNATS(ctx context.Context, cfg config.Config, logger *zap.Logger) (*eventbus.NATSPublisher, *eventbus.NATSSubscriber, error) {
	dialCtx, cancel := context.WithTimeout(ctx, natsConnectTimeout)
	defer cancel()

	pub, err := eventbus.NewNATSPublisher(dialCtx, cfg.NATS.URLs, cfg.NATS.Account,
		eventbus.WithPublisherLogger(logger.Named("publisher")),
		eventbus.WithPublisherName("cmd-telephony-bridge-publisher"),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("cmd/telephony-bridge: nats publisher: %w", err)
	}

	sub, err := eventbus.NewNATSSubscriber(dialCtx, cfg.NATS.URLs, cfg.NATS.Account,
		eventbus.WithSubscriberLogger(logger.Named("subscriber")),
		eventbus.WithSubscriberName("cmd-telephony-bridge-subscriber"),
	)
	if err != nil {
		// Drain the half-open publisher so we don't leak the connection
		// across the fallback path.
		_ = pub.Close()
		return nil, nil, fmt.Errorf("cmd/telephony-bridge: nats subscriber: %w", err)
	}
	return pub, sub, nil
}

// redactNATSURLs returns a slice of URLs with credential segments
// stripped so boot logs can be safely shipped to a log aggregator. Best-
// effort: an unparseable URL falls back to the original string.
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
