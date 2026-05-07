package outbox

import (
	"context"

	"github.com/sociopulse/platform/pkg/postgres"
)

// Writer persists an Event inside an existing transaction. Modules
// call it from inside the same pkg/postgres.WithTenantTx callback that
// performs the state change so the event is committed atomically with
// the row update.
type Writer interface {
	// Write inserts ev into the outbox table on tx. The caller owns
	// the transaction lifecycle.
	Write(ctx context.Context, tx postgres.Tx, ev Event) error
}

// PostgresWriter implements Writer against the project's outbox
// table. Constructor + concrete schema lands in Plan 03 Task 6.
type PostgresWriter struct {
	// unexported config (table name, schema) — initialised in Plan 03 Task 6
}

// NewPostgresWriter constructs a Writer that persists to the standard
// outbox table.
func NewPostgresWriter() *PostgresWriter {
	panic("not implemented: see Plan 03 Task 6")
}

// Write satisfies Writer.
func (w *PostgresWriter) Write(ctx context.Context, tx postgres.Tx, ev Event) error {
	panic("not implemented: see Plan 03 Task 6")
}
