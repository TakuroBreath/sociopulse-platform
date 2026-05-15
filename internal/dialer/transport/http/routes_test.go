package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	transporthttp "github.com/sociopulse/platform/internal/dialer/transport/http"
)

// =============================================================================
// Fakes
// =============================================================================

// fakeFSM records calls and returns canned values. Each method has a
// dedicated set of input-capture / canned-return / canned-error fields
// so a test can assert what the handler dispatched.
type fakeFSM struct {
	mu sync.Mutex

	startIn  dialerapi.StartShiftRequest
	startRet dialerapi.Snapshot
	startErr error

	endTenant   uuid.UUID
	endOperator uuid.UUID
	endRet      dialerapi.Snapshot
	endErr      error

	pauseIn  dialerapi.GoPauseRequest
	pauseRet dialerapi.Snapshot
	pauseErr error

	resumeTenant   uuid.UUID
	resumeOperator uuid.UUID
	resumeRet      dialerapi.Snapshot
	resumeErr      error

	getTenant   uuid.UUID
	getOperator uuid.UUID
	getRet      dialerapi.Snapshot
	getErr      error

	statusIn  dialerapi.SubmitStatusRequest
	statusRet dialerapi.Snapshot
	statusErr error

	verifyTenant   uuid.UUID
	verifyOperator uuid.UUID
	verifyRet      dialerapi.Snapshot
	verifyErr      error

	verifyDoneTenant   uuid.UUID
	verifyDoneOperator uuid.UUID
	verifyDoneRet      dialerapi.Snapshot
	verifyDoneErr      error

	forceTenant   uuid.UUID
	forceOperator uuid.UUID
	forceTarget   dialerapi.State
	forceReason   dialerapi.ForceReason
	forceRet      dialerapi.Snapshot
	forceErr      error
}

func (f *fakeFSM) StartShift(_ context.Context, req dialerapi.StartShiftRequest) (dialerapi.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startIn = req
	return f.startRet, f.startErr
}
func (f *fakeFSM) EndShift(_ context.Context, t, op uuid.UUID) (dialerapi.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endTenant, f.endOperator = t, op
	return f.endRet, f.endErr
}
func (f *fakeFSM) GoReady(_ context.Context, _, _ uuid.UUID) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}
func (f *fakeFSM) GoPause(_ context.Context, req dialerapi.GoPauseRequest) (dialerapi.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauseIn = req
	return f.pauseRet, f.pauseErr
}
func (f *fakeFSM) Resume(_ context.Context, t, op uuid.UUID) (dialerapi.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeTenant, f.resumeOperator = t, op
	return f.resumeRet, f.resumeErr
}
func (f *fakeFSM) RecordCallStarted(_ context.Context, _ dialerapi.CallStartedRequest) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}
func (f *fakeFSM) RecordCallEnded(_ context.Context, _ dialerapi.CallEndedRequest) (dialerapi.Snapshot, error) {
	return dialerapi.Snapshot{}, nil
}
func (f *fakeFSM) SubmitStatus(_ context.Context, req dialerapi.SubmitStatusRequest) (dialerapi.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusIn = req
	return f.statusRet, f.statusErr
}
func (f *fakeFSM) GoVerify(_ context.Context, t, op uuid.UUID) (dialerapi.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifyTenant, f.verifyOperator = t, op
	return f.verifyRet, f.verifyErr
}
func (f *fakeFSM) VerifyDone(_ context.Context, t, op uuid.UUID) (dialerapi.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifyDoneTenant, f.verifyDoneOperator = t, op
	return f.verifyDoneRet, f.verifyDoneErr
}
func (f *fakeFSM) GetState(_ context.Context, t, op uuid.UUID) (dialerapi.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getTenant, f.getOperator = t, op
	return f.getRet, f.getErr
}
func (f *fakeFSM) Force(_ context.Context, t, op uuid.UUID, target dialerapi.State, reason dialerapi.ForceReason) (dialerapi.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceTenant, f.forceOperator = t, op
	f.forceTarget, f.forceReason = target, reason
	return f.forceRet, f.forceErr
}

// fakeRouter records every Hangup / Dial / Subscribe call and returns
// canned errors. The transport layer never calls Dial / Subscribe;
// only Hangup is exercised via the operator hangup endpoint.
type fakeRouter struct {
	mu          sync.Mutex
	hangupCall  uuid.UUID
	hangupRsn   string
	hangupErr   error
	hangupCount int
}

