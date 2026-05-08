package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
)

// actorContextKey is the unexported context key the crm services use to
// pull the acting user id when emitting audit rows. The HTTP transport
// (Plan 06 Task 5) will populate it from the JWT claims; tests inject
// the actor via WithActorID directly.
type actorContextKey struct{}

// WithActorID returns a context that carries the supplied actor user id.
// crm services inspect the context for this value when writing audit
// rows; absent value -> ActorKind=system, nil ActorID (system bootstrap
// or worker-driven action).
func WithActorID(ctx context.Context, actorID uuid.UUID) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actorID)
}

// actorIDFromContext returns the actor id stored on ctx by WithActorID,
// or a nil pointer when no actor is present.
func actorIDFromContext(ctx context.Context) *uuid.UUID {
	v, ok := ctx.Value(actorContextKey{}).(uuid.UUID)
	if !ok || v == uuid.Nil {
		return nil
	}
	return &v
}

// writeAudit fills in the boilerplate fields (timestamp, actor) and
// invokes the audit Logger. The supplied event is always materialised
// with a non-zero Timestamp; ActorKind defaults to user when an actor
// is present in ctx and to system otherwise.
//
// nilable signature: a nil logger is treated as "drop the row" so
// composition roots can fall back to a noop without sprinkling guards
// at every call site. The crm module composition root registers a real
// logger (or the noop fallback when audit hasn't registered yet); tests
// inject a fakeAudit fixture that records every Write.
func (s *ProjectService) writeAudit(ctx context.Context, ev auditapi.Event) error {
	if s.audit == nil {
		return nil
	}
	if ev.ActorKind == "" {
		ev.ActorKind = auditapi.ActorUser
	}
	if ev.ActorID == nil {
		ev.ActorID = actorIDFromContext(ctx)
		if ev.ActorID == nil {
			ev.ActorKind = auditapi.ActorSystem
		}
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = s.clock()
	}
	if err := s.audit.Write(ctx, ev); err != nil {
		return fmt.Errorf("audit write: %w", err)
	}
	return nil
}
