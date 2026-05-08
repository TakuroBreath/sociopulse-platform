// Package pool owns the ESL connection fleet to the configured FreeSWITCH
// nodes. It is the only consumer of internal/telephony/esl.Client (Plan 09
// Task 2) and the only producer of healthy-node selection signals consumed by
// internal/telephony/router (Plan 09 Task 5) and the readyz handler in
// cmd/telephony-bridge.
//
// Plan 09 Task 4 owns this file: each configured FS node gets one supervisor
// goroutine that owns its *esl.Client lifecycle (Dial → Subscribe → initial
// SofiaStatus health-gate → periodic SofiaStatus probe + event forwarding)
// and reconnects with jittered exponential backoff on any failure. Per-call
// originate dispatch (Plan 09 Task 5) reads the current healthy client via
// (*ESLPool).Get; the readyz handler in cmd/telephony-bridge reads
// AnyHealthy(); router, nats_bridge consume the fan-out events stream from
// (*ESLPool).Events.
//
// Concurrency model:
//
//   - One supervisor goroutine per node; supervisors are the SOLE writer to
//     their *nodeState (single-writer rule). Read paths (Get / AnyHealthy /
//     HealthyNodes) take RLock.
//   - Supervisors NEVER close p.events — Close() is the sole writer of that
//     channel's close state, mirroring the same single-writer discipline
//     readLoop applies to esl.Client.events.
//   - Close() cancels the parent ctx and waits on every supervisor before
//     returning; goleak verifies no stragglers.
package pool

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/telephony/api"
	"github.com/sociopulse/platform/internal/telephony/esl"
)

// DefaultSubscriptions is the canonical event-list the bridge wants from
// every FS node. Declared at package scope so tests can override cleanly
// (passing a shorter list to Config.Subscriptions) without touching the
// defaults() seam. References doc gotcha #7: subscribing to ALL is a
// floods-the-bridge anti-pattern; this set is the curated minimum the
// downstream router/nats_bridge consume.
var DefaultSubscriptions = []string{
	"CHANNEL_CREATE",
	"CHANNEL_ANSWER",
	"CHANNEL_HANGUP_COMPLETE",
	"CHANNEL_BRIDGE",
	"CHANNEL_UNBRIDGE",
	"DTMF",
	"RECORD_STOP",
	"CUSTOM sofia::register",
	"CUSTOM mod_callcenter::*",
}

// Default tuning constants. Exposed indirectly via Config; declared as
// named constants so the supervisor body reads cleanly and the values are
// findable by `grep`.
const (
	defaultConnectTimeout = 10 * time.Second
	defaultHealthInterval = 5 * time.Second
	defaultBackoffBase    = 500 * time.Millisecond
	defaultBackoffCap     = 30 * time.Second
	defaultEventBuffer    = 4096

	// healthProbeTimeout caps every SofiaStatus probe (initial gate +
	// periodic). The supervisor's parent ctx has no deadline by design,
	// so an unbounded probe would hang indefinitely on a stalled FS.
	healthProbeTimeout = 3 * time.Second
)

// DialFunc is the seam tests use to substitute esl.Dial with a fake-wired
// alternative. Production callers leave Config.DialFunc nil; the supervisor
// falls back to esl.Dial. Tests construct a fakeESLServer and inject a
// closure that points Dial at the fake's bound address.
type DialFunc func(ctx context.Context, cfg esl.Config) (*esl.Client, error)

