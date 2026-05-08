package http

import (
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// listUsersDefaultLimit is the default page size when ?limit is absent.
// The service layer clamps to its own bounds (50/500), but we surface a
// matching default so the wire shape is predictable.
const listUsersDefaultLimit = 50

// Deps captures the collaborators that handlers need. Logger may be
// nil in tests — render paths gate on nil.
type Deps struct {
	Logger    *zap.Logger
	Auth      authapi.Authenticator
	Users     authapi.UserService
	TOTP      authapi.TOTPService
	RBAC      authapi.RBACChecker
	Validator authapi.ClaimsValidator
}

// handlers groups the per-endpoint methods so they share the Deps.
type handlers struct {
	deps Deps
}

// login handles POST /api/auth/login.
func (h *handlers) login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	in := authapi.LoginInput{
		OrgID:     req.OrgID,
		Login:     req.Login,
		Password:  req.Password,
		IP:        clientIP(c),
		UserAgent: c.Request.UserAgent(),
	}
	res, err := h.deps.Auth.Login(c.Request.Context(), in)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, authResultToResponse(res))
}

// loginTOTP handles POST /api/auth/login/totp.
func (h *handlers) loginTOTP(c *gin.Context) {
	var req LoginTOTPRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	in := authapi.LoginTOTPInput{
		PartialToken: req.PartialToken,
		Code:         req.Code,
		IP:           clientIP(c),
		UserAgent:    c.Request.UserAgent(),
	}
	res, err := h.deps.Auth.LoginTOTP(c.Request.Context(), in)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, authResultToResponse(res))
}

// refresh handles POST /api/auth/refresh.
func (h *handlers) refresh(c *gin.Context) {
	var req RefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	res, err := h.deps.Auth.Refresh(c.Request.Context(), req.RefreshToken, clientIP(c))
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, RefreshResponse{
		AccessToken:      res.AccessToken,
		RefreshToken:     res.RefreshToken,
		AccessExpiresAt:  res.AccessExpiresAt,
		RefreshExpiresAt: res.RefreshExpiresAt,
	})
}

// logout handles POST /api/auth/logout. Idempotent — the service
// swallows malformed tokens so we always return 204.
func (h *handlers) logout(c *gin.Context) {
	var req LogoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	if err := h.deps.Auth.Logout(c.Request.Context(), req.RefreshToken); err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// me handles GET /api/auth/me. The middleware has already validated
// the token and put Claims on the context.
func (h *handlers) me(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	user, err := h.deps.Users.Get(c.Request.Context(), claims.UserID)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, userToDTO(user))
}

// changePassword handles POST /api/auth/me/password.
func (h *handlers) changePassword(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	var req ChangePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	if err := h.deps.Users.ChangePassword(c.Request.Context(), claims.UserID, req.OldPassword, req.NewPassword); err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// totpEnroll handles POST /api/auth/me/totp/enroll.
func (h *handlers) totpEnroll(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	en, err := h.deps.TOTP.Enroll(c.Request.Context(), claims.UserID)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, TOTPEnrollResponse{
		Secret:      en.Secret,
		OTPAuthURL:  en.OTPAuthURL,
		BackupCodes: en.BackupCodes,
	})
}

// totpConfirm handles POST /api/auth/me/totp/confirm.
func (h *handlers) totpConfirm(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	var req TOTPConfirmRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	if err := h.deps.TOTP.Confirm(c.Request.Context(), claims.UserID, req.Code); err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// totpDisable handles POST /api/auth/me/totp/disable.
func (h *handlers) totpDisable(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	if err := h.deps.TOTP.Disable(c.Request.Context(), claims.UserID); err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// totpStatus handles GET /api/auth/me/totp/status.
func (h *handlers) totpStatus(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	st, err := h.deps.TOTP.Status(c.Request.Context(), claims.UserID)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, TOTPStatusResponse{
		Enabled:         st.Enabled,
		EnrolledAt:      st.EnrolledAt,
		LastVerifiedAt:  st.LastVerifiedAt,
		BackupRemaining: st.BackupRemaining,
	})
}

// createUser handles POST /api/auth/users (admin only).
func (h *handlers) createUser(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	var req CreateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	in := authapi.CreateUserInput{
		TenantID: claims.TenantID,
		Login:    req.Login,
		FullName: req.FullName,
		Email:    req.Email,
		Roles:    rolesFromStrings(req.Roles),
		ActorID:  claims.UserID,
	}
	user, tempPwd, err := h.deps.Users.Create(c.Request.Context(), in)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusCreated, CreateUserResponse{
		User:         userToDTO(user),
		TempPassword: tempPwd,
	})
}

