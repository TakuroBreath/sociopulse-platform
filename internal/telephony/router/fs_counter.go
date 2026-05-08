package router

import (
	"context"
	"fmt"

	"github.com/sociopulse/platform/internal/telephony/esl"
)

// ClientLookup is the narrow seam the ESL-backed FSCounter consumes from
// the pool: a single Get(addr) → *esl.Client lookup. Defining it here (not
// importing *pool.ESLPool directly) keeps the router free of a concrete
// pool dependency and lets tests inject a fake without standing up the
// real ESL fleet. The production implementation in
// internal/telephony/pool.ESLPool satisfies this interface implicitly.
//
// Get returns the live client for addr, or an error wrapping
// esl.ErrNotConnected when the node is unknown / disconnected / not yet
// declared healthy. Callers MUST NOT call (*esl.Client).Close on the
// returned client — the pool drives the lifecycle.
type ClientLookup interface {
	Get(addr string) (*esl.Client, error)
}

// ESLFSCounter implements FSCounter by issuing `api show channels count`
// against the appropriate node's *esl.Client (looked up via ClientLookup).
// The Reconciler (Plan 09 Task 6) uses this to fetch the FS-truth count
// for drift correction against the Redis op:active_channels counter.
//
// Compile-time check that ESLFSCounter satisfies FSCounter is in
// reconciler.go (alongside the FSCounter definition) so a future signature
// change surfaces here at boot, not at first sweep.
type ESLFSCounter struct {
	pool ClientLookup
}

// NewESLFSCounter wires the counter to a ClientLookup. pool MUST be
// non-nil — the constructor returns an error rather than deferring the
// nil-deref to the first sweep.
func NewESLFSCounter(pool ClientLookup) (*ESLFSCounter, error) {
	if pool == nil {
		return nil, fmt.Errorf("router: ClientLookup is required")
	}
	return &ESLFSCounter{pool: pool}, nil
}

// ActiveChannels returns the live channel count of node by issuing
// `api show channels count` over the pool's ESL client. Errors from the
// pool lookup propagate verbatim (so callers can errors.Is(...,
// esl.ErrNotConnected)); errors from the ESL command path are wrapped
// with "show channels count" context.
//
// The reconciler bounds ctx with a per-node timeout so a single stalled
// FS node does not block the whole sweep — see Reconciler.sweep.
func (c *ESLFSCounter) ActiveChannels(ctx context.Context, node string) (int, error) {
	cli, err := c.pool.Get(node)
	if err != nil {
		return 0, fmt.Errorf("router/fs_counter: pool lookup %s: %w", node, err)
	}
	n, err := cli.ChannelsCount(ctx)
	if err != nil {
		return 0, fmt.Errorf("router/fs_counter: show channels count %s: %w", node, err)
	}
	return n, nil
}
