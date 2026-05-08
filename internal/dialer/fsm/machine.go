package fsm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// defaultHashTTL is the per-operator hash TTL refreshed on every
// successful CAS write. 24h covers a worst-case shift (8h × 3 shift
// rotations on the same key) without ever expiring while the operator
// is genuinely active. Heartbeat (Task 2c) refreshes a separate
// presence:<t>:user:<o> key on a tighter cadence.
const defaultHashTTL = 24 * time.Hour

// TxRunner is the abstraction over postgres.Pool used by the FSM for
// audit writes. Production wiring passes a *postgres.Pool, which
// satisfies this interface via WithTenant (the operator_sessions row
// carries tenant_id and is RLS-protected; the audit tx runs inside a
// per-tenant transaction so RLS isolates the writes).
type TxRunner interface {
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error
}

// Config bundles the dependencies and settings for a Machine. Required
// fields are documented per-field; nil-tolerated fields fall back to
// safe defaults.
type Config struct {
	// Redis is the connection used for the operator hash + Lua CAS.
	// Required.
	Redis *redis.Client

	// PG is the Postgres tx runner used for operator_sessions /
	// operator_state_log writes. Required. Production passes a
	// *postgres.Pool; tests pass a fake.
	PG TxRunner

	// Outbox is the outbox writer used for operator_state.changed events.
	// Required.
	Outbox outbox.Writer

	// Sessions is the session/state-log store. nil → the canonical
	// pgSessionStore which writes the documented SQL. Tests pass a fake.
	Sessions SessionStore

	// Logger receives per-method diagnostics. nil → zap.NewNop().
	Logger *zap.Logger

	// Clock returns the current time. nil → time.Now. Tests pass a
	// frozen clock so transitions yield deterministic state_entered_at
	// timestamps.
	Clock func() time.Time

	// HashTTL is the per-operator Redis hash TTL refreshed on every
	// successful CAS write. 0 → defaultHashTTL (24h).
	HashTTL time.Duration

	// Metrics is the per-package collector group. nil → no metrics
	// (the Machine is fully functional without it).
	Metrics *Metrics
}

// Machine implements api.OperatorFSM. It coordinates atomic Redis hash
// CAS writes (live state) with Postgres operator_sessions /
// operator_state_log audit writes via the outbox.
type Machine struct {
	rdb      *redis.Client
	pg       TxRunner
	outbox   outbox.Writer
	sessions SessionStore
	log      *zap.Logger
	clock    func() time.Time
	hashTTL  time.Duration
	metrics  *Metrics
}

// Compile-time interface check. Surfaces api.OperatorFSM signature drift
// the moment it happens.
var _ api.OperatorFSM = (*Machine)(nil)

