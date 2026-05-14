package fsm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
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
	// LastStateLog returns the most-recent operator_state_log row for
	// sessionID. Returns ErrNoStateLog when none exist (the caller is
	// about to insert the first row for this session). The analytics
	// dual-publish path (Plan 13.2 § Q7) uses the returned ts to
	// compute duration_in_state_sec as the delta between the prior
	// row and the row about to be appended.
	LastStateLog(ctx context.Context, tx postgres.Tx, sessionID uuid.UUID) (LastStateLogRow, error)
}

// LastStateLogRow is the projection of operator_state_log used by
// SessionStore.LastStateLog. Reason is normalised to "" for NULL.
type LastStateLogRow struct {
	OccurredAt time.Time
	State      api.State
	Reason     string
}

// ErrNoStateLog is the sentinel returned by SessionStore.LastStateLog
// when the session has no prior operator_state_log rows.
var ErrNoStateLog = errors.New("fsm/store: no prior state-log row")

// auditEntry bundles the data needed to record one FSM transition into
// Postgres + outbox in a single Tx. Every successful transition produces
// exactly one auditEntry.
//
// extraOutbox is an optional hook that runs INSIDE the audit Tx after the
// canonical (state_log, per-tenant outbox, analytics outbox) rows. Used
// by the EventStatusSubmitted handler to append the call.finalized outbox
// rows in the same Tx as the operator state-log row (Plan 13.2 § Q7).
// When nil, the hook is skipped and the audit Tx commits as before.
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
	extraOutbox func(ctx context.Context, tx postgres.Tx) error
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