func (f *fakeRouter) Dial(_ context.Context, _ dialerapi.DialRequest) error {
	return errors.New("fakeRouter: Dial not used by transport tests")
}
func (f *fakeRouter) Hangup(_ context.Context, callID uuid.UUID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hangupCall, f.hangupRsn = callID, reason
	f.hangupCount++
	return f.hangupErr
}
func (f *fakeRouter) Subscribe(_ context.Context, _ uuid.UUID, _ dialerapi.ChannelEventHandler) (func(), error) {
	return func() {}, errors.New("fakeRouter: Subscribe not used by transport tests")
}

// fakeRBAC always allows.
type fakeRBAC struct{}

func (fakeRBAC) Check(_ context.Context, _ authapi.Claims, _ authapi.Action, _ authapi.Resource) error {
	return nil
}

// fakeCallTenantResolver is the test-side fake for dialerapi.CallTenantResolver.
// It backs the RequireSameTenant guard on /api/calls/:id/hangup. Tests
// seed callID → tenantID pairs via set; unseeded ids fall through to
// dialerapi.ErrCallNotFound (mirrors PG NOT FOUND). setErr overrides
// with an arbitrary error to exercise the resolver-internal-failure
// path.
type fakeCallTenantResolver struct {
	mu  sync.Mutex
	m   map[uuid.UUID]uuid.UUID
	err error
}

func newFakeCallTenantResolver() *fakeCallTenantResolver {
	return &fakeCallTenantResolver{m: map[uuid.UUID]uuid.UUID{}}
}

func (r *fakeCallTenantResolver) set(callID, tenantID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[callID] = tenantID
}

func (r *fakeCallTenantResolver) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *fakeCallTenantResolver) LookupCallTenant(_ context.Context, callID uuid.UUID) (uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return uuid.Nil, r.err
	}
	t, ok := r.m[callID]
	if !ok {
		return uuid.Nil, dialerapi.ErrCallNotFound
	}
	return t, nil
}

// fakeValidator returns canned Claims for any token.
type fakeValidator struct {
	claims authapi.Claims
	err    error
}

func (f *fakeValidator) Validate(_ context.Context, _ string) (authapi.Claims, error) {
	if f.err != nil {
		return authapi.Claims{}, f.err
	}
	return f.claims, nil
}

// fakePubSub is a minimal SnapshotPubSub. Each Subscribe receives
// snapshots pushed via Publish. The cancel func closes the channel so
// the WS pump exits cleanly.
type fakePubSub struct {
	mu          sync.Mutex
	subs        map[string]chan dialerapi.Snapshot
	subscribeFn func(t, op uuid.UUID) (chan dialerapi.Snapshot, func())
}

func newFakePubSub() *fakePubSub {
	return &fakePubSub{subs: map[string]chan dialerapi.Snapshot{}}
}

func (p *fakePubSub) Subscribe(tenantID, operatorID uuid.UUID) (<-chan dialerapi.Snapshot, func()) {
	if p.subscribeFn != nil {
		ch, cancel := p.subscribeFn(tenantID, operatorID)
		return ch, cancel
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := tenantID.String() + ":" + operatorID.String()
	ch := make(chan dialerapi.Snapshot, 16)
	p.subs[key] = ch
	cancel := func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		// Idempotent cancel — Subscribe contract requires single-call
		// safety even if the WS handler exits early.
		if c, ok := p.subs[key]; ok {
			close(c)
			delete(p.subs, key)
		}
	}
	return ch, cancel
}

func (p *fakePubSub) Publish(tenantID, operatorID uuid.UUID, snap dialerapi.Snapshot) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := tenantID.String() + ":" + operatorID.String()
	ch, ok := p.subs[key]
	if !ok {
		return false
	}
	select {
	case ch <- snap:
		return true
	default:
		return false
	}
}

// =============================================================================
// Test fixture
// =============================================================================

