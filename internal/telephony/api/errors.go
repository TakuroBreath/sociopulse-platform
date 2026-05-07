package api

import "errors"

// Sentinel errors returned by telephony interfaces.
// Other modules use errors.Is to discriminate.
var (
	// ErrAuthFailed is returned when ESL authentication is rejected.
	ErrAuthFailed = errors.New("telephony: esl auth failed")
	// ErrNotConnected is returned when no healthy FS connection is available.
	ErrNotConnected = errors.New("telephony: not connected")
	// ErrCommandFailed is returned when FS rejects an ESL command.
	ErrCommandFailed = errors.New("telephony: command failed")
	// ErrTimeout is returned when a command does not produce an event within the deadline.
	ErrTimeout = errors.New("telephony: timeout")
	// ErrAllNodesFull is returned by LineCapacityTracker.Acquire when every node is at cap.
	ErrAllNodesFull = errors.New("telephony: no healthy node with capacity")
	// ErrNoTrunkAvailable is returned by Router.Select when no trunk satisfies the strategy.
	ErrNoTrunkAvailable = errors.New("telephony: no available trunk")
	// ErrIdempotentReplay is returned (informationally) when a command_id replay is detected;
	// the caller should treat this as success.
	ErrIdempotentReplay = errors.New("telephony: command_id already executed")
)
