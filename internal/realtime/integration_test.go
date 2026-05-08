//go:build integration

// Package realtime_test (integration build tag) holds the end-to-end
// integration test for the realtime module — Plan 11 Task 9.
//
// Build discipline:
//
//   - Default `go test ./...` does NOT compile this file (no `integration`
//     tag, no run). Keeps CI fast and the unit-test surface deterministic.
//   - `go test -tags=integration ./internal/realtime/...` includes this
//     file alongside the existing unit tests in module_test.go (same
//     `realtime_test` package, so the goleak TestMain in module_test.go
//     applies here too).
//
// Scope:
//
//   - One scenario: NATS publish → events.NATSSubscriber → Hub.Broadcast →
//     Connection.Send → coder/websocket WriteFrame → real WS client.
//   - All real components: embedded NATS JetStream, real
//     pkg/eventbus.NATSPublisher / NATSSubscriber, real *service.Hub,
//     real events.NATSSubscriber, real coder/websocket.Dial, real
//     httptest.NewServer.
//   - The only test seam is fakeAuth — a minimal authapi.Authenticator
//     that accepts a single hardcoded token. RBAC, RBAC matrix,
//     metrics, and presence remain real.
package realtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	rtapi "github.com/sociopulse/platform/internal/realtime/api"
	"github.com/sociopulse/platform/internal/realtime/events"  //nolint:depguard // integration test wires the real dispatcher
	"github.com/sociopulse/platform/internal/realtime/service" //nolint:depguard // integration test wires the real Hub
	rthttp "github.com/sociopulse/platform/internal/realtime/transport/http"
	"github.com/sociopulse/platform/pkg/eventbus"
)

// The integration test deliberately reaches into the realtime module's
// internal packages (events, service, transport/http) to wire the real
// Hub + dispatcher + HTTP transport without mocks — that's the whole
// point of an end-to-end test. The depguard rule above is meant to
// forbid cross-module imports from production code; an integration test
// inside the same module is the legitimate exception, hence the
// per-import nolint pragmas.

// wireSubprotocol mirrors the constant baked into
// internal/realtime/transport/http/ws_handler.go. We can't import the
// unexported constant; pinning it here keeps the integration test
// self-contained and surfaces a wire-protocol drift as a compile-time
// (literal mismatch) failure during a future bump.
const wireSubprotocol = "sociopulse-v1"

