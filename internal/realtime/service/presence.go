package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// defaultPresenceTTL is the per-user key lifetime in Redis. The Hub
// touches each session at roughly TTL/3 so a momentary stall in the
// touch loop doesn't lapse a healthy connection.
//
// 30s matches the Plan 11 reference implementation; the deployment
// budget allows the dispatcher to detect a missing replica within a
// single TTL window.
const defaultPresenceTTL = 30 * time.Second

// presenceKeyPrefix is the global prefix used by every presence key.
// Combined with the tenant + user it forms a 4-segment key:
//
//	presence:<tenantID>:user:<userID>
const presenceKeyPrefix = "presence"

// presenceScanCount is the COUNT hint passed to SCAN in OnlineUsers.
// Redis interprets this as a soft batch size — the cursor still walks
// the full keyspace. 100 is the value used in the Plan 11 reference.
const presenceScanCount = 100

// ErrPresenceLapsed is returned by Touch when the per-user presence
// key no longer exists (either it expired between OnConnect and Touch
// or no OnConnect ever fired). Hub-level callers use errors.Is to
// detect this case and react — typically by closing the WS so the
// client reconnects with a fresh OnConnect.
//
// Defined locally rather than in api/errors.go because the contract
// is service-internal: external packages shouldn't have to discriminate
// it from generic Redis errors. Plan 11 Task 7 (HTTP handlers)
// re-exports through the rtapi sentinel set if cross-module discrimination
// is needed.
var ErrPresenceLapsed = errors.New("realtime: presence key not found; session lapsed")

// PresenceOption is a functional option accepted by
// NewRedisPresenceTracker. Mirrors the option pattern used by
// Plan 09 dialer / Plan 10 transport.
type PresenceOption func(*presenceOptions)

// presenceOptions is the internal flat-struct backing the option
// functions. Fields are private — callers tweak them through
// WithPresenceTTL / WithPresenceMetrics.
type presenceOptions struct {
	ttl     time.Duration
	metrics *PresenceMetrics
}

// WithPresenceTTL overrides the default 30-second per-user TTL. Tests
// pass shorter durations so miniredis.FastForward can cross the
// boundary cheaply.
//
// A zero or negative value is silently ignored — Redis interprets a
// 0ms TTL as "no expiry", which would defeat the lapse detection in
// Touch. Callers that want a long TTL should pass it explicitly.
func WithPresenceTTL(d time.Duration) PresenceOption {
	return func(o *presenceOptions) {
		if d <= 0 {
			return
		}
		o.ttl = d
	}
}

// WithPresenceMetrics wires a *PresenceMetrics so the tracker can
// emit connect / disconnect / touch counters. Nil is allowed — the
// tracker treats a nil *PresenceMetrics the same as no metrics.
func WithPresenceMetrics(m *PresenceMetrics) PresenceOption {
	return func(o *presenceOptions) {
		o.metrics = m
	}
}

// RedisPresenceTracker is the cross-replica presence map: each
// connected (tenant, user) pair maps to a Redis key with a TTL so a
// silently-dead replica naturally drops its sessions after the TTL
// elapses (Plan 11 Task 10's janitor handles the diff broadcast).
//
// Concurrency: safe for concurrent use — every method goes straight
// through to Redis, which serialises commands per-connection.
type RedisPresenceTracker struct {
	rdb     redis.UniversalClient
	logger  *zap.Logger
	ttl     time.Duration
	metrics *PresenceMetrics
}

// Compile-time assertion that *RedisPresenceTracker satisfies the
// public api.PresenceTracker contract.
var _ rtapi.PresenceTracker = (*RedisPresenceTracker)(nil)

// NewRedisPresenceTracker constructs a *RedisPresenceTracker bound to
// rdb. A nil rdb is a wiring bug and panics — propagating a typed
// error from a constructor that has nothing to do at runtime would
// just defer the failure to the first OnConnect.
//
// A nil logger falls back to zap.NewNop so the package stays usable
// in benchmarks and one-off scripts.
//
// Default options: 30-second TTL, no metrics. Pass WithPresenceTTL
// and WithPresenceMetrics to override.
func NewRedisPresenceTracker(
	rdb redis.UniversalClient,
	logger *zap.Logger,
	opts ...PresenceOption,
) *RedisPresenceTracker {
	if rdb == nil {
		panic("service.NewRedisPresenceTracker: rdb must be non-nil")
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	o := presenceOptions{ttl: defaultPresenceTTL}
	for _, opt := range opts {
		opt(&o)
	}

	return &RedisPresenceTracker{
		rdb:     rdb,
		logger:  logger,
		ttl:     o.ttl,
		metrics: o.metrics,
	}
}

// OnConnect marks (tenantID, userID) as online and stores replicaID
// as the value. Idempotent: a second call overwrites the prior value
// — when a user moves between replicas the map updates accordingly.
//
// Implementation: SET key replicaID PX <ttl_ms>. The atomic SET
// guarantees the TTL is bound to the value in a single round-trip.
func (p *RedisPresenceTracker) OnConnect(ctx context.Context, tenantID, userID, replicaID string) error {
	key := presenceKey(tenantID, userID)
	if err := p.rdb.Set(ctx, key, replicaID, p.ttl).Err(); err != nil {
		return fmt.Errorf("realtime/service: presence: OnConnect: %w", err)
	}
	p.metrics.observePresenceConnect()
	return nil
}

// OnDisconnect deletes the presence entry for (tenantID, userID).
// Idempotent: DEL on a missing key returns 0 with no error. The
// metrics counter increments on every call — we count "disconnect
// events" issued by the Hub, not "keys actually deleted".
func (p *RedisPresenceTracker) OnDisconnect(ctx context.Context, tenantID, userID string) error {
	key := presenceKey(tenantID, userID)
	if err := p.rdb.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("realtime/service: presence: OnDisconnect: %w", err)
	}
	p.metrics.observePresenceDisconnect()
	return nil
}

