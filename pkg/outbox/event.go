// Package outbox is the project's transactional-outbox helper. The
// pattern: a write to an aggregate AND the corresponding domain event
// land in the same Postgres transaction, then a relay goroutine drains
// new event rows to NATS. This guarantees at-least-once delivery
// without a 2-phase commit between Postgres and NATS.
//
// Used by dialer, recording, audit, and tenancy. The relay runs once
// per binary (cmd/api, cmd/worker, ...) with the binary's own pool;
// events are partitioned by aggregate id so per-aggregate ordering is
// preserved.
//
// See docs/architecture/01-package-layout.md and Plan 03 Task 6 for
// the full rationale and concrete wiring.
package outbox

import (
	"time"

	"github.com/google/uuid"
)

// Event is one row in the outbox table. It is what writers persist
// inside the same transaction as the state change, and what the relay
// drains to NATS.
type Event struct {
	// ID is the row primary key; UUIDv7 so timestamps are inherent.
	ID uuid.UUID
	// AggregateID groups events that must be delivered in order
	// (e.g. all events for one call go through the same NATS subject
	// partition).
	AggregateID uuid.UUID
	// Subject is the NATS subject the relay publishes to.
	Subject string
	// Payload is the serialised event (JSON or protobuf — owners pick
	// per subject).
	Payload []byte
	// CreatedAt is when the writer enqueued the event.
	CreatedAt time.Time
	// PublishedAt is when the relay successfully handed the event to
	// NATS. Nil means the event is still pending.
	PublishedAt *time.Time
}
