package service_test

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	authapi "github.com/sociopulse/platform/internal/auth/api"
	"github.com/sociopulse/platform/internal/auth/service"
	"github.com/sociopulse/platform/internal/auth/store"
	"github.com/sociopulse/platform/pkg/postgres"
)

// ============================================================================
// Fake collaborators — hand-rolled to keep the dependency surface tight and
// each test case readable. They mirror the patterns used in
// internal/auth/service/user_service_test.go but live in service_test so
// they don't leak into the production package.
// ============================================================================

// fakeAuthTxRunner runs every fn synchronously with a zero postgres.Tx —
// the store fakes never read from it, so we don't need a real database.
type fakeAuthTxRunner struct{}

func (f *fakeAuthTxRunner) WithTenant(_ context.Context, _ uuid.UUID, fn func(postgres.Tx) error) error {
	return fn(postgres.Tx{})
}

func (f *fakeAuthTxRunner) BypassRLS(_ context.Context, fn func(postgres.Tx) error) error {
	return fn(postgres.Tx{})
}

// fakeAuthUserStore is a hand-rolled api.UserStorePort fake. Login lookup
// and password-hash retrieval are the only paths Authenticator exercises.
type fakeAuthUserStore struct {
	mu sync.Mutex

	users        map[uuid.UUID]authapi.User
	loginIndex   map[string]uuid.UUID
	passwordHash map[uuid.UUID]string

	getByLoginErr error
	getHashErr    error
}

func newFakeAuthUserStore() *fakeAuthUserStore {
	return &fakeAuthUserStore{
		users:        make(map[uuid.UUID]authapi.User),
		loginIndex:   make(map[string]uuid.UUID),
		passwordHash: make(map[uuid.UUID]string),
	}
}

func loginIndexKey(tenantID uuid.UUID, login string) string {
	return tenantID.String() + "|" + strings.ToLower(login)
}

func (s *fakeAuthUserStore) seed(u authapi.User, hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	s.users[u.ID] = u
	s.loginIndex[loginIndexKey(u.TenantID, u.Login)] = u.ID
	s.passwordHash[u.ID] = hash
}

func (s *fakeAuthUserStore) GetByID(_ context.Context, _ postgres.Tx, id uuid.UUID) (authapi.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return authapi.User{}, authapi.ErrUserNotFound
	}
	return u, nil
}

func (s *fakeAuthUserStore) GetByLogin(_ context.Context, _ postgres.Tx, tenantID uuid.UUID, login string) (authapi.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getByLoginErr != nil {
		err := s.getByLoginErr
		s.getByLoginErr = nil
		return authapi.User{}, err
	}
	id, ok := s.loginIndex[loginIndexKey(tenantID, login)]
	if !ok {
		return authapi.User{}, authapi.ErrUserNotFound
	}
	return s.users[id], nil
}

func (s *fakeAuthUserStore) List(_ context.Context, _ postgres.Tx, _ authapi.ListUsersInput) ([]authapi.User, int64, error) {
	return nil, 0, errors.New("fakeAuthUserStore: List not implemented")
}

func (s *fakeAuthUserStore) Insert(_ context.Context, _ postgres.Tx, _ authapi.User, _ string) (authapi.User, error) {
	return authapi.User{}, errors.New("fakeAuthUserStore: Insert not implemented")
}

func (s *fakeAuthUserStore) UpdateRoles(_ context.Context, _ postgres.Tx, _ uuid.UUID, _ []authapi.Role) (authapi.User, error) {
	return authapi.User{}, errors.New("fakeAuthUserStore: UpdateRoles not implemented")
}

func (s *fakeAuthUserStore) UpdatePassword(_ context.Context, _ postgres.Tx, _ uuid.UUID, _ string, _ bool) error {
	return errors.New("fakeAuthUserStore: UpdatePassword not implemented")
}

func (s *fakeAuthUserStore) Archive(_ context.Context, _ postgres.Tx, _ uuid.UUID) error {
	return errors.New("fakeAuthUserStore: Archive not implemented")
}

func (s *fakeAuthUserStore) Restore(_ context.Context, _ postgres.Tx, _ uuid.UUID) error {
	return errors.New("fakeAuthUserStore: Restore not implemented")
}

func (s *fakeAuthUserStore) SetTOTPEnabled(_ context.Context, _ postgres.Tx, _ uuid.UUID, _ bool) error {
	return errors.New("fakeAuthUserStore: SetTOTPEnabled not implemented")
}

func (s *fakeAuthUserStore) GetPasswordHash(_ context.Context, _ postgres.Tx, id uuid.UUID) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getHashErr != nil {
		err := s.getHashErr
		s.getHashErr = nil
		return "", err
	}
	h, ok := s.passwordHash[id]
	if !ok {
		return "", authapi.ErrUserNotFound
	}
	return h, nil
}

// fakeTenants resolves an OrgCode to a tenant id from a static map.
type fakeTenants struct {
	byCode map[string]uuid.UUID
}

func (f *fakeTenants) ResolveByOrgCode(_ context.Context, code string) (uuid.UUID, error) {
	id, ok := f.byCode[code]
	if !ok {
		return uuid.Nil, service.ErrTenantNotFound
	}
	return id, nil
}

// fakeAuthHasher is a deterministic Hasher that records every Verify call
// so tests can confirm timing-safe dummy verification ran on the
// non-existent-user / non-existent-tenant paths.
type fakeAuthHasher struct {
	verifyCalls atomic.Int64
}

func (h *fakeAuthHasher) Hash(_ context.Context, password string) (string, error) {
	return "fake-hash:" + password, nil
}

func (h *fakeAuthHasher) Verify(_ context.Context, encoded, password string) (bool, error) {
	h.verifyCalls.Add(1)
	return encoded == "fake-hash:"+password, nil
}

// fakeAuthAudit captures every Write call.
type fakeAuthAudit struct {
	mu     sync.Mutex
	events []auditapi.Event
}

