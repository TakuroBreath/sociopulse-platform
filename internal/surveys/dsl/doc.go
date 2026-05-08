// Package dsl exposes the surface used by the schema validator (and the
// future runtime) to parse and evaluate the conditional expressions that
// live on edges' "when" clauses in a survey schema.
//
// Plan 07 splits responsibility for the DSL across two tasks:
//
//   - Task 2 (this commit) provides only the parse-and-check side, in the
//     form of a [StubEvaluator]. It accepts any non-empty expression with
//     balanced parentheses and rejects everything else with [ErrDSLParse].
//     The stub is wired into the schema validator's graph pass so the
//     pipeline (JSON-Schema → graph) can be built and tested against the
//     fixture set without waiting for the real DSL.
//   - Task 3 will replace the stub with an expr-lang/expr-backed
//     implementation that whitelists identifiers (`q<id>.value`,
//     `q<id>.answered`, `answer`, ...) and exposes the AST so the graph
//     validator can derive forward-reference checks from real identifier
//     scans rather than the placeholder no-op the stub returns.
//
// The interface boundary [Evaluator] stays stable across both tasks. The
// schema validator depends only on the interface so swapping the
// implementation in Task 3 is a one-line constructor change.
package dsl
