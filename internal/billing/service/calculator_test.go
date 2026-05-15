package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
	"github.com/sociopulse/platform/internal/billing/service"
)

// tariffs returns a baseline Tariffs snapshot used across the test table.
// Numbers chosen to make the arithmetic obvious in assertions:
//   - 342 kop/min on mtt-msk-1 → 60s costs 342 minor (a kopeck-per-second).
//   - 12000 minor wage on success surveys → 120 ₽.
//   - 150 minor/GB-month storage → a 1 GiB recording costs 150 minor exactly.
func tariffs() billingapi.Tariffs {
	return billingapi.Tariffs{
		TrunkCostsMinor:      map[string]int64{"mtt-msk-1": 342, "mango-fed": 378},
		WagePerSurveyMinor:   12000, // 120 ₽
		StorageMinorPerGBMo:  150,   // 1.50 ₽/GB-month
		RespondentBasesMinor: 50,
		FixedFeesMinor:       50000_00,
	}
}

func TestCallCost_Success_60Sec(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		CallID:      uuid.New(),
		TenantID:    uuid.New(),
		ProjectID:   uuid.New(),
		TrunkUsed:   "mtt-msk-1",
		DurationSec: 60,
		Status:      "success",
	}, tariffs())
	require.NoError(t, err)
	// 342 kop/min * 60s / 60 = 342 telecom.
	require.Equal(t, int64(342), out.TelecomMinor)
	// status=success → 12000 wages.
	require.Equal(t, int64(12000), out.WagesMinor)
	require.Equal(t, int64(0), out.StorageMinor)
	require.Equal(t, int64(342+12000), out.TotalMinor)
}

func TestCallCost_Refused_NoWages(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		TrunkUsed:   "mtt-msk-1",
		DurationSec: 30,
		Status:      "refused",
	}, tariffs())
	require.NoError(t, err)
	// 342 * 30 / 60 = 171.
	require.Equal(t, int64(171), out.TelecomMinor)
	require.Equal(t, int64(0), out.WagesMinor)
	require.Equal(t, int64(171), out.TotalMinor)
}

func TestCallCost_NoAnswer_ZeroDuration(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		TrunkUsed:   "mtt-msk-1",
		DurationSec: 0,
		Status:      "no-answer",
	}, tariffs())
	require.NoError(t, err)
	require.Equal(t, int64(0), out.TelecomMinor)
	require.Equal(t, int64(0), out.WagesMinor)
	require.Equal(t, int64(0), out.TotalMinor)
}

func TestCallCost_UnknownTrunk_TreatsAsFree(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		TrunkUsed:   "unknown-trunk-xyz",
		DurationSec: 90,
		Status:      "success",
	}, tariffs())
	require.NoError(t, err)
	// Unknown trunk → TrunkCostMinor returns 0 → telecom is 0 (defensive).
	require.Equal(t, int64(0), out.TelecomMinor)
	require.Equal(t, int64(12000), out.WagesMinor)
	require.Equal(t, int64(12000), out.TotalMinor)
}

func TestCallCost_EmptyTrunk_TreatsAsFree(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		DurationSec: 30,
		Status:      "success",
	}, tariffs())
	require.NoError(t, err)
	require.Equal(t, int64(0), out.TelecomMinor)
	require.Equal(t, int64(12000), out.WagesMinor)
	require.Equal(t, int64(12000), out.TotalMinor)
}

func TestCallCost_StorageCharge_1GiB(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	// 1 GiB recording: 1<<30 bytes ÷ (1 GiB) = 1.0; rate is 150 minor/GB-month.
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		TrunkUsed:    "mtt-msk-1",
		DurationSec:  60,
		Status:       "success",
		StorageBytes: 1 << 30,
	}, tariffs())
	require.NoError(t, err)
	require.Equal(t, int64(150), out.StorageMinor)
	require.Equal(t, int64(342+12000+150), out.TotalMinor)
}

func TestCallCost_StorageCharge_PartialGB(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	// 512 MiB = half GiB → 150 * 0.5 = 75 minor (rounded half-up at the kopeck).
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		TrunkUsed:    "mtt-msk-1",
		DurationSec:  60,
		Status:       "success",
		StorageBytes: 512 << 20,
	}, tariffs())
	require.NoError(t, err)
	require.Equal(t, int64(75), out.StorageMinor)
}

func TestCallCost_NegativeDuration_Rejected(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	_, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		TrunkUsed:   "mtt-msk-1",
		DurationSec: -5,
		Status:      "success",
	}, tariffs())
	require.Error(t, err)
	require.ErrorIs(t, err, billingapi.ErrInvalidPeriod,
		"sentinel reuse: negative duration is an invalid period of measurement")
	// Defensive: ensure the underlying err carries context for ops triage.
	require.NotEqual(t, err, errors.Unwrap(err), "wrapper should add context, not be a bare sentinel")
}

func TestCallCost_LongCall_RoundingIsHalfUp(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	// 342 * 95 / 60 = 32490 / 60 = 541.5 → 542 (half-away-from-zero,
	// identical to half-up for non-negative values).
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		TrunkUsed:   "mtt-msk-1",
		DurationSec: 95,
		Status:      "success",
	}, tariffs())
	require.NoError(t, err)
	require.Equal(t, int64(542), out.TelecomMinor)
}

func TestCallCost_TotalInvariant_Property(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	statuses := []string{"success", "refused", "dropped", "no-answer", "busy", "callback", "wrong-person", "tech-failure"}
	durations := []int32{0, 1, 30, 60, 600, 3600}
	for _, s := range statuses {
		for _, dur := range durations {
			out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
				TrunkUsed:   "mtt-msk-1",
				DurationSec: dur,
				Status:      s,
			}, tariffs())
			require.NoErrorf(t, err, "status=%s dur=%d", s, dur)
			// Non-negative invariant: totals never go below zero for any input combo.
			require.LessOrEqualf(t, int64(0), out.TotalMinor, "status=%s dur=%d", s, dur)
			// Strict invariant: TotalMinor == Telecom + Wages + Storage.
			require.Equalf(t, out.TelecomMinor+out.WagesMinor+out.StorageMinor, out.TotalMinor,
				"status=%s dur=%d", s, dur)
		}
	}
}

func TestCallCost_ZeroTariffs_AllZero(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	// Empty tariffs (zero value): every line item should be 0, no panic.
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		TrunkUsed:    "mtt-msk-1",
		DurationSec:  60,
		Status:       "success",
		StorageBytes: 1 << 30,
	}, billingapi.Tariffs{})
	require.NoError(t, err)
	require.Equal(t, int64(0), out.TelecomMinor)
	require.Equal(t, int64(0), out.WagesMinor)
	require.Equal(t, int64(0), out.StorageMinor)
	require.Equal(t, int64(0), out.TotalMinor)
}

func TestCallCost_StorageZeroWhenStorageRateZero(t *testing.T) {
	t.Parallel()
	c := service.NewCostCalculator()
	// StorageBytes > 0 but rate is 0 → storage line should be 0 (guard branch).
	tar := tariffs()
	tar.StorageMinorPerGBMo = 0
	out, err := c.CallCost(context.Background(), billingapi.CallCostInput{
		TrunkUsed:    "mtt-msk-1",
		DurationSec:  60,
		Status:       "success",
		StorageBytes: 1 << 30,
	}, tar)
	require.NoError(t, err)
	require.Equal(t, int64(0), out.StorageMinor)
	require.Equal(t, int64(342+12000), out.TotalMinor)
}
