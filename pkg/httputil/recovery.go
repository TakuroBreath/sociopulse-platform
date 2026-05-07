package httputil

import (
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// RecoveryMiddleware catches panics from downstream handlers, logs
// them with the standard field set, and renders the project's
// internal_error envelope. It is attached at the very top of the
// middleware chain so even a panic inside a stale handler leaves a
// well-formed response.
func RecoveryMiddleware(logger *zap.Logger) gin.HandlerFunc {
	panic("not implemented: see Plan 02 Task 3")
}
