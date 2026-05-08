// Package telephony — Module registration entry point inside cmd/api.
//
// Architectural note (Plan 09): the actual ESL fleet, NATS bridge, router,
// and per-call backpressure all live in cmd/telephony-bridge — a separate
// binary that boots its own composition root from this same module's
// subpackages (pool/, router/, nats_bridge/). cmd/api does NOT host those
// subsystems; it merely needs a placeholder telephony.CommandPublisher in
// the service locator so other modules (Plan 10's dialer in particular) can
// look up the contract at registration time without panicking.
//
// The placeholder returns ErrTelephonyBridgeOffline on every call. This
// deliberately differs from a no-op: a dialer running inside cmd/api whose
// originate silently succeeded would create call records with no real call
// behind them — far worse than failing fast. The dialer is expected to
// either error its caller or queue the command for replay once the bridge
// publisher (Plan 11) wires a real *nats.Conn-backed implementation.
//
// Locator entries this module registers:
//
//	telephony.CommandPublisher — stub returning ErrTelephonyBridgeOffline.
package telephony

import (
	"context"
	"errors"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/modules"
	"github.com/sociopulse/platform/internal/telephony/api"
)

// LocatorCommandPublisher is the locator key under which this module
// registers its CommandPublisher implementation. Dialer (Plan 10) and other
// downstream consumers Lookup this exact key.
const LocatorCommandPublisher = "telephony.CommandPublisher"

// ErrTelephonyBridgeOffline is returned by every method on the placeholder
// CommandPublisher. cmd/api is not a telephony bridge — call cmd/telephony-
// bridge for real ESL traffic. The error is exported so callers can branch
// on it via errors.Is().
var ErrTelephonyBridgeOffline = errors.New(
	"telephony: cmd/api does not host the bridge — run cmd/telephony-bridge for real ESL traffic")

// Module is the top-level registration handle for the telephony module
// inside cmd/api. Stateless; safe to construct as a zero value.
type Module struct{}

// Name returns the module's unique identifier within the registry.
func (Module) Name() string { return "telephony" }

// Register wires the cmd/api-side telephony composition. See the package
// comment for the architectural split between cmd/api and cmd/telephony-bridge.
func (Module) Register(d modules.Deps) error {
	if d.Locator == nil {
		// No locator means cmd/api booted in a degraded mode where no
		// cross-module lookups can happen — there is nothing useful for us
		// to do, but failing here would cascade into other modules. Stay
		// silent.
		return nil
	}

	logger := zap.NewNop()
	if d.Logger != nil {
		logger = d.Logger.Named("telephony")
	}
	logger.Info("registering placeholder CommandPublisher; real bridge runs in cmd/telephony-bridge",
		zap.String("locator_key", LocatorCommandPublisher),
	)

	d.Locator.Register(LocatorCommandPublisher, api.CommandPublisher(stubCommandPublisher{}))
	return nil
}

// stubCommandPublisher is the placeholder api.CommandPublisher cmd/api
// installs in the locator. Every method returns ErrTelephonyBridgeOffline
// so that a caller (e.g. dialer running in cmd/api) fails loudly instead of
// silently dropping a call.
type stubCommandPublisher struct{}

// Originate returns ErrTelephonyBridgeOffline — cmd/api is not the bridge.
func (stubCommandPublisher) Originate(_ context.Context, _ api.OriginateCommand) error {
	return ErrTelephonyBridgeOffline
}

// Hangup returns ErrTelephonyBridgeOffline — cmd/api is not the bridge.
func (stubCommandPublisher) Hangup(_ context.Context, _ api.HangupCommand) error {
	return ErrTelephonyBridgeOffline
}

// Mixmonitor returns ErrTelephonyBridgeOffline — cmd/api is not the bridge.
func (stubCommandPublisher) Mixmonitor(_ context.Context, _ api.MixmonitorCommand) error {
	return ErrTelephonyBridgeOffline
}

// Play returns ErrTelephonyBridgeOffline — cmd/api is not the bridge.
func (stubCommandPublisher) Play(_ context.Context, _ api.PlayCommand) error {
	return ErrTelephonyBridgeOffline
}

// CreateUser returns ErrTelephonyBridgeOffline — cmd/api is not the bridge.
func (stubCommandPublisher) CreateUser(_ context.Context, _ api.CreateUserCommand) error {
	return ErrTelephonyBridgeOffline
}

// DeleteUser returns ErrTelephonyBridgeOffline — cmd/api is not the bridge.
func (stubCommandPublisher) DeleteUser(_ context.Context, _ api.DeleteUserCommand) error {
	return ErrTelephonyBridgeOffline
}

// Compile-time check that stubCommandPublisher satisfies the public
// api.CommandPublisher contract — keeps the placeholder honest as the api
// surface evolves.
var _ api.CommandPublisher = stubCommandPublisher{}
