package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	transporthttp "github.com/sociopulse/platform/internal/auth/transport/http"
)

// =============================================================================
// Fakes
// =============================================================================

// fakeAuthenticator captures the last call and returns canned values.
type fakeAuthenticator struct {
	loginIn      authapi.LoginInput
	loginRes     authapi.AuthResult
	loginErr     error
	loginTOTPIn  authapi.LoginTOTPInput
	loginTOTPRes authapi.AuthResult
	loginTOTPErr error
	refreshTok   string
	refreshIP    netip.Addr
	refreshRes   authapi.AuthResult
	refreshErr   error
	logoutTok    string
	logoutErr    error
}

func (f *fakeAuthenticator) Login(_ context.Context, in authapi.LoginInput) (authapi.AuthResult, error) {
	f.loginIn = in
	return f.loginRes, f.loginErr
}

func (f *fakeAuthenticator) LoginTOTP(_ context.Context, in authapi.LoginTOTPInput) (authapi.AuthResult, error) {
	f.loginTOTPIn = in
	return f.loginTOTPRes, f.loginTOTPErr
}

func (f *fakeAuthenticator) Refresh(_ context.Context, tok string, ip netip.Addr) (authapi.AuthResult, error) {
	f.refreshTok = tok
	f.refreshIP = ip
	return f.refreshRes, f.refreshErr
}

func (f *fakeAuthenticator) Logout(_ context.Context, tok string) error {
	f.logoutTok = tok
	return f.logoutErr
}

func (f *fakeAuthenticator) ValidateAccessToken(_ context.Context, _ string) (authapi.Claims, error) {
	return authapi.Claims{}, nil
}

// fakeUserService records calls and returns canned values.
type fakeUserService struct {
	getCalls          []uuid.UUID
	getRet            authapi.User
	getErr            error
	createIn          authapi.CreateUserInput
	createRetUser     authapi.User
	createRetTempPwd  string
	createErr         error
	listIn            authapi.ListUsersInput
	listRet           []authapi.User
	listTotal         int64
	listErr           error
	updateRoleID      uuid.UUID
	updateRoleRoles   []authapi.Role
	updateRoleRetUser authapi.User
	updateRoleErr     error
	archiveID         uuid.UUID
	archiveErr        error
	restoreID         uuid.UUID
	restoreErr        error
	resetID           uuid.UUID
	resetTempPwd      string
	resetErr          error
	changePwdID       uuid.UUID
	changePwdOld      string
	changePwdNew      string
	changePwdErr      error
}

func (f *fakeUserService) Create(_ context.Context, in authapi.CreateUserInput) (authapi.User, string, error) {
	f.createIn = in
	return f.createRetUser, f.createRetTempPwd, f.createErr
}

func (f *fakeUserService) List(_ context.Context, in authapi.ListUsersInput) ([]authapi.User, int64, error) {
	f.listIn = in
	return f.listRet, f.listTotal, f.listErr
}

func (f *fakeUserService) Get(_ context.Context, id uuid.UUID) (authapi.User, error) {
	f.getCalls = append(f.getCalls, id)
	return f.getRet, f.getErr
}

func (f *fakeUserService) UpdateRole(_ context.Context, id uuid.UUID, roles []authapi.Role) (authapi.User, error) {
	f.updateRoleID = id
	f.updateRoleRoles = roles
	return f.updateRoleRetUser, f.updateRoleErr
}

func (f *fakeUserService) Archive(_ context.Context, id uuid.UUID) error {
	f.archiveID = id
	return f.archiveErr
}

func (f *fakeUserService) Restore(_ context.Context, id uuid.UUID) error {
	f.restoreID = id
	return f.restoreErr
}

func (f *fakeUserService) ResetPassword(_ context.Context, id uuid.UUID) (string, error) {
	f.resetID = id
	return f.resetTempPwd, f.resetErr
}

func (f *fakeUserService) ChangePassword(_ context.Context, id uuid.UUID, oldPwd, newPwd string) error {
	f.changePwdID = id
	f.changePwdOld = oldPwd
	f.changePwdNew = newPwd
	return f.changePwdErr
}