type fixture struct {
	router      *gin.Engine
	fsm         *fakeFSM
	rt          *fakeRouter
	rbac        fakeRBAC
	validator   *fakeValidator
	pubsub      *fakePubSub
	callTenants *fakeCallTenantResolver

	tenantID uuid.UUID
	userID   uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()

	tenantID := uuid.New()
	userID := uuid.New()

	f := &fixture{
		router: r,
		fsm:    &fakeFSM{},
		rt:     &fakeRouter{},
		rbac:   fakeRBAC{},
		validator: &fakeValidator{
			claims: authapi.Claims{
				UserID:   userID,
				TenantID: tenantID,
				Login:    "alice",
				Roles:    []authapi.Role{authapi.RoleOperator},
			},
		},
		pubsub:      newFakePubSub(),
		callTenants: newFakeCallTenantResolver(),
		tenantID:    tenantID,
		userID:      userID,
	}
	api := r.Group("/api")
	transporthttp.Mount(api, transporthttp.Deps{
		FSM:                f.fsm,
		Router:             f.rt,
		Validator:          f.validator,
		RBAC:               f.rbac,
		SnapshotPubSub:     f.pubsub,
		CallTenantResolver: f.callTenants,
		Logger:             nil,
	})
	return f
}

func (f *fixture) setRoles(rs ...authapi.Role) {
	f.validator.claims.Roles = rs
}

func (f *fixture) doAuth(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var br *bytes.Buffer
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		br = bytes.NewBuffer(raw)
	} else {
		br = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, path, br)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer dummy.token")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "body=%s", rec.Body.String())
	return out
}

func canonicalSnap() dialerapi.Snapshot {
	pid := uuid.New()
	return dialerapi.Snapshot{
		State:          dialerapi.StateReady,
		StateEnteredAt: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
		ProjectID:      &pid,
		HeartbeatAt:    time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
	}
}

// =============================================================================
// /api/sessions/start
// =============================================================================

func TestStartShift_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fsm.startRet = canonicalSnap()
	pid := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/sessions/start",
		transporthttp.StartShiftDTO{ProjectID: pid})
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.StartShiftResponse](t, rec)
	assert.Equal(t, "ready", resp.Snapshot.State)
	assert.Equal(t, pid, f.fsm.startIn.ProjectID)
	assert.Equal(t, f.tenantID, f.fsm.startIn.TenantID)
	assert.Equal(t, f.userID, f.fsm.startIn.OperatorID)
}

func TestStartShift_BadRequest_NoProjectID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/sessions/start",
		map[string]string{})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "dialer.bad_request", body.Code)
}

func TestStartShift_InvalidTransition_409(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fsm.startErr = dialerapi.ErrInvalidTransition
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/sessions/start",
		transporthttp.StartShiftDTO{ProjectID: uuid.New()})
	require.Equal(t, stdhttp.StatusConflict, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "dialer.invalid_transition", body.Code)
}

func TestStartShift_NoRoles_Forbidden(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles() // no roles
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/sessions/start",
		transporthttp.StartShiftDTO{ProjectID: uuid.New()})
	require.Equal(t, stdhttp.StatusForbidden, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "auth.insufficient_role", body.Code)
}

func TestStartShift_NoToken_Unauthorized(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	req := httptest.NewRequest(stdhttp.MethodPost, "/api/sessions/start", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	require.Equal(t, stdhttp.StatusUnauthorized, rec.Code)
}

func TestStartShift_InvalidToken_Unauthorized(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.err = authapi.ErrTokenInvalid
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/sessions/start",
		transporthttp.StartShiftDTO{ProjectID: uuid.New()})
	require.Equal(t, stdhttp.StatusUnauthorized, rec.Code)
}

// =============================================================================
// /api/sessions/end
// =============================================================================

func TestEndShift_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fsm.endRet = dialerapi.Snapshot{State: dialerapi.StateOffline}
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/sessions/end", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code)
	resp := decode[transporthttp.SnapshotDTO](t, rec)
	assert.Equal(t, "offline", resp.State)
	assert.Equal(t, f.tenantID, f.fsm.endTenant)
	assert.Equal(t, f.userID, f.fsm.endOperator)
}

func TestEndShift_TenantMismatch_403(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fsm.endErr = dialerapi.ErrTenantMismatch
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/sessions/end", nil)
	require.Equal(t, stdhttp.StatusForbidden, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "dialer.tenant_mismatch", body.Code)
}

// =============================================================================
// /api/sessions/pause + /api/sessions/resume
// =============================================================================

func TestGoPause_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	pause := "bio_break"
	f.fsm.pauseRet = dialerapi.Snapshot{State: dialerapi.StatePause, PauseReason: &pause}
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/sessions/pause",
		transporthttp.GoPauseDTO{Reason: pause})
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.SnapshotDTO](t, rec)
	assert.Equal(t, "pause", resp.State)
	require.NotNil(t, resp.PauseReason)
	assert.Equal(t, "bio_break", *resp.PauseReason)
	assert.Equal(t, "bio_break", f.fsm.pauseIn.Reason)
}

