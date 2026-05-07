package outbox

import (
	"context"
	"time"

	"github.com/sociopulse/platform/pkg/eventbus"
	"github.com/sociopulse/platform/pkg/postgres"
)

// Relay drains the outbox table to NATS. One Relay per binary; it
// runs in its own goroutine for the lifetime of the process.
//
// Delivery semantics: at-least-once. The Relay marks a row as
// published only after eventbus.Publisher.Publish returns nil; on
// process crash mid-publish the row is re-delivered next pass.
type Relay struct {
	// unexported pool, publisher, batch size, poll interval —
	// initialised in Plan 03 Task 6
}

// RelayConfig parameterises Relay. Each binary that runs the relay
// pulls these from its config block.
type RelayConfig struct {
	// BatchSize bounds the number of events drained in one pass.
	BatchSize int
	// PollInterval controls how often the relay polls when there is
	// no work; under load it drains continuously.
	PollInterval time.Duration
	// PublishTimeout bounds an individual Publisher.Publish call.
	PublishTimeout time.Duration
}

// NewRelay constructs a Relay backed by pool and publisher. The
// publisher must already be connected to NATS.
func NewRelay(pool *postgres.Pool, publisher eventbus.Publisher, cfg RelayConfig) *Relay {
	panic("not implemented: see Plan 03 Task 6")
}

// Run drives the relay loop until ctx is cancelled. It returns the
// first non-recoverable error it encounters or nil on graceful
// shutdown.
func (r *Relay) Run(ctx context.Context) error {
	panic("not implemented: see Plan 03 Task 6")
}
