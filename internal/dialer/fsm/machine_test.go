package fsm_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	"github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/dialer/fsm"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// fakeSessionStore is an in-memory SessionStore double. CreateSession
// returns a fresh UUID and records the (tenant, user, project) tuple;
// CloseSession marks the session as ended; AppendStateLog records every
// (session_id, ts, state, reason) row in order. The fake never touches
// the postgres.Tx — its zero value is fine.
type fakeSessionStore struct {
	mu       sync.Mutex
	sessions map[uuid.UUID]fakeSession
	stateLog []fakeStateLogRow
	errOn    error
}

type fakeSession struct {
	id        uuid.UUID
	tenantID  uuid.UUID
	userID    uuid.UUID
	projectID uuid.UUID
	endedAt   *time.Time
}

type fakeStateLogRow struct {
	sessionID uuid.UUID
	ts        time.Time
	state     api.State
	reason    string
}

func (f *fakeSessionStore) CreateSession(_ context.Context, _ postgres.Tx, tenantID, userID, projectID uuid.UUID) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errOn != nil {
		return uuid.Nil, f.errOn
	}
	if f.sessions == nil {
		f.sessions = make(map[uuid.UUID]fakeSession)
	}
	id := uuid.New()
	f.sessions[id] = fakeSession{
		id:        id,
		tenantID:  tenantID,
		userID:    userID,
		projectID: projectID,
	}
	return id, nil
}

func (f *fakeSessionStore) CloseSession(_ context.Context, _ postgres.Tx, sessionID uuid.UUID, endedAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errOn != nil {
		return f.errOn
	}
	if s, ok := f.sessions[sessionID]; ok && s.endedAt == nil {
		s.endedAt = &endedAt
		f.sessions[sessionID] = s
	}
	return nil
}

func (f *fakeSessionStore) AppendStateLog(_ context.Context, _ postgres.Tx, sessionID uuid.UUID, ts time.Time, state api.State, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errOn != nil {
		return f.errOn
	}
	f.stateLog = append(f.stateLog, fakeStateLogRow{
		sessionID: sessionID,
		ts:        ts,
		state:     state,
		reason:    reason,
	})
	return nil
}

// LastStateLog returns the most-recent fakeStateLogRow for sessionID, or
// ErrNoStateLog when the in-memory slice has none. The fake never touches
// postgres.Tx — its zero value is fine.
func (f *fakeSessionStore) LastStateLog(_ context.Context, _ postgres.Tx, sessionID uuid.UUID) (fsm.LastStateLogRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errOn != nil {
		return fsm.LastStateLogRow{}, f.errOn
	}
	for i := len(f.stateLog) - 1; i >= 0; i-- {
		if f.stateLog[i].sessionID == sessionID {
			return fsm.LastStateLogRow{
				OccurredAt: f.stateLog[i].ts,
				State:      f.stateLog[i].state,
				Reason:     f.stateLog[i].reason,
			}, nil
		}
	}
	return fsm.LastStateLogRow{}, fsm.ErrNoStateLog
}

func (f *fakeSessionStore) stateLogSnapshot() []fakeStateLogRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeStateLogRow, len(f.stateLog))
	copy(out, f.stateLog)
	return out
}

// fakeOutbox captures every Append call so tests can assert the
// canonical subject + payload shape. Mirrors tenancy/service/test fakes.
type fakeOutbox struct {
	mu    sync.Mutex
	calls []outboxCall
	errOn error
}

type outboxCall struct {
	tenantID    *uuid.UUID
	aggregateID *uuid.UUID
	subject     string
	payload     []byte
}

func (f *fakeOutbox) Append(_ context.Context, _ postgres.Tx, ev outbox.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errOn != nil {
		return f.errOn
	}
	f.calls = append(f.calls, outboxCall{
		tenantID:    ev.TenantID,
		aggregateID: ev.AggregateID,
		subject:     ev.Subject,
		payload:     append([]byte(nil), ev.Payload...),
	})
	return nil
}

