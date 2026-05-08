package http

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// goVerify handles POST /api/operator/verify/start (supervisor).
//
// A supervisor enters the verify mode for the operator pinned in
// claims — this is the supervisor's own operator-id slot. Verify is a
// status → verify FSM transition; per plan-10 it is the rehearsal
// channel a supervisor uses before a Force.
func (h *handlers) goVerify(c *gin.Context) {
	claims, ok := claimsFromContext(c)
	if !ok {
		return
	}
	snap, err := h.deps.FSM.GoVerify(c.Request.Context(), claims.TenantID, claims.UserID)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, snapshotToDTO(snap))
}

// verifyDone handles POST /api/operator/verify/done (supervisor).
func (h *handlers) verifyDone(c *gin.Context) {
	claims, ok := claimsFromContext(c)
	if !ok {
		return
	}
	snap, err := h.deps.FSM.VerifyDone(c.Request.Context(), claims.TenantID, claims.UserID)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, snapshotToDTO(snap))
}

// force handles POST /api/operator/:id/force (admin).
//
// Path :id is the *target* operator (NOT the caller). Tenant comes
// from the admin's claims — defence in depth above RLS. Target state
// and reason are validated before dispatch so an invalid enum surfaces
// as 400 rather than as ErrInvalidTransition (which the FSM would
// otherwise return for an unknown state).
func (h *handlers) force(c *gin.Context) {
	claims, ok := claimsFromContext(c)
	if !ok {
		return
	}
	operatorID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		renderBindError(c, err)
		return
	}
	var req ForceDTO
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	if !req.Target.Valid() {
		renderBindError(c, errors.New("target is not a valid operator state"))
		return
	}
	if !req.Reason.Valid() {
		renderBindError(c, errors.New("reason is not a recognised force reason"))
		return
	}
	// RBAC defence-in-depth: even though requireRole(admin) gates the
	// transport route, an admin should not be able to force-target an
	// operator in a different tenant. We pass claims.TenantID as the
	// authoritative tenant; the FSM additionally validates the loaded
	// hash's stored tenant_id matches and returns ErrTenantMismatch.
	snap, err := h.deps.FSM.Force(c.Request.Context(), claims.TenantID, operatorID, req.Target, req.Reason)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, snapshotToDTO(snap))
}
