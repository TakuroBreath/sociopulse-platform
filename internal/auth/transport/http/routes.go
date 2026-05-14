package http

import (
	"context"
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
	tenantmw "github.com/sociopulse/platform/pkg/middleware/tenant"
)

// Mount registers every /api/auth route on the supplied gin
// RouterGroup. The caller passes the parent (e.g. the /api group);
// Mount creates the /auth child internally so the wire shape is owned
// by this package.
//
// Three sub-groups:
//
//	/auth                — public (login/refresh/logout)
//	/auth + JWT          — authenticated self-service (/me/*)
//	/auth + JWT + admin  — admin-only user CRUD (/users/*)
//
// Mount panics if any required Deps field is nil so a misconfigured
// composition root fails loudly during cmd/api boot rather than at
// first request.
func Mount(group *gin.RouterGroup, deps Deps) {
	mustNotBeNil(deps)
	h := &handlers{deps: deps}

	auth := group.Group("/auth")
	auth.POST("/login", h.login)
	auth.POST("/login/totp", h.loginTOTP)
	auth.POST("/refresh", h.refresh)
	auth.POST("/logout", h.logout)

	authed := auth.Group("")
	authed.Use(authmw.JWTMiddleware(deps.Validator))
	authed.GET("/me", h.me)
	authed.POST("/me/password", h.changePassword)
	authed.POST("/me/totp/enroll", h.totpEnroll)
	authed.POST("/me/totp/confirm", h.totpConfirm)
	authed.POST("/me/totp/disable", h.totpDisable)
	authed.GET("/me/totp/status", h.totpStatus)

	admin := authed.Group("/users")
	admin.Use(requireRole(deps.RBAC, authapi.RoleAdmin))
	admin.POST("", h.createUser)
	admin.GET("", h.listUsers)

	// Plan 13.2.5 Task 1 — cross-tenant guard on every :id route.
	// The middleware verifies claims.TenantID owns the user id BEFORE
	// the handler runs (404 on mismatch — existence-probe defence).
	// Service methods still take callerTenantID as an explicit param
	// so RLS rejects cross-tenant rows even if this chain is broken.
	sameTenant := tenantmw.RequireSameTenant(userTenantResolver(deps.Users))
	admin.GET("/:id", sameTenant, h.getUser)
	admin.PATCH("/:id/roles", sameTenant, h.updateRoles)
	admin.POST("/:id/archive", sameTenant, h.archiveUser)
	admin.POST("/:id/restore", sameTenant, h.restoreUser)
	admin.POST("/:id/reset_password", sameTenant, h.resetPassword)
}

// userTenantResolver wraps UserService.ResolveTenant for the
// tenant.RequireSameTenant middleware. ErrUserNotFound is translated to
// the middleware's sentinel so the response is a 404 with no body.
func userTenantResolver(users authapi.UserService) tenantmw.ResolveTenantFn {
	return func(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
		t, err := users.ResolveTenant(ctx, id)
		if err != nil {
			if errors.Is(err, authapi.ErrUserNotFound) {
				return uuid.Nil, tenantmw.ErrNotFound
			}
			return uuid.Nil, err
		}
		return t, nil
	}
}

// mustNotBeNil verifies every required collaborator. We panic so a
// composition-root misconfiguration fails loudly during cmd/api boot.
func mustNotBeNil(d Deps) {
	switch {
	case d.Auth == nil:
		panic("auth/transport/http: Auth is required")
	case d.Users == nil:
		panic("auth/transport/http: Users is required")
	case d.TOTP == nil:
		panic("auth/transport/http: TOTP is required")
	case d.RBAC == nil:
		panic("auth/transport/http: RBAC is required")
	case d.Validator == nil:
		panic("auth/transport/http: Validator is required")
	}
}
