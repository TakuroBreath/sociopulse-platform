// Package schemas hosts the canonical JSON-Schema document that
// describes a valid survey graph, and exposes it as an embedded
// resource via [embed.FS] so the validator can compile it once at
// startup without filesystem I/O.
//
// Graph-level constraints (start uniqueness, reachability, dangling
// edges, cycle-without-exit, DSL forward refs) live in the validator
// package, not in this schema. The schema only catches structural
// violations: missing required fields, type mismatches, invalid
// enums, malformed identifiers, and per-question-type shape rules
// (e.g. single/multi/select questions must declare options).
package schemas

import _ "embed"

//go:embed survey-1.0.json
var schemaV1 []byte

// Schema returns the JSON-Schema 2020-12 document as raw bytes.
// Callers compile it once via santhosh-tekuri/jsonschema's compiler
// at startup and reuse the resulting *jsonschema.Schema.
//
// The returned slice is a reference to the embedded data and MUST
// NOT be mutated.
func Schema() []byte { return schemaV1 }
