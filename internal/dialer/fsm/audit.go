package fsm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// SessionStore persists the operator_sessions / operator_state_log rows.
// Pulled out behind an interface so the FSM unit tests can pass a
// pure-Go fake; production wiring uses pgSessionStore (this file).
type SessionStore interface {
	// CreateSession INSERTs a new operator_sessions row inside tx and
	// returns the generated session_id.
	CreateSession(ctx context.Context, tx postgres.Tx, tenantID, userID, projectID uuid.UUID) (uuid.UUID, error)
	// CloseSession UPDATEs ended_at on the operator_sessions row to
	// occurredAt iff it was previously NULL. Idempotent: a re-run on an
	// already-ended row is a no-op for the UPDATE.
	CloseSession(ctx context.Context, tx postgres.Tx, sessionID uuid.UUID, occurredAt time.Time) error
	// AppendStateLog INSERTs one row into operator_state_log inside tx.
	AppendStateLog(ctx context.Context, tx postgres.Tx, sessionID uuid.UUID, occurredAt time.Time, state api.State, reason string) error
}

// auditEntry bundles the data needed to record one FSM transition into
// Postgres + outbox in a single Tx. Every successful transition produces
// exactly one auditEntry.
type auditEntry struct {
	TenantID    uuid.UUID
	OperatorID  uuid.UUID
	SessionID   uuid.UUID // operator_state_log.session_id FK
	NewState    api.State
	Reason      string // optional; populated by Force / GoPause
	OccurredAt  time.Time
	ProjectID   *uuid.UUID
	CallID      *uuid.UUID
	PauseReason *string
}

// pgSessionStore is the default SessionStore implementation. The
// queries live here (not in a separate store/ package) because the
// SessionStore exists solely to make the FSM unit-testable; the
// production composition root passes &pgSessionStore{} to fsm.New.
type pgSessionStore struct{}

// CreateSession satisfies SessionStore — INSERT ... RETURNING id.
func (pgSessionStore) CreateSession(ctx context.Context, tx postgres.Tx, tenantID, userID, projectID uuid.UUID) (uuid.UUID, error) {
	const q = `
		INSERT INTO operator_sessions (tenant_id, user_id, project_id)
		VALUES ($1, $2, $3)
		RETURNING id`
	var sessionID uuid.UUID
	if err := tx.QueryRow(ctx, q, tenantID, userID, projectID).Scan(&sessionID); err != nil {
		return uuid.Nil, fmt.Errorf("insert operator_sessions: %w", err)
	}
	return sessionID, nil
}

// CloseSession satisfies SessionStore — sets ended_at if NULL.
func (pgSessionStore) CloseSession(ctx context.Context, tx postgres.Tx, sessionID uuid.UUID, occurredAt time.Time) error {
	const q = `
		UPDATE operator_sessions
		SET ended_at = COALESCE(ended_at, $2)
		WHERE id = $1`
	if _, err := tx.Exec(ctx, q, sessionID, occurredAt); err != nil {
		return fmt.Errorf("update operator_sessions: %w", err)
	}
	return nil
}

// AppendStateLog satisfies SessionStore — single-row INSERT.
func (pgSessionStore) AppendStateLog(ctx context.Context, tx postgres.Tx, sessionID uuid.UUID, occurredAt time.Time, state api.State, reason string) error {
	const q = `
		INSERT INTO operator_state_log (session_id, ts, state, reason)
		VALUES ($1, $2, $3, NULLIF($4, ''))`
	if _, err := tx.Exec(ctx, q, sessionID, occurredAt, string(state), reason); err != nil {
		return fmt.Errorf("insert operator_state_log: %w", err)
	}
	return nil
}

