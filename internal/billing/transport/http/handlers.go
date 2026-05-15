package http

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/service"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// Handlers is the gin handler bundle. It depends on the Service composer
// for the business surface, an optional AuditEmitter (best-effort tariff-
// update audit), and an injectable Now func for deterministic period
// parsing in tests.
type Handlers struct {
	svc   *service.Service
	now   func() time.Time
	audit *service.AuditEmitter // may be nil — degrades to log-only audit
}

// NewHandlers wires a Handlers around a Service. The audit emitter is
// optional: pass nil when running a degraded boot (no pool, no outbox)
// and the PATCH endpoint will still update the tariff but skip the audit
// event. now defaults to time.Now when nil.
//
// Panics on a nil svc: every legitimate caller has a real Service. A nil
// would dereference inside every handler.
func NewHandlers(svc *service.Service, audit *service.AuditEmitter, now func() time.Time) *Handlers {
	if svc == nil {
		panic("billing/transport/http: NewHandlers: svc must be non-nil")
	}
	if now == nil {
		now = time.Now
	}
	return &Handlers{svc: svc, now: now, audit: audit}
}

// claimsOrAbort extracts JWT claims; on absence, aborts the chain with
// 401 + the canonical envelope. Returned bool is true iff claims are
// available. The shape mirrors the dialer + reports transports.
func claimsOrAbort(c *gin.Context) (authapi.Claims, bool) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorEnvelope{
			Code:    "billing.unauthenticated",
			Message: "missing auth claims",
		})
		return authapi.Claims{}, false
	}
	return claims, true
}

// Dashboard handles GET /api/finance/dashboard.
//
// Composes the four KPI tiles, the breakdown pie chart, the byMonth bar
// chart, and the top-5 projects mirror of the AdminFinance prototype.
// The "vs previous" deltas use a previous period of the same length
// (see previousSameLength helper).
func (h *Handlers) Dashboard(c *gin.Context) {
	claims, ok := claimsOrAbort(c)
	if !ok {
		return
	}
	period, err := parsePeriod(c, h.now())
	if err != nil {
		renderError(c, h.svc.Logger, err)
		return
	}

	ctx := c.Request.Context()
	curr, err := h.svc.SpendReport.MonthSpend(ctx, claims.TenantID, nil, period)
	if err != nil {
		renderError(c, h.svc.Logger, err)
		return
	}

	// Previous period of the same length. ErrInvalidPeriod here is
	// tolerated as "no comparison" (e.g. when From.Sub would yield a
	// zero-time anchor); other errors propagate.
	prevPeriod := previousSameLength(period)
	prev, err := h.svc.SpendReport.MonthSpend(ctx, claims.TenantID, nil, prevPeriod)
	if err != nil && !errors.Is(err, billingapi.ErrInvalidPeriod) {
		renderError(c, h.svc.Logger, err)
		return
	}

	margin, err := h.svc.MarginReport.Margin(ctx, claims.TenantID, period)
	if err != nil {
		renderError(c, h.svc.Logger, err)
		return
	}

	byMonth, err := h.svc.SpendReport.SpendByMonth(ctx, claims.TenantID, 6)
	if err != nil {
		renderError(c, h.svc.Logger, err)
		return
	}

	revenue := int64(0)
	for _, m := range margin {
		revenue += m.RevenueMin
	}

	c.JSON(http.StatusOK, billingapi.DashboardResponse{
		TenantID:    claims.TenantID,
		Period:      period,
		MonthSpend:  curr.TotalMin,
		PrevSpend:   prev.TotalMin,
		DeltaPct:    pctDelta(curr.TotalMin, prev.TotalMin),
		CostPerSrv:  curr.CostPerSurveyMinor(),
		PrevCostSrv: prev.CostPerSurveyMinor(),
		AvgCostMinM: curr.AvgCostPerMinuteMinor(),
		RevenueMin:  revenue,
		MarginMin:   revenue - curr.TotalMin,
		MarginPct:   marginPct(revenue, curr.TotalMin),
		Breakdown:   buildBreakdown(curr),
		ByMonth:     toByMonthItems(byMonth),
		TopProjects: topN(margin, 5),
	})
}

// Breakdown handles GET /api/finance/breakdown.
//
// Returns the five-component pie-chart projection of the current period's
// MonthBreakdown. Same period semantics as Dashboard.
func (h *Handlers) Breakdown(c *gin.Context) {
	claims, ok := claimsOrAbort(c)
	if !ok {
		return
	}
	period, err := parsePeriod(c, h.now())
	if err != nil {
		renderError(c, h.svc.Logger, err)
		return
	}
	curr, err := h.svc.SpendReport.MonthSpend(c.Request.Context(), claims.TenantID, nil, period)
	if err != nil {
		renderError(c, h.svc.Logger, err)
		return
	}
	c.JSON(http.StatusOK, buildBreakdown(curr))
}

// ByMonth handles GET /api/finance/byMonth?count=6.
//
// Returns the trailing-`count`-months series (oldest first). count
// defaults to 6 and is clamped to [1, 24] — out-of-range values yield
// 400 billing.invalid_period.
func (h *Handlers) ByMonth(c *gin.Context) {
	claims, ok := claimsOrAbort(c)
	if !ok {
		return
	}
	count := 6
	if cStr := c.Query("count"); cStr != "" {
		n, ok := parsePositiveInt(cStr, 1, 24)
		if !ok {
			renderError(c, h.svc.Logger, billingapi.ErrInvalidPeriod)
			return
		}
		count = n
	}
	series, err := h.svc.SpendReport.SpendByMonth(c.Request.Context(), claims.TenantID, count)
	if err != nil {
		renderError(c, h.svc.Logger, err)
		return
	}
	c.JSON(http.StatusOK, toByMonthItems(series))
}