// Touch refreshes the TTL on the per-user presence key. Returns
// ErrPresenceLapsed (wrapped) if the key has already expired or was
// never created — the caller MUST treat this as "session lost" and
// either re-OnConnect or close the WS.
//
// Important: Touch does NOT re-SET the key. A naive
// "SET key replicaID PX ttl" would happily resurrect a missing key,
// which would mask a session that legitimately lapsed — the Hub
// would never see ErrPresenceLapsed and would happily keep streaming
// frames to a session whose owning replica considers it dead.
//
// Implementation: PEXPIRE key ttl_ms. PEXPIRE returns 1 on success,
// 0 if the key doesn't exist; we map 0 to ErrPresenceLapsed.
//
// Note we use PExpire (millisecond resolution) rather than Expire
// (second resolution) so tests can shrink the TTL below one second
// without go-redis silently rounding it up to 1s.
func (p *RedisPresenceTracker) Touch(ctx context.Context, tenantID, userID string) error {
	key := presenceKey(tenantID, userID)
	ok, err := p.rdb.PExpire(ctx, key, p.ttl).Result()
	if err != nil {
		p.metrics.observePresenceTouch(presenceTouchResultError)
		return fmt.Errorf("realtime/service: presence: Touch: %w", err)
	}
	if !ok {
		p.metrics.observePresenceTouch(presenceTouchResultLapsed)
		return fmt.Errorf("realtime/service: presence: Touch: %w", ErrPresenceLapsed)
	}
	p.metrics.observePresenceTouch(presenceTouchResultOK)
	return nil
}

// IsOnline returns whether (tenantID, userID) has at least one active
// presence key. The check is cross-replica because every replica
// writes to the same Redis namespace; any live session keeps the
// user "online".
func (p *RedisPresenceTracker) IsOnline(ctx context.Context, tenantID, userID string) (bool, error) {
	key := presenceKey(tenantID, userID)
	n, err := p.rdb.Exists(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("realtime/service: presence: IsOnline: %w", err)
	}
	return n > 0, nil
}

// OnlineUsers returns every user ID currently online for the tenant,
// alphabetically sorted (callers iterate this list for delta
// detection).
//
// Implementation: SCAN against the tenant-scoped pattern, then
// strings.SplitN to recover the userID. SCAN is cursor-based so a
// large keyspace doesn't block the Redis worker pool — one COUNT=100
// batch at a time.
//
// Cross-tenant isolation is enforced by the SCAN pattern itself:
// "presence:<tenantID>:user:*" matches only the tenant's keys.
func (p *RedisPresenceTracker) OnlineUsers(ctx context.Context, tenantID string) ([]string, error) {
	pattern := fmt.Sprintf("%s:%s:user:*", presenceKeyPrefix, tenantID)
	var (
		cursor uint64
		users  []string
	)
	for {
		// Bail early on a cancelled ctx — the SCAN goroutine
		// otherwise keeps walking the cursor.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("realtime/service: presence: OnlineUsers: %w", err)
		}

		keys, next, err := p.rdb.Scan(ctx, cursor, pattern, presenceScanCount).Result()
		if err != nil {
			return nil, fmt.Errorf("realtime/service: presence: OnlineUsers: %w", err)
		}
		for _, k := range keys {
			if user, ok := userIDFromKey(k); ok {
				users = append(users, user)
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	slices.Sort(users)
	return users, nil
}

// presenceKey builds the canonical 4-segment key. Centralised so
// every read/write path uses the exact same encoding; OnlineUsers'
// SCAN pattern is the only consumer that doesn't go through here
// (it builds a glob with the same prefix).
func presenceKey(tenantID, userID string) string {
	return fmt.Sprintf("%s:%s:user:%s", presenceKeyPrefix, tenantID, userID)
}

// userIDFromKey recovers the userID from a presence key. Uses
// strings.SplitN with N=4 so a userID containing a literal ":" stays
// intact (the tenantID can't contain one — it's a UUID — but a
// future userID format change should not silently corrupt presence).
//
// Returns ("", false) if the key doesn't match the expected shape; a
// SCAN match should always look like our pattern, but defensive code
// here lets us survive a stray key in the same namespace.
func userIDFromKey(k string) (string, bool) {
	parts := strings.SplitN(k, ":", 4)
	if len(parts) != 4 || parts[0] != presenceKeyPrefix || parts[2] != "user" {
		return "", false
	}
	return parts[3], true
}
