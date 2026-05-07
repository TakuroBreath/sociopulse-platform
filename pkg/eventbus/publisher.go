// Package eventbus is the project-wide abstraction over the message bus
// (NATS JetStream in production). Publishers and subscribers in modules
// program against the interfaces defined here so the underlying
// transport can change without touching business code.
//
// Concrete implementations live alongside as nats.go (production) and
// in-memory test doubles maintained per module.
//
// See docs/architecture/01-package-layout.md and
// docs/architecture/02-module-contracts.md for context.
package eventbus

import "context"

// Publisher is the write side of the bus. Modules use it to emit
// domain events to a subject; the relay in pkg/outbox uses it to drain
// the transactional outbox to NATS.
type Publisher interface {
	// Publish sends payload to subject. Implementations must be safe for
	// concurrent use and must respect ctx cancellation.
	Publish(ctx context.Context, subject string, payload []byte) error
}

// Subscriber is the read side of the bus. Modules attach handlers to a
// subject inside a queue group so messages are load-balanced across
// replicas of the same service.
type Subscriber interface {
	// Subscribe registers handler on subject within the named queue
	// group. The handler receives the resolved subject (which may
	// contain wildcards bound at delivery time) and the raw payload.
	// Returning an error from handler triggers redelivery according to
	// the underlying stream's policy.
	Subscribe(ctx context.Context, subject string, queue string, handler func(subject string, payload []byte) error) error
}