// startSessionAndAudit opens a per-tenant transaction that:
//
//  1. INSERTs a fresh row into operator_sessions (tenant, user, project)
//     and returns the new session_id.
//  2. INSERTs the first operator_state_log row (state=ready, reason=NULL).
//  3. Appends the canonical operator_state_changed event to the outbox.
//
// All three writes commit atomically. If step 1 succeeds but the caller's
// downstream Redis CAS fails, the caller rolls back by EndShifting the
// session (or letting the orphan_session reaper close it later).
//
// Returns the new session_id on success.
func (m *Machine) startSessionAndAudit(
	ctx context.Context,
	req api.StartShiftRequest,
	occurredAt time.Time,
) (uuid.UUID, error) {
	var sessionID uuid.UUID
	err := m.pg.WithTenant(ctx, req.TenantID, func(tx postgres.Tx) error {
		var err error
		sessionID, err = m.sessions.CreateSession(ctx, tx, req.TenantID, req.OperatorID, req.ProjectID)
		if err != nil {
			return err
		}
		entry := auditEntry{
			TenantID:   req.TenantID,
			OperatorID: req.OperatorID,
			SessionID:  sessionID,
			NewState:   api.StateReady,
			Reason:     "",
			OccurredAt: occurredAt,
			ProjectID:  &req.ProjectID,
		}
		return m.appendStateLogAndOutbox(ctx, tx, entry)
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("fsm/start-session: %w", err)
	}
	return sessionID, nil
}

// closeSessionAndAudit opens a per-tenant transaction that:
//
//  1. UPDATEs the operator_sessions row for sessionID to set ended_at = now().
//  2. INSERTs the final operator_state_log row (state=offline, reason=optional).
//  3. Appends the canonical operator_state_changed event to the outbox.
//
// Idempotent on a session that's already ended (ended_at != NULL): the
// UPDATE is a no-op for that row, but the operator_state_log + outbox
// rows are still appended. Re-runs on the public path are guarded by the
// Machine.applyEvent idempotency check (cur.State == next), so this
// function is only invoked on a real transition.
func (m *Machine) closeSessionAndAudit(
	ctx context.Context,
	tenantID, operatorID, sessionID uuid.UUID,
	reason string,
	occurredAt time.Time,
) error {
	err := m.pg.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		if err := m.sessions.CloseSession(ctx, tx, sessionID, occurredAt); err != nil {
			return err
		}
		entry := auditEntry{
			TenantID:   tenantID,
			OperatorID: operatorID,
			SessionID:  sessionID,
			NewState:   api.StateOffline,
			Reason:     reason,
			OccurredAt: occurredAt,
		}
		return m.appendStateLogAndOutbox(ctx, tx, entry)
	})
	if err != nil {
		return fmt.Errorf("fsm/close-session: %w", err)
	}
	return nil
}

// auditTransition opens a per-tenant transaction that writes one
// operator_state_log row and one outbox event. Used for every non-shift
// transition (GoPause, Resume, RecordCallStarted, ...). The caller
// supplies the session_id loaded from the Redis hash.
func (m *Machine) auditTransition(
	ctx context.Context,
	entry auditEntry,
) error {
	err := m.pg.WithTenant(ctx, entry.TenantID, func(tx postgres.Tx) error {
		return m.appendStateLogAndOutbox(ctx, tx, entry)
	})
	if err != nil {
		return fmt.Errorf("fsm/audit: %w", err)
	}
	return nil
}

// appendStateLogAndOutbox writes the operator_state_log row + the
// outbox event in tx. Pulled out so the three call sites (start, close,
// transition) share the same canonical shape.
func (m *Machine) appendStateLogAndOutbox(
	ctx context.Context,
	tx postgres.Tx,
	entry auditEntry,
) error {
	if err := m.sessions.AppendStateLog(ctx, tx, entry.SessionID, entry.OccurredAt, entry.NewState, entry.Reason); err != nil {
		return err
	}

	payload, err := json.Marshal(api.OperatorStateChangedEvent{
		TenantID:    entry.TenantID,
		OperatorID:  entry.OperatorID,
		State:       entry.NewState,
		ChangedAt:   entry.OccurredAt.UnixMilli(),
		ProjectID:   uuidPtrToString(entry.ProjectID),
		CallID:      uuidPtrToString(entry.CallID),
		PauseReason: stringPtrToString(entry.PauseReason),
	})
	if err != nil {
		return fmt.Errorf("marshal operator_state_changed: %w", err)
	}
	tenantID := entry.TenantID
	operatorID := entry.OperatorID
	if err := m.outbox.Append(ctx, tx, outbox.Event{
		TenantID:    &tenantID,
		AggregateID: &operatorID,
		Subject:     api.SubjectOpStateFor(tenantID, operatorID),
		Payload:     payload,
	}); err != nil {
		return fmt.Errorf("outbox append: %w", err)
	}
	return nil
}