// Projects handles GET /api/finance/projects.
//
// Returns the full per-project margin slice (already sorted by TotalMin
// desc by MarginReport.Margin). The frontend slices to top-N on its side.
func (h *Handlers) Projects(c *gin.Context) {
	claims, ok := claimsOrAbort(c)
	if !ok {
		return
	}
	period, err := parsePeriod(c, h.now())
	if err != nil {
		renderError(c, h.svc.Logger, err)
		return
	}
	rows, err := h.svc.MarginReport.Margin(c.Request.Context(), claims.TenantID, period)
	if err != nil {
		renderError(c, h.svc.Logger, err)
		return
	}
	c.JSON(http.StatusOK, rows)
}

// GetTariffs handles GET /api/billing/tariffs.
//
// Returns the tenant's tariff snapshot. ErrNoTariffs is non-fatal: the
// handler falls back to the platform defaults and sets IsDefault=true so
// the admin UI can flag "still on defaults" to the user.
func (h *Handlers) GetTariffs(c *gin.Context) {
	claims, ok := claimsOrAbort(c)
	if !ok {
		return
	}
	t, err := h.svc.Tariffs.Get(c.Request.Context(), claims.TenantID)
	isDefault := false
	switch {
	case errors.Is(err, billingapi.ErrNoTariffs):
		t = h.svc.DefaultTariffs
		t.TenantID = claims.TenantID
		isDefault = true
	case err != nil:
		renderError(c, h.svc.Logger, err)
		return
	}
	c.JSON(http.StatusOK, billingapi.TariffsResponse{
		TenantID:  claims.TenantID,
		Tariffs:   t,
		IsDefault: isDefault,
	})
}

// PatchTariffs handles PATCH /api/billing/tariffs.
//
// Admin-only (enforced at the routes layer). Pipeline:
//  1. Load current tariffs (ErrNoTariffs → start from defaults).
//  2. Apply the patch field-by-field; record changed field keys.
//  3. Validate the merged snapshot (negative scalars → 400).
//  4. Persist via TariffStore.Update (bumps version).
//  5. Best-effort audit emit (post-Update). Audit failure does NOT roll
//     back the tariff. See service.AuditEmitter type-comment for the
//     at-most-once trade-off.
//
// Returns the updated tariff snapshot wrapped in TariffsResponse with
// IsDefault=false.
func (h *Handlers) PatchTariffs(c *gin.Context) {
	claims, ok := claimsOrAbort(c)
	if !ok {
		return
	}

	var p billingapi.TariffsPatchRequest
	if err := c.ShouldBindJSON(&p); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, ErrorEnvelope{
			Code:    "billing.invalid_tariff",
			Message: err.Error(),
		})
		return
	}

	ctx := c.Request.Context()
	curr, err := h.svc.Tariffs.Get(ctx, claims.TenantID)
	if errors.Is(err, billingapi.ErrNoTariffs) {
		curr = h.svc.DefaultTariffs
		curr.TenantID = claims.TenantID
	} else if err != nil {
		renderError(c, h.svc.Logger, err)
		return
	}

	versionBefore := curr.Version
	changedKeys := applyPatch(&curr, p)
	if err := curr.Validate(); err != nil {
		renderError(c, h.svc.Logger, err)
		return
	}

	updated, err := h.svc.Tariffs.Update(ctx, claims.TenantID, curr)
	if err != nil {
		renderError(c, h.svc.Logger, err)
		return
	}

	// Best-effort audit emit (post-Update). Never blocks the response —
	// see service.AuditEmitter.EmitTariffUpdated docstring.
	if h.audit != nil && len(changedKeys) > 0 {
		h.audit.EmitTariffUpdated(ctx, claims.TenantID, claims.UserID,
			versionBefore, updated.Version, changedKeys)
	}

	c.JSON(http.StatusOK, billingapi.TariffsResponse{
		TenantID:  claims.TenantID,
		Tariffs:   updated,
		IsDefault: false,
	})
}

// applyPatch mutates curr in place with the non-nil fields of p, returns
// the list of dotted field-keys that changed. The key strings are stable
// (used in audit payloads — see service.AuditEmitter.EmitTariffUpdated).
//
// TrunkCostsMinor is replace-all (nil patch leaves curr unchanged; non-nil
// patch replaces the entire map). Scalar fields are pointer-typed, so nil
// means "leave unchanged".
func applyPatch(curr *billingapi.Tariffs, p billingapi.TariffsPatchRequest) []string {
	keys := []string{}
	if p.TrunkCostsMinor != nil {
		curr.TrunkCostsMinor = p.TrunkCostsMinor
		keys = append(keys, "trunk_costs_minor")
	}
	if p.WagePerSurveyMinor != nil {
		curr.WagePerSurveyMinor = *p.WagePerSurveyMinor
		keys = append(keys, "wage_per_survey_minor")
	}
	if p.RespondentBasesMinor != nil {
		curr.RespondentBasesMinor = *p.RespondentBasesMinor
		keys = append(keys, "respondent_bases_minor")
	}
	if p.StorageMinorPerGBMo != nil {
		curr.StorageMinorPerGBMo = *p.StorageMinorPerGBMo
		keys = append(keys, "storage_minor_per_gb_mo")
	}
	if p.FixedFeesMinor != nil {
		curr.FixedFeesMinor = *p.FixedFeesMinor
		keys = append(keys, "fixed_fees_minor")
	}
	return keys
}
