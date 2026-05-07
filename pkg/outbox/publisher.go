package outbox

import (
	"context"

	"github.com/sociopulse/platform/pkg/eventbus"
)

// publisherAdapter bridges the package's internal needs (timeout
// wrapping, retry on transient failure, redaction) to a plain
// eventbus.Publisher. Concrete implementation lands in Plan 03 Task 6.
type publisherAdapter struct {
	upstream eventbus.Publisher
}

// newPublisherAdapter wraps upstream with the relay's timeout/retry
// policy. Exported through Relay rather than the package surface — the
// adapter is an implementation detail.
func newPublisherAdapter(upstream eventbus.Publisher) *publisherAdapter {
	return &publisherAdapter{upstream: upstream}
}

// publish forwards the call to upstream after applying the package's
// retry / timeout policy.
func (a *publisherAdapter) publish(ctx context.Context, ev Event) error {
	panic("not implemented: see Plan 03 Task 6")
}
