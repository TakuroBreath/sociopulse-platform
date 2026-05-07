package httputil

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
)

// IdempotencyStore persists the response of an idempotent request so
// a retry with the same key returns the original outcome. Concrete
// implementation (Redis-backed) lands in Plan 02 Task 3.
type IdempotencyStore interface {
	// Get returns the stored response body and status, or (nil, 0,
	// false, nil) if no entry exists for key under tenantID.
	Get(ctx context.Context, tenantID, key string) (body []byte, status int, found bool, err error)
	// Put stores the response under tenantID/key with the supplied TTL.
	Put(ctx context.Context, tenantID, key string, body []byte, status int, ttl time.Duration) error
}

// IdempotencyMiddleware honours the Idempotency-Key header for unsafe
// HTTP verbs. On a cache hit it short-circuits the handler chain and
// replays the stored response; on a miss it captures the response
// before letting it leave the server.
func IdempotencyMiddleware(store IdempotencyStore, ttl time.Duration) gin.HandlerFunc {
	panic("not implemented: see Plan 02 Task 3")
}
