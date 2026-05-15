//go:build !smoke

package main

import recwire "github.com/sociopulse/platform/internal/recording/wire"

// smokeOverrideRecordingPorts is the production stand-in for the
// build-tagged smoke override symbol. Always nil — production callers
// of buildRecordingPorts must fall through to recwire.LocalPorts. The
// _ = reference inside the production helper keeps the symbol live so
// goimports does not strip it.
var smokeOverrideRecordingPorts *recwire.Ports
