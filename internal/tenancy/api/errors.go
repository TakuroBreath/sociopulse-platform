package api

import "errors"

// Sentinel errors returned by tenancy. Wrap with %w; check with errors.Is.
var (
	// ErrNotFound — the tenant or setting key does not exist.
	ErrNotFound = errors.New("tenancy: not found")

	// ErrAlreadyExists — duplicate org_code on Create, or duplicate setting key on insert.
	ErrAlreadyExists = errors.New("tenancy: already exists")

	// ErrInvalidArgument — caller-provided value violates an invariant
	// (empty org_code, unknown status, unknown setting key, value type mismatch).
	ErrInvalidArgument = errors.New("tenancy: invalid argument")

	// ErrSuspended — a tenant is suspended and cannot perform the requested op.
	// Service-Owner CRUD is still allowed; only data-plane operations should
	// surface this to end-users.
	ErrSuspended = errors.New("tenancy: suspended")

	// ErrArchived — a tenant is archived (read-only graveyard).
	ErrArchived = errors.New("tenancy: archived")

	// ErrKMSUnavailable — Yandex KMS is unreachable / returned a transient error.
	// Callers must retry with backoff, NOT degrade silently.
	ErrKMSUnavailable = errors.New("tenancy: kms unavailable")

	// ErrPermissionDenied — request lacks Service-Owner mTLS identity.
	ErrPermissionDenied = errors.New("tenancy: permission denied")
)
