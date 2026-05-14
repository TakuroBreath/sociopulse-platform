package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/analytics/store"
)

// TestConfig_Validate is the unit-level RED→GREEN guard for store.Config.
// Each sub-test exercises one rejection branch (or the happy path) of
// Config.Validate. The contract is sentinel-based: every rejection wraps
// store.ErrInvalidConfig so callers can errors.Is against it.
func TestConfig_Validate(t *testing.T) {
	t.Parallel()

	valid := store.Config{
		DSN:           "clickhouse://default:@127.0.0.1:9000/default",
		BatchSize:     500,
		FlushInterval: 2 * time.Second,
	}

	cases := []struct {
		name    string
		cfg     store.Config
		wantErr bool
	}{
		{
			name: "RejectsEmptyDSN",
			cfg: store.Config{
				DSN:           "",
				BatchSize:     500,
				FlushInterval: 2 * time.Second,
			},
			wantErr: true,
		},
		{
			name: "RejectsZeroBatch",
			cfg: store.Config{
				DSN:           valid.DSN,
				BatchSize:     0,
				FlushInterval: 2 * time.Second,
			},
			wantErr: true,
		},
		{
			name: "RejectsNegativeBatch",
			cfg: store.Config{
				DSN:           valid.DSN,
				BatchSize:     -1,
				FlushInterval: 2 * time.Second,
			},
			wantErr: true,
		},
		{
			name: "RejectsZeroFlushInterval",
			cfg: store.Config{
				DSN:           valid.DSN,
				BatchSize:     500,
				FlushInterval: 0,
			},
			wantErr: true,
		},
		{
			name:    "HappyPath",
			cfg:     valid,
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cfg.Validate()
			if tc.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, store.ErrInvalidConfig)
				return
			}
			require.NoError(t, err)
		})
	}
}
