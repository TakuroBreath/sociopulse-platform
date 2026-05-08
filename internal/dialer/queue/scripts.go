package queue

import (
	_ "embed"

	"github.com/redis/go-redis/v9"
)

// The Lua sources are embedded at compile time so a single binary carries
// every script. redis.NewScript caches the SHA1 lazily on the first Run,
// after which go-redis prefers EVALSHA — no runtime SCRIPT LOAD round-trip.

//go:embed lua/enqueue.lua
var enqueueLua string

//go:embed lua/pop_next.lua
var popNextLua string

//go:embed lua/requeue.lua
var requeueLua string

//go:embed lua/remove.lua
var removeLua string

// Package-level handles so the SHA1 cache survives across calls. They are
// assigned at init via redis.NewScript (a pure constructor — no Redis
// round-trip happens here, matching the FSM store.go pattern).
var (
	enqueueScript = redis.NewScript(enqueueLua)
	popNextScript = redis.NewScript(popNextLua)
	requeueScript = redis.NewScript(requeueLua)
	removeScript  = redis.NewScript(removeLua)
)
