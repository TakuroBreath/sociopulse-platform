package http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	surveysapi "github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/internal/surveys/schemavalidator"
	surveysservice "github.com/sociopulse/platform/internal/surveys/service"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// listSurveysDefaultLimit is the default page size when ?limit is
// absent. The service layer also clamps; we surface a matching default
// so the wire shape is predictable.
const listSurveysDefaultLimit = 50

// maxSchemaBodyBytes caps the raw body for SaveVersion / preview /
// validate. Surveys schemas in practice are well below 100 KiB; 1 MiB
// is a generous ceiling that still protects the validator + DSL
// evaluator from accidental DoS via giant payloads. gin's
// MaxBytesReader truncates the body so json.Unmarshal errors out
// cleanly with a binding failure.
const maxSchemaBodyBytes = 1 << 20

// schemaValidator narrows the schemavalidator.SchemaValidator surface
// to the one method this transport uses. Defining it consumer-side
// keeps the package free from a hard dependency on the concrete type
// when constructing test fakes.
type schemaValidator interface {
	Validate(ctx context.Context, schemaJSON []byte) schemavalidator.ValidationReport
}

// Deps captures the collaborators that handlers need.
//
// Logger may be nil in tests — render paths gate on nil. Every other
// field is required; Mount panics on nil deps so a misconfigured
// composition root surfaces during cmd/api boot rather than at first
// request.
type Deps struct {
	Logger    *zap.Logger
	Surveys   surveysapi.SurveyService
	Runtime   surveysapi.Runtime
	Validator schemaValidator
	Auth      authapi.ClaimsValidator
	RBAC      authapi.RBACChecker
}

// handlers groups the per-endpoint methods so they share Deps.
type handlers struct {
	deps Deps
}

// createSurvey handles POST /api/surveys (admin).
func (h *handlers) createSurvey(c *gin.Context) {
	claims, ok := claimsOrFail(c, h.deps.Logger)
	if !ok {
		return
	}
	var req CreateSurveyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	mode := surveysapi.PrimaryMode(req.PrimaryMode)
	if mode == "" {
		mode = surveysapi.ModeForm
	}
	in := surveysapi.CreateSurveyInput{
		Name:        req.Name,
		Description: req.Description,
		PrimaryMode: mode,
	}
	ctx := withCallerScope(c, claims)
	id, err := h.deps.Surveys.Create(ctx, in)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusCreated, CreateSurveyResponse{ID: id.String()})
}

// listSurveys handles GET /api/surveys (operator+).
func (h *handlers) listSurveys(c *gin.Context) {
	claims, ok := claimsOrFail(c, h.deps.Logger)
	if !ok {
		return
	}
	filter := surveysapi.ListFilter{
		Status: surveysapi.SurveyStatus(strings.TrimSpace(c.Query("status"))),
		Search: strings.TrimSpace(c.Query("search")),
		Limit:  parseInt(c.Query("limit"), listSurveysDefaultLimit),
		Offset: parseInt(c.Query("offset"), 0),
	}
	ctx := withCallerScope(c, claims)
	rows, err := h.deps.Surveys.List(ctx, filter)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, ListSurveysResponse{
		Surveys: surveysToDTO(rows),
		Total:   len(rows),
	})
}

// getSurvey handles GET /api/surveys/:id (operator+).
//
// Plan 13.2.5 Task 1: tenant.RequireSameTenant on the route chain
// has already verified the caller's tenant owns :id. Threading the
// tenant via withCallerScope so the service runs per-tenant (defence
// in depth).
func (h *handlers) getSurvey(c *gin.Context) {
	claims, ok := claimsOrFail(c, h.deps.Logger)
	if !ok {
		return
	}
	id, err := parseIDParam(c, "id")
	if err != nil {
		renderBindError(c, err)
		return
	}
	ctx := withCallerScope(c, claims)
	s, err := h.deps.Surveys.Get(ctx, id)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, surveyToDTO(s))
}

