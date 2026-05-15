// Plan 14 Step A — TDD tests for Tariffs.Validate / TrunkCostMinor and the
// Month period helper. These tests are written FIRST (red phase); the
// matching Validate / TrunkCostMinor methods land in dto.go (green phase).
package api_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
)

func TestTariffs_Validate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      billingapi.Tariffs
		wantErr bool
	}{
		{
			name: "valid",
			in: billingapi.Tariffs{
				TrunkCostsMinor:    map[string]int64{"mtt-msk-1": 342},
				WagePerSurveyMinor: 12000,
			},
		},
		{name: "zero value", in: billingapi.Tariffs{}},
		{name: "negative trunk cost", in: billingapi.Tariffs{TrunkCostsMinor: map[string]int64{"x": -1}}, wantErr: true},
		{name: "negative wage", in: billingapi.Tariffs{WagePerSurveyMinor: -1}, wantErr: true},
		{name: "negative bases", in: billingapi.Tariffs{RespondentBasesMinor: -1}, wantErr: true},
		{name: "negative storage", in: billingapi.Tariffs{StorageMinorPerGBMo: -1}, wantErr: true},
		{name: "negative fixed", in: billingapi.Tariffs{FixedFeesMinor: -1}, wantErr: true},
		{name: "empty trunk id", in: billingapi.Tariffs{TrunkCostsMinor: map[string]int64{"": 100}}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.in.Validate()
			if tc.wantErr {
				require.ErrorIs(t, err, billingapi.ErrInvalidTariff)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestTariffs_TrunkCostMinor(t *testing.T) {
	t.Parallel()

	tar := billingapi.Tariffs{TrunkCostsMinor: map[string]int64{"mtt-msk-1": 342}}
	require.Equal(t, int64(342), tar.TrunkCostMinor("mtt-msk-1"))
	require.Equal(t, int64(0), tar.TrunkCostMinor("unknown"))
	require.Equal(t, int64(0), tar.TrunkCostMinor(""))
}

func TestMonthHelper(t *testing.T) {
	t.Parallel()

	p := billingapi.Month(2026, 5)
	require.Equal(t, 2026, p.From.Year())
	require.Equal(t, 5, int(p.From.Month()))
	require.Equal(t, 1, p.From.Day())
	require.Equal(t, 0, p.From.Hour())
	require.Equal(t, 6, int(p.To.Month()))
}