func (f *fakeOutbox) snapshot() []outboxCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]outboxCall, 0, len(f.calls))
	// Default snapshot filters to the per-tenant operator-state outbox
	// rows the audit path emits — one per FSM transition. Plan 13.2 § Q7
	// added three "extra" subjects that legacy state-transition tests do
	// not want to see:
	//   - analytics.event.operator_state  (per-transition analytics row)
	//   - analytics.event.calls           (call.finalized analytics row)
	//   - tenant.<t>.dialer.call.finalized (per-tenant call.finalized row)
	// All of these are legitimate outbox writes but they're event-level,
	// not transition-level. Use snapshotAllSubjects to inspect them.
	for _, c := range f.calls {
		if c.subject == analyticsapi.SubjectOperatorStateAnalytics ||
			c.subject == analyticsapi.SubjectCallsAnalytics {
			continue
		}
		if isCallFinalizedSubject(c.subject) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// isCallFinalizedSubject reports whether subj is one of the per-tenant
// tenant.<t>.dialer.call.finalized subjects. The exact tenant token is
// not knowable at filter time, so we match on the stable suffix.
func isCallFinalizedSubject(subj string) bool {
	const suffix = ".dialer.call.finalized"
	const prefix = "tenant."
	if len(subj) <= len(prefix)+len(suffix) {
		return false
	}
	return subj[:len(prefix)] == prefix && subj[len(subj)-len(suffix):] == suffix
}

// fakeTxRunner satisfies fsm.TxRunner without a database. It invokes fn
// with a zero postgres.Tx (the fakeOutbox / fake SessionStore don't use
// it). The fake records the tenantID it's called with so tests can
// assert the audit-tx contract.
type fakeTxRunner struct {
	mu       sync.Mutex
	calls    int
	tenants  []uuid.UUID
	errOnTx  error
	errInTx  error
	beforeFn func()
}

func (f *fakeTxRunner) WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error {
	f.mu.Lock()
	if f.beforeFn != nil {
		f.beforeFn()
	}
	f.calls++
	f.tenants = append(f.tenants, tenantID)
	if f.errOnTx != nil {
		err := f.errOnTx
		f.mu.Unlock()
		return err
	}
	f.mu.Unlock()
	if err := fn(postgres.Tx{}); err != nil {
		return err
	}
	if f.errInTx != nil {
		return f.errInTx
	}
	return nil
}

// helper: full machine wired against miniredis and the in-memory fakes.
type machineFixture struct {
	mr       *miniredis.Miniredis
	rdb      *redis.Client
	pg       *fakeTxRunner
	ob       *fakeOutbox
	sessions *fakeSessionStore
	clock    *fakeClock
	metrics  *fsm.Metrics
	machine  *fsm.Machine
}

func newFixture(t *testing.T) *machineFixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	pg := &fakeTxRunner{}
	ob := &fakeOutbox{}
	sessions := &fakeSessionStore{}
	clk := &fakeClock{now: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)}
	m := fsm.RegisterMetrics(prometheus.NewRegistry())
	mach, err := fsm.New(fsm.Config{
		Redis:    rdb,
		PG:       pg,
		Outbox:   ob,
		Sessions: sessions,
		Logger:   zaptest.NewLogger(t),
		Clock:    clk.Now,
		Metrics:  m,
	})
	require.NoError(t, err)
	return &machineFixture{
		mr:       mr,
		rdb:      rdb,
		pg:       pg,
		ob:       ob,
		sessions: sessions,
		clock:    clk,
		metrics:  m,
		machine:  mach,
	}
}

// fakeClock returns a frozen time, advance via Advance.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newReq(tenantID, operatorID, projectID uuid.UUID) api.StartShiftRequest {
	return api.StartShiftRequest{
		TenantID:   tenantID,
		OperatorID: operatorID,
		ProjectID:  projectID,
		ClientIP:   "127.0.0.1",
	}
}

// TestNew_RejectsMissingDependencies covers the constructor's required-
// field validation. Tests pass nil for the missing field one at a time
// and assert the error mentions the field name.
func TestNew_RejectsMissingDependencies(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	pg := &fakeTxRunner{}
	ob := &fakeOutbox{}

	_, err := fsm.New(fsm.Config{PG: pg, Outbox: ob})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Redis")

	_, err = fsm.New(fsm.Config{Redis: rdb, Outbox: ob})
	require.Error(t, err)
	require.Contains(t, err.Error(), "PG")

	_, err = fsm.New(fsm.Config{Redis: rdb, PG: pg})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Outbox")
}

// TestNew_DefaultsAreApplied verifies nil-tolerant Logger / Clock /
// HashTTL / Metrics / Sessions fields all fill in without erroring.
// Sessions defaults to the canonical pgSessionStore — that's fine here
// because we never actually invoke a method that touches it.
func TestNew_DefaultsAreApplied(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	mach, err := fsm.New(fsm.Config{
		Redis:  rdb,
		PG:     &fakeTxRunner{},
		Outbox: &fakeOutbox{},
	})
	require.NoError(t, err)
	require.NotNil(t, mach)
}

