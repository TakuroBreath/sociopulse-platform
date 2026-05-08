package http

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/service"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// Force-action enum values. Stringly-typed because the wire shape is
// JSON; the constants are package-private so the only legitimate
// emitter is force_handler.go.
const (
	forceActionPause    = "force-pause"
	forceActionEndShift = "force-end-shift"
)

// forcePayloadDTO is the wire shape of a force-command payload. Sent
// by the handler over Hub.Broadcast on TopicForceCommands; the
// operator UI parses Action to render the appropriate banner.
type forcePayloadDTO struct {
	Action   string    `json:"action"`
	IssuedBy string    `json:"issued_by"`
	IssuedAt time.Time `json:"issued_at"`
}

// forceResponseDTO is the body of a 202 response: the local Hub
// recipient count. Cross-replica fan-out via NATS is the dispatcher's
// job (Plan 11 Task 4); this number is replica-local.
type forceResponseDTO struct {
	Recipients int `json:"recipients"`
}

// errorEnvelope is the JSON shape every 4xx response uses. Mirrors the
// auth + dialer envelopes for wire uniformity.
type errorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// forceHandlerConfig groups the collaborators a *forceHandler needs.
// Hub is the only mandatory field; logger is nil-safe.
type forceHandlerConfig struct {
	hub    *service.Hub
	logger *zap.Logger
}

// forceHandler is the gin handler for the force-action endpoints.
type forceHandler struct {
	cfg forceHandlerConfig
}

// newForceHandler constructs a *forceHandler. A nil hub panics.
func newForceHandler(cfg forceHandlerConfig) *forceHandler {
	if cfg.hub == nil {
		panic("realtime/transport/http: newForceHandler: hub is required")
	}
	if cfg.logger == nil {
		cfg.logger = zap.NewNop()
	}
	return &forceHandler{cfg: cfg}
}

// mount registers the force-action endpoints on the supplied gin
// RouterGroup. The caller is expected to have already attached
// JWTMiddleware so claims are available via authmw.ClaimsFromContext.
//
// Routes:
//
//	POST /operators/:id/force-pause
//	POST /operators/:id/force-end-shift
func (h *forceHandler) mount(group *gin.RouterGroup) {
	group.POST("/operators/:id/force-pause",
		requireAdminOrSupervisor(),
		h.handleAction(forceActionPause),
	)
	group.POST("/operators/:id/force-end-shift",
		requireAdminOrSupervisor(),
		h.handleAction(forceActionEndShift),
	)
}

// handleAction returns a gin handler bound to the supplied action
// label. Parses the :id parameter as a UUID, builds the JSON payload,
// and dispatches via Hub.Broadcast on TopicForceCommands scoped to
// (claims.TenantID, operatorID).
func (h *forceHandler) handleAction(action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := claimsFromContext(c)
		if !ok {
			return
		}

		operatorID, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, errorEnvelope{
				Code:    "realtime.bad_operator_id",
				Message: "operator id must be a UUID",
			})
			return
		}

		payload := forcePayloadDTO{
			Action:   action,
			IssuedBy: claims.UserID.String(),
			IssuedAt: time.Now().UTC(),
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			// json.Marshal on a flat struct cannot fail in practice,
			// but defensive logging surfaces a future regression.
			h.cfg.logger.Error("realtime/force: marshal payload failed",
				zap.String("action", action),
				zap.Error(err))
			c.AbortWithStatusJSON(http.StatusInternalServerError, errorEnvelope{
				Code:    "realtime.internal",
				Message: "internal error",
			})
			return
		}

		recipients := h.cfg.hub.Broadcast(c.Request.Context(),
			rtapi.TopicForceCommands,
			raw,
			rtapi.BroadcastFilter{
				TenantID: claims.TenantID.String(),
				UserID:   operatorID.String(),
			},
		)

		h.cfg.logger.Info("realtime/force: dispatched",
			zap.String("action", action),
			zap.String("tenant_id", claims.TenantID.String()),
			zap.String("issued_by", claims.UserID.String()),
			zap.String("operator_id", operatorID.String()),
			zap.Int("recipients", recipients),
		)

		c.JSON(http.StatusAccepted, forceResponseDTO{Recipients: recipients})
	}
}

// requireAdminOrSupervisor is the role gate for force-action endpoints.
// admin and supervisor are allowed; operator is denied with 403.
//
// The middleware mirrors the dialer's requireRole pattern but is
// inlined here because the realtime transport is a small package and
// re-using dialer's middleware would create a cross-package import we
// don't otherwise need.
func requireAdminOrSupervisor() gin.HandlerFunc {
	allowed := []authapi.Role{authapi.RoleAdmin, authapi.RoleSupervisor}
	return func(c *gin.Context) {
		claims, ok := authmw.ClaimsFromContext(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, errorEnvelope{
				Code:    "auth.token_invalid",
				Message: "authentication required",
			})
			return
		}
		if slices.ContainsFunc(allowed, claims.HasRole) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusForbidden, errorEnvelope{
			Code:    "auth.insufficient_role",
			Message: "admin or supervisor role required",
		})
	}
}

// claimsFromContext is the central read point for the per-request
// authapi.Claims attached by JWTMiddleware. On a missing-claims
// request (defence-in-depth — should not happen when JWTMiddleware
// runs upstream) we abort with 401.
func claimsFromContext(c *gin.Context) (authapi.Claims, bool) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, errorEnvelope{
			Code:    "auth.token_invalid",
			Message: "authentication required",
		})
		return authapi.Claims{}, false
	}
	return claims, true
}
