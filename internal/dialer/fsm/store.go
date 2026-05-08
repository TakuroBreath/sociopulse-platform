package fsm

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/sociopulse/platform/internal/dialer/api"
)

// Snapshot is the in-process view of one operator's FSM state. It is
// persisted as a Redis hash and as a Postgres operator_state_log row
// (the latter is the audit trail; the former is the live source of truth).
//
// Version is the optimistic-concurrency token incremented by the CAS Lua
// script on every successful write. A Snapshot freshly loaded from Redis
// carries the current version; the caller passes that back when issuing
// the next CAS write.
type Snapshot struct {
	State          api.State
	StateEnteredAt time.Time
	HeartbeatAt    time.Time
	TenantID       uuid.UUID  // defence-in-depth: stored on the hash so cross-tenant access surfaces immediately
	SessionID      *uuid.UUID // FK into operator_sessions; nil while offline
	ProjectID      *uuid.UUID
	CurrentCallID  *uuid.UUID
	RespondentID   *uuid.UUID
	PauseReason    *string
	Version        int64
}

// toAPI converts the package-private Snapshot into the public DTO that
// callers (HTTP handlers, gRPC, NATS publishers) consume.
func (s Snapshot) toAPI(tenantID, operatorID uuid.UUID) api.Snapshot {
	return api.Snapshot{
		TenantID:       tenantID,
		OperatorID:     operatorID,
		State:          s.State,
		StateEnteredAt: s.StateEnteredAt,
		ProjectID:      s.ProjectID,
		CurrentCallID:  s.CurrentCallID,
		RespondentID:   s.RespondentID,
		PauseReason:    s.PauseReason,
		HeartbeatAt:    s.HeartbeatAt,
	}
}

// opKey formats the canonical Redis hash key for an operator. The prefix
// is stable across plans — Plan 10 dialer reads + writes; the watchdog
// (Task 2c) reads heartbeat_at via the same key.
func opKey(tenantID, operatorID uuid.UUID) string {
	return "op:" + tenantID.String() + ":user:" + operatorID.String()
}

// errVersionMismatch is the package-private CAS-failed signal. It wraps
// api.ErrConflict so callers across module boundaries can detect the
// conflict via errors.Is(err, api.ErrConflict) and retry by re-loading
// the current snapshot.
var errVersionMismatch = fmt.Errorf("fsm/store: %w", api.ErrConflict)

// errStartShiftBusy is the package-internal sentinel returned by
// startShiftCAS when the operator's hash exists in a non-offline state
// (mid-shift). Callers translate this to api.ErrInvalidTransition with
// the actual current state for diagnostics.
var errStartShiftBusy = errors.New("fsm: start_shift but operator is mid-shift")

// load reads the Redis hash and parses it. A missing key yields a synthetic
// "offline at version 0" snapshot — first time we see this operator they
// are implicitly offline.
func (m *Machine) load(ctx context.Context, tenantID, operatorID uuid.UUID) (Snapshot, error) {
	res, err := m.rdb.HGetAll(ctx, opKey(tenantID, operatorID)).Result()
	if err != nil {
		return Snapshot{}, fmt.Errorf("fsm/load: hgetall: %w", err)
	}
	if len(res) == 0 {
		// Synthetic offline snapshot. tenant_id is bound on first
		// successful StartShift; until then the hash is missing.
		return Snapshot{
			State:          api.StateOffline,
			StateEnteredAt: m.now(),
			HeartbeatAt:    m.now(),
			TenantID:       uuid.Nil,
			Version:        0,
		}, nil
	}
	s, err := parseHash(res)
	if err != nil {
		return Snapshot{}, err
	}
	// Defence-in-depth tenant check: if the stored tenant_id is set and
	// differs from the request, surface ErrTenantMismatch.
	if s.TenantID != uuid.Nil && s.TenantID != tenantID {
		return Snapshot{}, fmt.Errorf("fsm/load: tenant_id=%s want=%s: %w",
			s.TenantID, tenantID, api.ErrTenantMismatch)
	}
	return s, nil
}

// parseHash decodes the field map returned by HGETALL into a Snapshot.
// All fields are optional except `state`; absent uuid / time fields are
// rendered as zero values. The caller validates state.Valid() and
// surfaces api.ErrUnknownState when a corrupt row is detected.
func parseHash(h map[string]string) (Snapshot, error) {
	var s Snapshot
	rawState := h["state"]
	s.State = api.State(rawState)
	if !s.State.Valid() {
		return Snapshot{}, fmt.Errorf("fsm/parse: state=%q: %w", rawState, api.ErrUnknownState)
	}
	if err := parseTimeFields(h, &s); err != nil {
		return Snapshot{}, err
	}
	if err := parseUUIDFields(h, &s); err != nil {
		return Snapshot{}, err
	}
	if v := h["pause_reason"]; v != "" {
		reason := v
		s.PauseReason = &reason
	}
	if v := h["version"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Snapshot{}, fmt.Errorf("fsm/parse: version=%q: %w", v, err)
		}
		s.Version = n
	}
	return s, nil
}

// parseTimeFields populates the RFC3339Nano timestamp fields on s.
func parseTimeFields(h map[string]string, s *Snapshot) error {
	if v := h["state_entered_at"]; v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return fmt.Errorf("fsm/parse: state_entered_at=%q: %w", v, err)
		}
		s.StateEnteredAt = t
	}
	if v := h["heartbeat_at"]; v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return fmt.Errorf("fsm/parse: heartbeat_at=%q: %w", v, err)
		}
		s.HeartbeatAt = t
	}
	return nil
}

