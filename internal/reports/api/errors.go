package api

import "errors"

// Sentinel errors returned by reports interfaces.
// Other modules use errors.Is to discriminate.
var (
	// ErrUnknownKind is returned when ReportKind is not recognised.
	ErrUnknownKind = errors.New("reports: unknown kind")
	// ErrUnsupportedFmt is returned when the (kind, format) pair is invalid (e.g. PDF for CallsByStatus).
	ErrUnsupportedFmt = errors.New("reports: unsupported format for kind")
	// ErrJobNotFound is returned by JobQueue.Get / Cancel when jobID is unknown.
	ErrJobNotFound = errors.New("reports: job not found")
	// ErrInvalidParams is returned when RenderInput.Params is missing a required key.
	ErrInvalidParams = errors.New("reports: invalid params")
	// ErrTooLarge is returned when the rendered output exceeds the per-tenant size cap.
	ErrTooLarge = errors.New("reports: result exceeds size cap")
	// ErrCanceled is returned by Cancel and surfaces in Job.Error after a successful cancel.
	ErrCanceled = errors.New("reports: job canceled")
	// ErrAsyncRequired is returned by ReportRunner.Run when the request
	// trips the async threshold (window > 30 d OR estimated rows >= 100k
	// OR kind == KindCustom). The HTTP handler routes such requests to
	// JobQueue.Enqueue instead of returning the bytes inline.
	ErrAsyncRequired = errors.New("reports: async required")
)
