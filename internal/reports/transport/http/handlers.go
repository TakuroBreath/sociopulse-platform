package http

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	rptservice "github.com/sociopulse/platform/internal/reports/service"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// Handlers is the per-endpoint container holding the runtime ports
// the handlers need: Runner for the sync path and Queue for the async
// path. Auth/RBAC + tenant guard are applied as middleware at the
// routes.go layer, not inside the handler bodies.
type Handlers struct {
	Runner reportsapi.ReportRunner
	Queue  reportsapi.JobQueue
}

// NewHandlers wires Handlers from the reports service aggregate (the
// production composition root in cmd/api Task 8).
func NewHandlers(svc *rptservice.Service) *Handlers {
	return &Handlers{Runner: svc.Runner, Queue: svc.Queue}
}

// NewHandlersFromParts is the unit-test seam: construct Handlers from
// raw Runner + Queue ports without spinning up the full service
// aggregate (which would require Postgres, analytics, etc.).
func NewHandlersFromParts(runner reportsapi.ReportRunner, queue reportsapi.JobQueue) *Handlers {
	return &Handlers{Runner: runner, Queue: queue}
}

// ListKinds handles GET /api/reports.
//
// Returns the seven report kinds (six predefined + custom) so the
// admin UI can render the kind picker dynamically. Stable order so
// the UI list is deterministic.
func (h *Handlers) ListKinds(c *gin.Context) {
	kinds := []reportsapi.ReportKind{
		reportsapi.KindOperatorEfficiency,
		reportsapi.KindProjectSummary,
		reportsapi.KindCallsByStatus,
		reportsapi.KindFinance,
		reportsapi.KindQualityControl,
		reportsapi.KindHourlyActivity,
		reportsapi.KindCustom,
	}
	c.JSON(http.StatusOK, gin.H{"kinds": kinds})
}

// exportRequestBody binds the JSON body of POST /api/reports/:kind/export
// and POST /api/reports/custom. Window-from/-to are decoded as RFC 3339
// timestamps; binding rejects payloads that omit them.
type exportRequestBody struct {
	Format     reportsapi.ExportFormat `json:"format"      binding:"required"`
	Params     map[string]any          `json:"params"`
	WindowFrom time.Time               `json:"window_from" binding:"required"`
	WindowTo   time.Time               `json:"window_to"   binding:"required"`
}

// Export handles POST /api/reports/:kind/export.
//
// Tries the synchronous path first via Runner.Run. If the request
// trips the async threshold (Runner returns ErrAsyncRequired), the
// handler enqueues the job via Queue.Enqueue and returns 202 +
// JobTicket. All other Runner errors map through mapServiceErr to
// the canonical {code, message} envelope.
//
// The success body is the raw rendered bytes with Content-Type set
// to the renderer's MIME (xlsx/csv/pdf). The HTTP layer does not
// re-encode — Render owns the wire shape.
func (h *Handlers) Export(c *gin.Context) {
	kind := reportsapi.ReportKind(c.Param("kind"))
	if !knownReportKind(kind) {
		writeErr(c, http.StatusBadRequest, "reports.unknown_kind", "unknown report kind: "+string(kind))
		return
	}
	in, herr := parseRenderInput(c, kind)
	if herr != nil {
		writeErr(c, herr.status, herr.code, herr.message)
		return
	}
	res, err := h.Runner.Run(c.Request.Context(), in)
	switch {
	case err == nil:
		c.Data(http.StatusOK, res.MIME, res.Bytes)
		return
	case errors.Is(err, reportsapi.ErrAsyncRequired):
		// Auto-route to the async path: the runner refused this sync
		// request because the window or estimated rows tripped the
		// threshold. Reuse the parsed RenderInput; the actor becomes
		// the notify target because the API contract for a sync
		// fallback is "tell the same user who hit the endpoint".
		ticket, qerr := h.Queue.Enqueue(c.Request.Context(), reportsapi.JobInput{
			RenderInput:  in,
			NotifyUserID: in.ActorID,
		})
		if qerr != nil {
			mapServiceErr(c, qerr)
			return
		}
		c.JSON(http.StatusAccepted, ticket)
		return
	default:
		mapServiceErr(c, err)
		return
	}
}

// Custom handles POST /api/reports/custom — always asynchronous.
//
// Custom reports skip the sync path entirely (the user explicitly
// asks for "async receipt" by hitting this endpoint). The handler
// validates the payload, enqueues a Job for kind=custom, and returns
// 202 + JobTicket.
func (h *Handlers) Custom(c *gin.Context) {
	in, herr := parseRenderInput(c, reportsapi.KindCustom)
	if herr != nil {
		writeErr(c, herr.status, herr.code, herr.message)
		return
	}
	ticket, err := h.Queue.Enqueue(c.Request.Context(), reportsapi.JobInput{
		RenderInput:  in,
		NotifyUserID: in.ActorID,
	})
	if err != nil {
		mapServiceErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, ticket)
}

// GetJob handles GET /api/reports/jobs/:jobID.
//
// The jobIDTenantGuard middleware (see routes.go) verified the job
// belongs to the caller's tenant before this handler runs, so the
// handler just fetches and serialises.
func (h *Handlers) GetJob(c *gin.Context) {
	jobID := c.Param("jobID")
	job, err := h.Queue.Get(c.Request.Context(), jobID)
	if err != nil {
		mapServiceErr(c, err)
		return
	}
	c.JSON(http.StatusOK, job)
}