func TestGoPause_BadRequest_NoReason(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/sessions/pause",
		transporthttp.GoPauseDTO{})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestResume_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fsm.resumeRet = dialerapi.Snapshot{State: dialerapi.StateReady}
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/sessions/resume", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code)
	resp := decode[transporthttp.SnapshotDTO](t, rec)
	assert.Equal(t, "ready", resp.State)
}

func TestResume_Conflict_409(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fsm.resumeErr = dialerapi.ErrConflict
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/sessions/resume", nil)
	require.Equal(t, stdhttp.StatusConflict, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "dialer.conflict", body.Code)
}

// =============================================================================
// /api/sessions/me
// =============================================================================

func TestGetMe_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fsm.getRet = canonicalSnap()
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/sessions/me", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code)
	resp := decode[transporthttp.SnapshotDTO](t, rec)
	assert.Equal(t, "ready", resp.State)
}

func TestGetMe_UnknownState_500Scrubbed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fsm.getErr = dialerapi.ErrUnknownState
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/sessions/me", nil)
	require.Equal(t, stdhttp.StatusInternalServerError, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "dialer.unknown_state", body.Code)
	assert.Equal(t, "internal error", body.Message)
}

// =============================================================================
// /api/calls/:id/status
// =============================================================================

func TestSubmitStatus_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fsm.statusRet = dialerapi.Snapshot{State: dialerapi.StateReady}
	callID := uuid.New()
	respID := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/calls/"+callID.String()+"/status",
		transporthttp.SubmitStatusDTO{
			CallID:       callID,
			RespondentID: respID,
			Status:       "success",
			Comment:      "ok",
		})
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, callID, f.fsm.statusIn.CallID)
	assert.Equal(t, respID, f.fsm.statusIn.RespondentID)
	assert.Equal(t, "success", f.fsm.statusIn.Status)
	assert.Equal(t, "ok", f.fsm.statusIn.Comment)
}

func TestSubmitStatus_BadRequest_PathBodyMismatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	pathID := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/calls/"+pathID.String()+"/status",
		transporthttp.SubmitStatusDTO{
			CallID:       uuid.New(), // different from path
			RespondentID: uuid.New(),
			Status:       "success",
		})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestSubmitStatus_BadRequest_InvalidStatus(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	callID := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/calls/"+callID.String()+"/status",
		map[string]any{
			"call_id":       callID.String(),
			"respondent_id": uuid.New().String(),
			"status":        "weirdo",
		})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestSubmitStatus_BadCallID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/calls/not-a-uuid/status",
		transporthttp.SubmitStatusDTO{
			CallID: uuid.New(), RespondentID: uuid.New(), Status: "success",
		})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestSubmitStatus_InvalidTransition_409(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fsm.statusErr = dialerapi.ErrInvalidTransition
	callID := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/calls/"+callID.String()+"/status",
		transporthttp.SubmitStatusDTO{
			CallID: callID, RespondentID: uuid.New(), Status: "success",
		})
	require.Equal(t, stdhttp.StatusConflict, rec.Code)
}

// =============================================================================
// /api/calls/:id/hangup
// =============================================================================

func TestHangup_HappyPath_NoBody(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	callID := uuid.New()
	// Seed so the cross-tenant guard passes — the call belongs to the
	// caller's own tenant.
	f.callTenants.set(callID, f.tenantID)
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/calls/"+callID.String()+"/hangup", nil)
	require.Equal(t, stdhttp.StatusNoContent, rec.Code)
	assert.Equal(t, callID, f.rt.hangupCall)
	assert.Equal(t, "operator_hangup", f.rt.hangupRsn)
}

func TestHangup_HappyPath_WithReason(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	callID := uuid.New()
	f.callTenants.set(callID, f.tenantID)
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/calls/"+callID.String()+"/hangup",
		transporthttp.HangupDTO{Reason: "supervisor_kick"})
	require.Equal(t, stdhttp.StatusNoContent, rec.Code)
	assert.Equal(t, "supervisor_kick", f.rt.hangupRsn)
}