// fakeTOTPService records calls.
type fakeTOTPService struct {
	enrollID    uuid.UUID
	enrollRet   authapi.TOTPEnrollment
	enrollErr   error
	confirmID   uuid.UUID
	confirmCode string
	confirmErr  error
	verifyID    uuid.UUID
	verifyCode  string
	verifyOK    bool
	verifyErr   error
	disableID   uuid.UUID
	disableErr  error
	statusID    uuid.UUID
	statusRet   authapi.TOTPStatus
	statusErr   error
}

func (f *fakeTOTPService) Enroll(_ context.Context, id uuid.UUID) (authapi.TOTPEnrollment, error) {
	f.enrollID = id
	return f.enrollRet, f.enrollErr
}

func (f *fakeTOTPService) Confirm(_ context.Context, id uuid.UUID, code string) error {
	f.confirmID = id
	f.confirmCode = code
	return f.confirmErr
}

func (f *fakeTOTPService) Verify(_ context.Context, id uuid.UUID, code string) (bool, error) {
	f.verifyID = id
	f.verifyCode = code
	return f.verifyOK, f.verifyErr
}

func (f *fakeTOTPService) Disable(_ context.Context, id uuid.UUID) error {
	f.disableID = id
	return f.disableErr
}

func (f *fakeTOTPService) Status(_ context.Context, id uuid.UUID) (authapi.TOTPStatus, error) {
	f.statusID = id
	return f.statusRet, f.statusErr
}

// fakeRBAC always allows; tests that need a deny override Check.
type fakeRBAC struct {
	denyAll bool
}

func (f *fakeRBAC) Check(_ context.Context, _ authapi.Claims, _ authapi.Action, _ authapi.Resource) error {
	if f.denyAll {
		return authapi.ErrInsufficientRole
	}
	return nil
}

// fakeValidator returns canned Claims for any token.
type fakeValidator struct {
	claims authapi.Claims
	err    error
}

func (f *fakeValidator) Validate(_ context.Context, _ string) (authapi.Claims, error) {
	return f.claims, f.err
}

// =============================================================================
// Test scaffolding
// =============================================================================

type fixture struct {
	router    *gin.Engine
	auth      *fakeAuthenticator
	users     *fakeUserService
	totp      *fakeTOTPService
	rbac      *fakeRBAC
	validator *fakeValidator
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	f := &fixture{
		router:    r,
		auth:      &fakeAuthenticator{},
		users:     &fakeUserService{},
		totp:      &fakeTOTPService{},
		rbac:      &fakeRBAC{},
		validator: &fakeValidator{},
	}
	api := r.Group("/api")
	transporthttp.Mount(api, transporthttp.Deps{
		Logger:    nil,
		Auth:      f.auth,
		Users:     f.users,
		TOTP:      f.totp,
		RBAC:      f.rbac,
		Validator: f.validator,
	})
	return f
}

// do issues a request through the test router. The signature accepts
// `method` even though the public-route tests in this package only use
// POST today — keeping the parameter mirrors doAuth and lets future
// GET/DELETE tests call the same helper without a forked variant.
//
//nolint:unparam // method param is shared between this and doAuth; future tests will exercise other verbs
func (f *fixture) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *bytes.Buffer
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		bodyReader = bytes.NewBuffer(raw)
	} else {
		bodyReader = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) doAuth(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *bytes.Buffer
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		bodyReader = bytes.NewBuffer(raw)
	} else {
		bodyReader = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, path, bodyReader)
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

// =============================================================================
// Public endpoints
// =============================================================================

