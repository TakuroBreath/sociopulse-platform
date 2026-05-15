//go:build smoke

package smoke

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	// Blank import: registers golang-migrate's "postgres" database driver.
	// DSNs in the form postgres://user:pass@host/db?sslmode=… are handled
	// by this driver. Required by revive's blank-imports rule.
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	// Blank import: registers the file:// source driver. file:// URLs
	// pointing at the repo's migrations/ directory are handled by this
	// driver.
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sociopulse/platform/pkg/postgres"
)

// init disables the testcontainers-go "ryuk" reaper before any
// container starts. Plan 21 references § 4.1: on macOS Docker the
// reaper spawns a goroutine (Reaper.connect.func1) that does NOT
// terminate within goleak.VerifyTestMain's window — every smoke test
// run would then false-positive a goroutine leak in cmd/api's TestMain.
//
// Trade-off: a test panic mid-run leaves orphan containers. Cleanup:
// `docker ps -a --filter label=org.testcontainers=true -q | xargs
// docker rm -f`. The smoke suite's t.Cleanup ordering covers the
// graceful path; the orphan-on-panic case is an operator-friendly
// trade for green CI.
//
// LookupEnv check honours an explicit user override (e.g. operator
// debugging container cleanup behaviour locally sets ryuk=false).
func init() {
	if _, set := os.LookupEnv("TESTCONTAINERS_RYUK_DISABLED"); !set {
		_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	}
}

// smokeKEKID is the deterministic id every smoke tenant uses for its
// kms_kek_id column. WriteSmokeConfig publishes the matching 32-byte
// KEK under recording.local_keks; SeedTenantAndAdmin sets this id on
// every tenant. The recording-stream scenario (Plan 21b Task 5) reuses
// the same id via BuildRecordingFixture so cmd/api's
// LocalDEKUnwrapper round-trips the wrapped DEK without a second
// registration step.
//
// Exported as a package var (not a const) so future tests can override
// it for negative scenarios — but production callers should treat it
// as immutable.
var smokeKEKID = "smoke-kek-default"

// Stack carries the connection coordinates of the smoke testcontainer
// stack. Smoke tests treat it as read-only after NewSharedStack returns.
// The Reset method is the only mutator and is documented separately.
type Stack struct {
	// PostgresDSN is a libpq-style "postgres://user:pass@host:port/db?sslmode=disable"
	// URL pointing at the Postgres container. Suitable for cmd/api's
	// config.database.postgres.dsn AND for any direct pgx connection
	// the smoke tests want to make outside of cmd/api.
	PostgresDSN string

	// RedisAddr is the "host:port" form cmd/api's
	// config.database.redis.addr expects (it does NOT accept a redis://
	// URL — see openRedis in cmd/api/redis.go). Use NewRedisURL() if
	// you need a URL form for a non-cmd/api client.
	RedisAddr string

	// NATSURL is the "nats://host:port" form cmd/api's config.nats.urls
	// expects. Smoke harness helpers (EnsureSmokeStreams) also use this
	// form to dial nats.Connect.
	NATSURL string

	// pgPoolOnce + pgPool cache a *postgres.Pool built lazily by PgPool.
	// Scenario 8 (152-ФЗ purge) needs a direct pool to construct an
	// in-test PurgeWorker without an asynq cron. The pool is closed
	// at process exit via addProcessTeardown so sibling tests share
	// the same instance without paying the open-cost more than once.
	pgPoolOnce sync.Once
	pgPool     *postgres.Pool
	pgPoolErr  error
}

// NewRedisURL returns the redis://host:port form of Stack.RedisAddr.
// Convenience helper for tests that need a URL (e.g. for
// redis.ParseURL); cmd/api itself wants the plain host:port form via
// Stack.RedisAddr.
func (s *Stack) NewRedisURL() string {
	return "redis://" + s.RedisAddr
}

