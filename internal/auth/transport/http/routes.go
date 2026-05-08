package http

import (
	"github.com/gin-gonic/gin"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
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
	admin.GET("/:id", h.getUser)
	admin.PATCH("/:id/roles", h.updateRoles)
	admin.POST("/:id/archive", h.archiveUser)
	admin.POST("/:id/restore", h.restoreUser)
	admin.POST("/:id/reset_password", h.resetPassword)
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
