package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// fakeValidator is a hand-rolled api.ClaimsValidator that returns
// predetermined results keyed on the supplied token string. Hand-rolled
// (rather than mockery) keeps this test free of code-generation deps and
// makes the per-token routing easy to read.
type fakeValidator struct {
	byToken map[string]validatorResult
	calls   int
}

type validatorResult struct {
	claims authapi.Claims
	err    error
}

func (f *fakeValidator) Validate(_ context.Context, token string) (authapi.Claims, error) {
	f.calls++
	r, ok := f.byToken[token]
	if !ok {
		return authapi.Claims{}, authapi.ErrTokenInvalid
	}
	return r.claims, r.err
}

// newRouter wires the middleware into a fresh test-mode engine with a
// minimal /protected endpoint that echoes the Claims if present.
func newRouter(t *testing.T, v authapi.ClaimsValidator) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/protected", authmw.JWTMiddleware(v), func(c *gin.Context) {
		claims, ok := authmw.ClaimsFromContext(c)
		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "no_claims"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"sub":   claims.UserID.String(),
			"login": claims.Login,
		})
	})
	return r
}

func TestJWTMiddleware_MissingHeader_Returns401(t *testing.T) {
	t.Parallel()
	r := newRouter(t, &fakeValidator{})

	req := httptest.NewRequest(http.MethodGet, "/protected", http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	body := decodeEnvelope(t, rec.Body.String())
	assert.Equal(t, "auth.token_invalid", body["error"])
}

func TestJWTMiddleware_WrongScheme_Returns401(t *testing.T) {
	t.Parallel()
	r := newRouter(t, &fakeValidator{})

	req := httptest.NewRequest(http.MethodGet, "/protected", http.NoBody)
	req.Header.Set("Authorization", "Basic Zm9vOmJhcg==")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	body := decodeEnvelope(t, rec.Body.String())
	assert.Equal(t, "auth.token_invalid", body["error"])
}

func TestJWTMiddleware_BearerWithNoToken_Returns401(t *testing.T) {
	t.Parallel()
	r := newRouter(t, &fakeValidator{})

	req := httptest.NewRequest(http.MethodGet, "/protected", http.NoBody)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestJWTMiddleware_ValidToken_HandlerSeesClaims(t *testing.T) {
	t.Parallel()

	want := authapi.Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Login:    "alice",
		Roles:    []authapi.Role{authapi.RoleOperator},
	}
	v := &fakeValidator{byToken: map[string]validatorResult{
		"good-token": {claims: want},
	}}
	r := newRouter(t, v)

	req := httptest.NewRequest(http.MethodGet, "/protected", http.NoBody)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	body := decodeEnvelope(t, rec.Body.String())
	assert.Equal(t, want.UserID.String(), body["sub"])
	assert.Equal(t, "alice", body["login"])
	assert.Equal(t, 1, v.calls, "validator must be called exactly once")
}

func TestJWTMiddleware_InvalidToken_Returns401TokenInvalid(t *testing.T) {
	t.Parallel()

	v := &fakeValidator{byToken: map[string]validatorResult{
		"bad-token": {err: authapi.ErrTokenInvalid},
	}}
	r := newRouter(t, v)

	req := httptest.NewRequest(http.MethodGet, "/protected", http.NoBody)
	req.Header.Set("Authorization", "Bearer bad-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	body := decodeEnvelope(t, rec.Body.String())
	assert.Equal(t, "auth.token_invalid", body["error"])
}

func TestJWTMiddleware_RevokedToken_Returns401TokenRevoked(t *testing.T) {
	t.Parallel()

	v := &fakeValidator{byToken: map[string]validatorResult{
		"revoked": {err: authapi.ErrTokenRevoked},
	}}
	r := newRouter(t, v)

	req := httptest.NewRequest(http.MethodGet, "/protected", http.NoBody)
	req.Header.Set("Authorization", "Bearer revoked")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	body := decodeEnvelope(t, rec.Body.String())
	assert.Equal(t, "auth.token_revoked", body["error"])
}

func TestJWTMiddleware_OtherError_MapsToTokenInvalid(t *testing.T) {
	t.Parallel()

	v := &fakeValidator{byToken: map[string]validatorResult{
		"weird": {err: errors.New("redis: connection refused")},
	}}
	r := newRouter(t, v)

	req := httptest.NewRequest(http.MethodGet, "/protected", http.NoBody)
	req.Header.Set("Authorization", "Bearer weird")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	body := decodeEnvelope(t, rec.Body.String())
	assert.Equal(t, "auth.token_invalid", body["error"])
}

func TestJWTMiddleware_LowercaseHeader_StillWorks(t *testing.T) {
	t.Parallel()

	want := authapi.Claims{UserID: uuid.New(), TenantID: uuid.New(), Login: "bob"}
	v := &fakeValidator{byToken: map[string]validatorResult{
		"lower": {claims: want},
	}}
	r := newRouter(t, v)

	req := httptest.NewRequest(http.MethodGet, "/protected", http.NoBody)
	// Use lowercase scheme + lowercase header — net/http canonicalises
	// the field name and our middleware compares the scheme
	// case-insensitively.
	req.Header.Set("authorization", "bearer lower")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

func TestClaimsFromContext_NilContext_ReturnsZero(t *testing.T) {
	t.Parallel()
	c, ok := authmw.ClaimsFromContext(nil)
	assert.False(t, ok)
	assert.Equal(t, authapi.Claims{}, c)
}

func TestClaimsFromContext_NoValue_ReturnsZero(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	got, ok := authmw.ClaimsFromContext(c)
	assert.False(t, ok)
	assert.Equal(t, authapi.Claims{}, got)
}

func TestJWTMiddleware_NilValidator_Panics(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() {
		_ = authmw.JWTMiddleware(nil)
	})
}

// decodeEnvelope unmarshals a JSON map for assertion convenience. The
// helper trims trailing whitespace because gin emits a final newline.
func decodeEnvelope(t *testing.T, raw string) map[string]any {
	t.Helper()
	out := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(raw)), &out), "raw: %q", raw)
	return out
}
