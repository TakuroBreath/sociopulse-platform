// http_handlers.go — gin HTTP transport for analytics MetricsQuery.
//
// Mounts six GET routes under /api/analytics/* on the supplied gin engine:
//
//	GET /api/analytics/calls                 → MetricsQuery.Calls
//	GET /api/analytics/operator-state        → MetricsQuery.OperatorState
//	GET /api/analytics/region-progress       → MetricsQuery.RegionProgress
//	GET /api/analytics/hourly                → MetricsQuery.Hourly
//	GET /api/analytics/operator-comparisons  → MetricsQuery.OperatorComparisons
//	GET /api/analytics/overview              → ServiceRO.Overview
//
// Each handler:
//  1. Reads tenant_id from JWT claims via tenantIDFromContext (defence-
//     in-depth — the caller MUST wire authmw.JWTMiddleware in front of
//     MountAnalyticsRoutes so this branch is unreachable in production).
//  2. Binds query-string params into a typed struct via
//     c.ShouldBindQuery. time.Time fields use RFC3339 (per gin's
//     default time format).
//  3. Invokes the underlying ServiceRO method.
//  4. Maps sentinel errors (api.ErrTenantRequired → 401, api.ErrInvalidWindow
//     → 400) to status codes; generic errors → 500 + structured log.
//  5. Returns the result as JSON.
//
// The transport is intentionally thin — all caching, CH access, and
// crm-port lookup live inside the QueryService. The handler's only job
// is the wire-protocol translation.

package service

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	apianalytics "github.com/sociopulse/platform/internal/analytics/api"
	"github.com/sociopulse/platform/internal/analytics/metrics"
)

// MountAnalyticsRoutes registers the six GET routes on r. qs is the
// read-side aggregate (production: *QueryService). logger and m may be
// nil — logger nil-falls-back to zap.NewNop; the metrics helpers are
// nil-safe.
//
// The caller MUST wire the project's JWT auth middleware
// (pkg/middleware/auth.JWTMiddleware) BEFORE invoking
// MountAnalyticsRoutes so tenant_id is on the gin context by the time
// the handler runs. The current handler treats a missing claims context
// as 401 (defence-in-depth) rather than 500.
func MountAnalyticsRoutes(r gin.IRouter, qs apianalytics.ServiceRO, logger *zap.Logger, m *metrics.QueryMetrics) {
	if qs == nil {
		// A nil qs would panic on the first request. Bail at registration
		// instead — analytics.Module.Register only calls this with a
		// successfully constructed QueryService, so a nil qs here is a
		// programming error in the composition root.
		panic("analytics/service: MountAnalyticsRoutes: qs is required")
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	g := r.Group("/api/analytics")
	g.GET("/calls", makeCallsHandler(qs, logger, m))
	g.GET("/operator-state", makeOperatorStateHandler(qs, logger, m))
	g.GET("/region-progress", makeRegionProgressHandler(qs, logger, m))
	g.GET("/hourly", makeHourlyHandler(qs, logger, m))
	g.GET("/operator-comparisons", makeOperatorComparisonsHandler(qs, logger, m))
	g.GET("/overview", makeOverviewHandler(qs, logger, m))
}

// =============================================================================
// Query-string binding structs + helpers
// =============================================================================
//
// Each *Params struct is the wire shape of one endpoint's query string for
// time-related and optional-flag fields. UUID fields are pulled separately
// via parseUUID / parseOptionalUUID because gin's form binder treats
// uuid.UUID as a [16]byte array and tries to multi-value it, producing
// "[uuid-string] is not valid value for uuid.UUID" errors. The dedicated
// helpers mirror the parse-then-bind pattern used by
// internal/recording/transport/http/search_handler.go::parseSearchQuery.
//
// time.Time fields rely on gin's default form binding (RFC3339). Required
// fields carry `binding:"required"`.

// callsParams binds GET /api/analytics/calls query string time fields.
// project_id is parsed separately (see parseOptionalUUID).
type callsParams struct {
	From time.Time `form:"from" binding:"required"`
	To   time.Time `form:"to" binding:"required"`
}

// operatorStateParams binds GET /api/analytics/operator-state query string
// time fields. project_id and operator_id are parsed separately.
type operatorStateParams struct {
	From time.Time `form:"from" binding:"required"`
	To   time.Time `form:"to" binding:"required"`
}

// regionProgressParams binds GET /api/analytics/region-progress query string
// time fields. project_id is parsed separately; it is REQUIRED for this
// endpoint because RegionProgress drills into one project's quotas — a
// tenant-wide aggregate is meaningless here.
type regionProgressParams struct {
	From time.Time `form:"from" binding:"required"`
	To   time.Time `form:"to" binding:"required"`
}

// hourlyParams binds GET /api/analytics/hourly query string time fields.
type hourlyParams struct {
	From time.Time `form:"from" binding:"required"`
	To   time.Time `form:"to" binding:"required"`
}

// operatorComparisonsParams binds GET /api/analytics/operator-comparisons
// query string time fields. project_id is parsed separately; required for
// this endpoint (the team-average comparison is per-project; cross-project
// aggregation would mix incomparable scopes).
type operatorComparisonsParams struct {
	From time.Time `form:"from" binding:"required"`
	To   time.Time `form:"to" binding:"required"`
}

// overviewParams binds GET /api/analytics/overview query string time fields.
type overviewParams struct {
	From time.Time `form:"from" binding:"required"`
	To   time.Time `form:"to" binding:"required"`
}

// parseRequiredUUID extracts a non-empty UUID from c.Query(name) and
// returns it. An empty value or invalid UUID returns ok=false with a
// descriptive error to bubble up to the handler's 400 response.
func parseRequiredUUID(c *gin.Context, name string) (uuid.UUID, bool, string) {
	raw := c.Query(name)
	if raw == "" {
		return uuid.Nil, false, name + " is required"
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, false, name + ": " + err.Error()
	}
	return id, true, ""
}

// parseOptionalUUID extracts a UUID from c.Query(name) if present.
// Returns (nil, true, "") when the param is absent (caller threads the
// nil pointer through to the upstream query). An invalid UUID returns
// (nil, false, msg) to bubble up to the handler's 400 response.
func parseOptionalUUID(c *gin.Context, name string) (*uuid.UUID, bool, string) {
	raw := c.Query(name)
	if raw == "" {
		return nil, true, ""
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, false, name + ": " + err.Error()
	}
	return &id, true, ""
}

// =============================================================================
// Handlers
// =============================================================================

func makeCallsHandler(qs apianalytics.ServiceRO, logger *zap.Logger, _ *metrics.QueryMetrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID, ok := tenantIDFromContext(c)
		if !ok {
			respondError(c, http.StatusUnauthorized, "analytics.unauthorized", "authentication required")
			return
		}
		var p callsParams
		if err := c.ShouldBindQuery(&p); err != nil {
			respondError(c, http.StatusBadRequest, "analytics.invalid_query", err.Error())
			return
		}
		projectID, ok, msg := parseOptionalUUID(c, "project_id")
		if !ok {
			respondError(c, http.StatusBadRequest, "analytics.invalid_query", msg)
			return
		}

		res, err := qs.Calls(c.Request.Context(), apianalytics.CallsQuery{
			TenantID:  tenantID,
			ProjectID: projectID,
			Window:    apianalytics.Window{From: p.From, To: p.To},
		})
		if err != nil {
			handleQueryError(c, logger, "calls", err)
			return
		}
		c.JSON(http.StatusOK, res)
	}
}