// TestRealtime_E2E_NATSToWebSocket exercises the full production
// pipeline end-to-end with no mocks beyond the auth seam:
//
//	NATS publish (pkg/eventbus.NATSPublisher)
//	  → NATS JetStream broker (embedded in-process)
//	  → events.NATSSubscriber receives via pkg/eventbus.NATSSubscriber
//	  → events.NATSSubscriber.dispatch tokenises subject + calls
//	    Hub.Broadcast
//	  → service.Hub iterates byTenant + bySubConn, calls Connection.Send
//	  → service.Connection writer goroutine WriteFrames the JSON
//	  → coder/websocket client Read returns the frame
//
// The scenario:
//  1. Boot embedded JetStream + provision a "TENANT" stream covering
//     "tenant.>" subjects.
//  2. Spin up real Publisher + Subscriber (pkg/eventbus).
//  3. Build the real Hub + RBAC + per-conn metrics.
//  4. Build the real events.NATSSubscriber dispatcher; Start it.
//  5. Mount the realtime HTTP transport via rthttp.Mount on a gin engine
//     served by httptest.NewServer.
//  6. Dial /api/realtime/ws with coder/websocket; drive the auth +
//     subscribe handshake.
//  7. Publish a tenant.<t>.telephony.event.<call>.bridged message.
//  8. Assert the WS client receives a FrameEvent on TopicCallEvents
//     with the published payload + the allocated SubID.
//  9. Tear down in reverse order under t.Cleanup so the goleak guard
//     in module_test.go's TestMain doesn't trip on a stray goroutine.
func TestRealtime_E2E_NATSToWebSocket(t *testing.T) {
	t.Parallel()

	logger := zaptest.NewLogger(t)

	// --- Step 1: embedded JetStream + stream provisioning -----------

	natsURL := startEmbeddedJetStream(t)
	ensureTenantStream(t, natsURL)

	// --- Step 2: real eventbus Publisher + Subscriber ---------------

	// We build the eventbus collaborators with their own ctx so a test
	// failure mid-flight doesn't cause the underlying nats.go background
	// goroutines to outlive the t.Cleanup chain.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)

	pub, err := eventbus.NewNATSPublisher(ctx, []string{natsURL}, "",
		eventbus.WithPublisherLogger(logger),
		eventbus.WithPublisherName("integration-test-pub"),
	)
	require.NoError(t, err, "build NATSPublisher")
	t.Cleanup(func() {
		if err := pub.Close(); err != nil {
			t.Logf("publisher Close: %v", err)
		}
	})

	sub, err := eventbus.NewNATSSubscriber(ctx, []string{natsURL}, "",
		eventbus.WithSubscriberLogger(logger),
		eventbus.WithSubscriberName("integration-test-sub"),
		// Tight NAK delay keeps the test deterministic — the canonical
		// happy path never NAKs (handler returns nil), but if a future
		// regression introduces an error, we'd see redelivery within
		// 10ms instead of the 250ms default.
		eventbus.WithSubscriberNakDelay(10*time.Millisecond),
	)
	require.NoError(t, err, "build NATSSubscriber")
	t.Cleanup(func() {
		if err := sub.Close(); err != nil {
			t.Logf("subscriber Close: %v", err)
		}
	})

	// --- Step 3: real Hub + RBAC + metrics --------------------------

	reg := prometheus.NewRegistry()
	hub := service.NewHub(logger, service.RegisterHubMetrics(reg), service.NewTopicRBAC())
	t.Cleanup(hub.Shutdown)

	connMetrics := service.RegisterMetrics(reg)
	dispatcherMetrics := events.RegisterMetrics(reg)

	// --- Step 4: real events.NATSSubscriber dispatcher --------------

	dispatcher := events.NewNATSSubscriber(sub, hub, logger, dispatcherMetrics,
		events.WithReplicaID("integration-replica"),
	)
	require.NoError(t, dispatcher.Start(ctx), "dispatcher Start")
	t.Cleanup(func() {
		if err := dispatcher.Stop(); err != nil {
			t.Logf("dispatcher Stop: %v", err)
		}
	})

	// --- Step 5: HTTP transport mount via rthttp.Mount --------------

	// Tenant + user IDs are uuid.UUID at the auth layer; the realtime
	// adapter strings them onto rtapi.Claims via authAdapter.Validate.
	tenantUUID := uuid.New()
	userUUID := uuid.New()
	tenantID := tenantUUID.String()

	authAuth := &fakeAuth{
		token: "valid",
		claims: authapi.Claims{
			UserID:    userUUID,
			TenantID:  tenantUUID,
			Login:     "integration-tester",
			Roles:     []authapi.Role{authapi.RoleAdmin},
			SessionID: uuid.NewString(),
			JTI:       uuid.NewString(),
			IssuedAt:  time.Now(),
			ExpiresAt: time.Now().Add(time.Hour),
		},
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := router.Group("/api/realtime")
	rthttp.Mount(api, rthttp.Deps{
		Hub:             hub,
		AuthValidator:   rthttp.NewAuthValidator(authAuth),
		ClaimsValidator: authAuth, // satisfies ClaimsValidator (same shape)
		ConnMetrics:     connMetrics,
		Logger:          logger,
		AllowedOrigins:  []string{"*"}, // accept the httptest localhost origin
		ConnConfig: service.ConnectionConfig{
			// Loose rate limit and snappy timers so the assertion finishes
			// inside the 3s require.Eventually window without flaking.
			AuthTimeout:     2 * time.Second,
			PingPeriod:      500 * time.Millisecond,
			PongTimeout:     2 * time.Second,
			WriteTimeout:    time.Second,
			WriteBufferSize: 16,
			RateLimitPerSec: 100,
			Logger:          logger,
		},
	})

	httpServer := httptest.NewServer(router)
	t.Cleanup(httpServer.Close)

	wsURL := "ws://" + strings.TrimPrefix(httpServer.URL, "http://") + "/api/realtime/ws"

	// --- Step 6: real coder/websocket Dial + auth + subscribe -------

	wsConn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{wireSubprotocol},
	})
	require.NoError(t, err, "websocket.Dial")
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = wsConn.CloseNow() })

	// Auth handshake: send FrameAuth, expect FrameAuthOK.
	require.NoError(t, writeFrame(ctx, wsConn, rtapi.Frame{
		Type:  rtapi.FrameAuth,
		Token: "valid",
	}), "send FrameAuth")

	authResp, err := readFrame(ctx, wsConn)
	require.NoError(t, err, "read FrameAuthOK")
	require.Equal(t, rtapi.FrameAuthOK, authResp.Type, "expected auth.ok, got %q (reason=%q)",
		string(authResp.Type), authResp.Reason)

	// Subscribe to call.events with CallID filter (TopicCallEvents
	// requires CallID by RBAC; admin role is permitted).
	const callID = "call-42"
	require.NoError(t, writeFrame(ctx, wsConn, rtapi.Frame{
		Type:  rtapi.FrameSubscribe,
		Topic: rtapi.TopicCallEvents,
		Filter: &rtapi.SubscriptionFilter{
			CallID: callID,
		},
	}), "send FrameSubscribe")

	subResp, err := readFrame(ctx, wsConn)
	require.NoError(t, err, "read FrameSubscribeOK")
	require.Equal(t, rtapi.FrameSubscribeOK, subResp.Type,
		"expected subscribe.ok, got %q (reason=%q)", string(subResp.Type), subResp.Reason)
	require.Equal(t, rtapi.TopicCallEvents, subResp.Topic)
	require.NotEmpty(t, subResp.SubID, "subscribe.ok must carry the allocated sub_id")

	// Wait for the Hub to record the subscription before publishing —
	// otherwise a fast publish could outrun the Hub's bookkeeping
	// (subscribeForConn runs on the conn reader goroutine).
	require.Eventually(t, func() bool {
		return hub.Stats().BySubscription[rtapi.TopicCallEvents] >= 1
	}, 3*time.Second, 10*time.Millisecond, "hub did not record subscription")

	// --- Step 7: publish a real NATS message ------------------------

	subject := fmt.Sprintf("tenant.%s.telephony.event.%s.bridged", tenantID, callID)
	payload := []byte(fmt.Sprintf(`{"call":%q,"event":"bridged"}`, callID))
	require.NoError(t, pub.Publish(ctx, subject, payload), "publish to NATS")

	// --- Step 8: read the FrameEvent on the WS client ---------------

	// require.Eventually with a 3s window absorbs the JetStream
	// ack→delivery hop without baking in a fixed sleep. Each iteration
	// performs a bounded read so a missing frame fails fast rather than
	// hanging the whole test.
	var got rtapi.Frame
	require.Eventually(t, func() bool {
		readCtx, readCancel := context.WithTimeout(ctx, 250*time.Millisecond)
		defer readCancel()
		f, err := readFrame(readCtx, wsConn)
		if err != nil {
			return false
		}
		// Skip server-initiated pings emitted by the per-conn pinger
		// (PingPeriod=500ms set above; with 3s budget we may see one
		// or two before the event arrives).
		if f.Type == rtapi.FramePing {
			return false
		}
		got = f
		return true
	}, 3*time.Second, 20*time.Millisecond, "did not receive FrameEvent within 3s")

	require.Equal(t, rtapi.FrameEvent, got.Type, "expected event frame, got %q", string(got.Type))
	require.Equal(t, rtapi.TopicCallEvents, got.Topic)
	require.Equal(t, subResp.SubID, got.SubID, "event must carry the originating sub_id")

	// Assert the payload round-tripped intact. Compare structurally so
	// formatting drift in the JSON encoder doesn't cause a flake.
	var gotPayload map[string]any
	require.NoError(t, json.Unmarshal(got.Payload, &gotPayload), "unmarshal payload")
	require.Equal(t, callID, gotPayload["call"])
	require.Equal(t, "bridged", gotPayload["event"])

	// --- Step 9: clean shutdown -------------------------------------

	// Close the WS client first so the server-side reader goroutine
	// exits cleanly (StatusNormalClosure -> conn.Run unwinds, the Hub's
	// onClose callback drops the conn, the Touch loop is a no-op
	// because we wired no PresenceTracker).
	require.NoError(t, wsConn.Close(websocket.StatusNormalClosure, "test done"))

	// The remaining teardown (httpServer.Close, dispatcher.Stop,
	// sub.Close, pub.Close, NATS server shutdown) runs through
	// t.Cleanup in reverse-registration order, which is the order the
	// task spec requires.
}

