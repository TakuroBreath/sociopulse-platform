package router

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	telephonyapi "github.com/sociopulse/platform/internal/telephony/api"
)

// Config bundles the dependencies and settings for a Router. Required
// fields are documented per-field; nil-tolerated fields fall back to
// safe defaults so the constructor stays trivially wireable from tests.
type Config struct {
	// Publisher is the telephony bridge command surface — the
	// destination for Originate / Hangup. Required. In production the
	// composition root looks this up under
	// telephony.LocatorCommandPublisher; today that yields the cmd/api
	// stub that fails loudly with telephony.ErrTelephonyBridgeOffline.
	// Plan 11 will install a real *nats.Conn-backed publisher behind
	// the same locator key.
	Publisher telephonyapi.CommandPublisher

	// Consumer is the telephony bridge event surface — the source of
	// ChannelEvent updates fed to Subscribe handlers. Required. Like
	// Publisher this is locator-resolved; the dialer never imports a
	// concrete implementation.
	Consumer telephonyapi.EventConsumer

	// Logger receives per-method diagnostics. nil → zap.NewNop().
	// Per Plan 09 carry-forward, fields are typed (zap.String /
	// zap.Stringer) and never carry PII (phone, operator name).
	Logger *zap.Logger

	// Clock returns the current time. nil → time.Now. Reserved for a
	// future subscription-renewal scheduler; not consumed today, but
	// kept on Config for symmetry with the FSM / queue packages so
	// composition-root wiring stays uniform across dialer subsystems.
	Clock func() time.Time

	// Metrics is the per-package collector group. nil → no metrics
	// (the Router is fully functional without it).
	Metrics *Metrics
}

// Router is the dialer's NATS-bridge adapter. It implements
// dialer.api.Router by forwarding each operation to the underlying
// telephony.api.CommandPublisher / EventConsumer, with a small
// translator on each side (see codec.go).
//
// Stateless and goroutine-safe: every method dispatches synchronously
// to the underlying telephony interface. Subscribe captures the user's
// handler in a closure passed to telephony's Consumer; the closure is
// invoked on the consumer's delivery goroutine.
type Router struct {
	pub     telephonyapi.CommandPublisher
	con     telephonyapi.EventConsumer
	log     *zap.Logger
	clock   func() time.Time
	metrics *Metrics
}

// Compile-time interface check. Surfaces dialer.api.Router signature
// drift the moment it happens (per Plan 09 lessons #8).
var _ dialerapi.Router = (*Router)(nil)