func makeOperatorStateHandler(qs apianalytics.ServiceRO, logger *zap.Logger, _ *metrics.QueryMetrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID, ok := tenantIDFromContext(c)
		if !ok {
			respondError(c, http.StatusUnauthorized, "analytics.unauthorized", "authentication required")
			return
		}
		var p operatorStateParams
		if err := c.ShouldBindQuery(&p); err != nil {
			respondError(c, http.StatusBadRequest, "analytics.invalid_query", err.Error())
			return
		}
		projectID, ok, msg := parseOptionalUUID(c, "project_id")
		if !ok {
			respondError(c, http.StatusBadRequest, "analytics.invalid_query", msg)
			return
		}
		operatorID, ok, msg := parseOptionalUUID(c, "operator_id")
		if !ok {
			respondError(c, http.StatusBadRequest, "analytics.invalid_query", msg)
			return
		}

		res, err := qs.OperatorState(c.Request.Context(), apianalytics.OperatorStateQuery{
			TenantID:   tenantID,
			ProjectID:  projectID,
			OperatorID: operatorID,
			Window:     apianalytics.Window{From: p.From, To: p.To},
		})
		if err != nil {
			handleQueryError(c, logger, "operator_state", err)
			return
		}
		c.JSON(http.StatusOK, res)
	}
}

func makeRegionProgressHandler(qs apianalytics.ServiceRO, logger *zap.Logger, _ *metrics.QueryMetrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID, ok := tenantIDFromContext(c)
		if !ok {
			respondError(c, http.StatusUnauthorized, "analytics.unauthorized", "authentication required")
			return
		}
		var p regionProgressParams
		if err := c.ShouldBindQuery(&p); err != nil {
			respondError(c, http.StatusBadRequest, "analytics.invalid_query", err.Error())
			return
		}
		projectID, ok, msg := parseRequiredUUID(c, "project_id")
		if !ok {
			respondError(c, http.StatusBadRequest, "analytics.invalid_query", msg)
			return
		}

		rows, err := qs.RegionProgress(c.Request.Context(), apianalytics.RegionProgressQuery{
			TenantID:  tenantID,
			ProjectID: projectID,
			Window:    apianalytics.Window{From: p.From, To: p.To},
		})
		if err != nil {
			handleQueryError(c, logger, "region_progress", err)
			return
		}
		// Avoid a `null` body on empty results — JSON encoders emit `null`
		// for a nil slice, which the dashboard typing prefers to receive
		// as `[]`. Defensive normalisation; the QueryService usually
		// returns a non-nil empty slice already.
		if rows == nil {
			rows = []apianalytics.RegionProgressRow{}
		}
		c.JSON(http.StatusOK, rows)
	}
}

