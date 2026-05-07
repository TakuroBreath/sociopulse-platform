// Package main is the entrypoint for cmd/worker.
//
// Implementation lands in subsequent plans (jobs queue + leader election).
// This stub allows `make build` to succeed and provides a clear runtime error.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "cmd/worker: not yet implemented; see docs/superpowers/plans/")
	os.Exit(64)
}
