// Plan 14 Task 12 — billing.recompute subcommand stub.
//
// A real recompute job is out of v1 scope per the user brief: call_costs
// is recomputed lazily by the dialer.call.finalized hook for new calls,
// and backfills for an old tariff version are rare and operator-driven.
// We ship this entry point so future operators have a known place to
// extend.
//
// The function is intentionally standalone — cmd/worker today is a
// long-running errgroup-orchestrated process (recording / dialer-retry /
// analytics ingest / reports consumer), not a switch-based subcommand
// dispatcher. When a future plan adds a CLI dispatch layer (cobra, urfave/cli,
// or a hand-rolled switch on os.Args[1]), this function plugs in as the
// "billing.recompute" branch without further refactoring.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

// runBillingRecompute is the entry point for a future "billing.recompute"
// CLI subcommand. It validates the required --tenant-id / --from / --to
// flags and prints a helpful message pointing the operator at the
// canonical workaround (manual SQL UPDATE keyed off tariff_version).
//
// The implementation is intentionally a stub — see file comment for the
// rationale. ctx is accepted for future symmetry with the other
// errgroup runners; the stub does not consume it today.
func runBillingRecompute(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("billing.recompute", flag.ContinueOnError)
	// Silence the default usage output to stderr on parse errors — tests
	// use fs.Parse via -h to assert flag definitions and we don't want
	// a noisy "Usage of billing.recompute:" stanza in their output.
	fs.SetOutput(os.Stderr)
	tenantID := fs.String("tenant-id", "", "tenant uuid (required)")
	from := fs.String("from", "", "ISO date inclusive (e.g. 2026-05-01)")
	to := fs.String("to", "", "ISO date exclusive (e.g. 2026-06-01)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("billing.recompute: %w", err)
	}
	if *tenantID == "" || *from == "" || *to == "" {
		fs.Usage()
		return fmt.Errorf("billing.recompute: missing required --tenant-id / --from / --to")
	}

	fmt.Fprintln(os.Stderr,
		"billing.recompute: not yet implemented — call_costs is recomputed lazily by the dialer hook for new calls.")
	fmt.Fprintln(os.Stderr,
		"For backfill, run a manual SQL UPDATE keyed off call_costs.tariff_version != current.")
	return nil
}
