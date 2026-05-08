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
	// ErrNoActiveVersion is returned when an action requires an active version but none exists.
	ErrNoActiveVersion = errors.New("surveys: no active version")
	// ErrVersionNotFound is returned when a version lookup misses inside the
	// scope of a surveyID. Distinct from ErrNotFound so callers can tell
	// "the survey is missing" from "the survey exists but has no such
	// version".
	ErrVersionNotFound = errors.New("surveys: version not found")
	// ErrSurveyArchived is returned when a state-changing action targets a
	// survey whose status is archived. Pure-read paths still succeed.
	ErrSurveyArchived = errors.New("surveys: survey archived")
	// ErrInvalidArgument is returned when the caller passed a structurally
	// invalid input (zero UUIDs, empty names, negative target counts, ...).
	ErrInvalidArgument = errors.New("surveys: invalid argument")
	// ErrNoMatchingEdge is returned by Runtime.NextNode when none of the
	// current node's outgoing edges match (no unconditional edge present
	// and every conditional edge's `when` evaluated to a non-truthy
	// result). This is a runtime-level signal that the current schema +
	// answer set put the survey into an unrecoverable state — typically
	// a fixture / authoring bug rather than a respondent action.
	ErrNoMatchingEdge = errors.New("surveys: no matching edge")
	// ErrNodeNotFound is returned by the runtime when a referenced node
	// id does not exist in the supplied schema (NextNode/ValidateAnswer/
	// CalculateProgress all surface this for unknown ids).
	ErrNodeNotFound = errors.New("surveys: node not found")
)
