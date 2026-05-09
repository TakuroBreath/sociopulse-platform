package grpcserver

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// PeerIdentity is the value extracted from a verified mTLS leaf
// cert. The handlers compare PeerIdentity.TenantID against the
// request's tenant_id field to enforce cross-tenant isolation at the
// gRPC layer (defence in depth on top of database RLS).
//
// SPIFFE URI format used by cmd/recording-uploader:
//
//	spiffe://sociopulse/ingest-agent/<agent-id>?tenant=<tenant-uuid>
type PeerIdentity struct {
	TenantID uuid.UUID
	AgentID  string
	URI      string
}

// peerIdentityCtxKey is the context key under which the unary
// interceptor stashes the parsed PeerIdentity. Type-safety guard: a
// private struct type means external packages cannot collide with us.
type peerIdentityCtxKey struct{}

// SPIFFE constants. We only accept the "spiffe" scheme + the
// "/ingest-agent/<id>" path prefix; anything else is treated as an
// unknown peer and rejected at the interceptor.
const (
	spiffeScheme   = "spiffe"
	spiffeIDPrefix = "/ingest-agent/"
	spiffeQueryKey = "tenant"
	spiffeIssuer   = "sociopulse"
)

// ParsePeerIdentity extracts the SPIFFE-style identity from a
// verified leaf certificate. Returns a non-nil error when:
//
//   - the cert has no URI SAN at all (clients must put the SPIFFE id
//     in URI SANs, not DNS or IP);
//   - the URI scheme is not "spiffe";
//   - the URI host is not "sociopulse";
//   - the path does not match /ingest-agent/<agent-id>;
//   - the tenant query parameter is missing or not a valid UUID.
//
// Callers (the interceptor) translate these into
// codes.Unauthenticated.
func ParsePeerIdentity(cert *x509.Certificate) (PeerIdentity, error) {
	if cert == nil {
		return PeerIdentity{}, errors.New("peer identity: nil cert")
	}
	if len(cert.URIs) == 0 {
		return PeerIdentity{}, errors.New("peer identity: cert has no URI SAN")
	}

	// We accept the FIRST URI SAN. cmd/recording-uploader emits
	// exactly one; rejecting multi-URI certs here would be brittle
	// against future SPIFFE bundle formats.
	u := cert.URIs[0]
	return parseSpiffeURL(u)
}

// parseSpiffeURL is the low-level URL→PeerIdentity transform,
// extracted so the test suite can call it without minting a full cert.
func parseSpiffeURL(u *url.URL) (PeerIdentity, error) {
	if u == nil {
		return PeerIdentity{}, errors.New("peer identity: nil URI")
	}
	if u.Scheme != spiffeScheme {
		return PeerIdentity{}, fmt.Errorf("peer identity: unsupported scheme %q (want %q)", u.Scheme, spiffeScheme)
	}
	if u.Host != spiffeIssuer {
		return PeerIdentity{}, fmt.Errorf("peer identity: unsupported issuer %q (want %q)", u.Host, spiffeIssuer)
	}
	if len(u.Path) <= len(spiffeIDPrefix) || u.Path[:len(spiffeIDPrefix)] != spiffeIDPrefix {
		return PeerIdentity{}, fmt.Errorf("peer identity: path %q does not start with %q", u.Path, spiffeIDPrefix)
	}
	agentID := u.Path[len(spiffeIDPrefix):]
	if agentID == "" {
		return PeerIdentity{}, errors.New("peer identity: empty agent id")
	}

	tenantStr := u.Query().Get(spiffeQueryKey)
	if tenantStr == "" {
		return PeerIdentity{}, errors.New("peer identity: missing tenant query parameter")
	}
	tenantID, err := uuid.Parse(tenantStr)
	if err != nil {
		return PeerIdentity{}, fmt.Errorf("peer identity: tenant uuid: %w", err)
	}

	return PeerIdentity{
		TenantID: tenantID,
		AgentID:  agentID,
		URI:      u.String(),
	}, nil
}

// peerTenantInterceptor returns a unary interceptor that, for every
// incoming RPC, pulls the verified leaf cert from peer.AuthInfo,
// parses it via ParsePeerIdentity, and stashes the result on ctx.
//
// On any failure the interceptor returns codes.Unauthenticated
// before invoking the handler — handlers may then assume that
// peerIdentityFromCtx returns a non-zero PeerIdentity.
func peerTenantInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		identity, err := peerIdentityFromTLS(ctx)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, err.Error())
		}
		return handler(withPeerIdentity(ctx, identity), req)
	}
}

// fakePeerIdentityInterceptor returns an interceptor that stamps a
// fixed PeerIdentity on every incoming ctx. Used by the bufconn test
// harness in NewForTest — production callers MUST use
// peerTenantInterceptor, which derives identity from the TLS layer.
func fakePeerIdentityInterceptor(id PeerIdentity) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		return handler(withPeerIdentity(ctx, id), req)
	}
}

// peerIdentityFromTLS pulls the verified peer cert chain off ctx and
// returns the parsed identity. Returns a structured error on any
// missing piece — translated to codes.Unauthenticated by the
// interceptor.
func peerIdentityFromTLS(ctx context.Context) (PeerIdentity, error) {
	p, ok := peer.FromContext(ctx)
	if !ok || p == nil {
		return PeerIdentity{}, errors.New("peer identity: no peer info on context")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return PeerIdentity{}, errors.New("peer identity: no TLS auth info on context")
	}
	if len(tlsInfo.State.PeerCertificates) == 0 {
		return PeerIdentity{}, errors.New("peer identity: no peer certificates")
	}
	return ParsePeerIdentity(tlsInfo.State.PeerCertificates[0])
}

// withPeerIdentity returns a child ctx carrying id under the package's
// private context key.
func withPeerIdentity(ctx context.Context, id PeerIdentity) context.Context {
	return context.WithValue(ctx, peerIdentityCtxKey{}, id)
}

// peerIdentityFromCtx is the handler-side accessor: it returns the
// identity stamped by peerTenantInterceptor (or
// fakePeerIdentityInterceptor in tests). The boolean is false when
// no interceptor ran — handlers translate that to
// codes.Unauthenticated.
func peerIdentityFromCtx(ctx context.Context) (PeerIdentity, bool) {
	id, ok := ctx.Value(peerIdentityCtxKey{}).(PeerIdentity)
	return id, ok
}

// parseTenantUUIDForTest is a tiny helper invoked by NewForTest so
// the panic message points at the test setup that supplied a bad
// UUID — not at the deeper request flow.
func parseTenantUUIDForTest(s string) (uuid.UUID, error) {
	return uuid.Parse(s)
}