// TestHangup_BadID covers the RequireSameTenant malformed-:id branch
// (400 BadRequest) — this gates BEFORE the inner hangup handler runs.
func TestHangup_BadID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/calls/not-uuid/hangup", nil)
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestHangup_RouterError_500(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.rt.hangupErr = errors.New("nats: connection lost")
	callID := uuid.New()
	// Seed the resolver so the cross-tenant guard passes for the
	// caller's own tenant (otherwise the middleware aborts 404 and we
	// never reach the router error branch).
	f.callTenants.set(callID, f.tenantID)
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/calls/"+callID.String()+"/hangup", nil)
	require.Equal(t, stdhttp.StatusInternalServerError, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "dialer.internal", body.Code)
	assert.Equal(t, "internal error", body.Message)
	assert.NotContains(t, rec.Body.String(), "nats: connection lost")
}

// TestHangup_CrossTenant_Returns404 pins the Plan 21 Task 3 cross-tenant
// guard (closing the Plan 13.2.5 out-of-scope finding). Tenant A's
// operator JWT must NOT be able to hang up a call owned by Tenant B —
// the previous behaviour silently dispatched the Router.Hangup
// publish, terminating the other tenant's call. The middleware now
// 404-no-body on tenant mismatch (existence-probe defence) BEFORE the
// router is invoked.
func TestHangup_CrossTenant_Returns404(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// The caller's JWT lives on f.tenantID (set by newFixture); seed a
	// DIFFERENT tenant as the call owner so the resolver returns a
	// mismatch.
	otherTenant := uuid.New()
	require.NotEqual(t, f.tenantID, otherTenant, "fixture sanity: tenants must differ")
	callID := uuid.New()
	f.callTenants.set(callID, otherTenant)

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/calls/"+callID.String()+"/hangup", nil)
	require.Equal(t, stdhttp.StatusNotFound, rec.Code,
		"cross-tenant hangup must 404 (no leak of existence)")
	assert.Empty(t, rec.Body.String(),
		"404 body must be empty per RequireSameTenant pattern")
	assert.Equal(t, 0, f.rt.hangupCount,
		"no hangup should be dispatched cross-tenant")
}

// TestHangup_UnknownCall_Returns404 covers the "call id has no row"
// branch — the resolver returns ErrCallNotFound, which the middleware
// translates to 404 (indistinguishable from "wrong tenant" so the
// response cannot be used to enumerate call ids).
func TestHangup_UnknownCall_Returns404(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// No seed → resolver returns ErrCallNotFound for any callID.
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/calls/"+uuid.New().String()+"/hangup", nil)
	require.Equal(t, stdhttp.StatusNotFound, rec.Code,
		"unknown call must 404 (same shape as cross-tenant)")
	assert.Empty(t, rec.Body.String())
	assert.Equal(t, 0, f.rt.hangupCount, "no hangup should be dispatched")
}

// TestHangup_ResolverError_500 covers the resolver-internal-failure
// branch — anything other than ErrCallNotFound is surfaced as 500 so a
// transient storage hiccup does not silently downgrade the caller's
// safety guarantee.
func TestHangup_ResolverError_500(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.callTenants.setErr(errors.New("storage: timeout"))
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/calls/"+uuid.New().String()+"/hangup", nil)
	require.Equal(t, stdhttp.StatusInternalServerError, rec.Code,
		"resolver-internal error must 500, not 404")
	assert.Equal(t, 0, f.rt.hangupCount, "no hangup should be dispatched")
}

// =============================================================================
// /api/operator/verify/start + /done
// =============================================================================

func TestGoVerify_OperatorForbidden(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Operator-only role does NOT have access to verify routes.
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/operator/verify/start", nil)
	require.Equal(t, stdhttp.StatusForbidden, rec.Code)
}

func TestGoVerify_SupervisorHappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleSupervisor)
	f.fsm.verifyRet = dialerapi.Snapshot{State: dialerapi.StateVerify}
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/operator/verify/start", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.SnapshotDTO](t, rec)
	assert.Equal(t, "verify", resp.State)
}

func TestVerifyDone_SupervisorHappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleSupervisor)
	f.fsm.verifyDoneRet = dialerapi.Snapshot{State: dialerapi.StateReady}
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/operator/verify/done", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code)
	resp := decode[transporthttp.SnapshotDTO](t, rec)
	assert.Equal(t, "ready", resp.State)
}

func TestVerifyDone_InvalidTransition_409(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleSupervisor)
	f.fsm.verifyDoneErr = dialerapi.ErrInvalidTransition
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/operator/verify/done", nil)
	require.Equal(t, stdhttp.StatusConflict, rec.Code)
}

