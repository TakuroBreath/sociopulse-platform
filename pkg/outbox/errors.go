package outbox

import "errors"

// Sentinel errors returned by pkg/outbox. Callers may errors.Is against
// these to drive retry / alerting policy.
var (
	// ErrPublisherFailed is returned by PublisherAdapter when every
	// retry attempt has been exhausted. Wraps the last upstream error.
	ErrPublisherFailed = errors.New("outbox: publisher failed after retries")
)