// New constructs a Router. Returns an error when a required dependency
// is missing; nil-tolerated fields are filled with defaults so callers
// can pass a minimal Config{Publisher: ..., Consumer: ...} for the
// simplest wiring.
func New(cfg Config) (*Router, error) {
	if cfg.Publisher == nil {
		return nil, errors.New("router.New: Publisher is required")
	}
	if cfg.Consumer == nil {
		return nil, errors.New("router.New: Consumer is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Router{
		pub:     cfg.Publisher,
		con:     cfg.Consumer,
		log:     logger,
		clock:   clock,
		metrics: cfg.Metrics,
	}, nil
}

// Dial implements dialer.api.Router.Dial. Translates the request into
// a telephony.api.OriginateCommand and forwards to the underlying
// CommandPublisher. The returned error is whatever the publisher
// surfaces — the dialer caller branches on errors.Is against the
// telephony sentinel set (ErrTelephonyBridgeOffline today; Plan 11
// will add bridge-publisher errors).
//
// On success, dialer_router_dials_total{result="ok"} ticks; on error,
// {result="error"}.
func (r *Router) Dial(ctx context.Context, req dialerapi.DialRequest) error {
	cmd := translateOriginate(req)
	if err := r.pub.Originate(ctx, cmd); err != nil {
		r.metrics.observeDial(resultError)
		// Log at debug — the dialer's caller already logs at warn/error
		// level on its own retry path; double-logging here would inflate
		// log volume on bridge outage. zap typed fields, no PII.
		r.log.Debug("originate failed",
			zap.Stringer("call_id", req.CallID),
			zap.Stringer("tenant_id", req.TenantID),
			zap.Stringer("command_id", cmd.CommandID),
			zap.Error(err),
		)
		return err
	}
	r.metrics.observeDial(resultOK)
	return nil
}

// Hangup implements dialer.api.Router.Hangup. Translates the
// (callID, reason) pair into a telephony.api.HangupCommand and
// forwards to the underlying CommandPublisher. Like Dial, the metric
// tick discriminates on the publisher's error/no-error result.
func (r *Router) Hangup(ctx context.Context, callID uuid.UUID, reason string) error {
	cmd := translateHangup(callID, reason)
	if err := r.pub.Hangup(ctx, cmd); err != nil {
		r.metrics.observeHangup(resultError)
		r.log.Debug("hangup failed",
			zap.Stringer("call_id", callID),
			zap.Stringer("command_id", cmd.CommandID),
			zap.Error(err),
		)
		return err
	}
	r.metrics.observeHangup(resultOK)
	return nil
}

// Subscribe implements dialer.api.Router.Subscribe. Wraps the
// telephony.api.EventConsumer.Subscribe call with a per-event
// translation step.
//
// Translation lives in translateChannelEvent (codec.go). Each incoming
// telephony.ChannelEvent is:
//
//  1. Counted in dialer_router_events_received_total{type=<raw>}.
//  2. Projected into dialer.ChannelEvent. If the projection drops
//     (Unbridge / DTMF / RecordStop, or an unknown future type), the
//     dialer's handler is NOT invoked; the EventsDropped metric ticks
//     and (for unknown) the EventsTranslationErrors counter as well.
//  3. Otherwise the dialer's handler is invoked with the projected
//     event. The handler's error is propagated verbatim to the
//     telephony Consumer so the bridge can NACK and re-deliver.
//
// The returned unsubscribe func is whatever the underlying Consumer
// returned; calling it stops both the translation wrapper and the
// underlying delivery. unsubscribe is safe to call from any goroutine
// at shutdown.
//
// On Subscribe error from the underlying consumer the unsubscribe
// return value is nil (per the api.Router contract — caller does not
// have to nil-check before defer).
func (r *Router) Subscribe(ctx context.Context, tenantID uuid.UUID, h dialerapi.ChannelEventHandler) (func(), error) {
	if h == nil {
		return nil, errors.New("router.Subscribe: handler is required")
	}

	wrapped := func(innerCtx context.Context, evt telephonyapi.ChannelEvent) error {
		// Metric tick on every received event before any drop logic
		// — gives operators visibility into the raw event mix
		// independent of what the dialer chooses to consume.
		r.metrics.observeEventReceived(string(evt.Type))

		projected, ok, known := translateChannelEvent(evt)
		if !ok {
			r.metrics.observeEventDropped(string(evt.Type))
			if !known {
				// Unrecognised telephony enum — this package has not
				// been taught about it. Surface as a separate metric
				// so a sudden non-zero rate alerts ops to a
				// telephony.api enum addition that the dialer Router
				// must learn.
				r.metrics.observeTranslationError()
				r.log.Warn("dropped unrecognised telephony event type",
					zap.Stringer("call_id", evt.CallID),
					zap.Stringer("tenant_id", evt.TenantID),
					zap.String("type", string(evt.Type)),
				)
				return nil
			}
			r.log.Debug("dropped intentional non-projected event",
				zap.Stringer("call_id", evt.CallID),
				zap.String("type", string(evt.Type)),
			)
			return nil
		}

		// Errors from the user handler propagate verbatim — the
		// underlying EventConsumer's contract documents that returning
		// an error causes a NACK + redelivery. Swallowing here would
		// silently break that loop.
		return h(innerCtx, projected)
	}

	unsubscribe, err := r.con.Subscribe(ctx, tenantID, wrapped)
	if err != nil {
		r.log.Debug("subscribe failed",
			zap.Stringer("tenant_id", tenantID),
			zap.Error(err),
		)
		return nil, err
	}
	return unsubscribe, nil
}
