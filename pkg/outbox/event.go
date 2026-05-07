// Package outbox is the project's transactional-outbox helper. The
// pattern: a write to an aggregate AND the corresponding domain event
// land in the same Postgres transaction, then a relay goroutine drains
// new event rows to NATS. This guarantees at-least-once delivery
// without a 2-phase commit between Postgres and NATS.
//
// Used by dialer, recording, audit, and tenancy. The relay runs once
// per binary (cmd/api, cmd/worker, ...) with the binary's own pool;
// every replica drains its share via FOR UPDATE SKIP LOCKED so leader
// election is not required.
//
// See docs/architecture/01-package-layout.md and Plan 03 Task 6 for
// the full rationale and concrete wiring.
package outbox

import (
	"time"

	"github.com/google/uuid"
)

// Event is one row in the event_outbox table. Writers persist an Event
// inside the same transaction as the state change; the relay drains
// pending rows to NATS.
//
// IDs are assigned by Postgres (BIGSERIAL) — callers leave Event.ID at
// its zero value when calling Append. The relay populates ID, CreatedAt,
// PublishedAt, LastError, and Attempts on rows it returns from drain.
type Event struct {
	// ID is the BIGSERIAL primary key. Zero on Append; populated by the
	// relay when it reads pending rows.
	ID int64

	// TenantID groups events by tenant and is nullable for
	// platform-global events (e.g. tenancy admin operations). The
	// outbox table is platform-internal infra and not subject to RLS,
	// so this field is informational rather than enforced.
	TenantID *uuid.UUID

	// AggregateID identifies the entity the event is about (e.g.
	// operator_id, call_id, recording_id). Optional — events that
	// describe a tenant-wide change leave it nil.
	AggregateID *uuid.UUID

	// Subject is the canonical NATS subject the relay publishes to.
	// Non-empty subjects are required; the writer rejects empty values
	// up-front to prevent unroutable rows from sitting in the outbox.
	Subject string

	// Payload is the serialised event body. The schema column is JSONB
	// so the writer validates the bytes at insert time.
	Payload []byte

	// CreatedAt is when the writer enqueued the event. Postgres assigns
	// it via a default; the relay populates this field when reading.
	CreatedAt time.Time

	// PublishedAt is when the relay successfully published the event.
	// Nil means the event is pending.
	PublishedAt *time.Time

	// LastError captures the most recent publish error (truncated for
	// log hygiene). Nil on freshly-inserted or successfully-published
	// rows.
	LastError *string

	// Attempts is the number of times the relay has tried to publish
	// this event. Zero on insert; the relay increments on each failed
	// publish. Once Attempts >= MaxRetry the row is parked and skipped
	// by future drains.
	Attempts int
}
