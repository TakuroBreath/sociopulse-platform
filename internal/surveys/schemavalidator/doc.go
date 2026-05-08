// Package schemavalidator runs the two-pass validation that gate-keeps
// every SaveVersion call on a survey schema:
//
//  1. JSON-Schema pass — structural validation against the embedded
//     survey-1.0.json document. Catches missing required fields, type
//     mismatches, malformed identifiers, per-question-type shape rules
//     (single/multi/select must declare options).
//  2. Graph pass — semantic checks the schema can't express:
//     start uniqueness, reachability from start, dangling edges, cycles
//     without an exit, parsability of conditional `when` expressions
//     (via the dsl.Evaluator stub from Task 2 — replaced in Task 3),
//     and forward-reference detection via dominator analysis on
//     identifiers referenced from each edge's `when` clause.
//
// The two passes are sequenced: if the JSON-Schema pass fails, the
// graph pass is skipped because a structurally invalid document may
// have nil pointers, missing arrays, or bad types that the graph
// walker can't tolerate. Each pass produces zero or more typed
// [Issue]s, and the entry-point [SchemaValidator.Validate] returns a
// single [ValidationReport] bundling them all with stable, low-
// cardinality codes the UI can localise.
//
// The package depends only on `internal/surveys/dsl` and
// `internal/surveys/schemas` — it has no DB, no logger, and no
// goroutines. A single instance is safe to share across goroutines.
package schemavalidator