func TestLogin_HappyPath_ReturnsTokens(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	userID := uuid.New()
	tenantID := uuid.New()
	expiresAt := time.Now().Add(15 * time.Minute).UTC()
	f.auth.loginRes = authapi.AuthResult{
		AccessToken:      "access",
		AccessExpiresAt:  expiresAt,
		RefreshToken:     "refresh",
		RefreshExpiresAt: expiresAt.Add(30 * 24 * time.Hour),
		User:             authapi.User{ID: userID, TenantID: tenantID, Login: "alice"},
	}

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/login", transporthttp.LoginRequest{
		OrgID:    "CC-MOSKVA-01",
		Login:    "alice",
		Password: "secret-password",
	})

	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.LoginResponse](t, rec)
	assert.Equal(t, "access", resp.AccessToken)
	assert.Equal(t, "refresh", resp.RefreshToken)
	assert.Equal(t, "alice", resp.User.Login)
	assert.Equal(t, "CC-MOSKVA-01", f.auth.loginIn.OrgID)
	assert.Equal(t, "alice", f.auth.loginIn.Login)
	assert.Equal(t, "secret-password", f.auth.loginIn.Password)
}

func TestLogin_BadRequest_ReturnsBindError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/login", map[string]string{
		"login": "alice",
	})

	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "auth.bad_request", body.Error)
}

func TestLogin_InvalidCredentials_Maps401(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.auth.loginErr = authapi.ErrInvalidCredentials

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/login", transporthttp.LoginRequest{
		OrgID: "CC-X", Login: "alice", Password: "pwd",
	})

	require.Equal(t, stdhttp.StatusUnauthorized, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "auth.invalid_credentials", body.Error)
}

func TestLogin_AccountLocked_Maps423(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.auth.loginErr = authapi.ErrAccountLocked

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/login", transporthttp.LoginRequest{
		OrgID: "CC-X", Login: "alice", Password: "pwd",
	})

	require.Equal(t, stdhttp.StatusLocked, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "auth.account_locked", body.Error)
}

func TestLogin_TOTPRequired_ReturnsPartial(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.auth.loginRes = authapi.AuthResult{
		AccessToken:     "partial.tok",
		AccessExpiresAt: time.Now().Add(5 * time.Minute).UTC(),
		TOTPRequired:    true,
		User:            authapi.User{ID: uuid.New(), Login: "alice"},
	}

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/login", transporthttp.LoginRequest{
		OrgID: "CC-X", Login: "alice", Password: "p",
	})

	require.Equal(t, stdhttp.StatusOK, rec.Code)
	resp := decode[transporthttp.LoginResponse](t, rec)
	assert.True(t, resp.TOTPRequired)
	assert.Equal(t, "partial.tok", resp.AccessToken)
	assert.Empty(t, resp.RefreshToken)
}

func TestLoginTOTP_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.auth.loginTOTPRes = authapi.AuthResult{AccessToken: "a", RefreshToken: "r"}

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/login/totp", transporthttp.LoginTOTPRequest{
		PartialToken: "partial.token.value",
		Code:         "123456",
	})

	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "partial.token.value", f.auth.loginTOTPIn.PartialToken)
	assert.Equal(t, "123456", f.auth.loginTOTPIn.Code)
}

func TestLoginTOTP_InvalidCode_Maps401(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.auth.loginTOTPErr = authapi.ErrTOTPInvalid

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/login/totp", transporthttp.LoginTOTPRequest{
		PartialToken: "p", Code: "000000",
	})

	require.Equal(t, stdhttp.StatusUnauthorized, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "auth.totp_invalid", body.Error)
}

func TestLoginTOTP_BadCodeFormat_400(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/login/totp", transporthttp.LoginTOTPRequest{
		PartialToken: "p", Code: "12345", // 5 digits, fails len=6
	})

	assert.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestRefresh_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.auth.refreshRes = authapi.AuthResult{AccessToken: "a2", RefreshToken: "r2"}

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/refresh", transporthttp.RefreshRequest{
		RefreshToken: "old-refresh",
	})

	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.RefreshResponse](t, rec)
	assert.Equal(t, "a2", resp.AccessToken)
	assert.Equal(t, "r2", resp.RefreshToken)
	assert.Equal(t, "old-refresh", f.auth.refreshTok)
}

func TestRefresh_Replay_Maps401(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.auth.refreshErr = authapi.ErrRefreshReplay

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/refresh", transporthttp.RefreshRequest{
		RefreshToken: "replay",
	})

	require.Equal(t, stdhttp.StatusUnauthorized, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "auth.refresh_replay", body.Error)
}