// New constructs a Machine. Returns an error when a required
// dependency is missing; nil-tolerated fields are filled with defaults.
func New(cfg Config) (*Machine, error) {
	if cfg.Redis == nil {
		return nil, errors.New("fsm.New: Redis is required")
	}
	if cfg.PG == nil {
		return nil, errors.New("fsm.New: PG is required")
	}
	if cfg.Outbox == nil {
		return nil, errors.New("fsm.New: Outbox is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	ttl := cfg.HashTTL
	if ttl <= 0 {
		ttl = defaultHashTTL
	}
	sessions := cfg.Sessions
	if sessions == nil {
		sessions = pgSessionStore{}
	}
	return &Machine{
		rdb:      cfg.Redis,
		pg:       cfg.PG,
		outbox:   cfg.Outbox,
		sessions: sessions,
		log:      logger,
		clock:    clock,
		hashTTL:  ttl,
		metrics:  cfg.Metrics,
	}, nil
}

// now returns m.clock() forced to UTC. All timestamps stored in Redis
// or Postgres are UTC; the operator UI does the local-time rendering.
func (m *Machine) now() time.Time { return m.clock().UTC() }

// applyEvent runs the canonical (load → check transition → CAS →
// audit) flow for every non-shift method. mutate runs against the
// computed `next` snapshot before the CAS write so callers can stash
// per-event fields (call_id, respondent_id, pause_reason, ...).
//
// Idempotency: if cur.State == next, the function returns the current
// snapshot without writing to Redis or Postgres.
//
// Error returns:
//   - api.ErrInvalidTransition (wrapped with from / event) on lookup miss
//   - api.ErrTenantMismatch on a cross-tenant access attempt
//   - api.ErrUnknownState on a corrupt Redis row
//   - errVersionMismatch (wrapped) on optimistic-concurrency conflict
func (m *Machine) applyEvent(
	ctx context.Context,
	tenantID, operatorID uuid.UUID,
	evt api.Event,
	mutate func(s *Snapshot),
) (api.Snapshot, error) {
	return m.applyEventWith(ctx, tenantID, operatorID, evt, nil, mutate, nil)
}

// applyEventWith is the extended form of applyEvent. It exposes two extra
// hooks for callers that need to consult the loaded snapshot before the
// transition lookup:
//
//   - preCheck runs immediately after load. A non-nil error short-circuits
//     the whole call; the err is returned verbatim and the invalid-transition
//     metric is incremented under (cur.State, evt).
//   - replayShortCircuit runs after preCheck. When it returns true, the
//     function returns the current snapshot as an idempotent no-op even if
//     the (state, event) edge is not in the transitions table. Used by
//     RecordCallStarted to absorb a "replay with same call_id from call"
//     without surfacing ErrInvalidTransition on the missing edge.
//
// Both hooks are nil-tolerant; when both are nil this is the canonical
// applyEvent. The point of folding the call_id mismatch check inside the
// already-loaded snapshot is to avoid a second HGETALL per
// RecordCallStarted invocation (see Plan 10 Task 2 code-quality fix-up).
func (m *Machine) applyEventWith(
	ctx context.Context,
	tenantID, operatorID uuid.UUID,
	evt api.Event,
	preCheck func(cur Snapshot) error,
	mutate func(s *Snapshot),
	replayShortCircuit func(cur Snapshot) bool,
) (api.Snapshot, error) {
	cur, err := m.load(ctx, tenantID, operatorID)
	if err != nil {
		return api.Snapshot{}, err
	}
	if preCheck != nil {
		if err := preCheck(cur); err != nil {
			m.metrics.observeInvalid(cur.State, evt)
			return api.Snapshot{}, err
		}
	}
	if replayShortCircuit != nil && replayShortCircuit(cur) {
		return cur.toAPI(tenantID, operatorID), nil
	}
	next, ok := transitions[edge{from: cur.State, event: evt}]
	if !ok {
		m.metrics.observeInvalid(cur.State, evt)
		return api.Snapshot{}, fmt.Errorf("%w: %s --%s-->", api.ErrInvalidTransition, cur.State, evt)
	}
	if cur.State == next {
		// Idempotent replay: nothing changes, no audit row.
		return cur.toAPI(tenantID, operatorID), nil
	}

	now := m.now()
	updated := cur
	updated.State = next
	updated.StateEnteredAt = now
	updated.HeartbeatAt = now
	if mutate != nil {
		mutate(&updated)
	}

	if err := m.casStore(ctx, tenantID, operatorID, cur.Version, updated); err != nil {
		return api.Snapshot{}, fmt.Errorf("fsm/apply: %w", err)
	}
	updated.Version = cur.Version + 1

	// Audit. A failure here is recoverable — the live state already
	// changed. Log loudly and return the new snapshot anyway. The
	// outbox-relay reconciler (or a manual replay) can backfill the
	// missing operator_state_log row.
	sessionID := uuid.Nil
	if cur.SessionID != nil {
		sessionID = *cur.SessionID
	}
	if sessionID != uuid.Nil {
		entry := auditEntry{
			TenantID:    tenantID,
			OperatorID:  operatorID,
			SessionID:   sessionID,
			NewState:    next,
			OccurredAt:  now,
			ProjectID:   updated.ProjectID,
			CallID:      updated.CurrentCallID,
			PauseReason: updated.PauseReason,
		}
		if err := m.auditTransition(ctx, entry); err != nil {
			m.log.Error("fsm: audit append failed after live state committed",
				zap.String("from", string(cur.State)),
				zap.String("to", string(next)),
				zap.String("event", string(evt)),
				zap.Stringer("tenant_id", tenantID),
				zap.Stringer("operator_id", operatorID),
				zap.Stringer("session_id", sessionID),
				zap.Error(err))
		}
	} else {
		// No bound session — happens only when a Force re-enters from
		// offline without going through StartShift. Log and skip.
		m.log.Warn("fsm: transition without session_id; skipping audit",
			zap.String("from", string(cur.State)),
			zap.String("to", string(next)),
			zap.String("event", string(evt)),
			zap.Stringer("tenant_id", tenantID),
			zap.Stringer("operator_id", operatorID))
	}

	m.metrics.observeTransition(cur.State, next, evt)
	return updated.toAPI(tenantID, operatorID), nil
}

// StartShift implements api.OperatorFSM. It opens a tenancy_admin tx
// that creates a new operator_sessions row + first operator_state_log
// row + outbox event, then atomically writes the Redis hash via the
// start_shift Lua script. Idempotent on replay: a second StartShift on
// an already-ready operator returns the existing snapshot without a
// second session row.
func (m *Machine) StartShift(ctx context.Context, req api.StartShiftRequest) (api.Snapshot, error) {
	// Pre-check: idempotent replay short-circuits before touching PG.
	cur, err := m.load(ctx, req.TenantID, req.OperatorID)
	if err != nil {
		return api.Snapshot{}, err
	}
	if cur.State == api.StateReady {
		// Already on shift. Return the existing snapshot.
		return cur.toAPI(req.TenantID, req.OperatorID), nil
	}
	if cur.State != api.StateOffline {
		// Mid-shift in some other state — surface invalid transition.
		m.metrics.observeInvalid(cur.State, api.EventStartShift)
		return api.Snapshot{},
			fmt.Errorf("%w: %s --start_shift-->", api.ErrInvalidTransition, cur.State)
	}

	// Step 1: insert the session row + audit. Returns a fresh session_id.
	now := m.now()
	sessionID, err := m.startSessionAndAudit(ctx, req, now)
	if err != nil {
		return api.Snapshot{}, fmt.Errorf("fsm/start_shift: %w", err)
	}

	// Step 2: write the Redis hash with state=ready and the bound
	// session_id. The Lua script is idempotent on concurrent
	// duplicate-StartShift: rc=0 means another goroutine already
	// wrote ready first, in which case we orphan our session row and
	// return the existing snapshot (eventual cleanup by reaper).
	updated := Snapshot{
		State:          api.StateReady,
		StateEnteredAt: now,
		HeartbeatAt:    now,
		TenantID:       req.TenantID,
		SessionID:      &sessionID,
		ProjectID:      &req.ProjectID,
	}
	rc, err := m.startShiftCAS(ctx, req.TenantID, req.OperatorID, updated)
	if err != nil {
		// errStartShiftBusy means the hash exists in some non-offline
		// state; leak the session row (it carries no PII) and surface
		// the invalid transition.
		if errors.Is(err, errStartShiftBusy) {
			m.log.Warn("fsm/start_shift: hash existed in busy state; orphaning session row",
				zap.Stringer("tenant_id", req.TenantID),
				zap.Stringer("operator_id", req.OperatorID),
				zap.Stringer("session_id", sessionID))
			m.metrics.observeInvalid(api.StateOffline, api.EventStartShift)
			return api.Snapshot{},
				fmt.Errorf("%w: hash already non-offline", api.ErrInvalidTransition)
		}
		return api.Snapshot{}, fmt.Errorf("fsm/start_shift: cas: %w", err)
	}
	if rc == 0 {
		// Idempotent replay observed at the Lua level — another writer
		// landed first. Re-load and return their snapshot. Our session
		// row is orphaned (ended_at remains NULL); a reaper closes it
		// later, or EndShift on the live session covers the case
		// already.
		again, err := m.load(ctx, req.TenantID, req.OperatorID)
		if err != nil {
			return api.Snapshot{}, fmt.Errorf("fsm/start_shift: reload: %w", err)
		}
		m.log.Warn("fsm/start_shift: concurrent ready-write detected; orphaning session row",
			zap.Stringer("tenant_id", req.TenantID),
			zap.Stringer("operator_id", req.OperatorID),
			zap.Stringer("orphan_session_id", sessionID))
		return again.toAPI(req.TenantID, req.OperatorID), nil
	}

	updated.Version = 1
	m.metrics.observeTransition(api.StateOffline, api.StateReady, api.EventStartShift)
	return updated.toAPI(req.TenantID, req.OperatorID), nil
}

// EndShift implements api.OperatorFSM. UPDATEs ended_at on the bound
// operator_sessions row, transitions the Redis hash to offline (clearing
// session_id / project_id / call_id / respondent_id / pause_reason),
// and writes the closing audit row.
func (m *Machine) EndShift(ctx context.Context, tenantID, operatorID uuid.UUID) (api.Snapshot, error) {
	cur, err := m.load(ctx, tenantID, operatorID)
	if err != nil {
		return api.Snapshot{}, err
	}
	if cur.State == api.StateOffline {
		// Idempotent replay.
		return cur.toAPI(tenantID, operatorID), nil
	}
	// EndShift is only valid from the documented set (ready/pause/status).
	if _, ok := transitions[edge{from: cur.State, event: api.EventEndShift}]; !ok {
		m.metrics.observeInvalid(cur.State, api.EventEndShift)
		return api.Snapshot{},
			fmt.Errorf("%w: %s --end_shift-->", api.ErrInvalidTransition, cur.State)
	}

	now := m.now()
	updated := cur
	updated.State = api.StateOffline
	updated.StateEnteredAt = now
	updated.HeartbeatAt = now
	updated.SessionID = nil
	updated.ProjectID = nil
	updated.CurrentCallID = nil
	updated.RespondentID = nil
	updated.PauseReason = nil

	if err := m.casStore(ctx, tenantID, operatorID, cur.Version, updated); err != nil {
		return api.Snapshot{}, fmt.Errorf("fsm/end_shift: cas: %w", err)
	}
	updated.Version = cur.Version + 1

	if cur.SessionID != nil {
		if err := m.closeSessionAndAudit(ctx, tenantID, operatorID, *cur.SessionID, "", now); err != nil {
			m.log.Error("fsm/end_shift: close-session audit failed; live state already offline",
				zap.Stringer("tenant_id", tenantID),
				zap.Stringer("operator_id", operatorID),
				zap.Stringer("session_id", *cur.SessionID),
				zap.Error(err))
		}
	} else {
		m.log.Warn("fsm/end_shift: no session_id bound; skipping audit",
			zap.Stringer("tenant_id", tenantID),
			zap.Stringer("operator_id", operatorID))
	}

	m.metrics.observeTransition(cur.State, api.StateOffline, api.EventEndShift)
	return updated.toAPI(tenantID, operatorID), nil
}

// GoReady implements api.OperatorFSM. Equivalent to Resume — both fire
// EventResume so they traverse the spec's single pause→ready edge.
// GoReady is the supervisor-style entry point; Resume is operator-facing.
func (m *Machine) GoReady(ctx context.Context, tenantID, operatorID uuid.UUID) (api.Snapshot, error) {
	return m.applyEvent(ctx, tenantID, operatorID, api.EventResume, func(s *Snapshot) {
		s.PauseReason = nil
	})
}

// GoPause implements api.OperatorFSM. Stashes the supervisor-supplied
// reason on the snapshot (for the operator UI + supervisor dashboards).
func (m *Machine) GoPause(ctx context.Context, req api.GoPauseRequest) (api.Snapshot, error) {
	return m.applyEvent(ctx, req.TenantID, req.OperatorID, api.EventGoPause, func(s *Snapshot) {
		reason := req.Reason
		if reason == "" {
			s.PauseReason = nil
			return
		}
		s.PauseReason = &reason
	})
}

// Resume implements api.OperatorFSM. Clears any stashed pause_reason.
func (m *Machine) Resume(ctx context.Context, tenantID, operatorID uuid.UUID) (api.Snapshot, error) {
	return m.applyEvent(ctx, tenantID, operatorID, api.EventResume, func(s *Snapshot) {
		s.PauseReason = nil
	})
}

// RecordCallStarted implements api.OperatorFSM. Used for two distinct
// transitions sharing the same event name:
//
//   - ready → dialing (operator was just allocated to a respondent)
//   - dialing → call (FreeSWITCH ANSWERED; the operator is now talking)
//
// Idempotency:
//
//   - A replay with the same call_id from call (or any state) is a
//     no-op — both call_id and operator state are already where the
//     caller wants them. Handled inside applyEvent's idempotency check
//     once we land in call; the call_id is already pinned on the hash.
//   - A replay with a DIFFERENT call_id while one is in flight returns
//     api.ErrInvalidTransition wrapped with both call IDs — see Plan 10
//     ref doc open-Q resolution. The check fires inside applyEventWith's
//     pre-mutate hook so we don't pay a second HGETALL.
func (m *Machine) RecordCallStarted(ctx context.Context, req api.CallStartedRequest) (api.Snapshot, error) {
	return m.applyEventWith(ctx, req.TenantID, req.OperatorID, api.EventCallStarted,
		func(cur Snapshot) error {
			// Detect a NEW call_id while another is in flight: applyEvent's
			// (state, event) lookup would silently let the transition through
			// and overwrite CurrentCallID. Reject explicitly.
			if cur.CurrentCallID != nil && *cur.CurrentCallID != req.CallID {
				return fmt.Errorf("%w: in-flight call_id=%s, new call_id=%s",
					api.ErrInvalidTransition, *cur.CurrentCallID, req.CallID)
			}
			return nil
		},
		func(s *Snapshot) {
			callID := req.CallID
			respID := req.RespondentID
			s.CurrentCallID = &callID
			s.RespondentID = &respID
		},
		// Replay-from-call short-circuit: if the operator is already in
		// `call` with the SAME call_id, treat as idempotent no-op rather
		// than rejecting on the missing (call, call_started) edge.
		func(cur Snapshot) bool {
			return cur.State == api.StateCall &&
				cur.CurrentCallID != nil &&
				*cur.CurrentCallID == req.CallID
		},
	)
}

// RecordCallEnded implements api.OperatorFSM. Both dialing→status
// (hangup before answer, no-answer) and call→status (normal hangup
// after talk) route to the wrap-up state. The call_id and
// respondent_id flow through unchanged so SubmitStatus has the
// call_id to attach the status row to.
func (m *Machine) RecordCallEnded(ctx context.Context, req api.CallEndedRequest) (api.Snapshot, error) {
	return m.applyEvent(ctx, req.TenantID, req.OperatorID, api.EventCallEnded, nil)
}

// SubmitStatus implements api.OperatorFSM. Clears the call_id /
// respondent_id; the disposition itself is owned by the caller's higher-
// level service (which writes the calls.status field).
func (m *Machine) SubmitStatus(ctx context.Context, req api.SubmitStatusRequest) (api.Snapshot, error) {
	return m.applyEvent(ctx, req.TenantID, req.OperatorID, api.EventStatusSubmitted, func(s *Snapshot) {
		s.CurrentCallID = nil
		s.RespondentID = nil
	})
}

// GoVerify implements api.OperatorFSM. Operator chooses to enter the
// supervisor-style "recheck" mode after submitting a status.
func (m *Machine) GoVerify(ctx context.Context, tenantID, operatorID uuid.UUID) (api.Snapshot, error) {
	return m.applyEvent(ctx, tenantID, operatorID, api.EventGoVerify, nil)
}

// VerifyDone implements api.OperatorFSM. Returns from verify back to
// ready (the next call will be allocated).
func (m *Machine) VerifyDone(ctx context.Context, tenantID, operatorID uuid.UUID) (api.Snapshot, error) {
	return m.applyEvent(ctx, tenantID, operatorID, api.EventVerifyDone, nil)
}

// GetState implements api.OperatorFSM. Read-only path.
func (m *Machine) GetState(ctx context.Context, tenantID, operatorID uuid.UUID) (api.Snapshot, error) {
	cur, err := m.load(ctx, tenantID, operatorID)
	if err != nil {
		return api.Snapshot{}, err
	}
	return cur.toAPI(tenantID, operatorID), nil
}

// Force implements api.OperatorFSM. Bypasses the transition table.
// Used by the heartbeat watchdog (Task 2c) and supervisor admin ops.
//
// Force performs the same Redis CAS write + audit as a transition, but:
//   - It writes target regardless of the source state.
//   - It clears CurrentCallID / RespondentID / PauseReason.
//   - The audit row carries the supplied (normalized) reason.
//   - When forcing to offline, the bound operator_sessions row is closed
//     (ended_at = now()).
//
// reason is normalized to ForceReasonOther when it doesn't match the
// recognized enum so the dialer_fsm_force_total{reason} Prometheus label
// stays low-cardinality. The same normalized value is written to the
// operator_state_log audit row.
func (m *Machine) Force(
	ctx context.Context,
	tenantID, operatorID uuid.UUID,
	target api.State,
	reason api.ForceReason,
) (api.Snapshot, error) {
	if !target.Valid() {
		return api.Snapshot{}, fmt.Errorf("fsm/force: invalid target state %q: %w",
			target, api.ErrUnknownState)
	}
	if !reason.Valid() {
		reason = api.ForceReasonOther
	}
	cur, err := m.load(ctx, tenantID, operatorID)
	if err != nil {
		return api.Snapshot{}, err
	}
	if cur.State == target {
		// Idempotent replay.
		return cur.toAPI(tenantID, operatorID), nil
	}

	now := m.now()
	updated := cur
	updated.State = target
	updated.StateEnteredAt = now
	updated.HeartbeatAt = now
	updated.CurrentCallID = nil
	updated.RespondentID = nil
	updated.PauseReason = nil
	if target == api.StateOffline {
		updated.SessionID = nil
		updated.ProjectID = nil
	}

	if err := m.casStore(ctx, tenantID, operatorID, cur.Version, updated); err != nil {
		return api.Snapshot{}, fmt.Errorf("fsm/force: cas: %w", err)
	}
	updated.Version = cur.Version + 1

	// Audit + session closure (offline only).
	reasonStr := string(reason)
	switch {
	case target == api.StateOffline && cur.SessionID != nil:
		if err := m.closeSessionAndAudit(ctx, tenantID, operatorID, *cur.SessionID, reasonStr, now); err != nil {
			m.log.Error("fsm/force: close-session audit failed; live state already forced",
				zap.Stringer("tenant_id", tenantID),
				zap.Stringer("operator_id", operatorID),
				zap.Stringer("session_id", *cur.SessionID),
				zap.String("reason", reasonStr),
				zap.Error(err))
		}
	case cur.SessionID != nil:
		entry := auditEntry{
			TenantID:    tenantID,
			OperatorID:  operatorID,
			SessionID:   *cur.SessionID,
			NewState:    target,
			Reason:      reasonStr,
			OccurredAt:  now,
			ProjectID:   updated.ProjectID,
			CallID:      updated.CurrentCallID,
			PauseReason: updated.PauseReason,
		}
		if err := m.auditTransition(ctx, entry); err != nil {
			m.log.Error("fsm/force: audit append failed; live state already forced",
				zap.Stringer("tenant_id", tenantID),
				zap.Stringer("operator_id", operatorID),
				zap.String("target", string(target)),
				zap.String("reason", reasonStr),
				zap.Error(err))
		}
	default:
		// No session bound — typically Force(target=offline) on an
		// operator who is already offline (caught above) or forced from
		// a Redis state out of sync with PG. Skip audit.
		m.log.Warn("fsm/force: no session_id bound; skipping audit",
			zap.Stringer("tenant_id", tenantID),
			zap.Stringer("operator_id", operatorID),
			zap.String("target", string(target)),
			zap.String("reason", reasonStr))
	}

	m.metrics.observeForce(target, reason)
	return updated.toAPI(tenantID, operatorID), nil
}