// TestStartShift_FromOffline covers the canonical flow: missing hash →
// ready, with a single operator_sessions insert + first state_log row.
func TestStartShift_FromOffline(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	snap, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)
	require.Equal(t, api.StateReady, snap.State)
	require.Equal(t, tenantID, snap.TenantID)
	require.Equal(t, operatorID, snap.OperatorID)
	require.NotNil(t, snap.ProjectID)
	require.Equal(t, projectID, *snap.ProjectID)
	require.Equal(t, f.clock.Now(), snap.StateEnteredAt)

	// One outbox event for the start_shift transition.
	ob := f.ob.snapshot()
	require.Len(t, ob, 1)
	require.Equal(t, api.SubjectOpStateFor(tenantID, operatorID), ob[0].subject)
	require.NotNil(t, ob[0].tenantID)
	require.Equal(t, tenantID, *ob[0].tenantID)
	require.NotNil(t, ob[0].aggregateID)
	require.Equal(t, operatorID, *ob[0].aggregateID)
}

// TestStartShift_Idempotent: starting twice in a row returns the same
// snapshot; only the first invocation hits the audit tx.
func TestStartShift_Idempotent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	snap1, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)

	// Second StartShift on the same operator returns the existing snapshot.
	snap2, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)
	require.Equal(t, snap1.State, snap2.State)
	require.Equal(t, snap1.StateEnteredAt, snap2.StateEnteredAt)

	// Only the first call wrote to the audit tx.
	ob := f.ob.snapshot()
	require.Len(t, ob, 1, "second StartShift should be a no-op")
}

// TestStartShift_FromBusyState — calling StartShift while the operator
// is mid-shift in pause must surface api.ErrInvalidTransition.
func TestStartShift_FromBusyState(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	// Bring the operator to pause.
	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)
	_, err = f.machine.GoPause(ctx, api.GoPauseRequest{
		TenantID:   tenantID,
		OperatorID: operatorID,
		Reason:     "bio_break",
	})
	require.NoError(t, err)

	// Now StartShift again — must error.
	_, err = f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.ErrorIs(t, err, api.ErrInvalidTransition)
}

// TestFullHappyPath drives every transition in sequence and asserts the
// final outbox emission count matches the expected number of writes.
func TestFullHappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	respondentID, callID := uuid.New(), uuid.New()
	ctx := context.Background()

	// 1. StartShift → ready
	snap, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)
	require.Equal(t, api.StateReady, snap.State)

	// 2. RecordCallStarted from ready → dialing
	f.clock.Advance(time.Second)
	snap, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID:     tenantID,
		OperatorID:   operatorID,
		CallID:       callID,
		RespondentID: respondentID,
		StartedAt:    f.clock.Now(),
	})
	require.NoError(t, err)
	require.Equal(t, api.StateDialing, snap.State)
	require.NotNil(t, snap.CurrentCallID)
	require.Equal(t, callID, *snap.CurrentCallID)

	// 3. RecordCallStarted from dialing → call (ANSWER)
	f.clock.Advance(time.Second)
	snap, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID:     tenantID,
		OperatorID:   operatorID,
		CallID:       callID,
		RespondentID: respondentID,
		StartedAt:    f.clock.Now(),
	})
	require.NoError(t, err)
	require.Equal(t, api.StateCall, snap.State)

	// 4. RecordCallEnded from call → status
	f.clock.Advance(time.Minute)
	snap, err = f.machine.RecordCallEnded(ctx, api.CallEndedRequest{
		TenantID:   tenantID,
		OperatorID: operatorID,
		CallID:     callID,
		EndedAt:    f.clock.Now(),
		Cause:      "NORMAL_CLEARING",
		DurationMS: 60000,
	})
	require.NoError(t, err)
	require.Equal(t, api.StateStatus, snap.State)

	// 5. SubmitStatus → ready (clears call_id / respondent_id)
	f.clock.Advance(5 * time.Second)
	snap, err = f.machine.SubmitStatus(ctx, api.SubmitStatusRequest{
		TenantID:     tenantID,
		OperatorID:   operatorID,
		CallID:       callID,
		RespondentID: respondentID,
		Status:       "success",
	})
	require.NoError(t, err)
	require.Equal(t, api.StateReady, snap.State)
	require.Nil(t, snap.CurrentCallID, "SubmitStatus must clear current_call_id")
	require.Nil(t, snap.RespondentID, "SubmitStatus must clear respondent_id")

	// 6. GoPause → pause
	f.clock.Advance(time.Second)
	snap, err = f.machine.GoPause(ctx, api.GoPauseRequest{
		TenantID:   tenantID,
		OperatorID: operatorID,
		Reason:     "bio_break",
	})
	require.NoError(t, err)
	require.Equal(t, api.StatePause, snap.State)
	require.NotNil(t, snap.PauseReason)
	require.Equal(t, "bio_break", *snap.PauseReason)

	// 7. Resume → ready (clears pause_reason)
	f.clock.Advance(time.Minute)
	snap, err = f.machine.Resume(ctx, tenantID, operatorID)
	require.NoError(t, err)
	require.Equal(t, api.StateReady, snap.State)
	require.Nil(t, snap.PauseReason)

	// 8. EndShift → offline
	f.clock.Advance(time.Second)
	snap, err = f.machine.EndShift(ctx, tenantID, operatorID)
	require.NoError(t, err)
	require.Equal(t, api.StateOffline, snap.State)

	// Outbox: one event per transition. We should see 8 emissions.
	ob := f.ob.snapshot()
	require.Len(t, ob, 8)
}