// Config configures the pool. Defaults are filled by New; pass zero values
// for any field whose default suits the caller. Logger and Metrics are
// nil-tolerated — the pool degrades to a no-op logger and metric-less
// operation respectively.
type Config struct {
	// Nodes is the list of FreeSWITCH ESL endpoints (host:port). Must be
	// non-empty; New rejects an empty slice so a misconfigured Helm chart
	// fails loudly at boot rather than at first dial.
	Nodes []string

	// Password is the shared ESL password. Per-node mTLS replaces this in
	// production deployments (Plan 09 Task 7+); the field is kept here so
	// the composition root has somewhere to plumb the dev/test secret.
	Password string

	// ConnectTimeout caps the Dial step of every connect attempt. Zero
	// uses defaultConnectTimeout (10s).
	ConnectTimeout time.Duration

	// HealthInterval is the period between SofiaStatus probes once a node
	// is connected. Zero uses defaultHealthInterval (5s).
	HealthInterval time.Duration

	// BackoffBase is the initial reconnect delay (attempt 0). Zero uses
	// defaultBackoffBase (500ms).
	BackoffBase time.Duration

	// BackoffCap is the max reconnect delay regardless of attempt. Zero
	// uses defaultBackoffCap (30s).
	BackoffCap time.Duration

	// Subscriptions is the event-list issued via SubscribeEvents on every
	// reconnect. Zero (nil or empty) uses DefaultSubscriptions.
	Subscriptions []string

	// Logger is a structured zap logger named for the pool subsystem. The
	// caller is expected to .Named("telephony.pool") before passing it in.
	// Nil-tolerated.
	Logger *zap.Logger

	// Metrics receives gauge / counter / histogram updates. Nil-tolerated.
	Metrics *Metrics

	// EventBuffer is the buffered capacity of the fan-out events channel.
	// Zero uses defaultEventBuffer (4096) — enough to absorb a coordinated
	// 60-call burst across two nodes (60 × 4 events × 2 = 480) without
	// blocking the per-node forwarder.
	EventBuffer int

	// DialFunc overrides the dial step. Leave nil for production; tests
	// inject a closure that points at a fakeESLServer's bound address.
	DialFunc DialFunc
}

// EventEnvelope wraps a parsed channel event with the originating node's
// address so downstream consumers (router, nats_bridge) know which FS node
// produced it. The Raw field carries the full esl.Event so callers needing
// uncommon headers (e.g. variable_uuid, Channel-Call-State) can read them
// without reconstructing the parse.
type EventEnvelope struct {
	// NodeAddr is the host:port of the FS node that produced the event.
	// Always set to the same string the caller passed via Config.Nodes,
	// so downstream code can use it as a stable map key.
	NodeAddr string

	// Event is the api.ChannelEvent surface produced by esl.MapEvent —
	// already shaped for downstream consumption. Zero-valued (Type == "")
	// when MapEvent returned ok=false; supervisor drops those before
	// publishing, so consumers can rely on Type being non-empty.
	Event api.ChannelEvent

	// Raw is the underlying esl.Event. Useful for consumers needing
	// headers MapEvent doesn't translate (e.g. CUSTOM events).
	Raw esl.Event
}

// nodeState is the per-node mutable state owned by exactly one supervisor
// goroutine (the writer) and read by Get / AnyHealthy / HealthyNodes
// (readers via RLock). The single-writer invariant simplifies the locking
// story: the supervisor never has to coordinate with another writer, only
// to gate readers from observing partial transitions.
type nodeState struct {
	addr string

	// mu guards the four mutable fields below. Held write-locked only
	// inside the supervisor's transition points; readers take RLock.
	mu      sync.RWMutex
	client  *esl.Client // nil while disconnected
	healthy bool        // true after first successful health probe
	lastErr error
	backoff esl.Backoff
}

// ESLPool is the typed handle the composition root holds. It owns the
// supervisor goroutine fleet, the per-node state map (keyed by addr), and
// the fan-out events channel.
type ESLPool struct {
	cfg Config

	// nodes is keyed by addr — the caller's Config.Nodes strings. The map
	// is built by New and never mutated after, so concurrent reads from
	// Get/AnyHealthy/HealthyNodes need no locking on the map itself; only
	// the per-state mu is taken.
	nodes map[string]*nodeState

	// cancel cancels the per-supervisor ctx New created from parent;
	// the live ctx is passed by value to runNode so the supervisor's
	// lifetime is explicit at every call site.
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// events is the fan-out channel. Supervisors PUBLISH onto it
	// (non-blocking with drop-on-full); Close() is the SOLE closer.
	events chan EventEnvelope

	// closeOnce guards Close()'s teardown so the second call doesn't
	// re-cancel and re-close (cancel is idempotent but close(events)
	// would panic on the second call).
	closeOnce sync.Once
}

