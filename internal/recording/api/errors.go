package api

import "errors"

// Sentinel errors returned by recording interfaces.
// Other modules use errors.Is to discriminate.
var (
	// ErrNotFound is returned when a recording lookup misses.
	ErrNotFound = errors.New("recording: not found")
	// ErrAlreadyDeleted is returned when an operation requires a non-deleted recording.
	ErrAlreadyDeleted = errors.New("recording: already deleted")
	// ErrTenantMismatch is returned when the request's tenant id does not match the row's.
	ErrTenantMismatch = errors.New("recording: tenant mismatch")
	// ErrCallNotFound is returned by Commit when the call_id has no parent call row.
	ErrCallNotFound = errors.New("recording: call not found")
	// ErrInvalidInput is returned when CommitInput has malformed fields (e.g. bad SHA256).
	ErrInvalidInput = errors.New("recording: invalid input")
	// ErrIntegrityFailed is returned by VerifyChecksum when the recomputed sha256 does not match.
	ErrIntegrityFailed = errors.New("recording: integrity check failed")
)