// TestVerifyFlow covers the supervisor-style verify path:
// status (with success outcome) → verify → ready. Per CONTEXT.md,
// verify is reachable only from `status` and only when the carried
// StatusOutcome is success-class. Driving the FSM through the canonical
// shift+call sequence keeps the test aligned with the production wiring.
func TestVerifyFlow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	respondentID, callID := uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)

	// Drive ready → dialing → call so the next RecordCallEnded lands
	// in `status` with a classified outcome.
	_, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID: tenantID, OperatorID: operatorID, CallID: callID, RespondentID: respondentID,
	})
	require.NoError(t, err)
	_, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID: tenantID, OperatorID: operatorID, CallID: callID, RespondentID: respondentID,
	})
	require.NoError(t, err)

	// RecordCallEnded with OutcomeSuccess → status with success-class
	// outcome stashed on the snapshot.
	snap, err := f.machine.RecordCallEnded(ctx, api.CallEndedRequest{
		TenantID: tenantID, OperatorID: operatorID, CallID: callID,
		Outcome: api.OutcomeSuccess,
	})
	require.NoError(t, err)
	require.Equal(t, api.StateStatus, snap.State)
	require.Equal(t, api.OutcomeSuccess, snap.Outcome)

	// status (success-class) → verify
	snap, err = f.machine.GoVerify(ctx, tenantID, operatorID)
	require.NoError(t, err)
	require.Equal(t, api.StateVerify, snap.State)

	// verify → ready
	snap, err = f.machine.VerifyDone(ctx, tenantID, operatorID)
	require.NoError(t, err)
	require.Equal(t, api.StateReady, snap.State)
}

// TestVerifyFlow_NonSuccessOutcome_RejectsGoVerify asserts the
// CONTEXT.md guarantee that non-success outcomes block the verify
// transition. A no-answer / busy / tech-failure operator must finish
// wrap-up via SubmitStatus rather than entering verify.
func TestVerifyFlow_NonSuccessOutcome_RejectsGoVerify(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	respondentID, callID := uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)

	_, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID: tenantID, OperatorID: operatorID, CallID: callID, RespondentID: respondentID,
	})
	require.NoError(t, err)

	// dialing → status with OutcomeNoAnswer (hangup before answer).
	snap, err := f.machine.RecordCallEnded(ctx, api.CallEndedRequest{
		TenantID: tenantID, OperatorID: operatorID, CallID: callID,
		Outcome: api.OutcomeNoAnswer,
	})
	require.NoError(t, err)
	require.Equal(t, api.StateStatus, snap.State)

	// GoVerify must fail-loud — outcome is not success class.
	_, err = f.machine.GoVerify(ctx, tenantID, operatorID)
	require.ErrorIs(t, err, api.ErrInvalidTransition)
}

// TestRecordCallStarted_ReplaySameCallID — replay with the same call_id
// is an idempotent no-op.
func TestRecordCallStarted_ReplaySameCallID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	respondentID, callID := uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)

	// First RecordCallStarted → dialing.
	snap, err := f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID:     tenantID,
		OperatorID:   operatorID,
		CallID:       callID,
		RespondentID: respondentID,
	})
	require.NoError(t, err)
	require.Equal(t, api.StateDialing, snap.State)

	// Second RecordCallStarted with the SAME call_id — dialing → call.
	snap, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID:     tenantID,
		OperatorID:   operatorID,
		CallID:       callID,
		RespondentID: respondentID,
	})
	require.NoError(t, err)
	require.Equal(t, api.StateCall, snap.State)

	// Third RecordCallStarted with the SAME call_id — call → call (idempotent replay).
	snap, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID:     tenantID,
		OperatorID:   operatorID,
		CallID:       callID,
		RespondentID: respondentID,
	})
	require.NoError(t, err)
	require.Equal(t, api.StateCall, snap.State, "replay with same call_id from call must be idempotent")
}