// LastStateLog satisfies SessionStore — returns the most-recent prior
// row for sessionID. Returns ErrNoStateLog for the empty-session case.
// MUST be called BEFORE AppendStateLog in the same Tx so the row about
// to be inserted does not shadow the genuine "previous" row.
func (pgSessionStore) LastStateLog(ctx context.Context, tx postgres.Tx, sessionID uuid.UUID) (LastStateLogRow, error) {
	const q = `
		SELECT ts, state, COALESCE(reason, '')
		FROM operator_state_log
		WHERE session_id = $1
		ORDER BY ts DESC
		LIMIT 1`
	var r LastStateLogRow
	var rawState string
	err := tx.QueryRow(ctx, q, sessionID).Scan(&r.OccurredAt, &rawState, &r.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return LastStateLogRow{}, ErrNoStateLog
	}
	if err != nil {
		return LastStateLogRow{}, fmt.Errorf("select operator_state_log: %w", err)
	}
	r.State = api.State(rawState)
	return r, nil
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

// appendStateLogAndOutbox writes the operator_state_log row + two outbox
// events in tx — both committed atomically with the state-log INSERT.
//
// Plan 13.2 § Q7 dual-publish:
//  1. tenant.<t>.dialer.op.<op_id>.state — per-tenant subject consumed by
//     operator UI / supervisor dashboards.
//  2. analytics.event.operator_state — cross-tenant subject consumed by
//     the ClickHouse ingest pipeline. The payload carries duration_in_state_sec
//     (= delta from the PRIOR state-log row's ts) so analytics can compute
//     per-state time-spent without joining back to operator_state_log.
//
// duration_in_state_sec is 0 for the first transition of a session (no
// prior log row to subtract from).
func (m *Machine) appendStateLogAndOutbox(
	ctx context.Context,
	tx postgres.Tx,
	entry auditEntry,
) error {
	// Look up the prior state-log row BEFORE inserting the current one so
	// the "previous" semantics is genuinely the prior state. We swallow
	// ErrNoStateLog: a session's first transition has no prior row and
	// reports duration_in_state_sec = 0.
	prev, err := m.sessions.LastStateLog(ctx, tx, entry.SessionID)
	hasPrev := err == nil
	if err != nil && !errors.Is(err, ErrNoStateLog) {
		return fmt.Errorf("lookup prior state-log: %w", err)
	}

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

	// Plan 13.2 § Q7: emit the cross-tenant analytics row in the same Tx.
	// The tenant_id is denormalised into the payload (the subject is
	// global), so we leave outbox.Event.TenantID nil; that also keeps
	// existing per-tenant outbox queries (filtered on tenant_id) from
	// surfacing analytics rows when they don't expect them.
	var prevDur uint32
	if hasPrev {
		secs := entry.OccurredAt.Sub(prev.OccurredAt).Seconds()
		switch {
		case secs <= 0:
			prevDur = 0
		case secs >= float64(math.MaxUint32):
			prevDur = math.MaxUint32
		default:
			prevDur = uint32(secs)
		}
	}
	analyticsPayload, err := json.Marshal(analyticsapi.AnalyticsOperatorStateEventPayload{
		Date:               entry.OccurredAt.UTC().Format("2006-01-02"),
		TS:                 entry.OccurredAt.UTC(),
		TenantID:           entry.TenantID,
		UserID:             entry.OperatorID,
		State:              string(entry.NewState),
		DurationInStateSec: prevDur,
		ProjectID:          entry.ProjectID,
		EventID:            uuid.New(),
	})
	if err != nil {
		return fmt.Errorf("marshal analytics operator_state: %w", err)
	}
	if err := m.outbox.Append(ctx, tx, outbox.Event{
		Subject: analyticsapi.SubjectOperatorStateAnalytics,
		Payload: analyticsPayload,
	}); err != nil {
		return fmt.Errorf("outbox append analytics op state: %w", err)
	}

	// Optional extra-outbox hook — used by EventStatusSubmitted to append
	// the call.finalized rows (per-tenant + analytics) in the same Tx so
	// the call-finalisation rollup is atomic with the state transition.
	if entry.extraOutbox != nil {
		if err := entry.extraOutbox(ctx, tx); err != nil {
			return fmt.Errorf("extra outbox: %w", err)
		}
	}
	return nil
}

// callFinalizedEntry carries the data the EventStatusSubmitted handler
// needs to append the call.finalized + analytics.event.calls outbox rows.
// Built INSIDE SubmitStatus before the audit Tx opens; appendCallFinalizedOutbox
// consumes it via the auditEntry.extraOutbox hook.
type callFinalizedEntry struct {
	TenantID     uuid.UUID
	OperatorID   uuid.UUID
	ProjectID    uuid.UUID
	CallID       uuid.UUID
	RespondentID uuid.UUID
	Status       string
	DurationSec  uint32
	TrunkUsed    string
	HangupCause  string // "" until telephony-bridge ↔ analytics wiring lands (Q8)
	RegionCode   string // "" until respondent flow lands (Q9)
	AttemptNo    uint8
	FinalizedAt  time.Time
	StorageBytes int64
}

// appendCallFinalizedOutbox writes TWO outbox rows inside tx:
//  1. tenant.<t>.dialer.call.finalized — the per-tenant subject consumed
//     by billing + downstream tenant-scoped subscribers. Payload =
//     api.CallFinalizedEvent.
//  2. analytics.event.calls — cross-tenant subject consumed by the
//     analytics ingest pipeline. Payload = analyticsapi.AnalyticsCallEventPayload.
//
// Both rows are emitted with fresh uuid.New() event_id values so the
// ingest dedup LRU treats them as distinct events.
func (m *Machine) appendCallFinalizedOutbox(
	ctx context.Context,
	tx postgres.Tx,
	entry callFinalizedEntry,
) error {
	// 1. Per-tenant tenant.<t>.dialer.call.finalized row.
	finalizedPayload, err := json.Marshal(api.CallFinalizedEvent{
		CallID:       entry.CallID,
		TenantID:     entry.TenantID,
		OperatorID:   entry.OperatorID,
		ProjectID:    entry.ProjectID,
		RespondentID: entry.RespondentID,
		TrunkUsed:    entry.TrunkUsed,
		DurationSec:  int32(entry.DurationSec), //nolint:gosec // DurationSec is bounded by uint32 in the entry
		Status:       entry.Status,
		StorageBytes: entry.StorageBytes,
		FinalizedAt:  entry.FinalizedAt.Unix(),
	})
	if err != nil {
		return fmt.Errorf("marshal call_finalized: %w", err)
	}
	tenantID := entry.TenantID
	callID := entry.CallID
	if err := m.outbox.Append(ctx, tx, outbox.Event{
		TenantID:    &tenantID,
		AggregateID: &callID,
		Subject:     api.SubjectCallFinalizedFor(entry.TenantID),
		Payload:     finalizedPayload,
	}); err != nil {
		return fmt.Errorf("outbox append call_finalized: %w", err)
	}

	// 2. Cross-tenant analytics.event.calls row.
	analyticsPayload, err := json.Marshal(analyticsapi.AnalyticsCallEventPayload{
		Date:        entry.FinalizedAt.UTC().Format("2006-01-02"),
		TS:          entry.FinalizedAt.UTC(),
		TenantID:    entry.TenantID,
		ProjectID:   entry.ProjectID,
		OperatorID:  entry.OperatorID,
		CallID:      entry.CallID,
		Status:      entry.Status,
		DurationSec: entry.DurationSec,
		HangupCause: entry.HangupCause,
		RegionCode:  entry.RegionCode,
		AttemptNo:   entry.AttemptNo,
		TrunkUsed:   entry.TrunkUsed,
		EventID:     uuid.New(),
	})
	if err != nil {
		return fmt.Errorf("marshal analytics calls: %w", err)
	}
	if err := m.outbox.Append(ctx, tx, outbox.Event{
		Subject: analyticsapi.SubjectCallsAnalytics,
		Payload: analyticsPayload,
	}); err != nil {
		return fmt.Errorf("outbox append analytics calls: %w", err)
	}
	return nil
}
