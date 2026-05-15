//go:build smoke

package smoke

import "time"

// FutureClock returns a closure that always returns time.Now() + d. The
// closure captures the offset, NOT a snapshot of "now" — every call
// re-reads the wall clock so the returned time advances naturally
// alongside real time.
//
// Used by scenario 8 (152-ФЗ purge) to fast-forward past the 30-day
// soft-delete grace window: passing 31*24*time.Hour to FutureClock
// makes PurgeWorker.Run see a clock that has skipped forward by 31
// days, so a respondent soft-deleted "now" is past the cutoff and
// gets hard-deleted.
//
// Returning a closure (rather than a struct with a Now method) keeps
// the production NewPurgeWorker signature — `clock func() time.Time` —
// satisfied without an adapter; pass the closure directly.
//
// d may be negative or zero — the closure simply mirrors that, allowing
// "snap back to now" or "slip back N hours" patterns if a scenario
// needs them. We do not validate the sign because every caller in
// scope (purge fast-forward) uses a positive duration; the helper is
// transparently general for future reuse.
func FutureClock(d time.Duration) func() time.Time {
	return func() time.Time {
		return time.Now().Add(d)
	}
}