// New constructs an ESLPool from cfg. It validates that at least one node is
// configured, fills defaults, starts one supervisor per node, and returns
// immediately — supervisors are reconnecting in the background even when
// every configured node is down. Callers can rely on AnyHealthy / Get
// from any goroutine right after New returns.
//
// The parent ctx bounds the pool's lifetime: cancelling parent or calling
// Close cancels every supervisor. Close additionally waits for them to
// exit before returning.
func New(parent context.Context, cfg Config) (*ESLPool, error) {
	if len(cfg.Nodes) == 0 {
		return nil, errors.New("telephony/pool: at least one FreeSWITCH node must be configured")
	}
	cfg = applyDefaults(cfg)

	ctx, cancel := context.WithCancel(parent)
	p := &ESLPool{
		cfg:    cfg,
		nodes:  make(map[string]*nodeState, len(cfg.Nodes)),
		cancel: cancel,
		events: make(chan EventEnvelope, cfg.EventBuffer),
	}
	for _, addr := range cfg.Nodes {
		p.nodes[addr] = &nodeState{
			addr: addr,
			backoff: esl.Backoff{
				Base: cfg.BackoffBase,
				Cap:  cfg.BackoffCap,
			},
		}
	}
	for _, addr := range cfg.Nodes {
		p.wg.Add(1)
		go p.runNode(ctx, addr)
	}
	return p, nil
}

// applyDefaults returns a copy of cfg with zero-valued fields filled.
// Implemented as a pure copy rather than mutating in place so the caller's
// Config struct is never modified — handy for tests that reuse a single
// Config across pool instances.
func applyDefaults(cfg Config) Config {
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = defaultConnectTimeout
	}
	if cfg.HealthInterval == 0 {
		cfg.HealthInterval = defaultHealthInterval
	}
	if cfg.BackoffBase == 0 {
		cfg.BackoffBase = defaultBackoffBase
	}
	if cfg.BackoffCap == 0 {
		cfg.BackoffCap = defaultBackoffCap
	}
	if cfg.EventBuffer == 0 {
		cfg.EventBuffer = defaultEventBuffer
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if len(cfg.Subscriptions) == 0 {
		cfg.Subscriptions = slices.Clone(DefaultSubscriptions)
	}
	if cfg.DialFunc == nil {
		cfg.DialFunc = esl.Dial
	}
	return cfg
}