func TestLogout_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/logout", transporthttp.LogoutRequest{
		RefreshToken: "to-revoke",
	})

	require.Equal(t, stdhttp.StatusNoContent, rec.Code)
	assert.Equal(t, "to-revoke", f.auth.logoutTok)
}

// =============================================================================
// Authenticated /me endpoints
// =============================================================================

func TestMe_NoAuth_Returns401(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.err = authapi.ErrTokenInvalid

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/auth/me", nil)

	assert.Equal(t, stdhttp.StatusUnauthorized, rec.Code)
}

func TestMe_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	uid := uuid.New()
	tid := uuid.New()
	f.validator.claims = authapi.Claims{
		UserID:   uid,
		TenantID: tid,
		Login:    "alice",
		Roles:    []authapi.Role{authapi.RoleOperator},
	}
	f.users.getRet = authapi.User{
		ID:       uid,
		TenantID: tid,
		Login:    "alice",
		FullName: "Alice Wonderland",
		Email:    "alice@example.com",
		Roles:    []authapi.Role{authapi.RoleOperator},
	}

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/auth/me", nil)

	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	dto := decode[transporthttp.UserDTO](t, rec)
	assert.Equal(t, uid.String(), dto.ID)
	assert.Equal(t, "alice", dto.Login)
	assert.Equal(t, []string{"operator"}, dto.Roles)
}

func TestChangePassword_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	uid := uuid.New()
	f.validator.claims = authapi.Claims{UserID: uid, TenantID: uuid.New()}

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/me/password", transporthttp.ChangePasswordRequest{
		OldPassword: "old-pwd",
		NewPassword: "new-secure-pwd",
	})

	require.Equal(t, stdhttp.StatusNoContent, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, uid, f.users.changePwdID)
	assert.Equal(t, "old-pwd", f.users.changePwdOld)
	assert.Equal(t, "new-secure-pwd", f.users.changePwdNew)
}

func TestChangePassword_WrongOld_Maps401(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{UserID: uuid.New(), TenantID: uuid.New()}
	f.users.changePwdErr = authapi.ErrInvalidCredentials

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/me/password", transporthttp.ChangePasswordRequest{
		OldPassword: "wrong",
		NewPassword: "new-secure-pwd",
	})

	require.Equal(t, stdhttp.StatusUnauthorized, rec.Code)
}

func TestChangePassword_TooShort_400(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{UserID: uuid.New(), TenantID: uuid.New()}

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/me/password", transporthttp.ChangePasswordRequest{
		OldPassword: "old", NewPassword: "short",
	})

	assert.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestTOTPEnroll_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	uid := uuid.New()
	f.validator.claims = authapi.Claims{UserID: uid, TenantID: uuid.New()}
	f.totp.enrollRet = authapi.TOTPEnrollment{
		Secret:      "JBSWY3DPEHPK3PXP",
		OTPAuthURL:  "otpauth://totp/SocioPulse:alice?secret=JBSWY3DPEHPK3PXP",
		BackupCodes: []string{"1111", "2222"},
	}

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/me/totp/enroll", nil)

	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.TOTPEnrollResponse](t, rec)
	assert.Equal(t, "JBSWY3DPEHPK3PXP", resp.Secret)
	assert.Len(t, resp.BackupCodes, 2)
	assert.Equal(t, uid, f.totp.enrollID)
}

func TestTOTPEnroll_AlreadyEnabled_Maps409(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{UserID: uuid.New(), TenantID: uuid.New()}
	f.totp.enrollErr = authapi.ErrTOTPAlreadyEnabled

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/me/totp/enroll", nil)

	assert.Equal(t, stdhttp.StatusConflict, rec.Code)
}

func TestTOTPConfirm_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	uid := uuid.New()
	f.validator.claims = authapi.Claims{UserID: uid, TenantID: uuid.New()}

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/me/totp/confirm", transporthttp.TOTPConfirmRequest{
		Code: "123456",
	})

	require.Equal(t, stdhttp.StatusNoContent, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, uid, f.totp.confirmID)
	assert.Equal(t, "123456", f.totp.confirmCode)
}