// =============================================================================
// /api/operator/:id/force
// =============================================================================

func TestForce_AdminHappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleAdmin)
	f.fsm.forceRet = dialerapi.Snapshot{State: dialerapi.StateOffline}
	target := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/operator/"+target.String()+"/force",
		transporthttp.ForceDTO{
			Target: dialerapi.StateOffline,
			Reason: dialerapi.ForceReasonSupervisorKick,
		})
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, target, f.fsm.forceOperator)
	assert.Equal(t, dialerapi.StateOffline, f.fsm.forceTarget)
	assert.Equal(t, dialerapi.ForceReasonSupervisorKick, f.fsm.forceReason)
	assert.Equal(t, f.tenantID, f.fsm.forceTenant)
}

func TestForce_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleSupervisor) // not admin
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/operator/"+uuid.New().String()+"/force",
		transporthttp.ForceDTO{
			Target: dialerapi.StateOffline,
			Reason: dialerapi.ForceReasonSupervisorKick,
		})
	require.Equal(t, stdhttp.StatusForbidden, rec.Code)
}

func TestForce_BadOperatorID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleAdmin)
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/operator/not-uuid/force",
		transporthttp.ForceDTO{
			Target: dialerapi.StateOffline,
			Reason: dialerapi.ForceReasonSupervisorKick,
		})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestForce_BadTarget(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleAdmin)
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/operator/"+uuid.New().String()+"/force",
		transporthttp.ForceDTO{
			Target: dialerapi.State("bogus"),
			Reason: dialerapi.ForceReasonSupervisorKick,
		})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestForce_BadReason(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleAdmin)
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/operator/"+uuid.New().String()+"/force",
		transporthttp.ForceDTO{
			Target: dialerapi.StateOffline,
			Reason: dialerapi.ForceReason("bogus"),
		})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

// =============================================================================
// Error mapping: every sentinel pinned
// =============================================================================

func TestErrorMapping_AllSentinels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"InvalidTransition", dialerapi.ErrInvalidTransition, stdhttp.StatusConflict, "dialer.invalid_transition"},
		{"Conflict", dialerapi.ErrConflict, stdhttp.StatusConflict, "dialer.conflict"},
		{"TenantMismatch", dialerapi.ErrTenantMismatch, stdhttp.StatusForbidden, "dialer.tenant_mismatch"},
		{"AllNodesFull", dialerapi.ErrAllNodesFull, stdhttp.StatusServiceUnavailable, "dialer.all_nodes_full"},
		{"OutsideWorkingHours", dialerapi.ErrOutsideWorkingHours, stdhttp.StatusUnprocessableEntity, "dialer.outside_working_hours"},
		{"Throttled", dialerapi.ErrThrottled, stdhttp.StatusTooManyRequests, "dialer.throttled"},
		{"QueueEmpty", dialerapi.ErrQueueEmpty, stdhttp.StatusNotFound, "dialer.queue_empty"},
		{"DuplicateInQueue", dialerapi.ErrDuplicateInQueue, stdhttp.StatusConflict, "dialer.duplicate_in_queue"},
		{"UnknownState", dialerapi.ErrUnknownState, stdhttp.StatusInternalServerError, "dialer.unknown_state"},
		{"InsufficientRole", authapi.ErrInsufficientRole, stdhttp.StatusForbidden, "auth.insufficient_role"},
		{"TokenInvalid", authapi.ErrTokenInvalid, stdhttp.StatusUnauthorized, "auth.token_invalid"},
		{"TokenRevoked", authapi.ErrTokenRevoked, stdhttp.StatusUnauthorized, "auth.token_revoked"},
		{"GenericInternal", errors.New("postgres exploded"), stdhttp.StatusInternalServerError, "dialer.internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.fsm.getErr = tc.err
			rec := f.doAuth(t, stdhttp.MethodGet, "/api/sessions/me", nil)
			require.Equal(t, tc.status, rec.Code, "body=%s", rec.Body.String())
			body := decode[transporthttp.ErrorEnvelope](t, rec)
			assert.Equal(t, tc.code, body.Code)
		})
	}
}

// =============================================================================
// Mount panic-on-nil
// =============================================================================

