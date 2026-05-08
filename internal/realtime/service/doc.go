// Package service implements the realtime module's runtime: the per-WS
// connection lifecycle and (in upcoming Plan 11 tasks) the Hub fan-out,
// per-topic RBAC, and listen-in service.
//
// # Connection lifecycle (Plan 11 Task 2)
//
// One *Connection is constructed for every accepted WebSocket upgrade.
// The lifecycle is split across a deterministic goroutine triple owned
// by the Connection itself:
//
//	+-------------------+      +--------------+
//	|     reader        |--->  |              |
//	+-------------------+      |   onFrame    |
//	                           |  (Hub call)  |
//	+-------------------+      |              |
//	|      writer       |<-----+--------------+
//	+-------------------+      ^
//	   ^                       |
//	   |                       sendChan
//	+-------------------+      |
//	|     pinger        |------+
//	+-------------------+
//
// The writer is the SOLE owner of conn.WriteFrame. Hub-side callers
// MUST go through Connection.Send, which is non-blocking: a full
// sendChan triggers a drop-oldest replacement strategy because slow
// consumers care about the latest state more than a stale event
// (operator state, queue depth, etc.). Each drop ticks the
// realtime_dropped_frames_total{conn_id} counter.
//
// # Auth handshake
//
// The first inbound frame after a successful upgrade MUST be of kind
// FrameAuth carrying a valid access token. AuthHandshake validates the
// token via the AuthValidator (production: auth.Authenticator), stores
// Claims, and replies with FrameAuthOK. Failure paths reply with
// FrameAuthError and close the underlying WSConn with a 4401 (custom
// "Unauthorized") code so the operator UI can distinguish a permanent
// auth failure from a transient network drop. AuthHandshake is bounded
// by ConnectionConfig.AuthTimeout (5s default) — a slow client that
// fails to send the auth frame is dropped without spawning the
// reader/writer/pinger goroutines.
//
// # Token refresh
//
// Long-lived sessions (> JWT TTL) use FrameRefresh: the client sends
// the new access token mid-session, the reader re-validates via
// AuthValidator, atomic-swaps Claims, and replies with FrameRefreshOK.
// Existing subscriptions stay attached; subsequent RBAC checks use the
// refreshed Claims. Validation failure closes 4401.
//
// # Ping / pong cadence
//
// Following Plan 10's dialer transport (the established repo-wide
// baseline): 30s ping period, 60s pong-grace. The pinger goroutine
// enqueues a FramePing every PingPeriod via the sendChan; the writer
// flushes it. The reader records every inbound FramePong into
// lastPongAt (atomic.Pointer[time.Time]). When the pinger observes
// lastPongAt drifting past PongTimeout it signals the close channel
// with CloseRateLimited / "no pong" — the conn is presumed dead and
// the lifecycle unwinds gracefully.
//
// # Close semantics
//
// Close(reason) is idempotent — sync.Once gates the actual WSConn
// close + close-channel signal so a double-Close from competing
// goroutines (writer error + Hub-initiated DisconnectByUser racing) is
// safe. All goroutines exit on the close-channel signal; Run blocks
// until every goroutine has returned. goleak verifies the package
// leak-free in TestMain.
//
// # Plan 09/10 carry-forward
//
//   - time.NewTicker (NOT time.After) in goroutine loops.
//   - *zap.Logger typed; no PII in logs (token / phone / SIP creds).
//   - var _ api.Connection = (*Connection)(nil) compile-time check.
//   - Per-package RegisterMetrics(reg) — never init()-time MustRegister.
//   - Sync.WaitGroup.Go (Go 1.25+).
//   - goleak.VerifyTestMain — every test must clean up its connections.
//
// # Out of scope for Task 2 (lands in Tasks 3-7)
//
//   - Subscribe / Unsubscribe — the Hub holds the canonical subscription
//     map; *Connection.Subscribe is a thin facade returning
//     ErrConnectionClosed until Task 3 wires the onFrame Hub callback.
//   - NATS-driven Broadcast — Task 4.
//   - Listen-in lifecycle — Task 6.
package service
