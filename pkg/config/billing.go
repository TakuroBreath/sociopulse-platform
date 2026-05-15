// Package config — billing block. Plan 14 Step A wires the runtime
// defaults consumed by the billing module's TariffStore:
//
//   - When a tenant has not yet PATCH'd /api/billing/tariffs, the store
//     falls back to BillingConfig.Defaults so the dashboard, cost
//     calculator, and revenue/margin reports keep working.
//   - Subsequent admin tariff updates write per-tenant overrides into
//     tenant_settings; the YAML defaults stay untouched.
//
// Validate delegates to the api-level Tariffs.Validate so a malformed
// `billing.defaults` block surfaces at boot rather than at the first
// HTTP request.
package config

import (
	billingapi "github.com/sociopulse/platform/internal/billing/api"
)

// BillingConfig carries the platform-wide billing tariff defaults.
//
// The shape is intentionally a thin wrapper around the api type so
// adding a new tariff field is a single-place change (Tariffs gets a
// new field; YAML/env binding flows automatically via mapstructure).
type BillingConfig struct {
	Defaults billingapi.Tariffs `mapstructure:"defaults"`
}

// Validate enforces non-negative invariants on the default tariff
// snapshot. Wired into Config.Validate so a wiring error surfaces at
// boot, not at the first finance request.
func (b BillingConfig) Validate() error {
	return b.Defaults.Validate()
}