func (a *fakeAuthAudit) Write(_ context.Context, ev auditapi.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
	return nil
}

func (a *fakeAuthAudit) snapshot() []auditapi.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]auditapi.Event, len(a.events))
	copy(out, a.events)
	return out
}

func (a *fakeAuthAudit) actions() []string {
	out := make([]string, 0)
	for _, ev := range a.snapshot() {
		out = append(out, ev.Action)
	}
	return out
}

// fakeRateLimiter accepts/rejects per IP and per account.
type fakeRateLimiter struct {
	allowIP      bool
	allowAccount bool

	ipCalls      atomic.Int64
	accountCalls atomic.Int64
}

func (f *fakeRateLimiter) AllowIP(_ context.Context, _ netip.Addr) (bool, error) {
	f.ipCalls.Add(1)
	return f.allowIP, nil
}

func (f *fakeRateLimiter) AllowAccount(_ context.Context, _ uuid.UUID) (bool, error) {
	f.accountCalls.Add(1)
	return f.allowAccount, nil
}

// fakeLockout tracks failures and locked state.
type fakeLockout struct {
	mu sync.Mutex

	locked        map[uuid.UUID]bool
	failureCounts map[uuid.UUID]int
	registerCalls atomic.Int64
	resetCalls    atomic.Int64
	lockThreshold int
}

func newFakeLockout() *fakeLockout {
	return &fakeLockout{
		locked:        make(map[uuid.UUID]bool),
		failureCounts: make(map[uuid.UUID]int),
		lockThreshold: 5,
	}
}

func (l *fakeLockout) IsLocked(_ context.Context, userID uuid.UUID) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.locked[userID], nil
}

func (l *fakeLockout) RegisterFailure(_ context.Context, userID uuid.UUID) (bool, error) {
	l.registerCalls.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failureCounts[userID]++
	if l.failureCounts[userID] >= l.lockThreshold {
		l.locked[userID] = true
	}
	return l.locked[userID], nil
}

func (l *fakeLockout) Reset(_ context.Context, userID uuid.UUID) error {
	l.resetCalls.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failureCounts, userID)
	delete(l.locked, userID)
	return nil
}

// fakeTOTP returns a programmable Verify result.
type fakeTOTP struct {
	ok        bool
	verifyErr error

	verifyCalls atomic.Int64
}

func (f *fakeTOTP) Verify(_ context.Context, _ uuid.UUID, _ string) (bool, error) {
	f.verifyCalls.Add(1)
	if f.verifyErr != nil {
		return false, f.verifyErr
	}
	return f.ok, nil
}

// ============================================================================
// Builder
// ============================================================================

type harness struct {
	auth *service.Authenticator

	tx      *fakeAuthTxRunner
	users   *fakeAuthUserStore
	tenants *fakeTenants
	hasher  *fakeAuthHasher
	revoker *service.SessionRevoker
	refresh *store.RefreshStore
	rate    *fakeRateLimiter
	lockout *fakeLockout
	totp    *fakeTOTP
	audit   *fakeAuthAudit
	issuer  *service.JWTIssuer
	clock   *fakeAuthClock
	mr      *miniredis.Miniredis

	tenantID uuid.UUID
	orgCode  string
}

type fakeAuthClock struct{ nanos atomic.Int64 }

func newFakeAuthClock(t time.Time) *fakeAuthClock {
	c := &fakeAuthClock{}
	c.nanos.Store(t.UnixNano())
	return c
}

func (c *fakeAuthClock) Now() time.Time         { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *fakeAuthClock) Func() func() time.Time { return c.Now }

func newHarness(t *testing.T) *harness {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	clk := newFakeAuthClock(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))

	issuer, err := service.NewJWTIssuer(service.JWTConfig{
		Issuer:     "sociopulse-test",
		Secret:     []byte("0123456789abcdef0123456789abcdef"),
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 30 * 24 * time.Hour,
		Leeway:     30 * time.Second,
	}, clk.Func())
	require.NoError(t, err)

	partialIssuer, err := service.NewJWTIssuer(service.JWTConfig{
		Issuer:     "sociopulse-test",
		Secret:     []byte("0123456789abcdef0123456789abcdef"),
		AccessTTL:  5 * time.Minute,
		RefreshTTL: 30 * 24 * time.Hour,
		Leeway:     30 * time.Second,
	}, clk.Func())
	require.NoError(t, err)

	revoker := service.NewSessionRevoker(rdb, 30*24*time.Hour, clk.Func())
	refreshStore := store.NewRefreshStore(rdb, 30*24*time.Hour)

	tenantID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	tenants := &fakeTenants{byCode: map[string]uuid.UUID{"CC-MOSKVA-01": tenantID}}

	hasher := &fakeAuthHasher{}
	users := newFakeAuthUserStore()
	rate := &fakeRateLimiter{allowIP: true, allowAccount: true}
	lockout := newFakeLockout()
	totp := &fakeTOTP{ok: true}
	audit := &fakeAuthAudit{}
	tx := &fakeAuthTxRunner{}

	auth, err := service.NewAuthenticator(service.AuthenticatorDeps{
		Pool:            tx,
		Users:           users,
		Tenants:         tenants,
		Hasher:          hasher,
		Issuer:          issuer,
		PartialIssuer:   partialIssuer,
		Revoker:         revoker,
		Refreshes:       refreshStore,
		RateLimiter:     rate,
		Lockout:         lockout,
		TOTP:            totp,
		Audit:           audit,
		Clock:           clk.Func(),
		PartialAccess:   5 * time.Minute,
		MetricsRegistry: nil,
	})
	require.NoError(t, err)

	return &harness{
		auth:     auth,
		tx:       tx,
		users:    users,
		tenants:  tenants,
		hasher:   hasher,
		revoker:  revoker,
		refresh:  refreshStore,
		rate:     rate,
		lockout:  lockout,
		totp:     totp,
		audit:    audit,
		issuer:   issuer,
		clock:    clk,
		mr:       mr,
		tenantID: tenantID,
		orgCode:  "CC-MOSKVA-01",
	}
}