// Get returns the *esl.Client for the named node. Returns ErrNotConnected
// (wrapped via fmt.Errorf %w) when the node is unknown, disconnected, or
// not yet declared healthy. The returned client is owned by the pool;
// callers MUST NOT call (*esl.Client).Close on it — the supervisor drives
// the lifecycle.
//
// Errors compose with errors.Is(err, esl.ErrNotConnected): higher layers
// (router) discriminate "no client right now" from per-command failures
// returned by esl.Client method calls.
func (p *ESLPool) Get(addr string) (*esl.Client, error) {
	st, ok := p.nodes[addr]
	if !ok {
		return nil, fmt.Errorf("pool.Get %s: %w", addr, esl.ErrNotConnected)
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	if st.client == nil || !st.healthy {
		return nil, fmt.Errorf("pool.Get %s: %w", addr, esl.ErrNotConnected)
	}
	return st.client, nil
}

// AnyHealthy reports whether at least one configured node is currently
// healthy. Used by the readyz handler in cmd/telephony-bridge to surface
// the bridge's ability to do useful work.
func (p *ESLPool) AnyHealthy() bool {
	for _, st := range p.nodes {
		st.mu.RLock()
		h := st.healthy
		st.mu.RUnlock()
		if h {
			return true
		}
	}
	return false
}

// HealthyNodes returns the addresses of nodes the pool currently considers
// healthy, sorted lexicographically for deterministic test output and
// stable router behaviour.
func (p *ESLPool) HealthyNodes() []string {
	out := make([]string, 0, len(p.nodes))
	for addr, st := range p.nodes {
		st.mu.RLock()
		h := st.healthy
		st.mu.RUnlock()
		if h {
			out = append(out, addr)
		}
	}
	sort.Strings(out)
	return out
}

// Events returns the fan-out channel of EventEnvelopes from every
// supervised node. The channel is closed by Close — consumers ranging over
// it observe the close as the natural shutdown signal.
//
// Backpressure: the supervisor publishes non-blockingly. A full buffer
// drops the event and increments the (optional) Reconnects metric with
// result="dropped" — consumers MUST drain promptly or accept loss.
func (p *ESLPool) Events() <-chan EventEnvelope { return p.events }

// Close cancels every supervisor's ctx, waits for them to exit, then
// closes the events channel. Idempotent: calling Close more than once is
// a no-op after the first call. Returns nil unconditionally — there is
// no per-supervisor error to surface here (errors are logged in-flight).
func (p *ESLPool) Close() error {
	p.closeOnce.Do(func() {
		p.cancel()
		p.wg.Wait()
		close(p.events)
	})
	return nil
}

// runNode is the per-node supervisor goroutine. It loops connectAndServe
// until ctx is cancelled, marking the node unhealthy and sleeping the
// backoff between failed attempts. The supervisor is the SOLE writer to
// its *nodeState; readers take RLock.
//
// Error handling: connectAndServe returns nil only on ctx cancel. Any
// other path (Dial / Subscribe / health-probe / events-chan-closed)
// loops with backoff. We log at Warn — operators care about the cause,
// but a healthy redundant deployment will recover without paging.
//
// ctx is the pool's parent-derived context (created in New). Passing it
// as a parameter (rather than reading p.ctx) makes the lifetime explicit
// and satisfies contextcheck — the supervisor is the goroutine that
// owns this ctx for its entire lifetime.
func (p *ESLPool) runNode(ctx context.Context, addr string) {
	defer p.wg.Done()
	st := p.nodes[addr]

	for {
		if ctx.Err() != nil {
			return
		}

		err := p.connectAndServe(ctx, st)

		// Mark unhealthy regardless of err (parent-cancel path also
		// leaves us with a stale healthy=true otherwise — Close()
		// observers should never see the pool report healthy).
		st.mu.Lock()
		st.healthy = false
		st.client = nil
		if err != nil {
			st.lastErr = err
		}
		st.mu.Unlock()
		if p.cfg.Metrics != nil {
			p.cfg.Metrics.NodeHealthy.WithLabelValues(addr).Set(0)
		}

		if err != nil && ctx.Err() == nil {
			p.cfg.Logger.Warn("esl node disconnected",
				zap.String("addr", addr),
				zap.Error(err),
			)
			if p.cfg.Metrics != nil {
				p.cfg.Metrics.Reconnects.WithLabelValues(addr, "err").Inc()
			}
		}

		if ctx.Err() != nil {
			return
		}
		if err := st.backoff.Sleep(ctx); err != nil {
			return
		}
	}
}

// connectAndServe runs one connect → subscribe → health-gate → forward
// cycle. Returns nil only when ctx cancels mid-loop; any other path
// returns an error so the outer supervisor loop reconnects.
//
// The Dial step uses ctx (bounded by Config.ConnectTimeout inside
// esl.Dial). SubscribeEvents and the initial SofiaStatus run with the
// same ctx so a Close() during boot tears them down promptly. The
// periodic health probe uses an explicit 3s timeout — inheriting the
// supervisor's parent (which has no deadline by design) would let a
// stalled FS hang the probe forever.
func (p *ESLPool) connectAndServe(ctx context.Context, st *nodeState) error {
	cli, err := p.cfg.DialFunc(ctx, esl.Config{
		Addr:           st.addr,
		Password:       p.cfg.Password,
		ConnectTimeout: p.cfg.ConnectTimeout,
		Logger:         p.cfg.Logger,
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	// Defer Close() so every error path tears the connection down. Close
	// is idempotent — readLoop observing EOF first is a no-op for the
	// second close.
	defer func() { _ = cli.Close() }()

	if err := cli.SubscribeEvents(ctx, p.cfg.Subscriptions); err != nil {
		return fmt.Errorf("subscribe events: %w", err)
	}

	// Initial health-gate: a successful Dial+Subscribe doesn't prove
	// mod_sofia is up. SofiaStatus is the cheapest way to confirm the FS
	// node is actually serving — once it returns +OK we're confident
	// enough to publish a healthy=true transition and reset the backoff.
	if err := p.healthProbe(ctx, cli, st.addr); err != nil {
		return fmt.Errorf("initial sofia status: %w", err)
	}

	st.mu.Lock()
	st.client = cli
	st.healthy = true
	st.lastErr = nil
	st.backoff.Reset()
	st.mu.Unlock()

	if p.cfg.Metrics != nil {
		p.cfg.Metrics.NodeHealthy.WithLabelValues(st.addr).Set(1)
		p.cfg.Metrics.Reconnects.WithLabelValues(st.addr, "ok").Inc()
	}
	p.cfg.Logger.Info("esl node connected", zap.String("addr", st.addr))

	return p.forwardLoop(ctx, cli, st)
}

// forwardLoop multiplexes (ctx, events-chan, health-tick) into the
// outer supervisor loop. It returns:
//
//   - nil iff ctx cancelled (clean shutdown);
//   - an error wrapping the cause otherwise (events chan closed by
//     readLoop EOF, or health-probe failed).
//
// time.NewTicker per references doc gotcha #1: time.After in a select
// inside a loop leaks a timer per iteration. The ticker is Stopped in
// defer so a clean exit doesn't strand it.
func (p *ESLPool) forwardLoop(ctx context.Context, cli *esl.Client, st *nodeState) error {
	tick := time.NewTicker(p.cfg.HealthInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-cli.Events():
			if !ok {
				return errors.New("event channel closed by readLoop")
			}
			p.publishEvent(st.addr, ev)
		case <-tick.C:
			if err := p.healthProbe(ctx, cli, st.addr); err != nil {
				return fmt.Errorf("health probe: %w", err)
			}
		}
	}
}

// publishEvent maps an esl.Event onto an EventEnvelope and pushes it onto
// the fan-out channel non-blockingly. Events that MapEvent returns
// ok=false for (HEARTBEAT, BACKGROUND_JOB, sofia::register, etc.) carry
// no per-call shape and are still published with a zero-valued Event so
// downstream consumers that care about CUSTOM events can inspect Raw.
//
// Drop-on-full mirrors esl.Client.dispatch's policy: blocking the
// supervisor on a slow consumer would let the per-node forwardLoop
// freeze, causing health-probe ticks to pile up and ultimately a false
// disconnect. Lost events are an accepted degradation mode (references
// doc gotcha #9: FS does not buffer events for disconnected clients
// anyway).
func (p *ESLPool) publishEvent(addr string, ev esl.Event) {
	mapped, _ := esl.MapEvent(ev) // zero-valued when mapEventType says no
	env := EventEnvelope{
		NodeAddr: addr,
		Event:    mapped,
		Raw:      ev,
	}

	select {
	case p.events <- env:
		if p.cfg.Metrics != nil {
			p.cfg.Metrics.EventsForwarded.WithLabelValues(addr, ev.Name).Inc()
		}
	default:
		if p.cfg.Metrics != nil {
			p.cfg.Metrics.EventsForwarded.WithLabelValues(addr, "_dropped").Inc()
		}
		p.cfg.Logger.Warn("pool events channel full, dropping",
			zap.String("addr", addr),
			zap.String("event", ev.Name),
		)
	}
}

// healthProbe runs a 3s-bounded SofiaStatus and asserts the response
// contains the words "Profile" or "RUNNING" — both appear in the standard
// `api sofia status` output and confirm mod_sofia is actually serving.
//
// Bounded explicitly because the supervisor's parent ctx has no deadline
// in production; a stalled FS without the explicit timeout would block
// forever in sendCommand's reply wait.
func (p *ESLPool) healthProbe(parent context.Context, cli *esl.Client, addr string) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, healthProbeTimeout)
	defer cancel()

	out, err := cli.SofiaStatus(ctx)

	if p.cfg.Metrics != nil {
		p.cfg.Metrics.HealthCheckDur.WithLabelValues(addr).Observe(time.Since(start).Seconds())
	}

	if err != nil {
		return fmt.Errorf("sofia status: %w", err)
	}

	// FS sofia status output is multi-line; both "Profile" (per-profile
	// rows) and "RUNNING" (the header) appear in any healthy response.
	// We accept either substring — a well-formed reply with neither is
	// surfaced as a probe failure and triggers reconnect.
	if !containsAny(out, "Profile", "RUNNING") {
		return fmt.Errorf("sofia status: unexpected reply %q", truncate(out, 80))
	}
	return nil
}

// containsAny returns true iff s contains any of the needles via
// strings.Contains. Empty needles are skipped — a vacuous match would
// silently pass every probe and hide a misconfiguration. Pulled out so
// healthProbe reads linearly and the substring policy lives in one place
// (a future tighter matcher — e.g. `Profile <name> RUNNING` — slots in
// here without touching the supervisor).
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if n == "" {
			continue
		}
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// truncate returns s clipped to maxBytes, with an ellipsis suffix when
// truncation occurs. Used inside error messages to keep log lines
// bounded — sofia status output can run several KB. Operates on bytes
// rather than runes because the matched substrings are ASCII anyway and
// callers only use this for human-readable error logging.
func truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + "…"
}
