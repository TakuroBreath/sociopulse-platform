//go:build smoke

package main

import recwire "github.com/sociopulse/platform/internal/recording/wire"

// smokeOverrideRecordingPorts is consulted by buildRecordingPorts (in
// recording.go) BEFORE calling recwire.LocalPorts. The smoke build tag
// activates this file; production builds get the !smoke variant from
// smoke_overrides_prod.go which always returns nil.
//
// Smoke tests populate this seam via SetSmokeRecordingPorts BEFORE
// invoking bootAPI so the cmd/api process and the test share ONE
// *recwire.Ports instance — the test pre-encrypts a fixture audio
// blob, Puts it under (bucket, key) on the shared LocalObjectStore,
// inserts a matching call_recordings row, and the HTTP recording-
// stream handler then reads the same bytes back through the same
// store. Without the shared instance the recording handler would build
// a fresh empty LocalObjectStore at boot and ErrObjectNotFound the
// scenario.
//
// Build-tag isolation: production binaries (no smoke tag) compile the
// !smoke twin file in this package; the smoke tag swaps it with this
// one. Either way, the symbol exists with the same package-private
// scope, so buildRecordingPorts has a single uniform call site.
var smokeOverrideRecordingPorts *recwire.Ports

// SetSmokeRecordingPorts injects the smoke test's *recwire.Ports so the
// next buildRecordingPorts call returns it instead of building a fresh
// LocalPorts. Build-tagged so production code cannot accidentally call
// it. Must be invoked BEFORE bootAPI to take effect on the same boot.
//
// Idempotent — overwriting with a fresh value is fine; passing nil
// reverts to the LocalPorts fall-through.
func SetSmokeRecordingPorts(p *recwire.Ports) {
	smokeOverrideRecordingPorts = p
}

// GetSmokeRecordingPorts returns the currently-installed override (or
// nil). Smoke tests use this to recover the shared LocalObjectStore
// instance after bootAPI ran — Put-ing the fixture audio bytes through
// the same store the handler reads from is the entire point of the
// override seam.
func GetSmokeRecordingPorts() *recwire.Ports {
	return smokeOverrideRecordingPorts
}
