package checks

import (
	"context"
	"time"
)

// RedisPinger is the minimal Redis surface readiness needs. The real
// *redis.Client (from github.com/redis/go-redis/v9) satisfies this through a
// thin Ping wrapper that swallows the *redis.StatusCmd return.
type RedisPinger interface {
	Ping(ctx context.Context) error
}

// RedisCheck pings Redis with a 1s deadline.
type RedisCheck struct {
	Client RedisPinger
}

// Name reports the dependency identifier surfaced in /readyz output.
func (RedisCheck) Name() string { return "redis" }

// Check pings the client honouring the deadline already set on ctx.
func (r RedisCheck) Check(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return r.Client.Ping(cctx)
}