func TestTOTPDisable_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	uid := uuid.New()
	f.validator.claims = authapi.Claims{UserID: uid, TenantID: uuid.New()}

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/me/totp/disable", nil)

	require.Equal(t, stdhttp.StatusNoContent, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, uid, f.totp.disableID)
}

func TestTOTPStatus_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	uid := uuid.New()
	f.validator.claims = authapi.Claims{UserID: uid, TenantID: uuid.New()}
	now := time.Now().UTC()
	f.totp.statusRet = authapi.TOTPStatus{
		Enabled:         true,
		EnrolledAt:      &now,
		BackupRemaining: 5,
	}

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/auth/me/totp/status", nil)

	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.TOTPStatusResponse](t, rec)
	assert.True(t, resp.Enabled)
	assert.Equal(t, 5, resp.BackupRemaining)
}

// =============================================================================
// Admin /users endpoints
// =============================================================================

func TestCreateUser_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tid := uuid.New()
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: tid,
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}
	createdID := uuid.New()
	f.users.createRetUser = authapi.User{
		ID: createdID, TenantID: tid, Login: "newbie",
		Roles: []authapi.Role{authapi.RoleOperator},
	}
	f.users.createRetTempPwd = "TempP@ss123"

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/users", transporthttp.CreateUserRequest{
		Login:    "newbie",
		FullName: "New User",
		Email:    "new@example.com",
		Roles:    []string{"operator"},
	})

	require.Equal(t, stdhttp.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.CreateUserResponse](t, rec)
	assert.Equal(t, createdID.String(), resp.User.ID)
	assert.Equal(t, "TempP@ss123", resp.TempPassword)
	assert.Equal(t, tid, f.users.createIn.TenantID)
}

func TestCreateUser_NotAdmin_Returns403(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleOperator},
	}
	f.rbac.denyAll = true

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/users", transporthttp.CreateUserRequest{
		Login: "x", FullName: "x", Roles: []string{"operator"},
	})

	assert.Equal(t, stdhttp.StatusForbidden, rec.Code)
}

func TestCreateUser_LoginTaken_Maps409(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}
	f.users.createErr = authapi.ErrLoginTaken

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/users", transporthttp.CreateUserRequest{
		Login: "dup", FullName: "x", Roles: []string{"operator"},
	})

	require.Equal(t, stdhttp.StatusConflict, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "user.login_taken", body.Error)
}

func TestCreateUser_BadRoles_400(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/users", transporthttp.CreateUserRequest{
		Login: "x", FullName: "x", Roles: []string{"superhacker"},
	})

	assert.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestListUsers_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tid := uuid.New()
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: tid,
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}
	f.users.listRet = []authapi.User{{ID: uuid.New(), TenantID: tid, Login: "u1"}}
	f.users.listTotal = 1

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/auth/users?limit=20&offset=10", nil)

	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.ListUsersResponse](t, rec)
	assert.Len(t, resp.Users, 1)
	assert.Equal(t, int64(1), resp.Total)
	assert.Equal(t, int32(20), f.users.listIn.Limit)
	assert.Equal(t, int32(10), f.users.listIn.Offset)
}

func TestListUsers_DefaultLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/auth/users", nil)

	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, int32(50), f.users.listIn.Limit)
	assert.Equal(t, int32(0), f.users.listIn.Offset)
	assert.False(t, f.users.listIn.IncludeArchived)
}

func TestListUsers_IncludeArchivedTrue(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/auth/users?include_archived=true", nil)

	require.Equal(t, stdhttp.StatusOK, rec.Code)
	assert.True(t, f.users.listIn.IncludeArchived)
}

func TestGetUser_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}
	target := uuid.New()
	f.users.getRet = authapi.User{ID: target, TenantID: uuid.New(), Login: "found"}

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/auth/users/"+target.String(), nil)

	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	dto := decode[transporthttp.UserDTO](t, rec)
	assert.Equal(t, target.String(), dto.ID)
}

