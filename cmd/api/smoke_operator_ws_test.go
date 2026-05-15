//go:build smoke

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/tests/smoke"
)

// TestSmoke_OperatorReadyAndStateBroadcast — Plan 21b Task 4.
//
// End-to-end smoke proving the dialer FSM + realtime WS broadcast pipeline:
//
//	HTTP /api/sessions/{start,pause,end}
//	    ─►  fsm.Machine.{StartShift,GoPause,EndShift}    (Redis Lua CAS)
//	    ─►  dialer.NATSPubSub.Publish                    (JetStream)
//	    ─►  NATSPubSub.handleBusMessage                  (local fan-out)
//	    ─►  transport/http.handlers.websocket            (per-(tenant,operator) Subscribe)
//	    ─►  coder/websocket.Conn → Text frame             (smoke OperatorWS.ReadEvent)
//
// Catches the cross-module wiring failure class — NATS subject pattern
// drift, locator-key drift between the dialer Publisher and the WS
// SnapshotPubSub, or any future refactor that breaks the in-pod fan-out
// path. Sister to TestSmoke_AuthFullFlow (auth pipeline) and
// TestSmoke_AdminCreatesProjectAndImportsRespondents (crm pipeline).
//
// Verified contracts (read from source BEFORE writing):
//
//   - Route mount: /api/operator/ws + /api/sessions/{start,end,pause,resume}
//     on the same gin engine as everything else
//     (internal/dialer/transport/http/routes.go:119-147).
//   - WS auth path: ?token=<jwt> query parameter (ws.go:80). The dialer
//     handler self-authenticates because browsers cannot set an
//     Authorization header on a WebSocket handshake.
//   - WS subprotocol: NONE — the server passes nil Subprotocols in
//     AcceptOptions (ws.go:127-130). Our DialOperator helper passes nil
//     DialOptions to match. A future server-side subprotocol bump must
//     be mirrored on the client.
//   - Initial frame on WS connect: NONE — the dialer's WS handler does
//     NOT send an initial snapshot on Accept. pumpSnapshots only forwards
//     frames Published into SnapshotPubSub after the WS subscribed
//     (ws.go:165-223). The plan's step-4 "read snapshot frame on dial"
//     would block forever; we instead read the first frame AFTER the
//     POST /api/sessions/start publishes one. See "Deviations" below.
//   - Frame wire shape: bare SnapshotDTO. NO {"type":"snapshot",...}
//     envelope — the WS handler json.Marshals SnapshotDTO directly
//     (ws.go:225-239 + dto.go:53-61). Asserted field: `state`.
//   - StartShift request shape: {"project_id":"<uuid>"} — REQUIRED,
//     binding:"required" (dto.go:14-17). The body is the operator's
//     desired project binding; tenant_id + operator_id come from the
//     JWT claims.
//   - StartShift response: 200 + {snapshot: SnapshotDTO, next_allowed_at?,
//     outside_allowed?} (dto.go:66-71 + session_handler.go:51).
//   - POST /api/sessions/pause body: {"reason":"<string>"} — REQUIRED,
//     binding:"required,min=1,max=64" (dto.go:19-21). Without a reason
//     the handler returns 400. The reason value flows to the audit row.
//   - POST /api/sessions/end body: empty (no DTO, the handler reads
//     claims only) — session_handler.go:62-73. Response: 200 + bare
//     SnapshotDTO (snapshotToDTO).
//   - FSM state literals: "offline", "ready", "pause" (NOT "paused")
//     per dialer/api/dto.go:15-21.
//   - StartShift requires a valid project_id FK that exists in projects
//     (migrations/000001_init.up.sql:262 — operator_sessions.project_id
//     references projects(id), NOT NULL). We SeedProject before login.
//
// Deviations from Plan 21b Task 4 (filed in plan amendments at close-out):
//
//  1. Step 4 — "read initial snapshot frame on connect" — REMOVED. The
//     dialer's WS handler does NOT publish an initial snapshot; the WS
//     read would block forever. We read AFTER each POST instead. The
//     plan text said "verify the literal type field by reading the
//     source"; the verification revealed that no such frame exists.
//  2. Step 5 — "if start_shift requires a project + survey assignment,
//     do the assignment via REST first" — REQUIRED + SIMPLER than the
//     plan implied. The FK is on operator_sessions.project_id; no
//     survey-assignment row is consulted by StartShift. We seed the
//     project via direct SQL (SeedProject) — POST /api/projects would
//     work too but adds an admin-login step that the test does not
//     otherwise need.
//  3. Step 7 — "frame type is likely state_change or fsm_event" —
//     NEITHER. The frame is a bare SnapshotDTO. Asserted field: state.
//  4. Step 8 — pause state literal is "pause" (NOT "paused" as plan
//     text suggested).
//
// WS goroutine hygiene: OperatorWS.Close() issues a graceful
// StatusNormalClosure frame which the server's pumpSnapshots observes
// (case <-readerDone) and tears down. goleak in cmd/api/main_test.go's
// TestMain catches any stray goroutine; t.Cleanup chains the WS close
// so a test failure mid-scenario still drains.
func TestSmoke_OperatorReadyAndStateBroadcast(t *testing.T) {
	t.Parallel()

	stack := smoke.GetSharedStack(t)
	httpAddr, _ := bootAPI(t, stack)

	// Seed: tenant + admin (we never use admin's JWT, but SeedOperator
	// requires the tenant to exist already — SeedTenantAndAdmin is the
	// canonical way to get a fresh tenants row). The operator login is
	// the JWT we drive the scenario with.
	admin := smoke.SeedTenantAndAdmin(t, stack, "SMOKE-OP-WS", "op-ws-admin", "AdminPass123!")
	operator := smoke.SeedOperator(t, stack, admin.TenantID, "op-login", "OpPass123!")

	// Seed a project under the SAME tenant so StartShift's FK insert
	// against operator_sessions(project_id) succeeds. Without this we
	// would 500 with a constraint violation; the scenario is about WS
	// broadcast not project-creation, so direct SQL keeps the surface
	// minimal (vs POST /api/projects + admin login).
	projectID := smoke.SeedProject(t, stack, admin.TenantID, "op-ws-proj", "Operator WS smoke project")

	// FSM-side cleanup: StartShift writes a row to operator_sessions
	// (FK→users, FK→projects, neither ON DELETE CASCADE). LIFO t.Cleanup
	// then runs SeedProject.cleanup → DELETE FROM projects → FK fail →
	// SeedOperator.cleanup → DELETE FROM users → FK fail → seed rows
	// leak into the next test invocation under `-count=N`, surfacing as
	// `duplicate key on tenants_org_code_key` on the second run.
	//
	// Register BEFORE the test exercises the FSM so this cleanup runs
	// FIRST in LIFO order (relative to the FSM-touching cleanups; the
	// seed helpers' cleanups were registered EARLIER and therefore run
	// AFTER this one). The FK chain is then unobstructed when the seed
	// cleanups DELETE FROM users / projects / tenants.
	//
	// We use pgx.Connect (NOT stack.PgPool) because cmd/api/main_test.go
	// inherits a goleak.VerifyTestMain that does NOT call
	// smoke.TerminateOnTestMainCleanup — so a lazily-built pgxpool's
	// backgroundHealthCheck goroutine would leak past the inspection
	// window. The Plan 21b Task 6 (smoke purge scenario) is the
	// canonical PgPool consumer; until cmd/api's TestMain wires the
	// teardown, the cheaper per-test pgx.Connect is the right tool.
	t.Cleanup(func() {
		bg := context.Background()
		conn, err := pgx.Connect(bg, stack.PostgresDSN)
		if err != nil {
			// Cleanup must not fail the test (the assertions already ran).
			// A failed cleanup leaves FK-protected rows for the next run
			// to encounter as a duplicate-key; we log via t.Log so the
			// diagnostic is visible without flipping the test result.
			t.Logf("smoke cleanup: connect to %s: %v", stack.PostgresDSN, err)
			return
		}
		defer func() { _ = conn.Close(bg) }()
		// operator_state_log.session_id has ON DELETE CASCADE → deleting
		// from operator_sessions takes both tables down. Filter on
		// tenant_id so parallel smoke tests are not touched.
		if _, err := conn.Exec(bg,
			"DELETE FROM operator_sessions WHERE tenant_id = $1",
			admin.TenantID); err != nil {
			t.Logf("smoke cleanup: delete operator_sessions (tenant=%s): %v", admin.TenantID, err)
		}
	})

	ctx := t.Context()
	cli := &http.Client{Timeout: 10 * time.Second}

	operatorJWT := loginAndAccessToken(ctx, t, cli, httpAddr, operator)

	baseURL := "http://" + httpAddr

	// Step 1: open the WS connection. We dial BEFORE issuing
	// /sessions/start so the per-(tenant, operator) Subscribe slot is
	// registered in the NATSPubSub map by the time the first Publish
	// fires. Subscribe-after-Publish would silently drop the snapshot
	// — pubsub.go:172-179 non-blocking send to a non-existent
	// subscriber list is a benign no-op, exactly the wire-shape
	// regression this scenario guards against.
	ws, err := smoke.DialOperator(ctx, t, httpAddr, operatorJWT)
	require.NoError(t, err, "DialOperator must succeed for operator JWT")
	t.Cleanup(func() { _ = ws.Close() })

	// Step 2: POST /api/sessions/start with the seeded project_id.
	// 200 + {snapshot: {state:"ready", ...}, ...}.
	startBody := fmt.Sprintf(`{"project_id":%q}`, projectID.String())
	startStatus, startBytes := postWithJWT(ctx, t, cli,
		baseURL+"/api/sessions/start", operatorJWT, startBody)
	require.Equalf(t, http.StatusOK, startStatus,
		"POST /api/sessions/start must 200; got %d body=%s",
		startStatus, string(startBytes))

	var startResp struct {
		Snapshot struct {
			State     string     `json:"state"`
			ProjectID *uuid.UUID `json:"project_id"`
		} `json:"snapshot"`
	}
	require.NoError(t, json.Unmarshal(startBytes, &startResp),
		"decode start_shift response: %s", string(startBytes))
	assert.Equal(t, "ready", startResp.Snapshot.State,
		"start_shift HTTP response must contain state=ready")
	require.NotNil(t, startResp.Snapshot.ProjectID,
		"start_shift HTTP response must echo project_id")
	assert.Equal(t, projectID, *startResp.Snapshot.ProjectID)

	// Step 3: read the ready-state snapshot from the WS. The frame is
	// a bare SnapshotDTO; we decode into map[string]any and assert on
	// the `state` field plus the echoed `project_id`. The timeout
	// budget is generous (5 s) — the NATSPubSub JetStream RTT is
	// ~10-100 ms on a warm container; we want a clean diagnostic on a
	// genuine wire-broken regression rather than a flake on a slow
	// scheduler.
	readyFrame := readNextWSStateFrame(ctx, t, ws, 5*time.Second)
	assert.Equalf(t, "ready", readyFrame["state"],
		"WS frame after start_shift must carry state=ready; got %v", readyFrame["state"])
	assertProjectIDEquals(t, readyFrame, projectID,
		"WS ready-state frame must echo the bound project_id")

	// Step 4: POST /api/sessions/pause — body REQUIRES `reason`. The
	// FSM transitions ready→pause, audits, and publishes the new
	// snapshot. The HTTP response is the bare SnapshotDTO.
	pauseBody := `{"reason":"bio_break"}`
	pauseStatus, pauseBytes := postWithJWT(ctx, t, cli,
		baseURL+"/api/sessions/pause", operatorJWT, pauseBody)
	require.Equalf(t, http.StatusOK, pauseStatus,
		"POST /api/sessions/pause must 200; got %d body=%s",
		pauseStatus, string(pauseBytes))
	assertHTTPSnapshotState(t, pauseBytes, "pause")

	pauseFrame := readNextWSStateFrame(ctx, t, ws, 5*time.Second)
	assert.Equalf(t, "pause", pauseFrame["state"],
		"WS frame after /sessions/pause must carry state=pause; got %v",
		pauseFrame["state"])

	// Step 5: POST /api/sessions/end. ResetEnd clears project_id /
	// session_id / pause_reason / etc. The HTTP response is the bare
	// SnapshotDTO with state="offline"; the WS frame mirrors it.
	//
	// Body is empty: session_handler.endShift reads only the JWT claims
	// (no DTO bind). Passing `{}` to satisfy the handler's
	// ContentLength check is fine; an entirely empty body also works
	// because the path does not call ShouldBindJSON.
	endStatus, endBytes := postWithJWT(ctx, t, cli,
		baseURL+"/api/sessions/end", operatorJWT, `{}`)
	require.Equalf(t, http.StatusOK, endStatus,
		"POST /api/sessions/end must 200; got %d body=%s",
		endStatus, string(endBytes))
	assertHTTPSnapshotState(t, endBytes, "offline")

	offlineFrame := readNextWSStateFrame(ctx, t, ws, 5*time.Second)
	assert.Equalf(t, "offline", offlineFrame["state"],
		"WS frame after /sessions/end must carry state=offline; got %v",
		offlineFrame["state"])

	// Step 6: graceful WS close. The deferred t.Cleanup above already
	// runs Close; we call it explicitly here too so a future scenario
	// reader sees the explicit life-cycle contract. Close is idempotent
	// (sync.Once-gated in wsclient.go) so double-call is safe.
	require.NoError(t, ws.Close(), "WS Close must return nil on graceful close")
}