// TestRecordCallStarted_DifferentCallIDInFlight — Plan 10 ref doc
// open-Q resolution: replay with a DIFFERENT call_id while a call is
// in flight returns ErrInvalidTransition.
func TestRecordCallStarted_DifferentCallIDInFlight(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	respondentID := uuid.New()
	ctx := context.Background()

	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)

	// First call starts.
	firstCall := uuid.New()
	_, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID:     tenantID,
		OperatorID:   operatorID,
		CallID:       firstCall,
		RespondentID: respondentID,
	})
	require.NoError(t, err)

	// A DIFFERENT call_id is sent while the first is still in flight.
	secondCall := uuid.New()
	_, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID:     tenantID,
		OperatorID:   operatorID,
		CallID:       secondCall,
		RespondentID: respondentID,
	})
	require.ErrorIs(t, err, api.ErrInvalidTransition)
	require.Contains(t, err.Error(), firstCall.String())
	require.Contains(t, err.Error(), secondCall.String())
}

// TestForce_FromAnyStateToOffline — the watchdog escape hatch.
func TestForce_FromAnyStateToOffline(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	respondentID, callID := uuid.New(), uuid.New()
	ctx := context.Background()

	// Drive to call state.
	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)
	_, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID: tenantID, OperatorID: operatorID, CallID: callID, RespondentID: respondentID,
	})
	require.NoError(t, err)
	_, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID: tenantID, OperatorID: operatorID, CallID: callID, RespondentID: respondentID,
	})
	require.NoError(t, err)

	// Force offline from call state — bypasses transition table.
	snap, err := f.machine.Force(ctx, tenantID, operatorID, api.StateOffline, api.ForceReasonHeartbeatLost)
	require.NoError(t, err)
	require.Equal(t, api.StateOffline, snap.State)
	require.Nil(t, snap.CurrentCallID)
	require.Nil(t, snap.RespondentID)
	require.Nil(t, snap.ProjectID)
}

// TestForce_RejectsInvalidTarget — Force must validate target.Valid().
func TestForce_RejectsInvalidTarget(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID := uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.machine.Force(ctx, tenantID, operatorID, api.State("garbage"), api.ForceReasonAdminOverride)
	require.ErrorIs(t, err, api.ErrUnknownState)
}

// TestForce_IdempotentOnSameTarget — re-forcing to the current state is a
// no-op; no Redis write, no audit row.
func TestForce_IdempotentOnSameTarget(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID := uuid.New(), uuid.New()
	ctx := context.Background()

	// Operator starts implicitly offline. Force-offline is a no-op.
	snap, err := f.machine.Force(ctx, tenantID, operatorID, api.StateOffline, api.ForceReasonHeartbeatLost)
	require.NoError(t, err)
	require.Equal(t, api.StateOffline, snap.State)

	// No outbox entry written.
	ob := f.ob.snapshot()
	require.Empty(t, ob, "force on identical state must be a no-op")
}

// TestGetState — read-only access never triggers an audit write.
func TestGetState(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	// Initial state on a never-seen operator: synthesised offline.
	snap, err := f.machine.GetState(ctx, tenantID, operatorID)
	require.NoError(t, err)
	require.Equal(t, api.StateOffline, snap.State)

	_, err = f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)

	snap, err = f.machine.GetState(ctx, tenantID, operatorID)
	require.NoError(t, err)
	require.Equal(t, api.StateReady, snap.State)
}

// TestGoReady_AliasForResume — GoReady from pause is equivalent to Resume.
func TestGoReady_AliasForResume(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)
	_, err = f.machine.GoPause(ctx, api.GoPauseRequest{
		TenantID: tenantID, OperatorID: operatorID, Reason: "bio_break",
	})
	require.NoError(t, err)

	snap, err := f.machine.GoReady(ctx, tenantID, operatorID)
	require.NoError(t, err)
	require.Equal(t, api.StateReady, snap.State)
	require.Nil(t, snap.PauseReason)
}

// TestInvalidTransition — calling EndShift while offline must surface
// ErrInvalidTransition (the synthesized initial state). Idempotency
// short-circuits BEFORE the transition lookup, so EndShift on offline
// is actually a no-op (NOT an error). Verify.
func TestEndShift_OnOffline_IsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID := uuid.New(), uuid.New()
	ctx := context.Background()

	snap, err := f.machine.EndShift(ctx, tenantID, operatorID)
	require.NoError(t, err)
	require.Equal(t, api.StateOffline, snap.State)
	require.Empty(t, f.ob.snapshot())
}