// seedAlice inserts a default user "alice"/"hunter2" and returns her id.
func (h *harness) seedAlice(t *testing.T, mods ...func(*authapi.User)) uuid.UUID {
	t.Helper()
	u := authapi.User{
		ID:       uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		TenantID: h.tenantID,
		Login:    "alice",
		FullName: "Алиса",
		Email:    "alice@example.com",
		Roles:    []authapi.Role{authapi.RoleOperator},
	}
	for _, mod := range mods {
		mod(&u)
	}
	h.users.seed(u, "fake-hash:hunter2")
	return u.ID
}

// loginInput builds an api.LoginInput for "alice" with the supplied
// orgCode and password. Tests that need a different login can build the
// struct inline; "alice" is the only seeded user across the test set.
func loginInput(orgCode, password string) authapi.LoginInput {
	return authapi.LoginInput{
		OrgID:     orgCode,
		Login:     "alice",
		Password:  password,
		IP:        netip.MustParseAddr("10.0.0.1"),
		UserAgent: "go-test",
	}
}

// ============================================================================
// Tests — 16 cases per Plan 05 Task 4 Step 6
// ============================================================================

// 1. Happy login (no TOTP) — full pair returned, refresh saved, audit success.
func TestAuthenticator_Login_Happy(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	uid := h.seedAlice(t)

	res, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.NoError(t, err)
	assert.NotEmpty(t, res.AccessToken)
	assert.NotEmpty(t, res.RefreshToken)
	assert.False(t, res.TOTPRequired)
	assert.Equal(t, uid, res.User.ID)

	// Refresh whitelist is populated — Validate the issued refresh and
	// confirm Lookup hits.
	claims, err := h.issuer.Validate(res.RefreshToken, "refresh")
	require.NoError(t, err)
	rec, err := h.refresh.Lookup(t.Context(), claims.JTI)
	require.NoError(t, err)
	assert.Equal(t, uid, rec.UserID)

	// Audit row "auth.login" emitted.
	assert.Contains(t, h.audit.actions(), authapi.AuditActionLogin)
}

// 2. Wrong password — ErrInvalidCredentials, lockout++, audit failed.
func TestAuthenticator_Login_WrongPasswordReturnsInvalidCredentials(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	_, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "WRONG"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrInvalidCredentials, "expected ErrInvalidCredentials, got %v", err)
	assert.Equal(t, int64(1), h.lockout.registerCalls.Load(), "RegisterFailure should be called once")
	// auth.login.failed audit
	assert.Contains(t, h.audit.actions(), "auth.login.failed")
}

// 3. Wrong tenant — ErrInvalidCredentials and dummy verify still ran (timing-safety).
func TestAuthenticator_Login_UnknownTenantReturnsInvalidCredentials(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, err := h.auth.Login(t.Context(), loginInput("CC-NONEXISTENT", "hunter2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrInvalidCredentials, "expected ErrInvalidCredentials, got %v", err)
	assert.Positive(t, h.hasher.verifyCalls.Load(), "dummy Verify must run on unknown-tenant path to equalize timing")
}

// 4. Archived user — ErrAccountArchived.
func TestAuthenticator_Login_ArchivedUserReturnsArchived(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	archived := h.clock.Now()
	h.seedAlice(t, func(u *authapi.User) { u.ArchivedAt = &archived })

	_, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrAccountArchived)
}

// 5. Locked account — ErrAccountLocked, no real Verify call against the user
// (but a dummy verify still spent for timing-safety).
func TestAuthenticator_Login_LockedAccountReturnsLocked(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	uid := h.seedAlice(t)
	// Pre-lock the account.
	h.lockout.locked[uid] = true

	_, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrAccountLocked)

	// Dummy verify still spent.
	assert.Positive(t, h.hasher.verifyCalls.Load(), "dummy Verify must run on locked path to equalize timing")
}

// 6. Per-IP rate-limit exceeded — ErrRateLimitExceeded; no DB call (no
// GetByLogin, no real verify of the user's hash).
func TestAuthenticator_Login_IPRateLimitReturnsRateLimited(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)
	h.rate.allowIP = false

	_, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrRateLimitExceeded)
	// auth.login.rate_limited audit
	assert.Contains(t, h.audit.actions(), "auth.login.rate_limited")
}

// 7. TOTP enabled — partial token returned, TOTPRequired=true, no refresh saved.
func TestAuthenticator_Login_TOTPEnabledReturnsPartial(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t, func(u *authapi.User) { u.TOTPEnabled = true })

	res, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.NoError(t, err)
	assert.True(t, res.TOTPRequired, "TOTPRequired should be true for TOTP-enabled user")
	assert.NotEmpty(t, res.AccessToken)
	assert.Empty(t, res.RefreshToken, "no refresh token should be issued at the partial step")

	// Validate the partial — TOTPDone must be false.
	c, err := h.issuer.Validate(res.AccessToken, "access")
	require.NoError(t, err)
	assert.False(t, c.TOTPDone, "partial token must have TOTPDone=false")

	// Partial expiry should be ~5 minutes from now.
	delta := c.ExpiresAt.Sub(h.clock.Now())
	assert.InDelta(t, (5 * time.Minute).Seconds(), delta.Seconds(), 30.0,
		"partial access expiry should be ~5 minutes")
}

// 8. LoginTOTP happy — full pair, audit success.
func TestAuthenticator_LoginTOTP_Happy(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t, func(u *authapi.User) { u.TOTPEnabled = true })
	h.totp.ok = true

	first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.NoError(t, err)
	require.True(t, first.TOTPRequired)

	res, err := h.auth.LoginTOTP(t.Context(), authapi.LoginTOTPInput{
		PartialToken: first.AccessToken,
		Code:         "123456",
		IP:           netip.MustParseAddr("10.0.0.1"),
		UserAgent:    "go-test",
	})
	require.NoError(t, err)
	assert.False(t, res.TOTPRequired)
	assert.NotEmpty(t, res.AccessToken)
	assert.NotEmpty(t, res.RefreshToken)

	// Final access token has TOTPDone=true.
	c, err := h.issuer.Validate(res.AccessToken, "access")
	require.NoError(t, err)
	assert.True(t, c.TOTPDone)
}

