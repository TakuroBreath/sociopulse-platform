package worker

import "hash/fnv"

// RetentionLockKey is the FNV-1a hash of "recording.retention_pass" cast
// to int64. Stable across replicas: every worker computes the same key
// so pg_try_advisory_lock contends on a single leader slot.
//
// Plan 12.4 cmd/worker imports this so it can construct retry.PgLeader
// without taking a transitive dep on this package's privates.
var RetentionLockKey = fnvHash("recording.retention_pass")

// IntegrityLockKey is the FNV-1a hash of "recording.integrity_pass" cast
// to int64. Distinct from RetentionLockKey so the retention sweep and
// the integrity verifier (Plan 12.4 Task 3) do not block each other on
// the same advisory-lock slot — both passes can lead simultaneously
// because they touch different rows / different columns.
var IntegrityLockKey = fnvHash("recording.integrity_pass")

// fnvHash computes the FNV-1a 64-bit hash of s and casts to int64
// (Postgres' pg_try_advisory_lock signature is bigint = int64).
//
// Mirrors internal/dialer/retry/leader_election.go: the same hash
// procedure keeps the keyspace stable and lets ops dashboards refer
// to the seed string without recomputing the hash by hand.
func fnvHash(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	//nolint:gosec // intentional: pg advisory keys are bigint; reinterpret the unsigned hash.
	return int64(h.Sum64())
}
