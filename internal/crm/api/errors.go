package api

import "errors"

// Sentinel errors returned by crm interfaces.
// Other modules use errors.Is to discriminate.
var (
	// ErrProjectNotFound is returned when a project lookup misses.
	ErrProjectNotFound = errors.New("crm: project not found")
	// ErrProjectCodeTaken is returned when CreateProject collides on (tenant_id, code).
	ErrProjectCodeTaken = errors.New("crm: project code already exists in tenant")
	// ErrProjectArchived is returned when an operation requires a non-archived project.
	ErrProjectArchived = errors.New("crm: project is archived")
	// ErrInvalidStatus is returned when a status transition is not allowed.
	ErrInvalidStatus = errors.New("crm: invalid status transition")
	// ErrRespondentNotFound is returned when a respondent lookup misses.
	ErrRespondentNotFound = errors.New("crm: respondent not found")
	// ErrInvalidPhone is returned when phone normalisation rejects the input.
	ErrInvalidPhone = errors.New("crm: invalid phone")
	// ErrPhoneInDNC is returned when a respondent's phone is on the DNC list.
	ErrPhoneInDNC = errors.New("crm: phone in DNC")
	// ErrDuplicateRespondent is returned when (project_id, phone_hash) collides.
	ErrDuplicateRespondent = errors.New("crm: duplicate respondent (phone_hash)")
	// ErrInvalidQuotaKind is returned when Quota.DimensionKind is unrecognised.
	ErrInvalidQuotaKind = errors.New("crm: unknown quota dimension")
	// ErrImportInProgress is returned when an import is requested while one is already running for the project.
	ErrImportInProgress = errors.New("crm: another import already running")
	// ErrImportPayloadTooBig is returned when the import body exceeds the per-tenant byte limit.
	ErrImportPayloadTooBig = errors.New("crm: import payload exceeds limit")
	// ErrAdvertisingRejected is returned when CreateProject sets is_advertising=true (deferred to v2).
	ErrAdvertisingRejected = errors.New("crm: is_advertising=true is not allowed in v1")
	// ErrInvalidArgument is returned for malformed inputs (empty operator
	// id slice, nil-uuid id, etc.) that do not warrant a more specific
	// sentinel. Callers can errors.Is for a 4xx-class fall-through.
	ErrInvalidArgument = errors.New("crm: invalid argument")
	// ErrImportNotFound is returned when GetImportStatus is called with
	// a job id that has no Redis-side status hash (TTL elapsed, or the
	// id was never enqueued).
	ErrImportNotFound = errors.New("crm: import job not found")
	// ErrImportFormatUnsupported is returned when ImportRequest.Format
	// is not one of the canonical values ("csv", "xlsx").
	ErrImportFormatUnsupported = errors.New("crm: unsupported import format")
)
