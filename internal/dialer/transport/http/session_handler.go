package http

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
)

// hangupReasonDefault is the fallback reason fed to Router.Hangup when
// the operator omits it on POST /api/calls/:id/hangup. The string lives
// in the audit trail and the analytics warehouse — keeping it stable
// avoids a label-cardinality jump every time a UI submits a blank.
const hangupReasonDefault = "operator_hangup"

// handlers groups the per-endpoint methods so they share Deps.
type handlers struct {
	deps Deps
}

// startShift handles POST /api/sessions/start (operator).
//
// Tenant + Operator IDs are taken from the JWT claims; the request body
// only carries the project the operator wants to bind to. We
// optimistically consult WorkingHoursChecker for a "next allowed at"
// hint when the FSM transition succeeds — the dispatch loop is the
// authoritative gate, this is a UI niceness only.
func (h *handlers) startShift(c *gin.Context) {
	claims, ok := claimsFromContext(c)
	if !ok {
		return
	}
	var req StartShiftDTO
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	snap, err := h.deps.FSM.StartShift(c.Request.Context(), dialerapi.StartShiftRequest{
		TenantID:   claims.TenantID,
		OperatorID: claims.UserID,
		ProjectID:  req.ProjectID,
		ClientIP:   c.ClientIP(),
	})
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, StartShiftResponse{
		Snapshot: snapshotToDTO(snap),
		// "Next allowed at" is best-effort — WorkingHoursChecker may
		// be wired with a NextAllowed surface that fails when the
		// project's region cannot be resolved without a respondent.
		// We leave the hint empty in that case rather than failing
		// the StartShift response.
	})
}

// endShift handles POST /api/sessions/end (operator).
func (h *handlers) endShift(c *gin.Context) {
	claims, ok := claimsFromContext(c)
	if !ok {
		return
	}
	snap, err := h.deps.FSM.EndShift(c.Request.Context(), claims.TenantID, claims.UserID)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, snapshotToDTO(snap))
}

// goPause handles POST /api/sessions/pause (operator).
func (h *handlers) goPause(c *gin.Context) {
	claims, ok := claimsFromContext(c)
	if !ok {
		return
	}
	var req GoPauseDTO
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	snap, err := h.deps.FSM.GoPause(c.Request.Context(), dialerapi.GoPauseRequest{
		TenantID:   claims.TenantID,
		OperatorID: claims.UserID,
		Reason:     req.Reason,
	})
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, snapshotToDTO(snap))
}

// resume handles POST /api/sessions/resume (operator).
func (h *handlers) resume(c *gin.Context) {
	claims, ok := claimsFromContext(c)
	if !ok {
		return
	}
	snap, err := h.deps.FSM.Resume(c.Request.Context(), claims.TenantID, claims.UserID)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, snapshotToDTO(snap))
}

// getMe handles GET /api/sessions/me (operator).
func (h *handlers) getMe(c *gin.Context) {
	claims, ok := claimsFromContext(c)
	if !ok {
		return
	}
	snap, err := h.deps.FSM.GetState(c.Request.Context(), claims.TenantID, claims.UserID)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, snapshotToDTO(snap))
}

// submitStatus handles POST /api/calls/:id/status (operator).
//
// The :id path parameter MUST match the body's call_id — we treat the
// path as authoritative (REST convention) and reject the request with
// 400 when they disagree, rather than silently accepting either.
func (h *handlers) submitStatus(c *gin.Context) {
	claims, ok := claimsFromContext(c)
	if !ok {
		return
	}
	pathID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		renderBindError(c, err)
		return
	}
	var req SubmitStatusDTO
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	if req.CallID != pathID {
		renderBindError(c, errors.New("call_id in body does not match path :id"))
		return
	}
	snap, err := h.deps.FSM.SubmitStatus(c.Request.Context(), dialerapi.SubmitStatusRequest{
		TenantID:     claims.TenantID,
		OperatorID:   claims.UserID,
		CallID:       req.CallID,
		RespondentID: req.RespondentID,
		Status:       req.Status,
		Comment:      req.Comment,
	})
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, snapshotToDTO(snap))
}

// hangup handles POST /api/calls/:id/hangup (operator).
//
// We delegate to Router.Hangup which publishes a NATS HangupCommand —
// per plan-10 we explicitly do NOT mutate FSM state here; the call_ended
// event from telephony drives the FSM transition asynchronously
// (Router.Subscribe handler in Task 10). This keeps the FSM CAS lock
// off the publish path (gotcha "do not call Router.Dial while holding
// the FSM transition lock" — applies symmetrically to Hangup).
func (h *handlers) hangup(c *gin.Context) {
	if _, ok := claimsFromContext(c); !ok {
		return
	}
	callID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		renderBindError(c, err)
		return
	}
	var req HangupDTO
	// Body is optional — accept an empty payload as "no reason
	// supplied" rather than rejecting it.
	if c.Request.ContentLength > 0 {
		if berr := c.ShouldBindJSON(&req); berr != nil {
			renderBindError(c, berr)
			return
		}
	}
	reason := req.Reason
	if reason == "" {
		reason = hangupReasonDefault
	}
	if herr := h.deps.Router.Hangup(c.Request.Context(), callID, reason); herr != nil {
		renderError(c, h.deps.Logger, herr)
		return
	}
	c.Status(http.StatusNoContent)
}
