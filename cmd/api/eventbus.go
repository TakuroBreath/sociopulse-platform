package main

import (
	"context"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/pkg/eventbus"
)

// noopPublisher is a stand-in eventbus.Publisher used until Plan 04 wires
// the real NATS-backed publisher. It logs every publish at debug level so
// developers can watch the outbox relay drain locally without a NATS
// cluster running.
//
// REPLACE: Plan 04 (NATS) constructs eventbus.NATSPublisher and passes it
// in place of this stub. Search for "noopPublisher" to find call sites.
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