// --- Helpers ----------------------------------------------------------

// startEmbeddedJetStream boots an in-process NATS server with JetStream
// enabled on a random TCP port. Mirrors pkg/eventbus/helpers_test.go's
// helper of the same name; the helper there is package-private so we
// duplicate it here rather than exporting it.
//
// The store directory lives under t.TempDir() so each test gets an
// isolated stream namespace and cleanup is automatic. Returns the
// client URL (nats://host:port) and registers t.Cleanup for graceful
// shutdown.
func startEmbeddedJetStream(t *testing.T) string {
	t.Helper()

	storeDir := filepath.Join(t.TempDir(), "jetstream")

	opts := &natsserver.Options{
		Host:                  "127.0.0.1",
		Port:                  -1, // random port
		NoLog:                 true,
		NoSigs:                true,
		MaxControlLine:        4096,
		DisableShortFirstPing: true,
		JetStream:             true,
		StoreDir:              storeDir,
	}

	srv, err := natsserver.NewServer(opts)
	require.NoError(t, err, "construct embedded NATS server")

	go srv.Start()

	if !srv.ReadyForConnections(5 * time.Second) {
		srv.Shutdown()
		srv.WaitForShutdown()
		t.Fatal("embedded NATS server did not become ready in 5s")
	}

	t.Cleanup(func() {
		srv.Shutdown()
		srv.WaitForShutdown()
	})

	return srv.ClientURL()
}