// listUsers handles GET /api/auth/users (admin only).
func (h *handlers) listUsers(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}

	limit := parseInt32(c.Query("limit"), listUsersDefaultLimit)
	offset := parseInt32(c.Query("offset"), 0)
	includeArchived := strings.EqualFold(c.Query("include_archived"), "true")

	in := authapi.ListUsersInput{
		TenantID:        claims.TenantID,
		IncludeArchived: includeArchived,
		Limit:           limit,
		Offset:          offset,
	}
	users, total, err := h.deps.Users.List(c.Request.Context(), in)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, ListUsersResponse{
		Users: usersToDTO(users),
		Total: total,
	})
}

// getUser handles GET /api/auth/users/:id (admin only).
func (h *handlers) getUser(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		renderBindError(c, err)
		return
	}
	user, err := h.deps.Users.Get(c.Request.Context(), id)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, userToDTO(user))
}

// updateRoles handles PATCH /api/auth/users/:id/roles (admin only).
func (h *handlers) updateRoles(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		renderBindError(c, err)
		return
	}
	var req UpdateRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	user, err := h.deps.Users.UpdateRole(c.Request.Context(), id, rolesFromStrings(req.Roles))
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, userToDTO(user))
}

// archiveUser handles POST /api/auth/users/:id/archive (admin only).
func (h *handlers) archiveUser(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		renderBindError(c, err)
		return
	}
	if err := h.deps.Users.Archive(c.Request.Context(), id); err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// restoreUser handles POST /api/auth/users/:id/restore (admin only).
func (h *handlers) restoreUser(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		renderBindError(c, err)
		return
	}
	if err := h.deps.Users.Restore(c.Request.Context(), id); err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// resetPassword handles POST /api/auth/users/:id/reset_password
// (admin only). Returns the freshly-minted temp password.
func (h *handlers) resetPassword(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		renderBindError(c, err)
		return
	}
	tempPwd, err := h.deps.Users.ResetPassword(c.Request.Context(), id)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, ResetPasswordResponse{TempPassword: tempPwd})
}

// authResultToResponse converts an api.AuthResult into the wire-format
// LoginResponse. The TOTP-required path leaves the refresh token empty
// because the client must complete /login/totp first.
func authResultToResponse(r authapi.AuthResult) LoginResponse {
	resp := LoginResponse{
		AccessToken:      r.AccessToken,
		AccessExpiresAt:  r.AccessExpiresAt,
		RefreshToken:     r.RefreshToken,
		RefreshExpiresAt: r.RefreshExpiresAt,
		TOTPRequired:     r.TOTPRequired,
	}
	if r.User.ID != uuid.Nil {
		resp.User = userToDTO(r.User)
	}
	return resp
}

// requireRole returns a gin middleware that enforces that the
// authenticated user holds at least one of the supplied roles.
//
// The check uses RBACChecker.Check with a synthesised "user.list"
// action — the matrix's per-role allowlist already guards admin-scoped
// actions, so any role permitted to user.list (admin) passes. Plan 06+
// will refine this to per-route Action constants once the matrix gains
// finer admin-only verbs.
func requireRole(checker authapi.RBACChecker, role authapi.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authmw.ClaimsFromContext(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorEnvelope{
				Error: "auth.token_invalid", Message: "authentication required",
			})
			return
		}
		// Fast path: explicit role membership. The matrix layer also
		// rejects unknown roles, but a transport-level guard is cheaper
		// than walking the allowlist for every admin route.
		if claims.HasRole(role) {
			c.Next()
			return
		}
		// Fall through to the matrix in case the claims carry a
		// higher-privilege role we should accept (current matrix has
		// only admin/supervisor/operator so this is a no-op today; it
		// keeps the policy single-sourced).
		if err := checker.Check(c.Request.Context(), claims, authapi.ActionUserList, authapi.ResourceTenantWide("user")); err != nil {
			renderError(c, nil, err)
			return
		}
		c.Next()
	}
}

// clientIP returns the request's source IP as a parsed netip.Addr.
// Falls back to the zero Addr when parsing fails so callers always
// receive a valid value (the rate-limiter handles zero gracefully).
func clientIP(c *gin.Context) netip.Addr {
	raw := c.ClientIP()
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}
	}
	return addr
}

// parseInt32 parses a query string into an int32 with a default
// fallback. Negative or unparseable values fall back to def — the
// service layer clamps the final values within its own bounds.
func parseInt32(raw string, def int32) int32 {
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n < 0 {
		return def
	}
	return int32(n)
}
