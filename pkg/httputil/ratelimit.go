package httputil

import (
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimitConfig parameterises RateLimitMiddleware. The current
// strategy is a token bucket per (tenant_id, route, key) tuple with
// the bucket persisted in Redis (so multiple replicas share state).
type RateLimitConfig struct {
	// RequestsPerMinute is the steady-state allowance.
	RequestsPerMinute int
	// Burst is the maximum number of requests allowed back-to-back.
	Burst int
	// Window is the bucket refill interval.
	Window time.Duration
	// KeyFunc derives the rate-limit key from the request context.
	// Common implementations: per-IP, per-user, per-tenant.
	KeyFunc func(c *gin.Context) string
}

// RateLimitMiddleware returns a gin middleware that rejects requests
// over the configured budget with HTTP 429 and the standard
// rate_limited envelope.
func RateLimitMiddleware(cfg RateLimitConfig) gin.HandlerFunc {
	panic("not implemented: see Plan 02 Task 3")
}
