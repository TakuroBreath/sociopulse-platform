package service

// Test-only seams. Lets external `package service_test` audit_test.go
// construct an AuditEmitter with a faked tenantTxRunner — production
// callers still go through NewAuditEmitter(*postgres.Pool, ...).
//
// This file is _test-tagged so it does not bloat the production build.

import (
	"go.uber.org/zap"
)

// NewAuditEmitterForTest wires an AuditEmitter around a fake pool runner
// + outbox writer. Logger defaults to zap.NewNop. Production code MUST use
// NewAuditEmitter — this constructor exists only because tenantTxRunner is
// unexported and the unit tests need to inject a non-Postgres fake to
// exercise the audit-emit happy/sad paths without testcontainers.
func NewAuditEmitterForTest(pool any, ob AuditWriter) *AuditEmitter {
	runner, _ := pool.(tenantTxRunner)
	return &AuditEmitter{pool: runner, ob: ob, log: zap.NewNop()}
}
