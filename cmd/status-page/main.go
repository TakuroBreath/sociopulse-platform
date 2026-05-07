// Package main is the entrypoint for cmd/status-page — a standalone HTTP
// service that reads Alertmanager API and renders a public status page.
// Deployed independently of cmd/api so an api outage does not also take down
// the status page.
//
// Implementation lands in a later plan (Alertmanager integration + UI).
// This stub allows `make build` to succeed and provides a clear runtime error.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "cmd/status-page: not yet implemented; see docs/superpowers/plans/")
	os.Exit(64)
}
