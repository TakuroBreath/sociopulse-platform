package outbox

import (
	"context"

	"github.com/sociopulse/platform/pkg/eventbus"
)

// PublisherAdapter bridges the package's internal needs (timeout
// wrapping, retry on transient failure, redaction) to a plain
// eventbus.Publisher. Concrete implementation lands in Plan 03 Task 6;
// exported so the relay can compose it explicitly during construction.
type PublisherAdapter struct {
	upstream eventbus.Publisher
}

// NewPublisherAdapter wraps upstream with the relay's timeout/retry policy.
func NewPublisherAdapter(upstream eventbus.Publisher) *PublisherAdapter {
	return &PublisherAdapter{upstream: upstream}
}

// Publish forwards the call to upstream after applying the package's
// retry / timeout policy.
func (a *PublisherAdapter) Publish(ctx context.Context, ev Event) error {
	_ = a.upstream
	_ = ctx
	_ = ev
	panic("not implemented: see Plan 03 Task 6")
}
