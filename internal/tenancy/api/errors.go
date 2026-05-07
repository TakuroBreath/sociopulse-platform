package api

import "errors"

// Sentinel errors returned by tenancy interfaces.
// Other modules use errors.Is to discriminate.
var (
	// ErrNotFound is returned when a tenant or setting cannot be found.
	ErrNotFound = errors.New("tenancy: not found")
	// ErrAlreadyExists is returned by TenantService.Create when OrgCode collides.
	ErrAlreadyExists = errors.New("tenancy: already exists")
	// ErrInvalidArgument is returned for malformed request fields (e.g. empty OrgCode).
	ErrInvalidArgument = errors.New("tenancy: invalid argument")
	// ErrSuspended is returned by guards that refuse to operate on a suspended tenant.
	ErrSuspended = errors.New("tenancy: suspended")
	// ErrArchived is returned by guards that refuse to operate on an archived tenant.
	ErrArchived = errors.New("tenancy: archived")
	// ErrKMSUnavailable is returned when the KMS provider is unreachable
	// after retries; the boundary maps it to HTTP 503 / gRPC Unavailable.
	ErrKMSUnavailable = errors.New("tenancy: kms unavailable")
	// ErrPermissionDenied is returned by Service-Owner guards.
	ErrPermissionDenied = errors.New("tenancy: permission denied")
)