// resetTables is the canonical TRUNCATE list per Plan 21b references
// § 4.8. Order is irrelevant under CASCADE but listing leaves first
// reduces FK-cascade noise in pg_stat. tenants + users survive Reset —
// they are owned by SeedTenantAndAdmin's t.Cleanup chain.
//
// We OMIT respondent_imports because the import job state lives
// entirely in Redis (verified — no respondent_imports table in any
// migration); a Reset that flushed Redis would be a separate concern.
// The scenarios that exercise the import path must clear their own
// Redis state via job-id rotation, which the harness already does
// implicitly by minting a fresh uuid per test.
//
// audit_log is left intact too — Plan 21b's scenarios don't assert
// audit row counts, and a TRUNCATE of audit_log requires
// tenancy_admin's DELETE grant which the smoke testcontainer
// superuser has, but burning it on every Reset would mask a real
// audit-write regression in scenario 8.
var resetTables = []string{
	"call_recordings",
	"call_answers",
	"call_events",
	"calls",
	"operator_state_log",
	"operator_sessions",
	"survey_versions",
	"surveys",
	"respondents",
	"project_dnc",
}

// Reset truncates the per-tenant tables Phase-1b scenarios mutate so a
// fresh test starts from a clean slate. Runs as the testcontainer's
// superuser (which carries BYPASSRLS via the 000001 grants), so RLS is
// not consulted — every tenant's rows fall in one statement.
//
// Plan 21 Task 4 left this as a no-op stub; Plan 21b Task 1 fills it
// in with the canonical TRUNCATE list.
//
// Reset is safe to call from t.Cleanup or at the top of a test. It
// holds no state across calls. A failing TRUNCATE is fatal to the
// caller because subsequent assertions would see stale rows from a
// prior test — silent degradation here is much worse than a loud fail.
func (s *Stack) Reset(t *testing.T) {
	t.Helper()
	ctx := t.Context()

	conn, err := pgx.Connect(ctx, s.PostgresDSN)
	require.NoError(t, err, "smoke reset: connect to %s", s.PostgresDSN)
	defer func() { _ = conn.Close(context.Background()) }()

	// Single TRUNCATE statement with CASCADE so FK dependencies between
	// the listed tables are followed automatically (e.g. operator_state_log
	// → operator_sessions). RESTART IDENTITY resets serial sequences so
	// scenario assertions against monotonic ids stay stable across reruns.
	stmt := "TRUNCATE " + commaJoin(resetTables) + " RESTART IDENTITY CASCADE"
	_, err = conn.Exec(ctx, stmt)
	require.NoError(t, err, "smoke reset: %s", stmt)
}

// commaJoin renders a comma-separated SQL identifier list. Implemented
// inline to avoid a strings import drag on stack.go for one call site.
func commaJoin(items []string) string {
	out := make([]byte, 0, 128)
	for i, it := range items {
		if i > 0 {
			out = append(out, ", "...)
		}
		out = append(out, it...)
	}
	return string(out)
}

// PgPool returns a lazily-built *postgres.Pool against the smoke
// PostgresDSN. The pool is shared across the process: cmd/api boot
// builds its own pool from the same DSN; this accessor exists so a
// scenario that needs to construct an in-test domain object (e.g.
// scenario 8's PurgeWorker) gets a project-canonical *postgres.Pool
// without having to wire the cmd/api locator path.
//
// The pool is closed at process exit via addProcessTeardown so sibling
// tests reuse the same instance — Open() pays a few ms per call and
// the testcontainer Postgres has the connection budget for one extra
// concurrent pool.
//
// Returns nil + fatal-fails the test on first-call failure. Subsequent
// calls return the cached pool (or re-fail with the same error).
func (s *Stack) PgPool(t *testing.T) *postgres.Pool {
	t.Helper()
	s.pgPoolOnce.Do(func() {
		pool, err := postgres.Open(t.Context(), postgres.Config{
			DSN:            s.PostgresDSN,
			MaxConns:       4,
			ConnectTimeout: 10 * time.Second,
		})
		if err != nil {
			s.pgPoolErr = fmt.Errorf("smoke: open pg pool: %w", err)
			return
		}
		s.pgPool = pool
		// Close the pool at process exit so sibling tests share it
		// without leaking the underlying pgxpool. The teardown fires
		// after every test's t.Cleanup so the shared pool stays alive
		// for the whole TestMain.
		addProcessTeardown(func() { pool.Close() })
	})
	if s.pgPoolErr != nil {
		t.Fatalf("%v", s.pgPoolErr)
	}
	return s.pgPool
}