// TestInvalidTransition_GoPauseFromOffline — pause from offline is
// invalid (operator must StartShift first).
func TestInvalidTransition_GoPauseFromOffline(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID := uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.machine.GoPause(ctx, api.GoPauseRequest{
		TenantID: tenantID, OperatorID: operatorID, Reason: "bio_break",
	})
	require.ErrorIs(t, err, api.ErrInvalidTransition)
}

// TestTenantMismatch — defence-in-depth: if the live hash's stored
// tenant_id differs from the request tenant, the FSM rejects with
// api.ErrTenantMismatch. The canonical path can't normally hit this
// (the Redis key encodes tenant) but we forge it by writing the hash
// with a different tenant_id field and fetching under a key that
// matches; this is the equivalent of a key-collision attack.
func TestTenantMismatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantA, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	otherTenant := uuid.New()
	ctx := context.Background()

	// Bring tenantA's hash into ready state.
	_, err := f.machine.StartShift(ctx, newReq(tenantA, operatorID, projectID))
	require.NoError(t, err)

	// Forge the stored tenant_id (simulate a corrupt write or a key
	// collision). We do this by overwriting the field directly on
	// miniredis. The key is the same; the stored tenant_id no longer
	// matches what the caller requests.
	f.mr.HSet("op:"+tenantA.String()+":user:"+operatorID.String(), "tenant_id", otherTenant.String())

	// Now any operation on tenantA's key surfaces ErrTenantMismatch
	// because the stored tenant_id no longer matches.
	_, err = f.machine.GetState(ctx, tenantA, operatorID)
	require.ErrorIs(t, err, api.ErrTenantMismatch)
}

// TestAuditFailureDoesNotRollbackLiveState — when the audit tx fails
// after the Redis CAS succeeded, the FSM logs and returns the new
// snapshot anyway. The state has already changed; outbox failure is
// recoverable.
func TestAuditFailureDoesNotRollbackLiveState(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()
	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)

	// Now poison the audit tx and trigger a transition.
	f.pg.errOnTx = errors.New("audit-tx-fail")

	// GoPause must not return the audit error — the live state has changed.
	snap, err := f.machine.GoPause(ctx, api.GoPauseRequest{
		TenantID: tenantID, OperatorID: operatorID, Reason: "bio_break",
	})
	require.NoError(t, err)
	require.Equal(t, api.StatePause, snap.State)

	// Verify Redis hash reflects pause.
	st := f.mr.HGet("op:"+tenantID.String()+":user:"+operatorID.String(), "state")
	require.Equal(t, "pause", st)
}

// TestRecordCallEnded_FromDialing — dialing → status preserves the call_id
// so SubmitStatus has the call to attach the wrap-up disposition to.
func TestRecordCallEnded_FromDialing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	respondentID, callID := uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)
	_, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID: tenantID, OperatorID: operatorID, CallID: callID, RespondentID: respondentID,
	})
	require.NoError(t, err)

	// CallEnded from dialing → status (no-answer scenario routes through wrap-up).
	snap, err := f.machine.RecordCallEnded(ctx, api.CallEndedRequest{
		TenantID:   tenantID,
		OperatorID: operatorID,
		CallID:     callID,
		Cause:      "NO_ANSWER",
	})
	require.NoError(t, err)
	require.Equal(t, api.StateStatus, snap.State)
	require.NotNil(t, snap.CurrentCallID, "CallEnded from dialing must preserve current_call_id for SubmitStatus")
	require.Equal(t, callID, *snap.CurrentCallID)
	require.NotNil(t, snap.RespondentID, "CallEnded from dialing must preserve respondent_id for SubmitStatus")
	require.Equal(t, respondentID, *snap.RespondentID)
}

// TestSnapshotPersistsHeartbeatAt — every successful transition refreshes
// heartbeat_at to the current clock time.
func TestHeartbeatAtRefreshedOnEveryTransition(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	t1 := f.clock.Now()
	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)

	f.clock.Advance(2 * time.Minute)
	t2 := f.clock.Now()
	snap, err := f.machine.GoPause(ctx, api.GoPauseRequest{
		TenantID: tenantID, OperatorID: operatorID, Reason: "bio_break",
	})
	require.NoError(t, err)
	require.True(t, snap.HeartbeatAt.After(t1))
	require.Equal(t, t2, snap.HeartbeatAt)
}

