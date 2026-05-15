package http_test

import (
	"context"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	transporthttp "github.com/sociopulse/platform/internal/dialer/transport/http"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// injectClaims is the test analogue of pkg/middleware/auth.JWTMiddleware.
// It puts the supplied Claims onto the gin.Context under the same key
// that claimsFromContext reads from, so middleware-under-test can be
// exercised without a real JWT validator.
func injectClaims(claims authapi.Claims) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(authmw.ClaimsContextKey, claims)
		c.Next()
	}
}

// =============================================================================
// RefreshPresenceMiddleware
// =============================================================================
//
// Behaviour pinned by these three tests:
//   1. Refreshes on every authenticated request (claims present).
//   2. Skips when claims are absent (auth middleware would normally have
//      aborted; we MUST chain to next without touching refresh — the
//      missing-claims branch is owned by JWTMiddleware itself).
//   3. Refresh failure does NOT block the request — the middleware is a
//      fire-and-forget side effect for graceful-disconnect detection.

func TestRefreshPresenceMiddleware_RefreshesOnAuthenticatedRequest(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	tenantID := uuid.New()
	operatorID := uuid.New()

	var refreshed atomic.Int32
	var gotTenant, gotOperator atomic.Value
	fakeRefresh := func(_ context.Context, tid, oid uuid.UUID) error {
		refreshed.Add(1)
		gotTenant.Store(tid)
		gotOperator.Store(oid)
		return nil
	}

	r := gin.New()
	r.Use(injectClaims(authapi.Claims{
		TenantID: tenantID,
		UserID:   operatorID,
		Roles:    []authapi.Role{authapi.RoleOperator},
	}))
	r.Use(transporthttp.RefreshPresenceMiddleware(fakeRefresh))
	r.GET("/sessions/me", func(c *gin.Context) { c.Status(stdhttp.StatusNoContent) })

	req := httptest.NewRequest(stdhttp.MethodGet, "/sessions/me", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusNoContent, w.Code)
	require.Equal(t, int32(1), refreshed.Load(),
		"refresh must be called exactly once on authenticated request")
	require.Equal(t, tenantID, gotTenant.Load(),
		"refresh must receive the claims' TenantID")
	require.Equal(t, operatorID, gotOperator.Load(),
		"refresh must receive the claims' UserID as operatorID")
}

func TestRefreshPresenceMiddleware_SkipsWhenNoClaims(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	var refreshed atomic.Int32
	fakeRefresh := func(_ context.Context, _, _ uuid.UUID) error {
		refreshed.Add(1)
		return nil
	}

	r := gin.New()
	// NOTE: no injectClaims — simulates a misconfigured chain where this
	// middleware ran before JWTMiddleware. We MUST chain to next without
	// invoking refresh; surfacing the auth-wiring bug is JWTMiddleware's
	// job, not ours.
	r.Use(transporthttp.RefreshPresenceMiddleware(fakeRefresh))
	r.GET("/sessions/me", func(c *gin.Context) { c.Status(stdhttp.StatusNoContent) })

	req := httptest.NewRequest(stdhttp.MethodGet, "/sessions/me", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusNoContent, w.Code,
		"request must still complete when claims are absent")
	require.Equal(t, int32(0), refreshed.Load(),
		"refresh must NOT be called without claims")
}

func TestRefreshPresenceMiddleware_FailureDoesNotBlockRequest(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	fakeRefresh := func(_ context.Context, _, _ uuid.UUID) error {
		return errors.New("redis down")
	}

	r := gin.New()
	r.Use(injectClaims(authapi.Claims{
		TenantID: uuid.New(),
		UserID:   uuid.New(),
		Roles:    []authapi.Role{authapi.RoleOperator},
	}))
	r.Use(transporthttp.RefreshPresenceMiddleware(fakeRefresh))
	r.GET("/sessions/me", func(c *gin.Context) { c.Status(stdhttp.StatusNoContent) })

	req := httptest.NewRequest(stdhttp.MethodGet, "/sessions/me", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Refresh failure must NOT block the request — it's a side effect for
	// graceful-disconnect detection, not a hard precondition. Redis-down
	// is observed via metrics; the operator's request still completes.
	require.Equal(t, stdhttp.StatusNoContent, w.Code)
}

func TestRefreshPresenceMiddleware_FailureIncrementsMetric(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	// Wire a real *Metrics so the failure path increments the counter.
	// We exercise the (private) refreshPresenceMiddleware via Mount so
	// the public surface — what cmd/api consumes — is what's tested.
	// A direct unit test of the counter via go-routine-free Inc() is
	// already covered by the Prometheus library; the value-add here is
	// proving the middleware threads metrics through correctly.
	reg := prometheus.NewRegistry()
	metrics := transporthttp.RegisterMetrics(reg)

	tenantID := uuid.New()
	operatorID := uuid.New()

	r := gin.New()
	api := r.Group("/api")
	transporthttp.Mount(api, transporthttp.Deps{
		FSM:    &fakeFSM{},
		Router: &fakeRouter{},
		Validator: &fakeValidator{
			claims: authapi.Claims{
				UserID:   operatorID,
				TenantID: tenantID,
				Login:    "alice",
				Roles:    []authapi.Role{authapi.RoleOperator},
			},
		},
		RBAC:               fakeRBAC{},
		SnapshotPubSub:     newFakePubSub(),
		CallTenantResolver: newFakeCallTenantResolver(),
		Logger:             zap.NewNop(),
		Metrics:            metrics,
		RefreshPresence: func(_ context.Context, _, _ uuid.UUID) error {
			return errors.New("redis: connection lost")
		},
	})

	req := httptest.NewRequest(stdhttp.MethodGet, "/api/sessions/me", nil)
	req.Header.Set("Authorization", "Bearer dummy.token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// The handler still runs (200 with body, since fakeFSM.GetState
	// returns a zero Snapshot which marshals fine). The point is the
	// failure didn't 5xx — and the metric ticked.
	require.Equal(t, stdhttp.StatusOK, w.Code, "body=%s", w.Body.String())
	require.InDelta(t, 1.0, testutil.ToFloat64(metrics.PresenceRefreshFailures), 0.0,
		"presence_refresh_failures_total must increment by 1")
}

func TestRefreshPresenceMiddleware_NilRefreshIsNoop(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	// Defensive: a composition root that hasn't wired refresh (e.g. a
	// Redis-less test setup) must not panic on request. The factory
	// returns a no-op middleware that simply chains to next.
	r := gin.New()
	r.Use(injectClaims(authapi.Claims{
		TenantID: uuid.New(),
		UserID:   uuid.New(),
		Roles:    []authapi.Role{authapi.RoleOperator},
	}))
	r.Use(transporthttp.RefreshPresenceMiddleware(nil))
	r.GET("/sessions/me", func(c *gin.Context) { c.Status(stdhttp.StatusNoContent) })

	req := httptest.NewRequest(stdhttp.MethodGet, "/sessions/me", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, stdhttp.StatusNoContent, w.Code,
		"nil RefreshFn must yield a no-op middleware")
}
