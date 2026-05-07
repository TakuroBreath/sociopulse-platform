// Package grpc provides constructors for the project's mTLS-secured
// gRPC servers and clients. It deliberately exposes only what every
// caller needs (a *grpc.Server / *grpc.ClientConn already wired up
// with TLS, keepalive, and the project-standard interceptors).
//
// The recording service (server) and recording-uploader (client) are
// the principal users; telephony control is the secondary user.
//
// Concrete TLS material loading and interceptor chain are filled in by
// Plan 02 Task 4 and Plan 09 Task 1.
package grpc

import "google.golang.org/grpc"

// NewMTLSServer returns a gRPC server preconfigured for mutual TLS.
// certFile + keyFile are the server's leaf cert/key; caFile is the
// root that issued the client certs the server is willing to accept.
//
// The returned server has:
//   - the project-standard logging / tracing / recovery interceptors,
//   - reasonable keepalive defaults (Plan 02 Task 4),
//   - tls.Config with MinVersion=TLS1.3 and ClientAuth=RequireAndVerifyClientCert.
func NewMTLSServer(certFile, keyFile, caFile string) (*grpc.Server, error) {
	panic("not implemented: see Plan 02 Task 4")
}
