// Package testdata exposes the survey JSON fixtures as an [embed.FS]
// so the validator package can iterate them in table-driven tests
// without depending on the os filesystem.
//
// Naming convention:
//
//   - valid-*.json   — should pass both JSON-Schema and graph validation
//   - invalid-*.json — should fail one specific check; the file's
//     "metadata.expected_error" and "metadata.fails_at" fields document
//     which one and at which layer (json-schema | graph)
package testdata

import "embed"

//go:embed *.json
var Fixtures embed.FS