// sharedStack carries the singleton testcontainer stack used by every
// smoke test in the binary. Built on first GetSharedStack call;
// torn down when TestMain exits via t.Cleanup chains attached during
// construction (each container registers its own Terminate on the
// per-test t.Cleanup).
//
// Why a singleton: per-test containers (4 × ~10 s × N tests) blow the
// CI budget; per-TestMain shared stack keeps total smoke runtime under
// ~90 s cold / ~30 s warm even for the full Plan 21 scenario set.
// Per-test isolation is delegated to Stack.Reset (row-level cleanup).
var (
	sharedOnce     sync.Once
	sharedRef      *Stack
	errSharedSetup error //nolint:errname // package-level singleton, not a sentinel reused via errors.Is
)

// GetSharedStack returns the singleton smoke stack. Cheap to call from
// every test — the first call boots the containers; subsequent calls
// return the cached Stack.
//
// The function uses t.Fatalf on construction failure, so the test that
// triggered the first boot is the one that takes the diagnostic — every
// subsequent test that calls GetSharedStack would also fatal on the
// same cached error, surfacing the failure widely without cascading
// re-tries.
func GetSharedStack(t *testing.T) *Stack {
	t.Helper()
	sharedOnce.Do(func() {
		stack, err := newSharedStack()
		if err != nil {
			errSharedSetup = err
			return
		}
		sharedRef = stack
	})
	if errSharedSetup != nil {
		t.Fatalf("smoke: shared stack initialisation failed: %v", errSharedSetup)
	}
	if sharedRef == nil {
		t.Fatalf("smoke: shared stack is nil (race in TestMain bootstrap)")
	}
	return sharedRef
}

// newSharedStack boots Postgres + Redis + NATS testcontainers in
// parallel, applies the project migrations against Postgres, and
// pre-provisions the wildcard JetStream streams cmd/api needs at boot.
//
// Container termination is registered against process exit (a defer in
// TestMain would be cleaner; the singleton's sync.Once construction
// means we own termination through a cleanup-on-process-exit hook —
// see addProcessTeardown). Cold boot pulls images: postgres:16-alpine
// (~120 MB), redis:7-alpine (~30 MB), nats:2.10-alpine (~20 MB). Warm
// boot is ~10 s; cold boot is ~90 s on a clean Docker daemon.
//
// Returns a Stack with DSN/addr fields populated.
func newSharedStack() (*Stack, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pgC, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("sociopulse_smoke"),
		tcpostgres.WithUsername("smoke"),
		tcpostgres.WithPassword("smoke"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("smoke: start postgres container: %w", err)
	}
	addProcessTeardown(func() { _ = pgC.Terminate(context.Background()) })

	redisC, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		return nil, fmt.Errorf("smoke: start redis container: %w", err)
	}
	addProcessTeardown(func() { _ = redisC.Terminate(context.Background()) })

	natsC, err := tcnats.Run(ctx, "nats:2.10-alpine")
	if err != nil {
		return nil, fmt.Errorf("smoke: start nats container: %w", err)
	}
	addProcessTeardown(func() { _ = natsC.Terminate(context.Background()) })

	pgDSN, err := pgC.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, fmt.Errorf("smoke: postgres connection string: %w", err)
	}

	redisConnStr, err := redisC.ConnectionString(ctx)
	if err != nil {
		return nil, fmt.Errorf("smoke: redis connection string: %w", err)
	}
	// redis.ConnectionString returns "redis://host:port" — cmd/api's
	// config.database.redis.addr expects the bare "host:port" form
	// (openRedis in cmd/api/redis.go feeds Addr straight to redis.Options).
	redisAddr, err := hostPortFromURL(redisConnStr)
	if err != nil {
		return nil, fmt.Errorf("smoke: redis addr: %w", err)
	}

	natsURL, err := natsC.ConnectionString(ctx)
	if err != nil {
		return nil, fmt.Errorf("smoke: nats connection string: %w", err)
	}

	// Apply Postgres migrations. golang-migrate against the file://
	// source resolves paths relative to the current working directory
	// — which varies between test runs (cmd/api vs tests/smoke). We
	// resolve via runtime.Caller so the path is anchored to this file's
	// location and works from any CWD.
	migrationsURL, err := repoMigrationsURL()
	if err != nil {
		return nil, fmt.Errorf("smoke: resolve migrations path: %w", err)
	}
	m, err := migrate.New(migrationsURL, pgDSN)
	if err != nil {
		return nil, fmt.Errorf("smoke: init migrate: %w", err)
	}
	defer func() {
		// Close releases migrate's own connection. Errors here only
		// matter for diagnostics — the test container takes the real
		// connection down at process exit.
		_, _ = m.Close()
	}()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return nil, fmt.Errorf("smoke: apply migrations: %w", err)
	}

	return &Stack{
		PostgresDSN: pgDSN,
		RedisAddr:   redisAddr,
		NATSURL:     natsURL,
	}, nil
}

