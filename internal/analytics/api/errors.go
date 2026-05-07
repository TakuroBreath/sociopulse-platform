package api

import "errors"

// Sentinel errors returned by analytics interfaces.
// Other modules use errors.Is to discriminate.
var (
	// ErrTenantRequired is returned when a query is missing the TenantID.
	ErrTenantRequired = errors.New("analytics: tenant required")
	// ErrInvalidWindow is returned by Window.Validate when From≥To or the span exceeds 1 year.
	ErrInvalidWindow = errors.New("analytics: invalid window")
	// ErrTransient is returned by the ingester when the failure should be retried.
	ErrTransient = errors.New("analytics: transient ingest error")
	// ErrInvalidPayload is returned when an EventEnvelope.Payload cannot be decoded for its Kind.
	// The ingester routes the message to the dead-letter stream.
	ErrInvalidPayload = errors.New("analytics: invalid payload")
)