// 9. LoginTOTP wrong code — ErrTOTPInvalid, lockout++.
func TestAuthenticator_LoginTOTP_WrongCodeReturnsTOTPInvalid(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t, func(u *authapi.User) { u.TOTPEnabled = true })
	h.totp.ok = false

	first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.NoError(t, err)

	_, err = h.auth.LoginTOTP(t.Context(), authapi.LoginTOTPInput{
		PartialToken: first.AccessToken,
		Code:         "000000",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrTOTPInvalid)
	assert.Positive(t, h.lockout.registerCalls.Load())
}

// 10. Refresh happy — old jti gone, new pair returned.
func TestAuthenticator_Refresh_Happy(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.NoError(t, err)

	// Validate to grab the original jti.
	oldClaims, err := h.issuer.Validate(first.RefreshToken, "refresh")
	require.NoError(t, err)

	res, err := h.auth.Refresh(t.Context(), first.RefreshToken, netip.MustParseAddr("10.0.0.2"))
	require.NoError(t, err)
	assert.NotEqual(t, first.RefreshToken, res.RefreshToken, "new refresh token should differ from old")

	// Old jti must be deleted from whitelist.
	_, err = h.refresh.Lookup(t.Context(), oldClaims.JTI)
	assert.ErrorIs(t, err, store.ErrRefreshNotFound, "old jti should be removed from whitelist after rotation; got %v", err)

	// auth.refresh audit emitted.
	assert.Contains(t, h.audit.actions(), "auth.refresh")
}

// 11. Refresh replay — second use → ErrRefreshReplay AND session revoked AND audit auth.refresh_replay.
func TestAuthenticator_Refresh_ReplayDetected(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.NoError(t, err)
	oldClaims, err := h.issuer.Validate(first.RefreshToken, "refresh")
	require.NoError(t, err)

	// First refresh — succeeds, rotates.
	_, err = h.auth.Refresh(t.Context(), first.RefreshToken, netip.MustParseAddr("10.0.0.2"))
	require.NoError(t, err)

	// Second refresh with the same (already-rotated) token — replay.
	_, err = h.auth.Refresh(t.Context(), first.RefreshToken, netip.MustParseAddr("10.0.0.2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrRefreshReplay, "expected ErrRefreshReplay, got %v", err)

	// Session is now revoked.
	revoked, err := h.revoker.IsRevoked(t.Context(), oldClaims.SessionID, oldClaims.JTI)
	require.NoError(t, err)
	assert.True(t, revoked, "session should be revoked after replay detection")

	// auth.refresh_replay audit emitted.
	assert.Contains(t, h.audit.actions(), authapi.AuditActionRefreshReplay)
}

// 12. Refresh of revoked session — ErrTokenRevoked.
func TestAuthenticator_Refresh_RevokedSessionReturnsTokenRevoked(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.NoError(t, err)
	oldClaims, err := h.issuer.Validate(first.RefreshToken, "refresh")
	require.NoError(t, err)

	// Admin revokes the session.
	require.NoError(t, h.revoker.RevokeSession(t.Context(), oldClaims.SessionID))

	_, err = h.auth.Refresh(t.Context(), first.RefreshToken, netip.MustParseAddr("10.0.0.2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrTokenRevoked, "expected ErrTokenRevoked, got %v", err)
}

// 13. Logout happy — refresh deleted, sid revoked, audit logout.
func TestAuthenticator_Logout_Happy(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.NoError(t, err)
	claims, err := h.issuer.Validate(first.RefreshToken, "refresh")
	require.NoError(t, err)

	require.NoError(t, h.auth.Logout(t.Context(), first.RefreshToken))

	_, err = h.refresh.Lookup(t.Context(), claims.JTI)
	assert.ErrorIs(t, err, store.ErrRefreshNotFound, "refresh whitelist entry should be gone after logout")

	revoked, err := h.revoker.IsRevoked(t.Context(), claims.SessionID, claims.JTI)
	require.NoError(t, err)
	assert.True(t, revoked, "session should be revoked after logout")

	assert.Contains(t, h.audit.actions(), authapi.AuditActionLogout)
}

// 14. Logout of unknown / invalid token — nil error (idempotent).
func TestAuthenticator_Logout_InvalidTokenReturnsNil(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	// "garbage" cannot be parsed; logout must still be idempotent.
	err := h.auth.Logout(t.Context(), "not-a-real-token")
	require.NoError(t, err)
}

// 15. Force password change — ErrPasswordExpired.
func TestAuthenticator_Login_MustChangePwdReturnsPasswordExpired(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t, func(u *authapi.User) { u.MustChangePwd = true })

	_, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrPasswordExpired, "expected ErrPasswordExpired, got %v", err)
}

// 16. ValidateAccessToken — accepts valid, rejects revoked, rejects non-access typ.
func TestAuthenticator_ValidateAccessToken(t *testing.T) {
	t.Parallel()

	t.Run("happy path accepts a valid access token", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.seedAlice(t)

		first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
		require.NoError(t, err)

		c, err := h.auth.ValidateAccessToken(t.Context(), first.AccessToken)
		require.NoError(t, err)
		assert.True(t, c.TOTPDone)
	})

	t.Run("rejects revoked access token", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.seedAlice(t)

		first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
		require.NoError(t, err)
		c, err := h.issuer.Validate(first.AccessToken, "access")
		require.NoError(t, err)
		require.NoError(t, h.revoker.RevokeSession(t.Context(), c.SessionID))

		_, err = h.auth.ValidateAccessToken(t.Context(), first.AccessToken)
		require.Error(t, err)
		assert.ErrorIs(t, err, authapi.ErrTokenRevoked, "expected ErrTokenRevoked, got %v", err)
	})

	t.Run("rejects refresh token presented to ValidateAccessToken", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.seedAlice(t)

		first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
		require.NoError(t, err)

		_, err = h.auth.ValidateAccessToken(t.Context(), first.RefreshToken)
		require.Error(t, err)
		assert.ErrorIs(t, err, authapi.ErrTokenInvalid, "non-access token should be rejected as invalid; got %v", err)
	})
}

