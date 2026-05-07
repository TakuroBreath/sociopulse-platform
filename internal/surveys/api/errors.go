package api

import "errors"

// Sentinel errors returned by surveys interfaces.
// Other modules use errors.Is to discriminate.
var (
	// ErrNotFound is returned when a survey or version lookup misses.
	ErrNotFound = errors.New("surveys: not found")
	// ErrValidation is returned by SaveVersion when graph validation fails.
	// Wrap inside *ValidationError to surface structured Issues.
	ErrValidation = errors.New("surveys: validation failed")
	// ErrSchema is returned when the JSON schema check fails.
	ErrSchema = errors.New("surveys: invalid schema")
	// ErrCycle is returned when the graph has a cycle without an exit.
	ErrCycle = errors.New("surveys: cycle without exit")
	// ErrUnreachable is returned when nodes are unreachable from start.
	ErrUnreachable = errors.New("surveys: unreachable nodes")
	// ErrDanglingEdge is returned when an edge references a non-existent node.
	ErrDanglingEdge = errors.New("surveys: dangling edge")
	// ErrForwardRef is returned when DSL refers to a node not yet visited.
	ErrForwardRef = errors.New("surveys: forward reference in DSL")
	// ErrBadAnswer is returned when ValidateAnswer rejects an answer for the node type.
	ErrBadAnswer = errors.New("surveys: bad answer for node type")
	// ErrAlreadyActive is returned by Activate when the version is already active.
	ErrAlreadyActive = errors.New("surveys: version already active")
	// ErrNoActiveVersion is returned when an action requires an active version but none exists.
	ErrNoActiveVersion = errors.New("surveys: no active version")
)
