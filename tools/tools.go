//go:build tools

// Package tools tracks build-time-only dependencies. The blank imports
// register modules with `go.mod` so `go install` from the repo root resolves
// the same versions as production code uses at runtime.
package tools

import (
	_ "google.golang.org/grpc/cmd/protoc-gen-go-grpc"
	_ "google.golang.org/protobuf/cmd/protoc-gen-go"
)