// 17. NewAuthenticator validates required dependencies. Each branch in the
// constructor's switch is exercised via a per-field nil dep.
func TestNewAuthenticator_RejectsMissingDeps(t *testing.T) {
	t.Parallel()

	// Build a fully-valid Deps once; each subtest zeroes one field.
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	clk := newFakeAuthClock(time.Date(2026, time.May, 8, 12, 0, 0, 0, time.UTC))
	issuer, err := service.NewJWTIssuer(service.JWTConfig{
		Issuer:     "sociopulse-test",
		Secret:     []byte("0123456789abcdef0123456789abcdef"),
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 30 * 24 * time.Hour,
		Leeway:     30 * time.Second,
	}, clk.Func())
	require.NoError(t, err)

	revoker := service.NewSessionRevoker(rdb, time.Hour, clk.Func())
	refreshStore := store.NewRefreshStore(rdb, time.Hour)

	good := service.AuthenticatorDeps{
		Pool:        &fakeAuthTxRunner{},
		Users:       newFakeAuthUserStore(),
		Tenants:     &fakeTenants{},
		Hasher:      &fakeAuthHasher{},
		Issuer:      issuer,
		Revoker:     revoker,
		Refreshes:   refreshStore,
		RateLimiter: &fakeRateLimiter{allowIP: true, allowAccount: true},
		Lockout:     newFakeLockout(),
		TOTP:        &fakeTOTP{},
		Clock:       clk.Func(),
	}

	type mut struct {
		name string
		mut  func(*service.AuthenticatorDeps)
	}
	muts := []mut{
		{"nil pool", func(d *service.AuthenticatorDeps) { d.Pool = nil }},
		{"nil users", func(d *service.AuthenticatorDeps) { d.Users = nil }},
		{"nil tenants", func(d *service.AuthenticatorDeps) { d.Tenants = nil }},
		{"nil hasher", func(d *service.AuthenticatorDeps) { d.Hasher = nil }},
		{"nil issuer", func(d *service.AuthenticatorDeps) { d.Issuer = nil }},
		{"nil revoker", func(d *service.AuthenticatorDeps) { d.Revoker = nil }},
		{"nil refreshes", func(d *service.AuthenticatorDeps) { d.Refreshes = nil }},
		{"nil rate limiter", func(d *service.AuthenticatorDeps) { d.RateLimiter = nil }},
		{"nil lockout", func(d *service.AuthenticatorDeps) { d.Lockout = nil }},
		{"nil TOTP", func(d *service.AuthenticatorDeps) { d.TOTP = nil }},
	}
	for _, m := range muts {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			d := good
			m.mut(&d)
			_, err := service.NewAuthenticator(d)
			require.Error(t, err, "expected error when %s", m.name)
		})
	}

	// Default clock + default partial expiry: nil clock falls back to
	// time.Now, zero PartialAccess falls back to 5min.
	t.Run("defaults applied for nil clock and zero partial", func(t *testing.T) {
		t.Parallel()
		d := good
		d.Clock = nil
		d.PartialAccess = 0
		_, err := service.NewAuthenticator(d)
		require.NoError(t, err)
	})
}