// readNextWSStateFrame reads ONE JSON frame from the WS and returns it
// as a generic map. Each dialer WS frame is a SnapshotDTO; we decode as
// map[string]any so the scenario asserts on `state` + `project_id`
// without binding to the typed DTO (which would re-encode the same
// fields the smoke harness already tests at the wire level).
//
// Extracted out of the scenario body to keep the linear "POST → read →
// assert" rhythm readable. Fatal-fails the test on read failure
// because every step of this scenario expects exactly one frame at the
// documented timeout; a missing frame IS the regression.
func readNextWSStateFrame(ctx context.Context, t *testing.T, ws *smoke.OperatorWS, timeout time.Duration) map[string]any {
	t.Helper()
	frame, err := ws.ReadEvent(ctx, timeout)
	require.NoError(t, err, "WS ReadEvent must succeed within %s", timeout)
	require.NotNil(t, frame, "WS ReadEvent must return a non-nil decoded frame")
	return frame
}

// assertHTTPSnapshotState decodes the bytes as a SnapshotDTO-shaped
// JSON object (subset: state only) and asserts it equals want. Used by
// the pause + end steps where the HTTP response is the bare DTO.
//
// We avoid binding the full SnapshotDTO type (which lives in the dialer
// transport package, off-limits via depguard `module-boundaries`); a
// local anonymous struct with only the `state` field is sufficient for
// the assertion and keeps the import surface minimal.
func assertHTTPSnapshotState(t *testing.T, body []byte, want string) {
	t.Helper()
	var got struct {
		State string `json:"state"`
	}
	require.NoErrorf(t, json.Unmarshal(body, &got),
		"decode SnapshotDTO body: %s", string(body))
	assert.Equalf(t, want, got.State,
		"HTTP SnapshotDTO state mismatch: want=%s got=%s body=%s",
		want, got.State, string(body))
}

// assertProjectIDEquals validates the WS frame's `project_id` field
// matches want. The pointer-or-omitted shape of SnapshotDTO.ProjectID
// serialises as either a string uuid or an absent key — we accept the
// first form here because the ready-state always carries it.
func assertProjectIDEquals(t *testing.T, frame map[string]any, want uuid.UUID, msgAndArgs ...any) {
	t.Helper()
	v, ok := frame["project_id"]
	require.Truef(t, ok, "WS frame missing project_id field: %v; %v", frame, msgAndArgs)
	s, ok := v.(string)
	require.Truef(t, ok, "WS frame project_id is not a string: %v; %v", v, msgAndArgs)
	got, err := uuid.Parse(s)
	require.NoErrorf(t, err, "WS frame project_id is not a valid uuid: %s; %v", s, msgAndArgs)
	assert.Equalf(t, want, got, "WS frame project_id mismatch; %v", msgAndArgs)
}
