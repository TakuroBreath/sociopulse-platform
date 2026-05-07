// Package main is the entrypoint for cmd/synthetic — a standalone canary
// binary that exercises critical user flows on a schedule (login, create-call,
// listen-in, download recording) and emits Prometheus metrics on success/
// failure rates.
//
// Implementation lands in a later plan (canary checks + cron-based scheduler).
// This stub allows `make build` to succeed and provides a clear runtime error.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "cmd/synthetic: not yet implemented; see docs/superpowers/plans/")
	os.Exit(64)
}
