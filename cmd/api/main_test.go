package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
)

// TestMain ensures cmd/api boot/shutdown does not leak goroutines.
//
// Tests boot a real *trace.TracerProvider whose OTLP exporter retries
// indefinitely against a missing collector at localhost:4317. On shutdown
// the batchSpanProcessor's drain blocks in the retry's wait() until the
// shutdown context expires, leaving short-lived OTel goroutines around when
// goleak inspects the runtime. We ignore those — they exit on their own
// once the retry's context deadline fires; in production with a reachable
// collector they exit promptly.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc/internal/retry.wait"),
		goleak.IgnoreAnyFunction("go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc.(*client).exportContext.func1"),
		goleak.IgnoreAnyFunction("go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).Shutdown.func1.1"),
	)
}

// TestRunStartsAndShutsDownCleanly drives the composition root through a full
// boot/shutdown cycle and asserts a clean exit. It picks free ports for HTTP
// and metrics listeners by writing a temporary config.yaml so other tests can
// run in parallel.
//
// This test does NOT exercise the outbox relay — the relay is a stub until
// Plan 03 Task 6 wires *pgxpool.Pool. See the integration smoke test deferred
// to Plan 03 (TestOutboxRelayStartsAndDrains).
func TestRunStartsAndShutsDownCleanly(t *testing.T) {
	t.Parallel()

	httpAddr := pickFreeAddr(t)
	metricsAddr := pickFreeAddr(t)
	configDir := writeMinimalDevConfig(t, httpAddr, metricsAddr)

	// The realtime dispatcher (errgroup goroutine wired in run() at
	// main.go:485) Subscribes to "tenant.*.dialer.op.*.state" via the
	// JetStream-backed eventbus and STRICT-fails on a missing stream —
	// returning an error to the errgroup which cancels gctx and trips
	// the shutdown path within milliseconds of boot. The dialer's own
	// Register treats the same missing-stream as a WARN (Plan 11), but
	// realtime's dispatcher start does not. Provision a wildcard stream
	// here so the test environment matches what the nats-bridge would
	// auto-create in prod.
	ensureTestStream(t, "TENANT_TEST", []string{"tenant.>"})
	ensureTestStream(t, "TRUNKS_TEST", []string{"trunks.>"})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, configDir)
	}()

	// Wait for the HTTP listener to come up. If run() exits with an error
	// FIRST (e.g. bind failure, module fatal Register), surface it
	// immediately rather than waiting out the polling deadline with a
	// useless "listener never came up" — diagnosing the actual cause is
	// what matters.
	select {
	case err := <-errCh:
		t.Fatalf("run() returned before listener was ready: %v", err)
	case <-listenerReadyChan(httpAddr, 10*time.Second):
		// listener accepted a TCP connection — boot succeeded
	}

	// Hit /healthz to prove the gin engine is wired and serving.
	healthReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+httpAddr+"/healthz", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(healthReq)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Hit /metrics on the separate listener.
	metricsReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+metricsAddr+"/metrics", nil)
	require.NoError(t, err)
	mresp, err := http.DefaultClient.Do(metricsReq)
	require.NoError(t, err)
	require.NoError(t, mresp.Body.Close())
	assert.Equal(t, http.StatusOK, mresp.StatusCode)

	// Trigger graceful shutdown via context cancellation.
	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err, "run() should exit cleanly on context cancel")
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not exit within 5s of cancel")
	}
}

// TestRunReturnsErrorOnInvalidConfig points run at a non-existent directory and
// asserts the load step surfaces an error rather than panicking.
func TestRunReturnsErrorOnInvalidConfig(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)

	err := run(ctx, "/nonexistent/path/that/should/not/exist")
	require.Error(t, err)
}

