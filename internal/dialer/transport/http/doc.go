// Package http is the gin HTTP + WebSocket transport for the dialer
// module. It exposes the operator-facing surface of the OperatorFSM —
// shift control, pause / resume, status submission, supervisor verify
// hand-off, admin force escape — and a per-(tenant, operator) WebSocket
// channel that pushes Snapshot updates to a connected operator UI.
//
// # Routes
//
// Mount(group, deps) registers the following endpoints (see routes.go
// for the canonical list and required roles):
//
//	POST   /api/sessions/start              operator     StartShift
//	POST   /api/sessions/end                operator     EndShift
//	POST   /api/sessions/pause              operator     GoPause
//	POST   /api/sessions/resume             operator     Resume
//	GET    /api/sessions/me                 operator     GetState
//	POST   /api/calls/:id/status            operator     SubmitStatus
//	POST   /api/calls/:id/hangup            operator     Router.Hangup
//	GET    /api/operator/ws                 operator     WebSocket: Snapshot push
//	POST   /api/operator/verify/start       supervisor   GoVerify
//	POST   /api/operator/verify/done        supervisor   VerifyDone
//	POST   /api/operator/:id/force          admin        Force(target, reason)
//
// Handlers are intentionally thin — they bind JSON, dispatch to the
// FSM / Router, and render the resulting Snapshot or a mapped error
// envelope. Business logic lives in internal/dialer/fsm and
// internal/dialer/router.
//
// # Authentication
//
// JWT via pkg/middleware/auth.JWTMiddleware on the parent group;
// requireRole(role) gates each route by RBAC role. Tenant + operator
// IDs are taken from the validated Claims, never from the request body
// — defence in depth above Postgres RLS (see plan-10-dialer.md
// "Tenant scoping").
//
// # WebSocket protocol
//
// Clients connect to /api/operator/ws with the access token supplied
// either as a query string parameter (?token=…) or via the
// Sec-WebSocket-Protocol subprotocol "bearer.<token>" — query is
// preferred because gin's URL parameter handling is robust. On a valid
// token the handler subscribes to the Deps.SnapshotPubSub channel for
// (tenantID, operatorID), fans out Snapshots as JSON, and runs a 30s
// server-side Ping with a 60s pong-grace timeout. On client close,
// network error, or pong timeout the handler unsubscribes and returns
// cleanly so goleak verifies no leaked goroutines.
//
// # Carry-over: Plan 09 lessons
//
//   - *zap.Logger typed (never *slog.Logger).
//   - No PII in logs (no phones); tenantID + operatorID OK.
//   - var _ api.X = (*impl)(nil) compile-time interface checks at the
//     top of impl files where applicable.
//   - goleak.VerifyTestMain in this package's test binary.
package http
