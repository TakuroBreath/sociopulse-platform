package outbox

import "context"

// PollOnce exposes the unexported pollOnce for the integration tests in
// the outbox_test package. Tests drive the DLQ poll manually to avoid
// depending on goroutine timing.
//
// Production code MUST NOT depend on this — it has no
// linker-symbol-stability promise and only exists in test builds via
// the _test.go suffix.
func (r *Relay) PollOnce(ctx context.Context) error {
	return r.pollOnce(ctx)
}