func TestMount_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	type tc struct {
		name string
		deps transporthttp.Deps
	}
	cases := []tc{
		{"nil FSM", transporthttp.Deps{Router: &fakeRouter{}, Validator: &fakeValidator{}, RBAC: fakeRBAC{}, SnapshotPubSub: newFakePubSub(), CallTenantResolver: newFakeCallTenantResolver()}},
		{"nil Router", transporthttp.Deps{FSM: &fakeFSM{}, Validator: &fakeValidator{}, RBAC: fakeRBAC{}, SnapshotPubSub: newFakePubSub(), CallTenantResolver: newFakeCallTenantResolver()}},
		{"nil Validator", transporthttp.Deps{FSM: &fakeFSM{}, Router: &fakeRouter{}, RBAC: fakeRBAC{}, SnapshotPubSub: newFakePubSub(), CallTenantResolver: newFakeCallTenantResolver()}},
		{"nil RBAC", transporthttp.Deps{FSM: &fakeFSM{}, Router: &fakeRouter{}, Validator: &fakeValidator{}, SnapshotPubSub: newFakePubSub(), CallTenantResolver: newFakeCallTenantResolver()}},
		{"nil SnapshotPubSub", transporthttp.Deps{FSM: &fakeFSM{}, Router: &fakeRouter{}, Validator: &fakeValidator{}, RBAC: fakeRBAC{}, CallTenantResolver: newFakeCallTenantResolver()}},
		{"nil CallTenantResolver", transporthttp.Deps{FSM: &fakeFSM{}, Router: &fakeRouter{}, Validator: &fakeValidator{}, RBAC: fakeRBAC{}, SnapshotPubSub: newFakePubSub()}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := gin.New()
			require.Panics(t, func() {
				transporthttp.Mount(r.Group("/api"), c.deps)
			})
		})
	}
}

// =============================================================================
// Cross-cutting / claims wiring
// =============================================================================

func TestRouteAuthAcceptsAdmin(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleAdmin)
	f.fsm.getRet = canonicalSnap()
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/sessions/me", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code)
}

func TestRouteAuthAcceptsSupervisor(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleSupervisor)
	f.fsm.getRet = canonicalSnap()
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/sessions/me", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code)
}

// TestForce_FSMError_500Logged ensures the renderError logger branch
// fires when an internal-status error is returned from Force. We pass
// a real *zap.Logger (no-op observer) so the logger is non-nil and
// the warn/error branch executes.
func TestForce_FSMError_500Logged(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	userID := uuid.New()
	val := &fakeValidator{
		claims: authapi.Claims{
			UserID: userID, TenantID: tenantID, Login: "alice",
			Roles: []authapi.Role{authapi.RoleAdmin},
		},
	}
	fsm := &fakeFSM{forceErr: errors.New("redis: connection lost")}
	r := gin.New()
	api := r.Group("/api")
	transporthttp.Mount(api, transporthttp.Deps{
		FSM:                fsm,
		Router:             &fakeRouter{},
		Validator:          val,
		RBAC:               fakeRBAC{},
		SnapshotPubSub:     newFakePubSub(),
		CallTenantResolver: newFakeCallTenantResolver(),
		Logger:             zap.NewNop(),
	})
	target := uuid.New()
	body := transporthttp.ForceDTO{
		Target: dialerapi.StateOffline,
		Reason: dialerapi.ForceReasonSupervisorKick,
	}
	bs, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(stdhttp.MethodPost, "/api/operator/"+target.String()+"/force",
		bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer dummy.token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, stdhttp.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "redis: connection lost",
		"5xx must NOT echo internal details")
}

func TestSnapshotDTORoundtrip(t *testing.T) {
	t.Parallel()
	pid := uuid.New()
	cid := uuid.New()
	rid := uuid.New()
	pause := "training"
	now := time.Date(2026, 5, 1, 9, 30, 0, 0, time.UTC)
	in := dialerapi.Snapshot{
		State:          dialerapi.StatePause,
		StateEnteredAt: now,
		ProjectID:      &pid,
		CurrentCallID:  &cid,
		RespondentID:   &rid,
		PauseReason:    &pause,
		HeartbeatAt:    now.Add(time.Second),
	}
	f := newFixture(t)
	f.fsm.getRet = in
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/sessions/me", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	out := decode[transporthttp.SnapshotDTO](t, rec)
	assert.Equal(t, "pause", out.State)
	require.NotNil(t, out.ProjectID)
	assert.Equal(t, pid, *out.ProjectID)
	require.NotNil(t, out.PauseReason)
	assert.Equal(t, "training", *out.PauseReason)
}
