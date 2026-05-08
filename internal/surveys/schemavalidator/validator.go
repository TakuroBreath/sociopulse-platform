package schemavalidator

import (
	"context"

	"github.com/sociopulse/platform/internal/surveys/dsl"
)

// SchemaValidator is the public entry-point: it runs the JSON-Schema
// pass first, then the graph pass only if the schema is structurally
// sound enough to walk. Construction wires both sub-validators
// together so callers depend on a single concrete type instead of two.
type SchemaValidator struct {
	js   *JSONSchemaValidator
	grph *GraphValidator
}

// NewSchemaValidator composes the two passes. Both arguments MUST be
// non-nil; the constructor panics on misconfiguration so the
// composition root surfaces the bug at startup rather than per
// request.
func NewSchemaValidator(js *JSONSchemaValidator, grph *GraphValidator) *SchemaValidator {
	if js == nil {
		panic("schemavalidator: NewSchemaValidator: JSONSchemaValidator is required")
	}
	if grph == nil {
		panic("schemavalidator: NewSchemaValidator: GraphValidator is required")
	}
	return &SchemaValidator{js: js, grph: grph}
}

// New is a convenience constructor that wires a default
// configuration: the embedded survey-1.0.json schema is compiled at
// construction time, and the graph pass runs against the supplied
// [dsl.Evaluator]. It returns an error iff schema compilation fails
// (a build-time regression). For tests that need a custom evaluator,
// inject it directly via [NewSchemaValidator] / [NewGraphValidator].
func New(ev dsl.Evaluator) (*SchemaValidator, error) {
	js, err := NewJSONSchemaValidator()
	if err != nil {
		return nil, err
	}
	if ev == nil {
		ev = dsl.NewStubEvaluator()
	}
	return NewSchemaValidator(js, NewGraphValidator(ev)), nil
}

// Validate runs both passes and returns a [ValidationReport] whose
// Valid field tells the caller whether SaveVersion can proceed.
// JSON-Schema runs first; if it fails, the graph pass is SKIPPED — a
// structurally invalid document may have nil pointers / wrong-typed
// fields the graph walker can't tolerate, and surfacing both pass
// failures simultaneously would clutter the error set with
// downstream-of-broken-input noise.
//
// ctx is plumbed through to the DSL evaluator so a future
// implementation can short-circuit on cancellation; the JSON-Schema
// pass and the graph walker themselves do not block.
func (v *SchemaValidator) Validate(ctx context.Context, schemaJSON []byte) ValidationReport {
	jsIssues := v.js.Validate(schemaJSON)
	if len(jsIssues) > 0 {
		return ValidationReport{Valid: false, Issues: jsIssues}
	}
	graphIssues := v.grph.Validate(ctx, schemaJSON)
	if len(graphIssues) > 0 {
		return ValidationReport{Valid: false, Issues: graphIssues}
	}
	return ValidationReport{Valid: true, Issues: nil}
}
