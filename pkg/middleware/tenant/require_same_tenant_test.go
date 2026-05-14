package tenant_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
	tenantmw "github.com/sociopulse/platform/pkg/middleware/tenant"
)

// resolverStub returns the canned tenant id or err for any call. We
// hand-roll this rather than reach for mockery because the surface is a
// single function value and the test names already capture the
// behaviour-under-test.
type resolverStub struct {
	tenantID uuid.UUID
	err      error
	calls    int
	gotID    uuid.UUID
}

func (s *resolverStub) Resolve(_ context.Context, id uuid.UUID) (uuid.UUID, error) {
	s.calls++
	s.gotID = id
	return s.tenantID, s.err
}

// newRouter wires the middleware into a fresh test-mode engine with a
// minimal /resource/:id endpoint that records whether the handler was
// reached and renders 200 with a body the test can assert on.
//
// claims, when non-zero, are pre-installed on the gin context via a
// stub middleware so the real JWTMiddleware does not need a validator.
// resolveFn is the production callback the middleware-under-test
// invokes to learn the resource's owning tenant.
func newRouter(claims authapi.Claims, resolveFn tenantmw.ResolveTenantFn) (*gin.Engine, *bool) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	handlerCalled := false
	// Stub the claims-installer that JWTMiddleware would normally do.
	stubClaims := func(c *gin.Context) {
		if claims.TenantID != uuid.Nil || claims.UserID != uuid.Nil {
			c.Set(authmw.ClaimsContextKey, claims)
		}
		c.Next()
	}
	r.GET("/resource/:id",
		stubClaims,
		tenantmw.RequireSameTenant(resolveFn),
		func(c *gin.Context) {
			handlerCalled = true
			c.JSON(http.StatusOK, gin.H{"ok": true})
		},
	)
	return r, &handlerCalled
}

func TestRequireSameTenant_MatchesAndProceeds(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	resID := uuid.New()
	res := &resolverStub{tenantID: tenantID}
	r, called := newRouter(authapi.Claims{
		UserID:   uuid.New(),
		TenantID: tenantID,
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}, res.Resolve)

	req := httptest.NewRequest(http.MethodGet, "/resource/"+resID.String(), http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.True(t, *called, "downstream handler must run on match")
	assert.Equal(t, 1, res.calls, "resolver must be called exactly once")
	assert.Equal(t, resID, res.gotID, "resolver must receive the parsed :id")
}

func TestRequireSameTenant_MismatchReturns404(t *testing.T) {
	t.Parallel()

	callerTenant := uuid.New()
	otherTenant := uuid.New()
	resID := uuid.New()
	res := &resolverStub{tenantID: otherTenant}
	r, called := newRouter(authapi.Claims{
		UserID:   uuid.New(),
		TenantID: callerTenant,
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}, res.Resolve)

	req := httptest.NewRequest(http.MethodGet, "/resource/"+resID.String(), http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code,
		"mismatch must yield 404 (existence-probe defence), not 403")
	assert.False(t, *called, "downstream handler must NOT run on mismatch")
	assert.Empty(t, rec.Body.String(),
		"404 mismatch must not write a body — leaks no detail to the attacker")
}

func TestRequireSameTenant_ResolverNotFoundReturns404(t *testing.T) {
	t.Parallel()

	res := &resolverStub{err: tenantmw.ErrNotFound}
	r, called := newRouter(authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}, res.Resolve)

	req := httptest.NewRequest(http.MethodGet, "/resource/"+uuid.New().String(), http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.False(t, *called, "downstream handler must NOT run when resolver reports not-found")
	assert.Empty(t, rec.Body.String())
}

func TestRequireSameTenant_MissingClaimsReturns401(t *testing.T) {
	t.Parallel()

	res := &resolverStub{tenantID: uuid.New()}
	// Pass zero claims so the stub installer skips setting them.
	r, called := newRouter(authapi.Claims{}, res.Resolve)

	req := httptest.NewRequest(http.MethodGet, "/resource/"+uuid.New().String(), http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"absent claims must yield 401, not 404 — JWTMiddleware was bypassed")
	assert.False(t, *called)
	assert.Zero(t, res.calls, "resolver must not be called without authenticated claims")
}

func TestRequireSameTenant_MalformedIDReturns400(t *testing.T) {
	t.Parallel()

	res := &resolverStub{tenantID: uuid.New()}
	r, called := newRouter(authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}, res.Resolve)

	req := httptest.NewRequest(http.MethodGet, "/resource/not-a-uuid", http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"malformed :id must yield 400 — distinguishes client error from auth")
	assert.False(t, *called)
	assert.Zero(t, res.calls, "resolver must not be called for malformed :id")
}

func TestRequireSameTenant_ResolverInternalErrorReturns500(t *testing.T) {
	t.Parallel()

	res := &resolverStub{err: errors.New("storage: timeout")}
	r, called := newRouter(authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []authapi.Role{authapi.RoleAdmin},
	}, res.Resolve)

	req := httptest.NewRequest(http.MethodGet, "/resource/"+uuid.New().String(), http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"non-not-found resolver errors must yield 500")
	assert.False(t, *called)
}

func TestRequireSameTenant_NilResolver_Panics(t *testing.T) {
	t.Parallel()

	assert.Panics(t, func() {
		_ = tenantmw.RequireSameTenant(nil)
	}, "nil resolveFn must panic at construction time")
}

// TestRequireSameTenant_NonDefaultIDParam covers the WithIDParam option
// for the routes that name the path parameter something other than the
// default "id" (e.g. /:opID, /:version_id). Failing to honour this is a
// silent cross-tenant guard bypass.
func TestRequireSameTenant_NonDefaultIDParam(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	resID := uuid.New()
	res := &resolverStub{tenantID: tenantID}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	called := false
	r.GET("/resource/:thing",
		func(c *gin.Context) {
			c.Set(authmw.ClaimsContextKey, authapi.Claims{
				UserID:   uuid.New(),
				TenantID: tenantID,
				Roles:    []authapi.Role{authapi.RoleAdmin},
			})
			c.Next()
		},
		tenantmw.RequireSameTenant(res.Resolve, tenantmw.WithIDParam("thing")),
		func(c *gin.Context) {
			called = true
			c.Status(http.StatusOK)
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/resource/"+resID.String(), http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, called)
	assert.Equal(t, resID, res.gotID)
}