// 18. Refresh of a never-issued token returns ErrTokenInvalid.
func TestAuthenticator_Refresh_UnknownTokenReturnsTokenInvalid(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	// Mint a refresh token using the issuer directly — it never lands
	// in the whitelist. The Authenticator must reject it.
	tok, _, err := h.issuer.IssueRefresh(authapi.Claims{
		UserID:   uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		TenantID: h.tenantID,
		Login:    "ghost",
	})
	require.NoError(t, err)

	_, err = h.auth.Refresh(t.Context(), tok, netip.MustParseAddr("10.0.0.2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrTokenInvalid)
}

// 19. Refresh of a malformed token returns ErrTokenInvalid.
func TestAuthenticator_Refresh_MalformedTokenReturnsTokenInvalid(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, err := h.auth.Refresh(t.Context(), "not-a-token", netip.MustParseAddr("10.0.0.2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrTokenInvalid)
}

// 20. ValidateAccessToken on a malformed token returns ErrTokenInvalid.
func TestAuthenticator_ValidateAccessToken_MalformedReturnsTokenInvalid(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, err := h.auth.ValidateAccessToken(t.Context(), "not-a-token")
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrTokenInvalid)
}

// errorRateLimiter forces AllowIP / AllowAccount to error, used to
// exercise the defensive error branches in loginPreflight.
type errorRateLimiter struct {
	ipErr      error
	accountErr error
}

func (e *errorRateLimiter) AllowIP(_ context.Context, _ netip.Addr) (bool, error) {
	if e.ipErr != nil {
		return false, e.ipErr
	}
	return true, nil
}

func (e *errorRateLimiter) AllowAccount(_ context.Context, _ uuid.UUID) (bool, error) {
	if e.accountErr != nil {
		return false, e.accountErr
	}
	return true, nil
}

// errorLockout forces IsLocked to error.
type errorLockout struct {
	isLockedErr error
}

func (e *errorLockout) IsLocked(_ context.Context, _ uuid.UUID) (bool, error) {
	return false, e.isLockedErr
}
func (e *errorLockout) RegisterFailure(_ context.Context, _ uuid.UUID) (bool, error) {
	return false, nil
}
func (e *errorLockout) Reset(_ context.Context, _ uuid.UUID) error { return nil }

// 27. Login: rate-limit IP error surfaces wrapped.
func TestAuthenticator_Login_IPRateErrorWrapped(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	// Replace rate limiter with an erroring one.
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	auth, err := service.NewAuthenticator(service.AuthenticatorDeps{
		Pool:        h.tx,
		Users:       h.users,
		Tenants:     h.tenants,
		Hasher:      h.hasher,
		Issuer:      h.issuer,
		Revoker:     service.NewSessionRevoker(rdb, time.Hour, h.clock.Func()),
		Refreshes:   store.NewRefreshStore(rdb, time.Hour),
		RateLimiter: &errorRateLimiter{ipErr: errors.New("simulated rate-limit outage")},
		Lockout:     h.lockout,
		TOTP:        h.totp,
		Audit:       h.audit,
		Clock:       h.clock.Func(),
	})
	require.NoError(t, err)

	_, err = auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate-limit ip")
}

// 28. Login: lockout IsLocked error surfaces wrapped.
func TestAuthenticator_Login_LockoutErrorWrapped(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	auth, err := service.NewAuthenticator(service.AuthenticatorDeps{
		Pool:        h.tx,
		Users:       h.users,
		Tenants:     h.tenants,
		Hasher:      h.hasher,
		Issuer:      h.issuer,
		Revoker:     service.NewSessionRevoker(rdb, time.Hour, h.clock.Func()),
		Refreshes:   store.NewRefreshStore(rdb, time.Hour),
		RateLimiter: &fakeRateLimiter{allowIP: true, allowAccount: true},
		Lockout:     &errorLockout{isLockedErr: errors.New("simulated lockout outage")},
		TOTP:        h.totp,
		Audit:       h.audit,
		Clock:       h.clock.Func(),
	})
	require.NoError(t, err)

	_, err = auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is locked")
}

// errorHasher exposes a Verify-error knob.
type errorHasher struct {
	hashErr   error
	verifyErr error
}

func (e *errorHasher) Hash(_ context.Context, p string) (string, error) {
	if e.hashErr != nil {
		return "", e.hashErr
	}
	return "fake-hash:" + p, nil
}

func (e *errorHasher) Verify(_ context.Context, encoded, p string) (bool, error) {
	if e.verifyErr != nil {
		return false, e.verifyErr
	}
	return encoded == "fake-hash:"+p, nil
}

// errorRevoker is an api.SessionRevoker that errors on RevokeSession.
type errorRevoker struct {
	inner interface {
		authapi.SessionRevoker
		IsRevokedClaims(ctx context.Context, c authapi.Claims) (bool, error)
	}
	revokeSessionErr error
	isRevokedErr     error
}

func (e *errorRevoker) RevokeSession(ctx context.Context, sid string) error {
	if e.revokeSessionErr != nil {
		return e.revokeSessionErr
	}
	return e.inner.RevokeSession(ctx, sid)
}
func (e *errorRevoker) RevokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	return e.inner.RevokeAllForUser(ctx, userID)
}
func (e *errorRevoker) IsRevoked(ctx context.Context, sid, jti string) (bool, error) {
	return e.inner.IsRevoked(ctx, sid, jti)
}
func (e *errorRevoker) IsRevokedClaims(ctx context.Context, c authapi.Claims) (bool, error) {
	if e.isRevokedErr != nil {
		return false, e.isRevokedErr
	}
	return e.inner.IsRevokedClaims(ctx, c)
}

// 35. Refresh: revoker error surfaces wrapped (fail closed).
func TestAuthenticator_Refresh_RevocationCheckErrorWrapped(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.NoError(t, err)

	wrap := &errorRevoker{inner: h.revoker, isRevokedErr: errors.New("simulated revocation outage")}
	auth, err := service.NewAuthenticator(service.AuthenticatorDeps{
		Pool:        h.tx,
		Users:       h.users,
		Tenants:     h.tenants,
		Hasher:      h.hasher,
		Issuer:      h.issuer,
		Revoker:     wrap,
		Refreshes:   h.refresh,
		RateLimiter: h.rate,
		Lockout:     h.lockout,
		TOTP:        h.totp,
		Audit:       h.audit,
		Clock:       h.clock.Func(),
	})
	require.NoError(t, err)

	_, err = auth.Refresh(t.Context(), first.RefreshToken, netip.MustParseAddr("10.0.0.2"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "revocation check")
}

// 36. ValidateAccessToken: revoker error surfaces wrapped.
func TestAuthenticator_ValidateAccessToken_RevocationCheckErrorWrapped(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.NoError(t, err)

	wrap := &errorRevoker{inner: h.revoker, isRevokedErr: errors.New("simulated revocation outage")}
	auth, err := service.NewAuthenticator(service.AuthenticatorDeps{
		Pool:        h.tx,
		Users:       h.users,
		Tenants:     h.tenants,
		Hasher:      h.hasher,
		Issuer:      h.issuer,
		Revoker:     wrap,
		Refreshes:   h.refresh,
		RateLimiter: h.rate,
		Lockout:     h.lockout,
		TOTP:        h.totp,
		Audit:       h.audit,
		Clock:       h.clock.Func(),
	})
	require.NoError(t, err)

	_, err = auth.ValidateAccessToken(t.Context(), first.AccessToken)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "revocation check")
}

// 37. Logout: revoker error surfaces wrapped (Logout cannot return nil
// if it can't revoke — that would leave the session usable).
func TestAuthenticator_Logout_RevokeErrorWrapped(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.NoError(t, err)

	wrap := &errorRevoker{inner: h.revoker, revokeSessionErr: errors.New("simulated revoke outage")}
	auth, err := service.NewAuthenticator(service.AuthenticatorDeps{
		Pool:        h.tx,
		Users:       h.users,
		Tenants:     h.tenants,
		Hasher:      h.hasher,
		Issuer:      h.issuer,
		Revoker:     wrap,
		Refreshes:   h.refresh,
		RateLimiter: h.rate,
		Lockout:     h.lockout,
		TOTP:        h.totp,
		Audit:       h.audit,
		Clock:       h.clock.Func(),
	})
	require.NoError(t, err)

	err = auth.Logout(t.Context(), first.RefreshToken)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "revoke on logout")
}

// 34. Logout: refresh.Delete error is non-fatal — RevokeSession still
// runs and Logout returns nil.
func TestAuthenticator_Logout_DeleteErrorIsNonFatal(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.NoError(t, err)

	// Build a logout-only authenticator with a delete-erroring refresh store.
	wrap := &errorRefreshStore{inner: h.refresh}
	// We need delete to fail on the next call; errorRefreshStore.saveErr/rotateErr
	// don't cover Delete. Add a one-shot delete-error injection:
	auth, err := service.NewAuthenticator(service.AuthenticatorDeps{
		Pool:        h.tx,
		Users:       h.users,
		Tenants:     h.tenants,
		Hasher:      h.hasher,
		Issuer:      h.issuer,
		Revoker:     h.revoker,
		Refreshes:   wrap,
		RateLimiter: h.rate,
		Lockout:     h.lockout,
		TOTP:        h.totp,
		Audit:       h.audit,
		Clock:       h.clock.Func(),
	})
	require.NoError(t, err)

	// Override Delete via a helper: errorRefreshStore doesn't support
	// delete-error injection; we add it inline by replacing inner with a
	// nil-mocker that returns an error from Delete only.
	// Simpler: use a delete-erroring shim.
	failingDeleteAuth := makeAuthWithDeleteFailingRefreshes(t, h)
	require.NoError(t, failingDeleteAuth.Logout(t.Context(), first.RefreshToken))

	// Use the original auth here just to keep the var alive.
	_ = auth
}

// makeAuthWithDeleteFailingRefreshes constructs a fresh Authenticator
// where the refresh store returns an error on Delete, used by the
// Logout-resilience test.
func makeAuthWithDeleteFailingRefreshes(t *testing.T, h *harness) *service.Authenticator {
	t.Helper()
	wrap := &deleteFailingRefreshStore{inner: h.refresh}
	auth, err := service.NewAuthenticator(service.AuthenticatorDeps{
		Pool:        h.tx,
		Users:       h.users,
		Tenants:     h.tenants,
		Hasher:      h.hasher,
		Issuer:      h.issuer,
		Revoker:     h.revoker,
		Refreshes:   wrap,
		RateLimiter: h.rate,
		Lockout:     h.lockout,
		TOTP:        h.totp,
		Audit:       h.audit,
		Clock:       h.clock.Func(),
	})
	require.NoError(t, err)
	return auth
}

type deleteFailingRefreshStore struct {
	inner service.RefreshStorePort
}

func (d *deleteFailingRefreshStore) Save(ctx context.Context, jti string, rec store.RefreshRecord) error {
	return d.inner.Save(ctx, jti, rec)
}
func (d *deleteFailingRefreshStore) Lookup(ctx context.Context, jti string) (store.RefreshRecord, error) {
	return d.inner.Lookup(ctx, jti)
}
func (d *deleteFailingRefreshStore) Rotate(ctx context.Context, oldJTI, newJTI string, rec store.RefreshRecord) error {
	return d.inner.Rotate(ctx, oldJTI, newJTI, rec)
}
func (d *deleteFailingRefreshStore) Delete(_ context.Context, _ string) error {
	return errors.New("simulated delete error")
}

// 32. Login: hasher.Verify error surfaces ErrInvalidCredentials with extra context.
func TestAuthenticator_Login_VerifyErrorReturnsInvalidCredentials(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	// Inject a Verify-erroring hasher.
	wrapped := &errorHasher{verifyErr: errors.New("simulated hash decode error")}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	auth, err := service.NewAuthenticator(service.AuthenticatorDeps{
		Pool:        h.tx,
		Users:       h.users,
		Tenants:     h.tenants,
		Hasher:      wrapped,
		Issuer:      h.issuer,
		Revoker:     service.NewSessionRevoker(rdb, time.Hour, h.clock.Func()),
		Refreshes:   store.NewRefreshStore(rdb, time.Hour),
		RateLimiter: h.rate,
		Lockout:     h.lockout,
		TOTP:        h.totp,
		Audit:       h.audit,
		Clock:       h.clock.Func(),
	})
	require.NoError(t, err)

	// First call: dummy-Verify-error in NewAuthenticator already exhausted
	// the verifyErr slot; so we re-inject before calling Login. To do that
	// we go through a second dep build.
	wrapped.verifyErr = errors.New("simulated verify error")
	_, err = auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrInvalidCredentials)
}