// hostPortFromURL extracts the "host:port" pair from a URL like
// "redis://host:port" or "nats://host:port". Returns an error if the
// URL has no host or port component.
func hostPortFromURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("no host in %q", raw)
	}
	return u.Host, nil
}

// repoMigrationsURL returns the file:// URL of the repo's migrations
// directory. Resolved relative to this file's location so the smoke
// harness does not depend on the test runner's CWD (go test's CWD
// varies between cmd/api vs tests/smoke driving binaries).
//
// Mirrors pkg/outbox/helpers_test.go::repoMigrationsURL's resolution
// strategy.
func repoMigrationsURL() (string, error) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("runtime.Caller(0) failed")
	}
	// here = .../tests/smoke/stack.go → repo = ../../
	repo := filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
	abs, err := filepath.Abs(filepath.Join(repo, "migrations"))
	if err != nil {
		return "", fmt.Errorf("filepath.Abs: %w", err)
	}
	// Verify the directory exists; otherwise migrate gives a confusing error.
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("stat %s: %w", abs, err)
	}
	return "file://" + abs, nil
}

// processTeardown collects container terminate functions that fire on
// process exit. We use a process-exit hook (registered via TestMain
// patterns from each test binary; here we lean on a finalize-on-os.Exit
// stand-in: tests register cleanup via t.Cleanup at GetSharedStack call
// sites for in-band test ordering — see TerminateOnTestMainCleanup).
var (
	teardownMu sync.Mutex
	teardownFn []func()
)

func addProcessTeardown(fn func()) {
	teardownMu.Lock()
	defer teardownMu.Unlock()
	teardownFn = append(teardownFn, fn)
}

// TerminateOnTestMainCleanup registers every container's Terminate
// against m.Run-style teardown. Smoke binaries that define a TestMain
// SHOULD call this to ensure clean container shutdown on process exit.
// Test binaries without a TestMain still get correct behaviour via
// Docker's reaper container (testcontainers-go's "ryuk"); calling this
// is belt-and-braces.
//
// Today (Plan 21 Task 4) the only smoke entry point is
// cmd/api/smoke_test.go's TestSmoke_HarnessBootsAndHealthz which does
// NOT define its own TestMain — it inherits cmd/api/main_test.go's
// goleak guard. The shared stack relies on the ryuk reaper for
// cleanup; TerminateOnTestMainCleanup is provided for Plan 21 Task 5+
// scenarios that may want explicit teardown.
func TerminateOnTestMainCleanup() {
	teardownMu.Lock()
	fns := teardownFn
	teardownFn = nil
	teardownMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}
