package grpc

import "google.golang.org/grpc"

// NewMTLSClient dials target with mutual TLS. certFile + keyFile are
// this client's leaf cert/key; caFile is the root that signed the
// server's cert.
//
// The returned ClientConn has:
//   - TLS pinning to caFile,
//   - the project-standard client interceptors (logging, tracing,
//     retries with backoff for idempotent methods),
//   - reasonable keepalive defaults (Plan 02 Task 4).
func NewMTLSClient(target, certFile, keyFile, caFile string) (*grpc.ClientConn, error) {
	panic("not implemented: see Plan 02 Task 4")
}
