//go:build !smoke

package main

import (
	"sync/atomic"

	recwire "github.com/sociopulse/platform/internal/recording/wire"
)

// smokeOverrideRecordingPorts is the production stand-in for the
// build-tagged smoke override symbol. The atomic stays empty — Load
// always returns nil — so production callers of buildRecordingPorts
// fall through to recwire.LocalPorts on every boot.
//
// The variable exists in the production build because buildRecordingPorts
// in cmd/api/recording.go references `smokeOverrideRecordingPorts.Load()`
// unconditionally; without this declaration the production link fails.
// Type / signature must match cmd/api/smoke_overrides.go's smoke twin.
var smokeOverrideRecordingPorts atomic.Pointer[recwire.Ports]