// TestBuildProviders_TenancyIsFirstEntry asserts the compile-time wiring of
// the tenancy.Module entry in cmd/api's providers registry, AND that the
// blank-import seam from internal/tenancy/service has fired (api.Register
// non-nil). Together the two assertions cover the FULL regression class
// the wiring guards against: drop the entry → first assertion fails; drop
// the blank-import → second assertion fails.
//
// Plan 21 Task 1 — tenancy publishes locator entries (TenantService,
// KMSResolver, PhoneHasher, Tenancy) that auth/crm/surveys consume in
// later tasks. The runtime locator-entries assertion lives in the smoke
// harness (Plan 21 Task 7) where a real Postgres container backs deps.Pool.
func TestBuildProviders_TenancyIsFirstEntry(t *testing.T) {
	t.Parallel()

	// 1. Blank-import seam: api.Register is populated by an init() in
	//    internal/tenancy/service. Without the blank-import in main.go,
	//    this var would still be nil and tenancy.Module.Register would
	//    no-op at runtime (internal/tenancy/module.go:50-52) — boot
	//    succeeds but ZERO locator entries are published. Asserting
	//    here makes the silent regression noisy.
	require.NotNil(t, tenancyapi.Register,
		"tenancyapi.Register is nil — the blank-import "+
			"`_ \"github.com/sociopulse/platform/internal/tenancy/service\"` "+
			"is missing from cmd/api/main.go (see internal/tenancy/module.go:50-52)")

	// 2. Providers-list shape: tenancy MUST be present AND first.
	//    Reordering breaks every downstream consumer silently because
	//    auth/crm/surveys look up tenancy.* keys at their own Register
	//    time.
	providers := buildProviders(buildProvidersDeps{})
	require.NotEmpty(t, providers.Modules, "providers list is empty")
	assert.Equal(t, "tenancy", providers.Modules[0].Name(),
		"tenancy.Module must be the FIRST entry in providers; "+
			"auth/crm/surveys (Plan 21 Tasks 2+) depend on its locator publish")
}

// TestBuildProviders_ContainsAuthCrmSurveys asserts the compile-time
// wiring of the three Plan 21 Task 2 modules. They sit after tenancy
// (Task 1) in dependency order: auth consumes tenancy.{TenantService,
// KMSResolver}; crm consumes tenancy.{KMSResolver, PhoneHasher} +
// (optional) auth.{RBACChecker, ClaimsValidator}; surveys consumes
// auth.{ClaimsValidator, RBACChecker}.
//
// The runtime locator-entries assertion (e.g. auth.Authenticator
// actually published to the locator) lives in the smoke harness
// (Plan 21 Task 7) where a real Postgres+Redis stack backs the
// auth/crm Register paths.
func TestBuildProviders_ContainsAuthCrmSurveys(t *testing.T) {
	t.Parallel()

	providers := buildProviders(buildProvidersDeps{})

	names := make(map[string]int) // name → index for ordering checks
	for i, mod := range providers.Modules {
		if mod == nil {
			continue
		}
		names[mod.Name()] = i
	}

	require.Contains(t, names, "tenancy", "Task 1 wiring lost")
	require.Contains(t, names, "auth", "auth.Module missing from providers")
	require.Contains(t, names, "crm", "crm.Module missing from providers")
	require.Contains(t, names, "surveys", "surveys.Module missing from providers")

	// Dependency order: tenancy < auth; auth < crm; auth < surveys.
	// (Module.Register is called in slice order; consumers must come
	// after producers so locator.Lookup succeeds at Register time.)
	assert.Less(t, names["tenancy"], names["auth"], "tenancy must precede auth (auth consumes tenancy.*)")
	assert.Less(t, names["auth"], names["crm"], "auth must precede crm (crm consumes auth.RBACChecker)")
	assert.Less(t, names["auth"], names["surveys"], "auth must precede surveys (surveys consumes auth.ClaimsValidator)")
}

