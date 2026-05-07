package api

import "errors"

// Sentinel errors returned by audit Logger / Reader / Archiver implementations.
// Other modules use errors.Is to discriminate.
var (
	// ErrInvalidEvent is returned when an Event is missing a required field.
	ErrInvalidEvent = errors.New("audit: invalid event")
	// ErrNotFound is returned when a Reader query targets a row that does not exist.
	ErrNotFound = errors.New("audit: not found")
)