// parseUUIDFields populates the optional uuid.UUID / *uuid.UUID fields
// on s. Each field is independent: an empty string leaves the field at
// its zero value; a non-empty unparseable value surfaces a wrapped
// error naming the field.
func parseUUIDFields(h map[string]string, s *Snapshot) error {
	if v := h["tenant_id"]; v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return fmt.Errorf("fsm/parse: tenant_id=%q: %w", v, err)
		}
		s.TenantID = id
	}
	if v := h["session_id"]; v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return fmt.Errorf("fsm/parse: session_id=%q: %w", v, err)
		}
		s.SessionID = &id
	}
	if v := h["project_id"]; v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return fmt.Errorf("fsm/parse: project_id=%q: %w", v, err)
		}
		s.ProjectID = &id
	}
	if v := h["current_call_id"]; v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return fmt.Errorf("fsm/parse: current_call_id=%q: %w", v, err)
		}
		s.CurrentCallID = &id
	}
	if v := h["respondent_id"]; v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return fmt.Errorf("fsm/parse: respondent_id=%q: %w", v, err)
		}
		s.RespondentID = &id
	}
	return nil
}

//go:embed lua/transition.lua
var transitionLua string

//go:embed lua/start_shift.lua
var startShiftLua string

// transitionScript and startShiftScript are package-level so the cached
// SHA-EVALSHA fast path is reused across calls. redis.NewScript handles
// SCRIPT LOAD lazily — there is no init-time Redis call.
var (
	transitionScript = redis.NewScript(transitionLua)
	startShiftScript = redis.NewScript(startShiftLua)
)

// snapshotPayload renders the field map written by the CAS Lua script.
// Empty / nil fields surface as empty strings, which the Lua script
// translates to HDEL — that's how a transition (e.g. SubmitStatus) clears
// `current_call_id` once a call wraps up.
func snapshotPayload(s Snapshot) ([]byte, error) {
	return json.Marshal(map[string]string{
		"state":            string(s.State),
		"state_entered_at": s.StateEnteredAt.UTC().Format(time.RFC3339Nano),
		"heartbeat_at":     s.HeartbeatAt.UTC().Format(time.RFC3339Nano),
		"tenant_id":        s.TenantID.String(),
		"session_id":       uuidPtrToString(s.SessionID),
		"project_id":       uuidPtrToString(s.ProjectID),
		"current_call_id":  uuidPtrToString(s.CurrentCallID),
		"respondent_id":    uuidPtrToString(s.RespondentID),
		"pause_reason":     stringPtrToString(s.PauseReason),
	})
}

// startShiftPayload differs from snapshotPayload in that uuid.Nil
// tenant_id is explicitly serialised (we need to bind it on first write)
// and the version field is omitted (the Lua script writes version=1).
func startShiftPayload(s Snapshot) ([]byte, error) {
	return json.Marshal(map[string]string{
		"state":            string(s.State),
		"state_entered_at": s.StateEnteredAt.UTC().Format(time.RFC3339Nano),
		"heartbeat_at":     s.HeartbeatAt.UTC().Format(time.RFC3339Nano),
		"tenant_id":        s.TenantID.String(),
		"session_id":       uuidPtrToString(s.SessionID),
		"project_id":       uuidPtrToString(s.ProjectID),
	})
}

// casStore runs the transition Lua script: HGET version → if matches
// expected, HSET fields and HINCRBY version 1 + EXPIRE. Returns
// errVersionMismatch on optimistic-concurrency conflict; any other Redis
// error is wrapped verbatim.
func (m *Machine) casStore(
	ctx context.Context,
	tenantID, operatorID uuid.UUID,
	expectedVersion int64,
	s Snapshot,
) error {
	payload, err := snapshotPayload(s)
	if err != nil {
		return fmt.Errorf("fsm/cas: marshal payload: %w", err)
	}
	res, err := transitionScript.Run(
		ctx, m.rdb,
		[]string{opKey(tenantID, operatorID)},
		expectedVersion, string(payload), int(m.hashTTL.Seconds()),
	).Result()
	if err != nil {
		return fmt.Errorf("fsm/cas: run script: %w", err)
	}
	v, ok := res.(int64)
	if !ok {
		return fmt.Errorf("fsm/cas: unexpected script result type %T", res)
	}
	if v == -1 {
		return errVersionMismatch
	}
	return nil
}

// startShiftCAS runs the start_shift Lua script: idempotent transition
// from missing/offline → ready. Returns:
//
//   - 1, nil — the hash was created/updated; caller proceeds with audit.
//   - 0, nil — idempotent replay (state was already ready); caller
//     re-loads and returns the existing snapshot.
//   - -1 → errStartShiftBusy — the operator is mid-shift in some other
//     state; caller surfaces api.ErrInvalidTransition with the actual
//     current state.
func (m *Machine) startShiftCAS(
	ctx context.Context,
	tenantID, operatorID uuid.UUID,
	s Snapshot,
) (int64, error) {
	payload, err := startShiftPayload(s)
	if err != nil {
		return 0, fmt.Errorf("fsm/start_shift: marshal payload: %w", err)
	}
	res, err := startShiftScript.Run(
		ctx, m.rdb,
		[]string{opKey(tenantID, operatorID)},
		string(payload), int(m.hashTTL.Seconds()),
	).Result()
	if err != nil {
		return 0, fmt.Errorf("fsm/start_shift: run script: %w", err)
	}
	v, ok := res.(int64)
	if !ok {
		return 0, fmt.Errorf("fsm/start_shift: unexpected script result type %T", res)
	}
	if v == -1 {
		return v, errStartShiftBusy
	}
	return v, nil
}

func uuidPtrToString(p *uuid.UUID) string {
	if p == nil {
		return ""
	}
	return p.String()
}

func stringPtrToString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
