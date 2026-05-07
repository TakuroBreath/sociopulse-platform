package api

import (
	"context"
	"time"
)

// Logger is the single Write entry point that other modules call after every
// state-changing action. The implementation strips redaction patterns from
// Event.Payload before INSERT.
type Logger interface {
	// Write inserts an audit row. Payload may include any JSON-encodable
	// value; the implementation strips redaction patterns before INSERT.
	Write(ctx context.Context, e Event) error
}

// Reader returns audit rows for a tenant filtered by action and time.
// Used by the admin "audit log" page and by 152-ФЗ subject-rights handlers (FR-K).
type Reader interface {
	// List returns one page of rows matching f. The second return value is
	// nextCursor — pass it back as ListFilter.Cursor to read the next page,
	// or empty string when there is no next page.
	List(ctx context.Context, f ListFilter) (rows []Event, nextCursor string, err error)
}

// Archiver moves rows older than cutoff to cold-tier S3 and deletes them
// from Postgres. Run by cmd/worker on a weekly schedule.
type Archiver interface {
	// ArchivePass moves rows with ts < cutoff to cold-tier S3 and deletes
	// them from Postgres. Idempotent: re-runs after partial failure.
	ArchivePass(ctx context.Context, cutoff time.Time) (movedRows int64, err error)
}