func makeHourlyHandler(qs apianalytics.ServiceRO, logger *zap.Logger, _ *metrics.QueryMetrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID, ok := tenantIDFromContext(c)
		if !ok {
			respondError(c, http.StatusUnauthorized, "analytics.unauthorized", "authentication required")
			return
		}
		var p hourlyParams
		if err := c.ShouldBindQuery(&p); err != nil {
			respondError(c, http.StatusBadRequest, "analytics.invalid_query", err.Error())
			return
		}
		projectID, ok, msg := parseOptionalUUID(c, "project_id")
		if !ok {
			respondError(c, http.StatusBadRequest, "analytics.invalid_query", msg)
			return
		}

		buckets, err := qs.Hourly(c.Request.Context(), apianalytics.HourlyQuery{
			TenantID:  tenantID,
			ProjectID: projectID,
			Window:    apianalytics.Window{From: p.From, To: p.To},
		})
		if err != nil {
			handleQueryError(c, logger, "hourly", err)
			return
		}
		if buckets == nil {
			buckets = []apianalytics.HourlyBucket{}
		}
		c.JSON(http.StatusOK, buckets)
	}
}

func makeOperatorComparisonsHandler(qs apianalytics.ServiceRO, logger *zap.Logger, _ *metrics.QueryMetrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID, ok := tenantIDFromContext(c)
		if !ok {
			respondError(c, http.StatusUnauthorized, "analytics.unauthorized", "authentication required")
			return
		}
		var p operatorComparisonsParams
		if err := c.ShouldBindQuery(&p); err != nil {
			respondError(c, http.StatusBadRequest, "analytics.invalid_query", err.Error())
			return
		}
		projectID, ok, msg := parseRequiredUUID(c, "project_id")
		if !ok {
			respondError(c, http.StatusBadRequest, "analytics.invalid_query", msg)
			return
		}

		rows, err := qs.OperatorComparisons(c.Request.Context(), apianalytics.OperatorComparisonsQuery{
			TenantID:  tenantID,
			ProjectID: projectID,
			Window:    apianalytics.Window{From: p.From, To: p.To},
		})
		if err != nil {
			handleQueryError(c, logger, "operator_comparisons", err)
			return
		}
		if rows == nil {
			rows = []apianalytics.OperatorComparisonRow{}
		}
		c.JSON(http.StatusOK, rows)
	}
}

func makeOverviewHandler(qs apianalytics.ServiceRO, logger *zap.Logger, _ *metrics.QueryMetrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID, ok := tenantIDFromContext(c)
		if !ok {
			respondError(c, http.StatusUnauthorized, "analytics.unauthorized", "authentication required")
			return
		}
		var p overviewParams
		if err := c.ShouldBindQuery(&p); err != nil {
			respondError(c, http.StatusBadRequest, "analytics.invalid_query", err.Error())
			return
		}
		projectID, ok, msg := parseOptionalUUID(c, "project_id")
		if !ok {
			respondError(c, http.StatusBadRequest, "analytics.invalid_query", msg)
			return
		}

		res, err := qs.Overview(c.Request.Context(), apianalytics.OverviewQuery{
			TenantID:  tenantID,
			ProjectID: projectID,
			Window:    apianalytics.Window{From: p.From, To: p.To},
		})
		if err != nil {
			handleQueryError(c, logger, "overview", err)
			return
		}
		c.JSON(http.StatusOK, res)
	}
}

// =============================================================================
// Error mapping
// =============================================================================

// handleQueryError translates a QueryService error into a status code +
// envelope. Sentinel discrimination via errors.Is per project convention
// (golang-error-handling). Generic errors are 500 with a scrubbed
// message; the underlying error is logged at ERROR with the method
// label so ops can triage without leaking driver internals to clients.
func handleQueryError(c *gin.Context, logger *zap.Logger, method string, err error) {
	switch {
	case errors.Is(err, apianalytics.ErrTenantRequired):
		// Defence-in-depth: the handler already returned 401 when claims
		// were missing. Reaching here means the QueryService received a
		// zero TenantID despite our extraction — surface as 401 anyway
		// with the same message tenantIDFromContext uses for consistency.
		respondError(c, http.StatusUnauthorized, "analytics.unauthorized", "authentication required")
	case errors.Is(err, apianalytics.ErrInvalidWindow):
		respondError(c, http.StatusBadRequest, "analytics.invalid_window", err.Error())
	default:
		logger.Error("analytics: query failed",
			zap.String("method", method),
			zap.Error(err))
		respondError(c, http.StatusInternalServerError, "analytics.internal_error", "query failed")
	}
}
