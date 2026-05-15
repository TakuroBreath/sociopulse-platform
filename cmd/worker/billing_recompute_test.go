package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBillingRecompute_MissingFlags_ReturnsError asserts the stub fails
// fast when any of the three required flags are missing. The error is
// intentionally a plain fmt.Errorf — the message is the human signal,
// not a programmatic contract — so we assert only that the call errored.
func TestBillingRecompute_MissingFlags_ReturnsError(t *testing.T) {
	t.Parallel()

	err := runBillingRecompute(context.Background(), []string{})
	require.Error(t, err)
}

// TestBillingRecompute_AllFlagsPresent_ReturnsNil asserts the stub
// returns nil when every required flag is supplied. The v1 stub does
// nothing useful but must complete cleanly so a future CLI dispatcher
// can extend it without changing the success contract.
func TestBillingRecompute_AllFlagsPresent_ReturnsNil(t *testing.T) {
	t.Parallel()

	err := runBillingRecompute(context.Background(),
		[]string{
			"-tenant-id=11111111-1111-1111-1111-111111111111",
			"-from=2026-05-01",
			"-to=2026-06-01",
		})
	require.NoError(t, err)
}