// TestStartShift_RedisCASBusy — when the start_shift Lua script
// returns -1 (hash exists in some non-offline state at the moment of
// CAS), the FSM surfaces ErrInvalidTransition and orphans the just-
// inserted PG session. We forge this by writing the hash directly to
// miniredis between load() and the CAS Lua call. Because the FSM is
// single-goroutine in this test, we instead use the path where the
// load() pre-check sees offline (the hash is missing) but a parallel
// fixture's StartShift wrote the hash — simulated by injecting state
// into the hash AFTER load via the beforeFn TxRunner hook.
func TestStartShift_RedisCASBusy(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	pg := &fakeTxRunner{}
	ob := &fakeOutbox{}
	sessions := &fakeSessionStore{}
	clk := &fakeClock{now: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)}
	mach, err := fsm.New(fsm.Config{
		Redis:    rdb,
		PG:       pg,
		Outbox:   ob,
		Sessions: sessions,
		Logger:   zaptest.NewLogger(t),
		Clock:    clk.Now,
	})
	require.NoError(t, err)

	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()

	// Hook: between session insert and Redis CAS, plant a busy hash.
	// pg.beforeFn fires as the audit Tx opens, which is *after*
	// load() but *before* the start_shift Lua runs. Result: load saw
	// "missing" (treats as offline) and CreateSession succeeded, but
	// when start_shift Lua runs it observes a busy hash and returns -1.
	pg.beforeFn = func() {
		key := "op:" + tenantID.String() + ":user:" + operatorID.String()
		now := clk.Now().UTC().Format(time.RFC3339Nano)
		mr.HSet(key,
			"state", "pause", // mid-shift in pause — Lua returns -1
			"tenant_id", tenantID.String(),
			"state_entered_at", now,
			"heartbeat_at", now,
			"version", "1",
		)
	}

	_, err = mach.StartShift(context.Background(), api.StartShiftRequest{
		TenantID: tenantID, OperatorID: operatorID, ProjectID: projectID,
	})
	require.ErrorIs(t, err, api.ErrInvalidTransition)
}

// TestStartShift_RedisCASIdempotentReplay — when start_shift Lua
// returns 0 (already-ready replay between load and CAS), the FSM
// re-loads and returns the existing snapshot.
func TestStartShift_RedisCASIdempotentReplay(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	pg := &fakeTxRunner{}
	ob := &fakeOutbox{}
	sessions := &fakeSessionStore{}
	clk := &fakeClock{now: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)}
	mach, err := fsm.New(fsm.Config{
		Redis:    rdb,
		PG:       pg,
		Outbox:   ob,
		Sessions: sessions,
		Logger:   zaptest.NewLogger(t),
		Clock:    clk.Now,
	})
	require.NoError(t, err)

	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()

	pg.beforeFn = func() {
		key := "op:" + tenantID.String() + ":user:" + operatorID.String()
		now := clk.Now().UTC().Format(time.RFC3339Nano)
		mr.HSet(key,
			"state", "ready",
			"tenant_id", tenantID.String(),
			"project_id", projectID.String(),
			"state_entered_at", now,
			"heartbeat_at", now,
			"version", "1",
		)
	}

	snap, err := mach.StartShift(context.Background(), api.StartShiftRequest{
		TenantID: tenantID, OperatorID: operatorID, ProjectID: projectID,
	})
	require.NoError(t, err)
	require.Equal(t, api.StateReady, snap.State)
}

// TestEndShift_InvalidFromCall — EndShift from call must surface
// ErrInvalidTransition (cannot end a shift mid-call; use Force).
func TestEndShift_InvalidFromCall(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	respondentID, callID := uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)
	_, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID: tenantID, OperatorID: operatorID, CallID: callID, RespondentID: respondentID,
	})
	require.NoError(t, err)
	_, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID: tenantID, OperatorID: operatorID, CallID: callID, RespondentID: respondentID,
	})
	require.NoError(t, err)

	_, err = f.machine.EndShift(ctx, tenantID, operatorID)
	require.ErrorIs(t, err, api.ErrInvalidTransition)
}

// TestForce_FromCallToOffline_AuditFailure — covers the audit-failed
// branch in Force when target=offline.
func TestForce_FromCallToOffline_AuditFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	respondentID, callID := uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)
	_, err = f.machine.RecordCallStarted(ctx, api.CallStartedRequest{
		TenantID: tenantID, OperatorID: operatorID, CallID: callID, RespondentID: respondentID,
	})
	require.NoError(t, err)

	// Poison audit and Force.
	f.pg.errOnTx = errors.New("audit-tx-fail")
	snap, err := f.machine.Force(ctx, tenantID, operatorID, api.StateOffline, api.ForceReasonSupervisorKick)
	require.NoError(t, err)
	require.Equal(t, api.StateOffline, snap.State)
}

