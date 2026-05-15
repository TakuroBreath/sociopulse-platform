// Package service implements internal/billing/api.
//
// CostCalculator is the pure-arithmetic call-to-cost decomposition. It has no
// IO, no DB, no clock and no mutable state — share a single value across
// goroutines without protection.
//
// Money policy (canonical, see docs/references/plan-14-billing.md §4.2):
//   - Every monetary value is int64 minor units (Russian rouble kopecks).
//   - github.com/shopspring/decimal is the only rounding library; float64
//     must NEVER appear on the cost path.
//   - Round once, at the very last step of each line item, to whole minor
//     units via Round(0). shopspring/decimal documents Round as "half away
//     from zero" which is identical to half-up for the non-negative values
//     this calculator produces (perMin >= 0, dur >= 0, bytes >= 0).
//   - TotalMinor is the exact sum of the three line items — an invariant
//     verified by TestCallCost_TotalInvariant_Property.
package service

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
)

// bytesPerGB is one binary gibibyte (1024^3 bytes). We deliberately use the
// binary definition because operators read object-storage dashboards that
// report bytes in powers of 1024 — a recording flagged "1.07 GB" by an S3
// console is exactly 1 GiB here, which matches the storage line item the
// finance UI displays. A future v2 may switch to decimal-GB (1e9) if user
// feedback says so; that is a tariff-schema change, not a calculator change.
const bytesPerGB = int64(1) << 30

// costCalculator is the production CostCalculator implementation. Stateless
// by design: no DB, no clock, no mutex — share freely across goroutines.
type costCalculator struct{}

// NewCostCalculator returns the production cost calculator. The returned
// value is safe to share across goroutines; it carries no state.
func NewCostCalculator() billingapi.CostCalculator { return costCalculator{} }

// Compile-time interface check guards against drift in CostCalculator's
// signature in the api package.
var _ billingapi.CostCalculator = costCalculator{}

// CallCost decomposes a finalised call into telecom, wages, storage, and a
// total in int64 minor units (kopecks).
//
// ctx is accepted to satisfy the CostCalculator contract but is unused: the
// function is pure arithmetic with no IO and no cancellation surface.
//
// Behaviour rules (mirrors the test contract in calculator_test.go):
//   - DurationSec < 0 → error wrapping billingapi.ErrInvalidPeriod (we
//     reuse this sentinel for "invalid measurement period of a call" so
//     callers can branch via errors.Is without a new sentinel).
//   - Unknown or empty TrunkUsed → TelecomMinor = 0 (Tariffs.TrunkCostMinor
//     defensively returns 0 for missing keys; never panics).
//   - WagesMinor > 0 only when Status == "success" (the canonical wage-paid
//     status; refused/no-answer/busy etc. earn nothing).
//   - StorageMinor is a per-call snapshot of (bytes / 1 GiB) *
//     StorageMinorPerGBMo, rounded half-up. The recurring monthly re-charge
//     for retained recordings is out of scope for v1 (see plan-14 §4.4).
func (costCalculator) CallCost(_ context.Context, in billingapi.CallCostInput, t billingapi.Tariffs) (billingapi.CallCostOutput, error) {
	if in.DurationSec < 0 {
		return billingapi.CallCostOutput{}, fmt.Errorf("billing.calculator: negative duration_sec=%d: %w", in.DurationSec, billingapi.ErrInvalidPeriod)
	}

	out := billingapi.CallCostOutput{}

	// Telecom line item: perMin * dur / 60, rounded half-up at the kopeck.
	// Skip the entire decimal pipeline when either factor is zero — both for
	// readability and to keep the zero-duration / unknown-trunk paths hot.
	if in.DurationSec > 0 {
		perMin := t.TrunkCostMinor(in.TrunkUsed)
		if perMin > 0 {
			out.TelecomMinor = decimal.NewFromInt(perMin).
				Mul(decimal.NewFromInt32(in.DurationSec)).
				Div(decimal.NewFromInt(60)).
				Round(0).
				IntPart()
		}
	}

	// Wages line item: paid only when the call landed on the success terminal.
	if in.Status == "success" {
		out.WagesMinor = t.WagePerSurveyMinor
	}

	// Storage line item: per-call snapshot of (bytes / 1 GiB) * monthly rate.
	// Both guards must hold — a zero bytes_size or a zero storage rate produce
	// a zero line item rather than rounding noise.
	if in.StorageBytes > 0 && t.StorageMinorPerGBMo > 0 {
		out.StorageMinor = decimal.NewFromInt(t.StorageMinorPerGBMo).
			Mul(decimal.NewFromInt(in.StorageBytes)).
			Div(decimal.NewFromInt(bytesPerGB)).
			Round(0).
			IntPart()
	}

	out.TotalMinor = out.TelecomMinor + out.WagesMinor + out.StorageMinor
	return out, nil
}