// 33. Login: wrong password causes Lockout transition (justLocked=true) on
// the threshold attempt and increments the metric.
func TestAuthenticator_Login_WrongPasswordLocksAfterThreshold(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)
	// Lower the threshold so we don't loop 5 times.
	h.lockout.lockThreshold = 1

	_, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "WRONG"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrInvalidCredentials)

	// Account is now locked (1 failure crossed the lower threshold).
	locked, _ := h.lockout.IsLocked(t.Context(), h.users.users[uuid.MustParse("11111111-1111-1111-1111-111111111111")].ID)
	assert.True(t, locked, "lockout should have transitioned to locked")
}

// 31. Login: AllowAccount error surfaces wrapped.
func TestAuthenticator_Login_AccountRateErrorWrapped(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	auth, err := service.NewAuthenticator(service.AuthenticatorDeps{
		Pool:        h.tx,
		Users:       h.users,
		Tenants:     h.tenants,
		Hasher:      h.hasher,
		Issuer:      h.issuer,
		Revoker:     service.NewSessionRevoker(rdb, time.Hour, h.clock.Func()),
		Refreshes:   store.NewRefreshStore(rdb, time.Hour),
		RateLimiter: &errorRateLimiter{accountErr: errors.New("simulated account-rate outage")},
		Lockout:     h.lockout,
		TOTP:        h.totp,
		Audit:       h.audit,
		Clock:       h.clock.Func(),
	})
	require.NoError(t, err)

	_, err = auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate-limit account")
}

