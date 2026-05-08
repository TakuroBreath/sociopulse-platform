package schemavalidator

import (
	"fmt"
	"strings"
)

// Issue codes — low-cardinality strings that the UI keys translations
// off and that log aggregators can index. New checks add new codes
// here; once published, codes are stable for the lifetime of the
// schema major version (changing a code is a breaking UI change).
//
// Naming convention: "<layer>.<symptom>", with layer ∈
// {json-schema, graph, dsl} and symptom in kebab-case.
const (
	// CodeJSONSchemaInvalid is attached when the input is not parsable
	// as JSON or the JSON-Schema compiler refused the embedded
	// document. Both are degenerate cases (caller bug or build-time
	// regression) but they share the user-visible code so the UI does
	// not need to discriminate.
	CodeJSONSchemaInvalid = "json-schema.invalid"

	// CodeJSONSchemaPrefix is the prefix every per-keyword JSON-Schema
	// failure carries (e.g. "json-schema.required",
	// "json-schema.pattern"). The exact suffix is the failing keyword
	// reported by santhosh-tekuri/jsonschema, mapped through
	// jsonSchemaKeywordCode below.
	CodeJSONSchemaPrefix = "json-schema."

	// Graph-layer codes. The "graph." prefix lets a caller filter
	// pre-DSL failures from DSL-only failures via strings.HasPrefix.
	CodeGraphNoStart         = "graph.no-start"
	CodeGraphMultipleStarts  = "graph.multiple-starts"
	CodeGraphDuplicateNodeID = "graph.duplicate-node-id"
	CodeGraphUnreachableNode = "graph.unreachable-node"
	CodeGraphDanglingEdge    = "graph.dangling-edge"
	CodeGraphCycleNoExit     = "graph.cycle-no-exit"
	CodeGraphNoEndReachable  = "graph.no-end-reachable"
	CodeGraphBadWhen         = "graph.bad-when"
	CodeGraphBadNodeRef      = "graph.bad-node-reference"
	CodeGraphForwardRef      = "graph.forward-ref"
)

// Issue is one validation finding produced by either of the two
// passes. The triple (Code, Path, Message) is shaped so the UI
// localises by Code, the editor highlights by Path, and the developer
// reads Message in logs.
//
// Path uses RFC 6901 JSON Pointer notation rooted at the schema
// document (`/nodes/3/next/0/to`) when the issue maps directly to a
// JSON node. For graph-level findings whose natural anchor is a node
// id rather than a JSON cursor, Path falls back to a synthetic
// "node:<id>" form (e.g. `node:q1.next[0]`); the UI editor knows both
// shapes.
//
// Message is the English short-form, intentionally low-cardinality
// (no tenant ids, no node ids unless they are the variable being
// reported on) so log aggregators index it cleanly.
type Issue struct {
	Code    string
	Path    string
	Message string
}

// ValidationReport bundles the outcome of [SchemaValidator.Validate].
// Valid is true iff Issues is empty after both passes. Callers must
// not mutate Issues — the slice is owned by the report.
//
// errname is suppressed below because ValidationReport is the
// canonical type name in the plan source and the surrounding API
// surface; renaming to "ValidationReportError" would conflict with
// the existing api.ValidationError boundary type that wraps this
// report.
//
//nolint:errname
type ValidationReport struct {
	Valid  bool
	Issues []Issue
}

// Compile-time interface check: ValidationReport satisfies error so
// callers can return it directly when integrating with the
// surveys/api layer that uses errors.Is/As. The error is non-nil iff
// the report is invalid.
var _ error = ValidationReport{}

// Error implements the error interface so callers can return a
// failed report through an `error` slot. The message is a short,
// low-cardinality summary; full details are exposed via Issues.
//
// Calling Error on a Valid report is allowed and returns the empty
// string, but consumers should normally check Valid first.
func (r ValidationReport) Error() string {
	if r.Valid || len(r.Issues) == 0 {
		return ""
	}
	codes := make([]string, 0, len(r.Issues))
	seen := make(map[string]struct{}, len(r.Issues))
	for _, iss := range r.Issues {
		if _, dup := seen[iss.Code]; dup {
			continue
		}
		seen[iss.Code] = struct{}{}
		codes = append(codes, iss.Code)
	}
	return fmt.Sprintf("schemavalidator: %d issue(s): %s", len(r.Issues), strings.Join(codes, ", "))
}

// HasCode reports whether at least one Issue carries the given code.
// Useful in tests and in the HTTP layer that surfaces specific failure
// classes (e.g. "did this fail because of a forward reference?").
func (r ValidationReport) HasCode(code string) bool {
	for _, iss := range r.Issues {
		if iss.Code == code {
			return true
		}
	}
	return false
}

// HasCodePrefix reports whether at least one Issue's code begins with
// the given prefix. Lets the HTTP layer separate JSON-Schema failures
// from graph failures with a single call.
func (r ValidationReport) HasCodePrefix(prefix string) bool {
	for _, iss := range r.Issues {
		if strings.HasPrefix(iss.Code, prefix) {
			return true
		}
	}
	return false
}