func TestGetUser_NotFound_404(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}
	f.users.getErr = authapi.ErrUserNotFound

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/auth/users/"+uuid.New().String(), nil)

	require.Equal(t, stdhttp.StatusNotFound, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "user.not_found", body.Error)
}

func TestGetUser_BadID_400(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/auth/users/not-a-uuid", nil)

	assert.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestUpdateRoles_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}
	target := uuid.New()
	f.users.updateRoleRetUser = authapi.User{
		ID: target, TenantID: uuid.New(), Login: "x",
		Roles: []authapi.Role{authapi.RoleSupervisor},
	}

	rec := f.doAuth(t, stdhttp.MethodPatch, fmt.Sprintf("/api/auth/users/%s/roles", target), transporthttp.UpdateRoleRequest{
		Roles: []string{"supervisor"},
	})

	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, target, f.users.updateRoleID)
	assert.Equal(t, []authapi.Role{authapi.RoleSupervisor}, f.users.updateRoleRoles)
}

func TestArchiveUser_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}
	target := uuid.New()

	rec := f.doAuth(t, stdhttp.MethodPost, fmt.Sprintf("/api/auth/users/%s/archive", target), nil)

	require.Equal(t, stdhttp.StatusNoContent, rec.Code)
	assert.Equal(t, target, f.users.archiveID)
}

func TestRestoreUser_NotArchived_Maps409(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}
	target := uuid.New()
	f.users.restoreErr = authapi.ErrUserNotArchived

	rec := f.doAuth(t, stdhttp.MethodPost, fmt.Sprintf("/api/auth/users/%s/restore", target), nil)

	assert.Equal(t, stdhttp.StatusConflict, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "user.not_archived", body.Error)
}

func TestResetPassword_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}
	target := uuid.New()
	f.users.resetTempPwd = "FreshT3mp"

	rec := f.doAuth(t, stdhttp.MethodPost, fmt.Sprintf("/api/auth/users/%s/reset_password", target), nil)

	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.ResetPasswordResponse](t, rec)
	assert.Equal(t, "FreshT3mp", resp.TempPassword)
	assert.Equal(t, target, f.users.resetID)
}

// =============================================================================
// Internal error mapping
// =============================================================================

func TestInternalError_ScrubsMessage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.auth.loginErr = errors.New("redis: connection refused inside the network")

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/login", transporthttp.LoginRequest{
		OrgID: "x", Login: "x", Password: "x",
	})

	require.Equal(t, stdhttp.StatusInternalServerError, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "auth.internal", body.Error)
	assert.NotContains(t, body.Message, "redis")
	assert.NotContains(t, body.Message, "connection refused")
}

// =============================================================================
// Mount validation
// =============================================================================

func TestMount_NilDeps_Panics(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	assert.Panics(t, func() {
		transporthttp.Mount(api, transporthttp.Deps{})
	})
}

// =============================================================================
// Bind-error paths for auth-required endpoints
// =============================================================================

func TestRefresh_BadJSON_400(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/refresh", map[string]string{})

	assert.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestLogout_BadJSON_400(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/logout", map[string]string{})

	assert.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestRefresh_InternalError_500(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.auth.refreshErr = errors.New("redis: connection refused")

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/refresh", transporthttp.RefreshRequest{
		RefreshToken: "x",
	})

	require.Equal(t, stdhttp.StatusInternalServerError, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "auth.internal", body.Error)
	assert.Equal(t, "internal error", body.Message)
}

func TestLogout_InternalError_500(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.auth.logoutErr = errors.New("redis: down")

	rec := f.do(t, stdhttp.MethodPost, "/api/auth/logout", transporthttp.LogoutRequest{
		RefreshToken: "x",
	})

	require.Equal(t, stdhttp.StatusInternalServerError, rec.Code)
}

func TestMe_StoreError_500(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{UserID: uuid.New(), TenantID: uuid.New()}
	f.users.getErr = errors.New("postgres: connection refused")

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/auth/me", nil)

	assert.Equal(t, stdhttp.StatusInternalServerError, rec.Code)
}

