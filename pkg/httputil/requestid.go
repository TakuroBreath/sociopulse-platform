// Package httputil is the project's gin toolkit: error envelope
// rendering, request-id propagation, idempotency, rate-limiting, and
// recovery. Modules attach the middlewares from here in their
// http/routes.go; the gateway (Plan 02 Task 3) attaches them at the
// top level.
//
// Concrete wiring (sentinel → status mapping table, idempotency-key
// store, token-bucket implementation) lands in Plan 02 Tasks 3-4 and
// the per-module HTTP plans.
package httputil

import "github.com/gin-gonic/gin"

// RequestIDMiddleware reads X-Request-Id, allocates a UUIDv7 when
// absent, stores it on *gin.Context, and echoes it back in the
// response. The same field is mirrored to the zap logger and the OTel
// span by the observability middleware.
//
// This is duplicated with observability.RequestIDMiddleware so HTTP
// handlers that don't depend on observability can still attach the id;
// they share the underlying utility (Plan 02 Task 3).
func RequestIDMiddleware() gin.HandlerFunc {
	panic("not implemented: see Plan 02 Task 3")
}

// RequestIDFromContext returns the X-Request-Id stored on the gin
// context, or the empty string when none was attached.
func RequestIDFromContext(c *gin.Context) string {
	panic("not implemented: see Plan 02 Task 3")
}