// ensureTenantStream provisions the JetStream stream covering every
// tenant.> subject. Mirrors ensureStream in pkg/eventbus/helpers_test.go
// but with the integration-test-specific subject pattern baked in.
//
// The stream uses InterestPolicy retention so messages are dropped once
// the dispatcher's consumer ack'd them — keeps the embedded JetStream
// store size bounded inside the test process.
func ensureTenantStream(t *testing.T, url string) {
	t.Helper()

	nc, err := nats.Connect(url)
	require.NoError(t, err, "connect to NATS for stream provisioning")
	defer nc.Close()

	js, err := nc.JetStream()
	require.NoError(t, err, "obtain JetStream context")

	cfg := &nats.StreamConfig{
		Name:      "TENANT",
		Subjects:  []string{"tenant.>"},
		Retention: nats.InterestPolicy,
		Storage:   nats.MemoryStorage,
		MaxAge:    1 * time.Minute,
	}
	if _, err := js.AddStream(cfg); err != nil {
		// Tolerate a recycled store dir (shouldn't happen with
		// t.TempDir but a safety net is cheap).
		if errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
			_, err = js.UpdateStream(cfg)
		}
		require.NoError(t, err, "ensure TENANT stream")
	}
}

// writeFrame JSON-marshals frame and writes it as a text WebSocket
// message. Mirrors the shape used by the real WS handler tests.
func writeFrame(ctx context.Context, conn *websocket.Conn, frame rtapi.Frame) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	return nil
}

// readFrame reads one text WebSocket message and JSON-unmarshals it into
// rtapi.Frame. ctx cancellation aborts the read.
func readFrame(ctx context.Context, conn *websocket.Conn) (rtapi.Frame, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return rtapi.Frame{}, fmt.Errorf("read frame: %w", err)
	}
	var frame rtapi.Frame
	if err := json.Unmarshal(data, &frame); err != nil {
		return rtapi.Frame{}, fmt.Errorf("unmarshal frame: %w", err)
	}
	return frame, nil
}

// fakeAuth is the test-seam authenticator. It accepts a single
// hardcoded token and returns canned authapi.Claims.
//
// fakeAuth satisfies BOTH authapi.Authenticator (consumed by
// rthttp.NewAuthValidator) and authapi.ClaimsValidator (consumed by the
// JWTMiddleware on the force-action / listen-in routes). The
// integration test only exercises the WS path, but rthttp.Mount panics
// if either field is nil — so we wire the same struct as both.
type fakeAuth struct {
	token  string
	claims authapi.Claims
}

// Compile-time guarantees the fake satisfies the auth-module surfaces
// the integration test needs.
var (
	_ authapi.Authenticator   = (*fakeAuth)(nil)
	_ authapi.ClaimsValidator = (*fakeAuth)(nil)
)

// ValidateAccessToken is the only method the realtime authAdapter
// invokes. Returns the canned claims for a matching token; otherwise an
// invalid-token error.
func (a *fakeAuth) ValidateAccessToken(_ context.Context, token string) (authapi.Claims, error) {
	if token != a.token {
		return authapi.Claims{}, errors.New("integration: invalid token")
	}
	return a.claims, nil
}

// Validate satisfies authapi.ClaimsValidator. Same backing claims as
// ValidateAccessToken — the integration test never drives the JWT
// middleware (the WS route bypasses it), but rthttp.Mount requires a
// non-nil ClaimsValidator so we plug ourselves in defensively.
func (a *fakeAuth) Validate(_ context.Context, token string) (authapi.Claims, error) {
	if token != a.token {
		return authapi.Claims{}, errors.New("integration: invalid token")
	}
	return a.claims, nil
}

// Login is unused by the integration test. Returning an error rather
// than panicking keeps a future code path that accidentally invokes it
// from crashing the test binary.
func (a *fakeAuth) Login(_ context.Context, _ authapi.LoginInput) (authapi.AuthResult, error) {
	return authapi.AuthResult{}, errors.New("integration: Login not implemented")
}

// LoginTOTP is unused.
func (a *fakeAuth) LoginTOTP(_ context.Context, _ authapi.LoginTOTPInput) (authapi.AuthResult, error) {
	return authapi.AuthResult{}, errors.New("integration: LoginTOTP not implemented")
}

// Refresh is unused.
func (a *fakeAuth) Refresh(_ context.Context, _ string, _ netip.Addr) (authapi.AuthResult, error) {
	return authapi.AuthResult{}, errors.New("integration: Refresh not implemented")
}

// Logout is unused.
func (a *fakeAuth) Logout(_ context.Context, _ string) error {
	return errors.New("integration: Logout not implemented")
}