func TestChangePassword_BadJSON_400(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{UserID: uuid.New(), TenantID: uuid.New()}

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/me/password", map[string]string{})

	assert.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestTOTPConfirm_BadJSON_400(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{UserID: uuid.New(), TenantID: uuid.New()}

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/me/totp/confirm", map[string]string{})

	assert.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestTOTPDisable_StoreError_500(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{UserID: uuid.New(), TenantID: uuid.New()}
	f.totp.disableErr = errors.New("internal")

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/me/totp/disable", nil)

	assert.Equal(t, stdhttp.StatusInternalServerError, rec.Code)
}

func TestTOTPStatus_StoreError_500(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{UserID: uuid.New(), TenantID: uuid.New()}
	f.totp.statusErr = errors.New("internal")

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/auth/me/totp/status", nil)

	assert.Equal(t, stdhttp.StatusInternalServerError, rec.Code)
}

func TestUpdateRoles_BadID_400(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}

	rec := f.doAuth(t, stdhttp.MethodPatch, "/api/auth/users/garbage/roles", transporthttp.UpdateRoleRequest{
		Roles: []string{"operator"},
	})

	assert.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestUpdateRoles_BadJSON_400(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}
	target := uuid.New()

	rec := f.doAuth(t, stdhttp.MethodPatch, "/api/auth/users/"+target.String()+"/roles", transporthttp.UpdateRoleRequest{
		Roles: []string{},
	})

	assert.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestArchiveUser_BadID_400(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/users/garbage/archive", nil)

	assert.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestRestoreUser_BadID_400(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/users/garbage/restore", nil)

	assert.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestResetPassword_BadID_400(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/auth/users/garbage/reset_password", nil)

	assert.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestListUsers_NegativeLimitFallsBackToDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/auth/users?limit=-5&offset=garbage", nil)

	require.Equal(t, stdhttp.StatusOK, rec.Code)
	assert.Equal(t, int32(50), f.users.listIn.Limit)
	assert.Equal(t, int32(0), f.users.listIn.Offset)
}

func TestListUsers_StoreError_500(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.claims = authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}
	f.users.listErr = errors.New("postgres: down")

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/auth/users", nil)

	assert.Equal(t, stdhttp.StatusInternalServerError, rec.Code)
}

// =============================================================================
// mapAuthError direct coverage — exercise every branch the fakes can reach.
// =============================================================================

func TestMapAuthError_AdditionalSentinels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		code int
		body string
	}{
		{"archived", authapi.ErrAccountArchived, stdhttp.StatusForbidden, "auth.account_archived"},
		{"pwd_expired", authapi.ErrPasswordExpired, stdhttp.StatusForbidden, "auth.password_expired"},
		{"totp_required", authapi.ErrTOTPRequired, stdhttp.StatusUnauthorized, "auth.totp_required"},
		{"totp_not_enrolled", authapi.ErrTOTPNotEnrolled, stdhttp.StatusBadRequest, "auth.totp_not_enrolled"},
		{"token_invalid", authapi.ErrTokenInvalid, stdhttp.StatusUnauthorized, "auth.token_invalid"},
		{"token_revoked", authapi.ErrTokenRevoked, stdhttp.StatusUnauthorized, "auth.token_revoked"},
		{"rate_limited", authapi.ErrRateLimitExceeded, stdhttp.StatusTooManyRequests, "auth.rate_limit_exceeded"},
		{"insufficient_role", authapi.ErrInsufficientRole, stdhttp.StatusForbidden, "auth.insufficient_role"},
		{"empty_roles", authapi.ErrEmptyRoles, stdhttp.StatusBadRequest, "auth.empty_roles"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.auth.loginErr = tc.err

			rec := f.do(t, stdhttp.MethodPost, "/api/auth/login", transporthttp.LoginRequest{
				OrgID: "x", Login: "x", Password: "x",
			})

			require.Equal(t, tc.code, rec.Code, "body=%s", rec.Body.String())
			body := decode[transporthttp.ErrorEnvelope](t, rec)
			assert.Equal(t, tc.body, body.Error)
		})
	}
}