// updateSurvey handles PATCH /api/surveys/:id (admin).
func (h *handlers) updateSurvey(c *gin.Context) {
	claims, ok := claimsOrFail(c, h.deps.Logger)
	if !ok {
		return
	}
	id, err := parseIDParam(c, "id")
	if err != nil {
		renderBindError(c, err)
		return
	}
	var req UpdateSurveyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	in := surveysapi.UpdateSurveyInput{
		Name:        req.Name,
		Description: req.Description,
	}
	if req.PrimaryMode != nil {
		mode := surveysapi.PrimaryMode(*req.PrimaryMode)
		in.PrimaryMode = &mode
	}
	ctx := withCallerScope(c, claims)
	if err := h.deps.Surveys.Update(ctx, id, in); err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// archiveSurvey handles POST /api/surveys/:id/archive (admin).
func (h *handlers) archiveSurvey(c *gin.Context) {
	claims, ok := claimsOrFail(c, h.deps.Logger)
	if !ok {
		return
	}
	id, err := parseIDParam(c, "id")
	if err != nil {
		renderBindError(c, err)
		return
	}
	ctx := withCallerScope(c, claims)
	if err := h.deps.Surveys.Archive(ctx, id); err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// saveVersion handles POST /api/surveys/:id/versions (admin).
//
// On a validation failure the service returns *api.ValidationError —
// we surface the structured report as a 422 ValidationReportDTO. Any
// other error follows the standard mapSurveyError path.
func (h *handlers) saveVersion(c *gin.Context) {
	claims, ok := claimsOrFail(c, h.deps.Logger)
	if !ok {
		return
	}
	surveyID, err := parseIDParam(c, "id")
	if err != nil {
		renderBindError(c, err)
		return
	}
	limitBody(c)
	var req SaveVersionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	if isEmptyJSON(req.Schema) {
		renderBindError(c, errors.New("schema must not be empty"))
		return
	}
	ctx := withCallerScope(c, claims)
	v, err := h.deps.Surveys.SaveVersion(ctx, surveyID, req.Schema, req.Minor)
	if err != nil {
		var verr *surveysapi.ValidationError
		if errors.As(err, &verr) {
			renderValidationReport(c, apiReportToDTO(verr.Report))
			return
		}
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusCreated, versionToDTO(v))
}

// activateVersion handles POST /api/surveys/:id/versions/:version_id/activate (admin).
func (h *handlers) activateVersion(c *gin.Context) {
	claims, ok := claimsOrFail(c, h.deps.Logger)
	if !ok {
		return
	}
	surveyID, err := parseIDParam(c, "id")
	if err != nil {
		renderBindError(c, err)
		return
	}
	versionID, err := parseIDParam(c, "version_id")
	if err != nil {
		renderBindError(c, err)
		return
	}
	ctx := withCallerScope(c, claims)
	if err := h.deps.Surveys.Activate(ctx, surveyID, versionID); err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// getActiveVersion handles GET /api/surveys/:id/versions/active (operator+).
func (h *handlers) getActiveVersion(c *gin.Context) {
	claims, ok := claimsOrFail(c, h.deps.Logger)
	if !ok {
		return
	}
	id, err := parseIDParam(c, "id")
	if err != nil {
		renderBindError(c, err)
		return
	}
	ctx := withCallerScope(c, claims)
	v, err := h.deps.Surveys.GetActiveVersion(ctx, id)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, versionToDTO(v))
}

// listVersions handles GET /api/surveys/:id/versions (operator+).
func (h *handlers) listVersions(c *gin.Context) {
	claims, ok := claimsOrFail(c, h.deps.Logger)
	if !ok {
		return
	}
	id, err := parseIDParam(c, "id")
	if err != nil {
		renderBindError(c, err)
		return
	}
	ctx := withCallerScope(c, claims)
	rows, err := h.deps.Surveys.ListVersions(ctx, id)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, ListVersionsResponse{Versions: versionsToDTO(rows)})
}

// previewRun handles POST /api/surveys/:id/preview/run (operator+).
//
// Stateless: the supplied schema is parsed in-place; runtime.NextNode
// returns the next node + termination state. No row is touched. The
// handler converts the wire-format AnswerPayload map into the typed
// api.Answer map before dispatch.
func (h *handlers) previewRun(c *gin.Context) {
	if _, ok := claimsOrFail(c, h.deps.Logger); !ok {
		return
	}
	if _, err := parseIDParam(c, "id"); err != nil {
		renderBindError(c, err)
		return
	}
	limitBody(c)
	var req PreviewRunRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	if isEmptyJSON(req.Schema) {
		renderBindError(c, errors.New("schema must not be empty"))
		return
	}
	answers := answersFromMap(req.Answers)
	res, err := h.deps.Runtime.NextNode(req.Schema, req.CurrentNodeID, answers)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, PreviewRunResponse{
		NextNodeID: res.NextNodeID,
		Terminated: res.Terminated,
		EndKind:    string(res.EndKind),
		Progress:   res.Progress,
	})
}