// TestForce_NonOfflineTarget_AuditFailure — covers Force where target
// is NOT offline and audit fails.
func TestForce_NonOfflineTarget_AuditFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)

	f.pg.errOnTx = errors.New("audit-tx-fail")
	snap, err := f.machine.Force(ctx, tenantID, operatorID, api.StatePause, api.ForceReasonSupervisorKick)
	require.NoError(t, err)
	require.Equal(t, api.StatePause, snap.State)
}

// TestGoPause_EmptyReason — empty reason clears the field rather than
// storing an empty string.
func TestGoPause_EmptyReason(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)

	snap, err := f.machine.GoPause(ctx, api.GoPauseRequest{
		TenantID: tenantID, OperatorID: operatorID, Reason: "",
	})
	require.NoError(t, err)
	require.Equal(t, api.StatePause, snap.State)
	require.Nil(t, snap.PauseReason, "empty reason must clear the field")
}

// TestEndShift_AuditFailure — when the EndShift audit Tx fails, the
// live state has already changed to offline; the function logs and
// returns the new snapshot.
func TestEndShift_AuditFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)

	// Poison audit; trigger EndShift.
	f.pg.errOnTx = errors.New("audit-tx-fail")
	snap, err := f.machine.EndShift(ctx, tenantID, operatorID)
	require.NoError(t, err)
	require.Equal(t, api.StateOffline, snap.State)
}

// TestRegisterMetrics_PanicOnNilRegistry — wiring guard.
func TestRegisterMetrics_PanicOnNilRegistry(t *testing.T) {
	t.Parallel()
	require.PanicsWithValue(t,
		"fsm.RegisterMetrics: reg must be non-nil; pass prometheus.NewRegistry() in tests",
		func() { fsm.RegisterMetrics(nil) },
	)
}

// TestNilMetricsTolerated — Machine without Metrics never panics.
func TestNilMetricsTolerated(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	mach, err := fsm.New(fsm.Config{
		Redis:    rdb,
		PG:       &fakeTxRunner{},
		Outbox:   &fakeOutbox{},
		Sessions: &fakeSessionStore{},
		Logger:   zaptest.NewLogger(t),
		// Metrics: nil
	})
	require.NoError(t, err)

	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()
	_, err = mach.StartShift(ctx, api.StartShiftRequest{
		TenantID: tenantID, OperatorID: operatorID, ProjectID: projectID,
	})
	require.NoError(t, err)
	// Trigger an invalid transition to exercise the nil-metrics path.
	// VerifyDone from ready (operator never entered verify) is invalid.
	_, err = mach.VerifyDone(ctx, tenantID, operatorID)
	require.ErrorIs(t, err, api.ErrInvalidTransition)
	// Force, also nil-metrics path.
	_, err = mach.Force(ctx, tenantID, operatorID, api.StateOffline, api.ForceReasonAdminOverride)
	require.NoError(t, err)
}

// TestMetricsCounters — verify the transitions / invalid / force metrics
// fire on the canonical pathways.
func TestMetricsCounters(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tenantID, operatorID, projectID := uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()

	_, err := f.machine.StartShift(ctx, newReq(tenantID, operatorID, projectID))
	require.NoError(t, err)

	// Trigger an invalid transition: VerifyDone from ready (operator
	// never entered verify) is rejected by the transition table.
	_, err = f.machine.VerifyDone(ctx, tenantID, operatorID)
	require.ErrorIs(t, err, api.ErrInvalidTransition)

	// Trigger a force.
	_, err = f.machine.Force(ctx, tenantID, operatorID, api.StateOffline, api.ForceReasonSupervisorKick)
	require.NoError(t, err)

	// Metrics counters: at least one transition, one invalid, one force.
	require.InDelta(t, 1.0, testCounterValue(t, f.metrics.Transitions, "offline", "ready", "start_shift"), 0.01)
	require.InDelta(t, 1.0, testCounterValue(t, f.metrics.InvalidTransitions, "ready", "verify_done"), 0.01)
	require.InDelta(t, 1.0, testCounterValue(t, f.metrics.Force, "offline", "supervisor_kick"), 0.01)
}

// testCounterValue extracts the current value of a CounterVec for the
// given label values. Returns 0 if the series doesn't exist.
func testCounterValue(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := cv.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	return testutil.ToFloat64(c)
}
