// Package auth — Module registration entry point.
//
// Plan 05 Task 1 fills this in by:
//  1. Building the store (internal/auth/store/).
//  2. Building the service (internal/auth/service/).
//  3. Registering HTTP handlers on d.HTTPRouter.
//  4. Registering NATS subscribers via d.Subscriber.
//  5. Registering services in d.Locator under "auth.<Type>".
//
// Until then, Register is a no-op so cmd/api compiles and starts up
// cleanly.
package auth

import "github.com/sociopulse/platform/internal/modules"

// Module is the top-level registration handle for the auth module.
// Stateless; safe to construct as a zero value.
type Module struct{}

// Name returns the module's unique identifier within the registry.
func (Module) Name() string { return "auth" }

// Register wires the module's components into the composition root.
// Plan 05 Task 1 fills this in.
func (Module) Register(d modules.Deps) error {
	_ = d // unused until Plan 05 Task 1
	return nil
}