// pickFreeAddr asks the kernel for a free TCP port, returns "127.0.0.1:N".
// The listener is closed immediately; race-prone in theory, fine in practice
// for serial test boots a few seconds apart.
func pickFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// ensureTestStream provisions a JetStream stream for the given subjects on
// the test NATS server (nats://localhost:4222 per writeMinimalDevConfig)
// IF a NATS server is reachable.
//
// Why we provision: cmd/api boot's realtime dispatcher and dialer pubsub
// SUBSCRIBE before any module PUBLISHES — the JetStream broker returns
// "no stream matches subject" if the stream doesn't yet exist, and the
// realtime dispatcher's Start treats that as a hard error in the errgroup.
// In production the nats-bridge sidecar auto-creates streams from a config
// inventory; macOS dev-up environments have NATS running but no streams,
// so this helper bridges the gap.
//
// Why skip-on-failure: GitHub Actions CI runs `make test` WITHOUT a NATS
// service. cmd/api's pkg/eventbus.NATSPublisher tolerates a missing server
// at boot (the connection retries lazily), so the boot path proceeds far
// enough for the listener check to succeed even without streams. The
// realtime dispatcher's Start under those conditions returns immediately
// because the underlying subscriber recognises the disconnected client and
// short-circuits. Locally with NATS UP and no streams, the dispatcher
// instead hits the "no stream matches subject" error path — which is what
// THIS helper fixes. Skipping on no-NATS leaves the CI behaviour
// unchanged.
//
// The stream uses InterestPolicy retention + memory storage so no on-disk
// artefacts accumulate; cleanup deletes it.
func ensureTestStream(t *testing.T, name string, subjects []string) {
	t.Helper()

	nc, err := nats.Connect("nats://localhost:4222",
		nats.Timeout(500*time.Millisecond),
		nats.RetryOnFailedConnect(false),
	)
	if err != nil {
		t.Logf("ensureTestStream: NATS unreachable at localhost:4222, skipping stream %q provisioning: %v", name, err)
		return
	}
	t.Cleanup(nc.Close)

	js, err := nc.JetStream()
	require.NoError(t, err)

	cfg := &nats.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Retention: nats.InterestPolicy,
		Storage:   nats.MemoryStorage,
		MaxAge:    1 * time.Minute,
	}
	if _, err := js.AddStream(cfg); err != nil {
		if errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
			_, err = js.UpdateStream(cfg)
		}
		require.NoError(t, err, "ensure stream %q", name)
	}
	t.Cleanup(func() {
		// Best-effort delete — a parallel test holding the same stream
		// name would have skipped re-creation above, so DeleteStream
		// might race. Swallow the error rather than fail cleanup.
		_ = js.DeleteStream(name)
	})
}

// listenerReadyChan polls addr in a background goroutine and closes the
// returned channel as soon as TCP-accept succeeds OR the deadline expires.
// The caller selects against this channel + the run() errCh so a boot
// failure surfaces immediately instead of waiting out the polling budget.
//
// The returned channel is never sent on — it is closed as a one-shot
// signal. A nil close after the deadline means "polling gave up"; the
// caller should still inspect err state to disambiguate.
func listenerReadyChan(addr string, timeout time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		deadline := time.Now().Add(timeout)
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for {
			conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return
			}
			if time.Now().After(deadline) {
				return
			}
			<-tick.C
		}
	}()
	return ch
}

// writeMinimalDevConfig writes a config.yaml that boots cmd/api without any
// real backing services. The OTel endpoint stays at the default localhost:4317
// — the gRPC client is non-blocking (grpc.NewClient), so a missing collector
// only surfaces in batched export retries, not in startup.
func writeMinimalDevConfig(t *testing.T, httpBind, metricsBind string) string {
	t.Helper()
	dir := t.TempDir()
	yaml := `service:
  env: development
  log_level: info
  region: yc-ru-central-1
  name: sociopulse-api-test
http:
  bind: "` + httpBind + `"
  read_timeout: 5s
  write_timeout: 10s
  idle_timeout: 30s
  max_body_size: 1MB
ws:
  bind: ":0"
  ping_interval: 20s
  read_buffer_size: 4096
  write_buffer_size: 4096
  max_message_size: 65536
  handshake_timeout: 10s
grpc:
  bind: ":0"
  reflection_enabled: true
  conn_timeout: 10s
database:
  postgres:
    dsn: postgres://app:devpass@localhost:5432/sociopulse?sslmode=disable
    max_conns: 5
    max_idle_time: 5m
    statement_cache: 100
  redis:
    addr: localhost:6379
    pool_size: 5
    db: 0
nats:
  urls: ["nats://localhost:4222"]
  account: cmd-api
auth:
  jwt:
    issuer: https://app.sociopulse.local
    access_ttl: 15m
    refresh_ttl: 720h
    algorithm: HS256
    # Plan 21 Task 2 — deterministic dev value so auth.Module.Register
    # doesn't WARN-skip on Config.Auth.JWT.Secret == "". Production
    # MUST load this from Yandex Lockbox via secret_lockbox_key
    # (ADR-0001); never use this literal outside dev/test.
    secret: smoke-test-secret-do-not-use-in-prod
observability:
  otel:
    endpoint: localhost:4317
    sampling_ratio: 1.0
    insecure: true
    service_name: sociopulse-api-test
  metrics:
    bind: "` + metricsBind + `"
    namespace: sociopulse_test
  logging:
    redact_patterns: []
    sample_info_logs: 1.0
    sample_debug_logs: 0.05
shutdown:
  grace_period: 2s
outbox:
  batch_size: 50
  tick: 500ms
  max_retry: 5
`
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	return dir
}