// validateSchema handles POST /api/surveys/:id/validate (admin).
//
// Reads the body as raw JSON and runs the schema validator on it.
// Returns 200 with {valid: true} on success or 422 with the
// structured report on failure. Nothing is persisted.
func (h *handlers) validateSchema(c *gin.Context) {
	if _, ok := claimsOrFail(c, h.deps.Logger); !ok {
		return
	}
	if _, err := parseIDParam(c, "id"); err != nil {
		renderBindError(c, err)
		return
	}
	limitBody(c)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		renderBindError(c, err)
		return
	}
	if len(body) == 0 {
		renderBindError(c, errors.New("schema body must not be empty"))
		return
	}
	report := h.deps.Validator.Validate(c.Request.Context(), body)
	if !report.Valid {
		renderValidationReportFromValidator(c, report)
		return
	}
	c.JSON(http.StatusOK, ValidationReportDTO{Valid: true})
}

// =============================================================================
// helpers
// =============================================================================

// claimsOrFail extracts the JWT claims attached by JWTMiddleware. When
// the middleware did not run (programming error) the helper renders a
// 401 and returns ok=false so handlers can short-circuit cleanly.
func claimsOrFail(c *gin.Context, log *zap.Logger) (authapi.Claims, bool) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, log, authapi.ErrTokenInvalid)
		return authapi.Claims{}, false
	}
	return claims, true
}

// withCallerScope returns a context populated with the tenant id and
// actor id from the supplied claims. The surveys service consumes
// both via WithTenantID / WithActorID — keeping this in one helper
// removes the boilerplate from every handler.
func withCallerScope(c *gin.Context, claims authapi.Claims) context.Context {
	ctx := surveysservice.WithTenantID(c.Request.Context(), claims.TenantID)
	if claims.UserID != uuid.Nil {
		ctx = surveysservice.WithActorID(ctx, claims.UserID)
	}
	return ctx
}

// parseIDParam parses a UUID gin path parameter. Returns a clean
// error so the caller can render a 400 binding failure.
func parseIDParam(c *gin.Context, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// parseInt parses a query-string int with a default fallback. Negative
// or unparseable values fall back to def — the service layer clamps
// the final values within its own bounds.
func parseInt(raw string, def int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n < 0 {
		return def
	}
	return int(n)
}

// limitBody installs a MaxBytesReader on the request body so the
// validator and runtime never see a payload larger than the
// configured ceiling. Idempotent — gin re-wraps the same Body on
// every call but the underlying limiter is sticky.
func limitBody(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxSchemaBodyBytes)
}

// isEmptyJSON reports whether the supplied raw JSON is missing,
// the zero-byte slice, or the literal "null". gin's json decoder
// captures `"schema": null` as the four-byte ASCII string "null"
// rather than nil, so a naive len(req.Schema)==0 check would let
// it through.
func isEmptyJSON(raw []byte) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null"
}

// answersFromMap converts the wire-format AnswerPayload map into the
// typed api.Answer map the runtime expects. Each payload's NodeID is
// the map key; the conversion is mechanical because AnswerPayload
// mirrors api.Answer field-for-field.
func answersFromMap(in map[string]AnswerPayload) map[string]surveysapi.Answer {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]surveysapi.Answer, len(in))
	for nodeID, payload := range in {
		out[nodeID] = surveysapi.Answer{
			NodeID:       nodeID,
			SingleChoice: payload.SingleChoice,
			MultiChoice:  payload.MultiChoice,
			Number:       payload.Number,
			Text:         payload.Text,
			AnsweredAt:   payload.AnsweredAt,
		}
	}
	return out
}
