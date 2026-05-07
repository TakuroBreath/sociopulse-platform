package api

import (
	"context"

	"github.com/google/uuid"
)

// EventHandler is invoked once per ChannelEvent on the consumer side.
// Returning an error causes the bridge to NACK the message for redelivery.
type EventHandler func(ctx context.Context, evt ChannelEvent) error

// CommandPublisher is what the dialer uses to ask the bridge to do something on FS.
type CommandPublisher interface {
	// Originate places an outbound call.
	Originate(ctx context.Context, cmd OriginateCommand) error
	// Hangup ends a call.
	Hangup(ctx context.Context, cmd HangupCommand) error
	// Mixmonitor starts a listen-in / recording stream.
	Mixmonitor(ctx context.Context, cmd MixmonitorCommand) error
	// Play pushes an audio URL into a call.
	Play(ctx context.Context, cmd PlayCommand) error
	// CreateUser provisions a SIP user in the per-tenant FS directory.
	CreateUser(ctx context.Context, cmd CreateUserCommand) error
	// DeleteUser removes a SIP user from the per-tenant FS directory.
	DeleteUser(ctx context.Context, cmd DeleteUserCommand) error
}

// EventConsumer registers a handler for bridge events. Returned unsubscribe()
// must be called at shutdown.
type EventConsumer interface {
	// Subscribe attaches h to the per-tenant event stream and returns an
	// unsubscribe function.
	Subscribe(ctx context.Context, tenantID uuid.UUID, h EventHandler) (unsubscribe func(), err error)
}

// Router selects {fs_node, trunk} for a given operator+phone+strategy.
// Used by the dialer just before issuing Originate.
type Router interface {
	// Select returns the chosen FS node and trunk for the request.
	Select(ctx context.Context, req SelectRequest) (SelectionResult, error)
}

// LineCapacityTracker enforces max_concurrent_per_node (default 60).
// Acquire returns ErrAllNodesFull when every node is at cap; the caller backs off.
type LineCapacityTracker interface {
	// Acquire reserves one channel on a healthy node and returns its name.
	Acquire(ctx context.Context) (node string, err error)
	// Release returns one channel to the pool for the named node.
	Release(ctx context.Context, node string) error
	// Stats returns the current per-node concurrency counts.
	Stats(ctx context.Context) (map[string]int64, error)
}