// Download handles GET /api/reports/jobs/:jobID/download.
//
// Returns 302 → presigned URL on a succeeded job; 409 reports.job_not_ready
// for any other state or a job whose DownloadURL is empty. The presigned
// URL is minted eagerly by the consumer at MarkSucceeded; this handler
// is a thin proxy.
func (h *Handlers) Download(c *gin.Context) {
	jobID := c.Param("jobID")
	job, err := h.Queue.Get(c.Request.Context(), jobID)
	if err != nil {
		mapServiceErr(c, err)
		return
	}
	if job.State != reportsapi.JobSucceeded || job.DownloadURL == "" {
		writeErr(c, http.StatusConflict, "reports.job_not_ready",
			"job is not in succeeded state or has no download URL")
		return
	}
	c.Redirect(http.StatusFound, job.DownloadURL)
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

// writeErr renders the canonical {code, message} envelope and aborts
// the gin chain so downstream handlers do not run.
func writeErr(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, ErrorEnvelope{Code: code, Message: msg})
}

// knownReportKind reports whether k is one of the seven valid kinds.
// Mirrors internal/reports/service.knownKind — duplicated here so the
// transport can reject unknown kinds at the URL level without spinning
// the Runner, and the cross-package import direction stays clean
// (transport depends on service, not vice versa, so duplicating this
// constant set is cheaper than exporting it).
func knownReportKind(k reportsapi.ReportKind) bool {
	switch k {
	case reportsapi.KindOperatorEfficiency, reportsapi.KindProjectSummary,
		reportsapi.KindCallsByStatus, reportsapi.KindFinance,
		reportsapi.KindQualityControl, reportsapi.KindHourlyActivity,
		reportsapi.KindCustom:
		return true
	}
	return false
}

// httpErr carries the typed-error parts parseRenderInput returns when
// validation fails. The caller renders it via writeErr.
type httpErr struct {
	status  int
	code    string
	message string
}

// parseRenderInput binds the JSON body, validates basic shape, and
// reads the caller's tenant + actor from the gin auth context. Returns
// a typed httpErr the handler surfaces via writeErr so the
// {code, message} envelope is uniform across all 400/401 branches.
//
// Format is validated explicitly here in addition to the binding tag:
// a missing format trips the binding validator, but a present-but-
// unsupported format ("html") needs the explicit switch.
func parseRenderInput(c *gin.Context, kind reportsapi.ReportKind) (reportsapi.RenderInput, *httpErr) {
	var b exportRequestBody
	if err := c.ShouldBindJSON(&b); err != nil {
		return reportsapi.RenderInput{}, &httpErr{
			status:  http.StatusBadRequest,
			code:    "reports.invalid_params",
			message: err.Error(),
		}
	}
	switch b.Format {
	case reportsapi.FormatXLSX, reportsapi.FormatCSV, reportsapi.FormatPDF:
		// ok
	default:
		return reportsapi.RenderInput{}, &httpErr{
			status:  http.StatusBadRequest,
			code:    "reports.unsupported_format",
			message: "format must be one of xlsx / csv / pdf",
		}
	}
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		return reportsapi.RenderInput{}, &httpErr{
			status:  http.StatusUnauthorized,
			code:    "reports.unauthenticated",
			message: "missing auth claims",
		}
	}
	return reportsapi.RenderInput{
		Kind:     kind,
		Format:   b.Format,
		Params:   b.Params,
		Window:   analyticsapi.Window{From: b.WindowFrom, To: b.WindowTo},
		TenantID: claims.TenantID,
		ActorID:  claims.UserID,
	}, nil
}

// mapServiceErr maps a service-layer error to {status, code} + abort.
//
// Sentinel → HTTP status + canonical envelope:
//
//	ErrJobNotFound     404 reports.job_not_found
//	ErrInvalidParams   400 reports.invalid_params
//	ErrInvalidWindow   400 reports.window_invalid   (bare sentinel — Plan 13.2 lesson #3)
//	ErrUnknownKind     400 reports.unknown_kind
//	ErrUnsupportedFmt  400 reports.unsupported_format
//	ErrTooLarge        422 reports.too_large
//	ErrAsyncRequired   400 reports.async_required   (defence — Export handles this inline)
//	ErrCanceled        409 reports.canceled
//	default            500 reports.internal
func mapServiceErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, reportsapi.ErrJobNotFound):
		writeErr(c, http.StatusNotFound, "reports.job_not_found", err.Error())
	case errors.Is(err, reportsapi.ErrInvalidParams):
		writeErr(c, http.StatusBadRequest, "reports.invalid_params", err.Error())
	case errors.Is(err, analyticsapi.ErrInvalidWindow):
		writeErr(c, http.StatusBadRequest, "reports.window_invalid", err.Error())
	case errors.Is(err, reportsapi.ErrUnknownKind):
		writeErr(c, http.StatusBadRequest, "reports.unknown_kind", err.Error())
	case errors.Is(err, reportsapi.ErrUnsupportedFmt):
		writeErr(c, http.StatusBadRequest, "reports.unsupported_format", err.Error())
	case errors.Is(err, reportsapi.ErrTooLarge):
		writeErr(c, http.StatusUnprocessableEntity, "reports.too_large", err.Error())
	case errors.Is(err, reportsapi.ErrAsyncRequired):
		writeErr(c, http.StatusBadRequest, "reports.async_required", err.Error())
	case errors.Is(err, reportsapi.ErrCanceled):
		writeErr(c, http.StatusConflict, "reports.canceled", err.Error())
	default:
		writeErr(c, http.StatusInternalServerError, "reports.internal", err.Error())
	}
}
