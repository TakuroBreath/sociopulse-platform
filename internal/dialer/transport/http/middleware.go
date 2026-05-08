package http

import (
	"context"
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// claimsFromContext is the central read point for the per-request
// Claims attached by JWTMiddleware. When the middleware ran on a
// route, claims are always present; the (false) branch exists for
// defence-in-depth (e.g. a future refactor that mounts a handler
// outside the JWTMiddleware chain). On the missing-claims path we
// abort with the canonical 401 envelope.
//
// Returning (Claims, true) on success / aborting with 401 on miss
// keeps the call sites a single line:
//
//	claims, ok := claimsFromContext(c)
//	if !ok { return }
//
// Sharing this helper across every handler collapses the missing-
// claims branch from N handlers into one place — both for readability
// and for coverage.
func claimsFromContext(c *gin.Context) (authapi.Claims, bool) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorEnvelope{
			Code:    "auth.token_invalid",
			Message: "authentication required",
		})
		return authapi.Claims{}, false
	}
	return claims, true
}

// requireRole returns a gin middleware that enforces the
// authenticated caller holds at least one of the supplied roles. It
// mirrors the sibling crm/transport/http.requireAnyRole shape: if the
// request reached this layer without claims (the JWTMiddleware would
// normally have aborted) we reject with 401; otherwise 403.
//
// The RBAC matrix layer is also evaluated at the service layer for any
// state-mutating operation, but a transport-level guard surfaces the
// rejection earlier with less round-trip cost. The role list lives at
// the route declaration so each operator-facing endpoint's required
// role is auditable in a single grep.
func requireRole(roles ...authapi.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authmw.ClaimsFromContext(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorEnvelope{
				Code:    "auth.token_invalid",
				Message: "authentication required",
			})
			return
		}
		// slices.ContainsFunc per golang-modernize Go 1.21+; replaces the
		// explicit for-range OR-membership loop. Functionally identical.
		if slices.ContainsFunc(roles, claims.HasRole) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusForbidden, ErrorEnvelope{
			Code:    "auth.insufficient_role",
			Message: "role not allowed for this action",
		})
	}
}

// RefreshFn is the adapter the composition root supplies to keep the
// middleware decoupled from fsm.RefreshPresence's Redis-typed signature.
// Tests inject a fake without touching Redis; production binds a closure
// over the dialer module's *redis.Client + presence-key TTL.
type RefreshFn func(ctx context.Context, tenantID, operatorID uuid.UUID) error

// RefreshPresenceMiddleware extends the operator's heartbeat presence
// every time the operator hits an authenticated route. The Heartbeat
// watchdog (fsm/heartbeat.go) forces the operator offline when the
// presence key expires; without this middleware the only TTL refresh
// happens at the WS keep-alive layer, so a long-running idle UI session
// (or HTTP-only flow) would be force-paused after one watchdog sweep.
//
// Behaviour:
//
//   - Refreshes on every authenticated request (claims present in
//     gin.Context). The refresh is fired synchronously on the request
//     ctx so a client disconnect bounds the side effect.
//   - Skips silently when claims are absent. The missing-claims path
//     belongs to JWTMiddleware; double-aborting here would mask wiring
//     bugs (a chain-order regression that mounts the middleware before
//     JWTMiddleware should surface as a 401 from JWTMiddleware, not a
//     200 with no presence refresh).
//   - Failures are NOT propagated. A Redis-down event is observed via
//     the metrics counter and a debug log; the request still completes
//     because graceful-disconnect detection is best-effort by design.
//   - nil RefreshFn yields a no-op middleware so a Redis-less test
//     setup or an HTTP-router-only boot doesn't trip on construction.
//
// Construction is cheap; mount once per route group AFTER JWTMiddleware
// so claims are populated.
func RefreshPresenceMiddleware(refresh RefreshFn) gin.HandlerFunc {
	return refreshPresenceMiddleware(refresh, nil, nil)
}

// refreshPresenceMiddleware is the metrics-aware private factory. The
// exported constructor delegates here with nil metrics + nil logger;
// Mount(...) wires the Deps.Metrics + Deps.Logger when present so the
// composition root opts into observability without an API change.
func refreshPresenceMiddleware(refresh RefreshFn, metrics *Metrics, logger *zap.Logger) gin.HandlerFunc {
	if refresh == nil {
		// No-op middleware: defer the (already-validated) request to
		// the next handler. The metric counter stays untouched — a nil
		// refresh isn't a failure, it's an explicit skip.
		return func(c *gin.Context) { c.Next() }
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(c *gin.Context) {
		// Read claims via authmw.ClaimsFromContext directly (NOT
		// claimsFromContext) because the latter aborts with 401 on
		// miss; the spec for this middleware is to chain to next
		// silently when claims are absent.
		claims, ok := authmw.ClaimsFromContext(c)
		if !ok {
			c.Next()
			return
		}
		// Fire on the request ctx — auto-cancel on client disconnect
		// bounds the side effect. Synchronous (no goroutine) so the
		// ctx remains valid and the handler runs after the refresh
		// completes. Refresh latency is sub-ms in production (a single
		// Redis SET); the side effect is on the critical path only as
		// long as Redis itself is healthy.
		if err := refresh(c.Request.Context(), claims.TenantID, claims.UserID); err != nil {
			metrics.observePresenceRefreshFailure()
			// Debug-level only: per-request WARN/INFO would flood logs
			// during a Redis incident. Operators see the metric instead.
			logger.Debug("dialer/transport/http: refresh presence failed", zap.Error(err))
		}
		c.Next()
	}
}