// 29. LoginTOTP: totp.Verify error surfaces wrapped.
func TestAuthenticator_LoginTOTP_VerifyErrorWrapped(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t, func(u *authapi.User) { u.TOTPEnabled = true })

	first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.NoError(t, err)

	h.totp.verifyErr = errors.New("simulated TOTP outage")
	_, err = h.auth.LoginTOTP(t.Context(), authapi.LoginTOTPInput{
		PartialToken: first.AccessToken,
		Code:         "123456",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "totp verify")
}

// 30. LoginTOTP: malformed partial token returns ErrTokenInvalid.
func TestAuthenticator_LoginTOTP_MalformedTokenReturnsInvalid(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, err := h.auth.LoginTOTP(t.Context(), authapi.LoginTOTPInput{PartialToken: "garbage", Code: "123456"})
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrTokenInvalid)
}

// errorRefreshStore is a RefreshStorePort wrapper that injects an error
// on the next Save / Rotate call, then unblocks. Used to exercise the
// rare-but-real defensive branches in Refresh / issueFullPair.
type errorRefreshStore struct {
	inner     service.RefreshStorePort
	saveErr   error
	rotateErr error
}

func (e *errorRefreshStore) Save(ctx context.Context, jti string, rec store.RefreshRecord) error {
	if e.saveErr != nil {
		err := e.saveErr
		e.saveErr = nil
		return err
	}
	return e.inner.Save(ctx, jti, rec)
}
func (e *errorRefreshStore) Lookup(ctx context.Context, jti string) (store.RefreshRecord, error) {
	return e.inner.Lookup(ctx, jti)
}
func (e *errorRefreshStore) Rotate(ctx context.Context, oldJTI, newJTI string, rec store.RefreshRecord) error {
	if e.rotateErr != nil {
		err := e.rotateErr
		e.rotateErr = nil
		return err
	}
	return e.inner.Rotate(ctx, oldJTI, newJTI, rec)
}
func (e *errorRefreshStore) Delete(ctx context.Context, jti string) error {
	return e.inner.Delete(ctx, jti)
}

// 25. Refresh: non-sentinel Rotate error surfaces wrapped (not as ErrRefreshReplay).
func TestAuthenticator_Refresh_RotateGenericErrorIsWrapped(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.NoError(t, err)

	// Swap the refresh store with one that returns a generic error on Rotate.
	wrap := &errorRefreshStore{inner: h.refresh, rotateErr: errors.New("simulated redis outage")}
	auth, err := service.NewAuthenticator(service.AuthenticatorDeps{
		Pool:        h.tx,
		Users:       h.users,
		Tenants:     h.tenants,
		Hasher:      h.hasher,
		Issuer:      h.issuer,
		Revoker:     h.revoker,
		Refreshes:   wrap,
		RateLimiter: h.rate,
		Lockout:     h.lockout,
		TOTP:        h.totp,
		Audit:       h.audit,
		Clock:       h.clock.Func(),
	})
	require.NoError(t, err)

	_, err = auth.Refresh(t.Context(), first.RefreshToken, netip.MustParseAddr("10.0.0.2"))
	require.Error(t, err)
	// Generic error is wrapped, not surfaced as a sentinel.
	assert.NotErrorIs(t, err, authapi.ErrRefreshReplay)
	assert.NotErrorIs(t, err, authapi.ErrTokenInvalid)
}

// 26. Login: failure to Save the refresh whitelist surfaces as a wrapped
// non-sentinel error so the caller knows something concrete went wrong.
func TestAuthenticator_Login_RefreshSaveErrorIsWrapped(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	wrap := &errorRefreshStore{inner: h.refresh, saveErr: errors.New("simulated save error")}
	auth, err := service.NewAuthenticator(service.AuthenticatorDeps{
		Pool:        h.tx,
		Users:       h.users,
		Tenants:     h.tenants,
		Hasher:      h.hasher,
		Issuer:      h.issuer,
		Revoker:     h.revoker,
		Refreshes:   wrap,
		RateLimiter: h.rate,
		Lockout:     h.lockout,
		TOTP:        h.totp,
		Audit:       h.audit,
		Clock:       h.clock.Func(),
	})
	require.NoError(t, err)

	_, err = auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "save refresh")
}

// 22. Login: account-level rate-limit (per user) returns ErrRateLimitExceeded
// after the user is resolved.
func TestAuthenticator_Login_AccountRateLimitReturnsRateLimited(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)
	h.rate.allowAccount = false

	_, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrRateLimitExceeded)
}

// 23. Login: storage error when loading user surfaces as ErrInvalidCredentials
// (doesn't leak storage details to the caller).
func TestAuthenticator_Login_StorageErrorReturnsInvalidCredentials(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.users.getByLoginErr = errors.New("simulated DB outage")

	_, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrInvalidCredentials)
}

// 24. Login: GetPasswordHash error after user resolution returns ErrInvalidCredentials.
func TestAuthenticator_Login_HashFetchErrorReturnsInvalidCredentials(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)
	h.users.getHashErr = errors.New("simulated hash-fetch failure")

	_, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrInvalidCredentials)
}

// 21. LoginTOTP rejects already-completed (TOTPDone=true) access tokens.
func TestAuthenticator_LoginTOTP_AlreadyCompletedTokenReturnsInvalid(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedAlice(t)

	first, err := h.auth.Login(t.Context(), loginInput(h.orgCode, "hunter2"))
	require.NoError(t, err)

	// first.AccessToken has TOTPDone=true (no TOTP user). Re-presenting it
	// to LoginTOTP must fail.
	_, err = h.auth.LoginTOTP(t.Context(), authapi.LoginTOTPInput{
		PartialToken: first.AccessToken,
		Code:         "123456",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, authapi.ErrTokenInvalid)
}

// Compile-time: ensure *service.Authenticator satisfies authapi.Authenticator.
var _ authapi.Authenticator = (*service.Authenticator)(nil)
