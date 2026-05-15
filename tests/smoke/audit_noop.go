//go:build smoke

package smoke

import (
	"context"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
)

// NoopAuditLogger is the zero-cost auditapi.Logger fake the smoke
// harness uses when constructing in-test workers (e.g. scenario 8's
// PurgeWorker) directly via NewPurgeWorker. Production wires the real
// audit module; smoke stays focused on the worker's externally-visible
// behaviour (rows physically gone after Run) and does not assert audit
// row writes — those are covered by per-module integration tests.
//
// The constructor for PurgeWorker rejects nil auditLogger (panics:
// "auditLogger is required (use a no-op fake in tests, never nil)") so
// a valid implementation must exist. NoopAuditLogger satisfies the
// interface with a zero-cost return path.
//
// Compile-time interface check guards against future audit.Logger
// signature drift: if Write's signature changes, this line fails to
// compile and the smoke build catches it before scenario 8 starts.
type NoopAuditLogger struct{}

var _ auditapi.Logger = (*NoopAuditLogger)(nil)

// Write satisfies auditapi.Logger by silently dropping the event.
// Returns nil so the caller's audit-write path treats the call as
// successful. The audit module's structured-log redaction is bypassed
// entirely — that is the point: the smoke harness does not test the
// audit module here, only the worker that calls into it.
func (NoopAuditLogger) Write(_ context.Context, _ auditapi.Event) error {
	return nil
}
