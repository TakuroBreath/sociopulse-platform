package schemas_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/surveys/schemas"
)

// TestSchema_IsValidJSON guards against accidental edits that break the
// embedded JSON document. If this fails, survey-1.0.json no longer parses
// and the validator can't compile it at startup.
func TestSchema_IsValidJSON(t *testing.T) {
	t.Parallel()
	var v any
	require.NoError(t, json.Unmarshal(schemas.Schema(), &v))
}

// TestSchema_HasExpectedTopLevel locks in the dialect and the presence of
// $defs (where node/option/edge live). Sanity check, not a full schema test.
func TestSchema_HasExpectedTopLevel(t *testing.T) {
	t.Parallel()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(schemas.Schema(), &doc))
	require.Equal(t, "https://json-schema.org/draft/2020-12/schema", doc["$schema"])
	require.Contains(t, doc, "$defs")
}

// TestSchema_NonEmpty guards against an empty embed (e.g. wrong build tag).
func TestSchema_NonEmpty(t *testing.T) {
	t.Parallel()
	require.NotEmpty(t, schemas.Schema())
}
