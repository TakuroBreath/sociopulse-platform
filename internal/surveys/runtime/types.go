package runtime

// schemaDoc is the runtime-internal mirror of the survey schema. We
// re-declare the fields here (rather than reusing the api.Survey /
// api.Version structs) for two reasons:
//
//  1. The runtime parses raw schema bytes (per the WASM-friendly
//     contract: every call accepts []byte). The api.* types are the
//     persistence-layer projection — they don't carry the graph nodes.
//
//  2. Re-declaring keeps this package free of api/store/service
//     coupling. The runtime depends on api only for the public DTOs
//     (Answer, NodeResult, EndKind, QuestionType, NodeKind) and on
//     dsl for the Evaluator interface — everything else is local.
//
// Field tags mirror schemas/survey-1.0.json so json.Unmarshal projects
// straight into this shape without an intermediate map[string]any.
type schemaDoc struct {
	Version     string       `json:"version"`
	PrimaryMode string       `json:"primary_mode"`
	Nodes       []schemaNode `json:"nodes"`
}

// schemaNode mirrors `survey-1.0.json#/$defs/node`. Unused-by-runtime
// fields (title, body, ui) are intentionally omitted: they are noise
// for evaluation and skipping them keeps the unmarshalled doc small.
//
// Min / Max are pointers so we can distinguish "absent" from "0" — a
// number-question with `min: 0` is meaningful (rejects negatives) and
// must NOT be conflated with "no lower bound".
type schemaNode struct {
	ID           string         `json:"id"`
	Kind         string         `json:"kind"`
	QuestionType string         `json:"question_type,omitempty"`
	Options      []schemaOption `json:"options,omitempty"`
	Min          *float64       `json:"min,omitempty"`
	Max          *float64       `json:"max,omitempty"`
	Required     bool           `json:"required,omitempty"`
	Next         []schemaEdge   `json:"next,omitempty"`
}

// schemaOption mirrors `survey-1.0.json#/$defs/option`. The Value
// field is `any` because the schema admits string | number | boolean
// per oneOf; the runtime only consults Option.ID for validation
// purposes (single/multi/select answers must reference an existing
// option id), so Value is captured for completeness but never
// dereferenced.
type schemaOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Value any    `json:"value,omitempty"`
}

// schemaEdge mirrors `survey-1.0.json#/$defs/edge`. The empty `when`
// string means "unconditional"; NextNode treats it as a default match.
type schemaEdge struct {
	To   string `json:"to"`
	When string `json:"when,omitempty"`
}
