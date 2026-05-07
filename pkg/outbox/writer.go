package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sociopulse/platform/pkg/postgres"
)

// Writer enqueues an Event for durable at-least-once delivery to NATS.
// Implementations MUST be safe to call inside an existing transaction —
// the canonical pattern is that a module performs its state change and
// calls Writer.Append on the same Tx, so the row update and outbox row
// commit atomically.
type Writer interface {
	// Append inserts ev into the event_outbox table on tx. The caller
	// owns the transaction lifecycle: a rollback after Append takes the
	// outbox row with it, which is the entire point of the pattern.
	Append(ctx context.Context, tx postgres.Tx, ev Event) error
}

// PostgresWriter is the canonical Writer backed by the project's
// pkg/postgres helpers. A single zero-value PostgresWriter is reusable
// across goroutines — there is no per-instance state.
type PostgresWriter struct{}

// NewPostgresWriter returns a Writer that persists into event_outbox.
// The constructor is exported for symmetry with future implementations
// (e.g. an in-memory writer for unit tests of business code).
func NewPostgresWriter() *PostgresWriter { return &PostgresWriter{} }

// Append satisfies Writer. It validates the event, marshals the payload
// for the JSONB column (failing fast if the bytes are not valid JSON),
// and inserts a row using tx so the caller's transaction owns the row.
func (w *PostgresWriter) Append(ctx context.Context, tx postgres.Tx, ev Event) error {
	if err := validateEvent(ev); err != nil {
		return err
	}

	const q = `INSERT INTO event_outbox(tenant_id, aggregate_id, subject, payload)
               VALUES ($1, $2, $3, $4)`

	if _, err := tx.Exec(ctx, q, ev.TenantID, ev.AggregateID, ev.Subject, ev.Payload); err != nil {
		return fmt.Errorf("outbox: insert row: %w", err)
	}
	return nil
}

// validateEvent applies cheap pre-flight checks. The JSONB column would
// reject malformed payloads anyway, but the explicit check keeps the
// error site close to the caller and avoids leaking obscure pgx errors
// up the stack for a basic mistake.
func validateEvent(ev Event) error {
	if ev.Subject == "" {
		return errors.New("outbox: subject must not be empty")
	}
	if len(ev.Payload) == 0 {
		return errors.New("outbox: payload must not be empty")
	}
	if !json.Valid(ev.Payload) {
		return errors.New("outbox: payload is not valid JSON")
	}
	return nil
}
