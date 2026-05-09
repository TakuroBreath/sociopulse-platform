package worker_test

// Compile-time assertion that *store.PostgresStore satisfies the
// worker-package LifecycleStore interface. Lives in an external _test
// package so the worker package does NOT take a runtime production
// import on internal/recording/store — keeping the dep arrow
// cmd/worker → worker → store, not worker → store.
//
// Without this guard, a signature drift on MarkColdTx / MarkDeletedTx /
// UpdateVerifyResultTx (the three Tx variants the rapi.LifecycleStore
// assertion at lifecycle.go does NOT cover) would only surface inside
// the integration tests that build NewRetentionPass / NewIntegrityPass.
// This catches it at the standard `go test` build.

import (
	"github.com/sociopulse/platform/internal/recording/store"
	"github.com/sociopulse/platform/internal/recording/worker"
)

var _ worker.LifecycleStore = (*store.PostgresStore)(nil)
