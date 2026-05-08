package schemavalidator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/sociopulse/platform/internal/surveys/schemas"
)

// schemaResourceURL is the canonical $id of the survey-1.0 document.
// It must match the "$id" field in schemas/survey-1.0.json so the
// compiler resolves $ref fragments inside the schema correctly.
const schemaResourceURL = "https://sociopulse.ru/schemas/survey-1.0.json"

// JSONSchemaValidator wraps a single compiled santhosh-tekuri schema.
// Construction parses and compiles the embedded schema once at
// startup — Validate is then a hot path, allocating only the
// json.Unmarshal target and the Issue slice it returns.
type JSONSchemaValidator struct {
	compiled *jsonschema.Schema
}

// NewJSONSchemaValidator compiles the embedded survey-1.0.json
// document. It returns an error only on a build-time regression
// (corrupted embedded bytes or a future incompatible jsonschema
// upgrade). In production the composition root either succeeds or
// crashes at startup — there is no recovery path.
func NewJSONSchemaValidator() (*JSONSchemaValidator, error) {
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	if err := compiler.AddResource(
		schemaResourceURL,
		bytes.NewReader(schemas.Schema()),
	); err != nil {
		return nil, fmt.Errorf("schemavalidator: register schema: %w", err)
	}
	sch, err := compiler.Compile(schemaResourceURL)
	if err != nil {
		return nil, fmt.Errorf("schemavalidator: compile schema: %w", err)
	}
	return &JSONSchemaValidator{compiled: sch}, nil
}

// Validate parses schemaJSON as a generic JSON document and runs the
// JSON-Schema 2020-12 check against it. It returns a slice of [Issue]s
// — empty when the document is structurally valid. The graph pass MUST
// NOT run if this returns a non-empty slice: a malformed document may
// be missing arrays or have wrong-type fields the graph walker can't
// tolerate.
//
// The returned issues use JSON-Pointer paths sourced from the
// jsonschema library's InstanceLocation field, so editor highlighting
// "just works" without a re-walk on the caller side.
func (v *JSONSchemaValidator) Validate(schemaJSON []byte) []Issue {
	var doc any
	if err := json.Unmarshal(schemaJSON, &doc); err != nil {
		return []Issue{{
			Code:    CodeJSONSchemaInvalid,
			Path:    "",
			Message: fmt.Sprintf("schema is not valid JSON: %s", err.Error()),
		}}
	}
	if err := v.compiled.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return collectValidationErrors(ve)
		}
		return []Issue{{
			Code:    CodeJSONSchemaInvalid,
			Path:    "",
			Message: err.Error(),
		}}
	}
	return nil
}

// collectValidationErrors flattens a santhosh-tekuri ValidationError
// tree into a list of [Issue] records, one per leaf failure. Internal
// nodes carry only a roll-up message; their information is fully
// captured by the leaves they aggregate, so we recurse and emit only
// at the bottom.
func collectValidationErrors(ve *jsonschema.ValidationError) []Issue {
	out := make([]Issue, 0, 4)
	collectInto(ve, &out)
	return out
}

// collectInto walks the ValidationError tree depth-first. When the
// walker hits a leaf (Causes is empty), it appends an Issue mapped
// from the keyword and instance location.
func collectInto(ve *jsonschema.ValidationError, out *[]Issue) {
	if len(ve.Causes) == 0 {
		*out = append(*out, issueFromValidationError(ve))
		return
	}
	for _, c := range ve.Causes {
		collectInto(c, out)
	}
}

// issueFromValidationError converts one leaf jsonschema ValidationError
// into an [Issue]. The Code is mapped from the failing keyword
// (extracted from KeywordLocation). When the location does not encode
// a recognisable keyword (rare but possible for top-level resource
// errors) we fall back to the generic CodeJSONSchemaInvalid.
func issueFromValidationError(ve *jsonschema.ValidationError) Issue {
	keyword := keywordFromLocation(ve.KeywordLocation)
	code := CodeJSONSchemaInvalid
	if keyword != "" {
		code = CodeJSONSchemaPrefix + keyword
	}
	msg := ve.Message
	if msg == "" {
		msg = "json-schema validation failed"
	}
	return Issue{
		Code:    code,
		Path:    ve.InstanceLocation,
		Message: msg,
	}
}

// keywordFromLocation extracts the trailing JSON-Pointer segment of a
// KeywordLocation string ("/properties/nodes/items/$ref/required" →
// "required"). It returns "" when the location has no segments. The
// jsonschema library uses unescaped pointer segments here so we don't
// need to handle ~ escaping.
func keywordFromLocation(loc string) string {
	if loc == "" {
		return ""
	}
	// Walk back to the last '/'.
	for i := len(loc) - 1; i >= 0; i-- {
		if loc[i] == '/' {
			return loc[i+1:]
		}
	}
	return loc
}
