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

	// ErrKEKNotFound — the requested KEK ID is unknown to the KMS provider.
	// Distinct from ErrKMSUnavailable: this is a permanent failure, not a
	// transient one. Callers must not retry.
	ErrKEKNotFound = errors.New("tenancy: kek not found")

	// ErrInvalidWrappedDEK — a wrapped DEK could not be decoded or its
	// authentication tag failed. Indicates ciphertext corruption or that the
	// DEK was encrypted with a different KEK. Distinct from ErrKMSUnavailable:
	// this is a permanent failure on the input, not on the KMS service.
	ErrInvalidWrappedDEK = errors.New("tenancy: invalid wrapped dek")

	// ErrPermissionDenied — request lacks Service-Owner mTLS identity.
	ErrPermissionDenied = errors.New("tenancy: permission denied")

	// ErrBucketProvisionPending — a tenant exists in Postgres and has its KEK
	// provisioned in KMS, but the per-tenant Object Storage bucket failed to
	// provision and the tenant is therefore in a degraded state. Operators
	// retry via /admin/tenants/{id}/repair. The Service-Owner UI shows the
	// tenant in red while this state is active.
	ErrBucketProvisionPending = errors.New("tenancy: bucket provision pending")

	// ErrBucketAlreadyExists — a request to provision a bucket collided with
	// an existing bucket of the same name owned by a different account.
	// Distinct from the idempotent same-tenant case: this is a permanent
	// failure indicating a naming collision and requires operator intervention.
	ErrBucketAlreadyExists = errors.New("tenancy: bucket already exists")

	// ErrBucketProvisionFailed — non-recoverable failure from the storage
	// provider during Provision (policy rejected, IAM denied, network down
	// past retries). Wrapped with %w by the bucket provisioner so callers
	// can errors.Is without depending on provider error types.
	ErrBucketProvisionFailed = errors.New("tenancy: bucket provision failed")
)
