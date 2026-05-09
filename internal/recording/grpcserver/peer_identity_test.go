package grpcserver

import (
	"crypto/x509"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Tests live in the package (`grpcserver`, not `grpcserver_test`) so
// they can exercise the unexported parseSpiffeURL helper directly —
// callers see only the exported ParsePeerIdentity which takes a full
// x509.Certificate. Building a cert from scratch in every test would
// add nothing.

func TestParsePeerIdentity_HappyPath(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	uri := mustURL(t, "spiffe://sociopulse/ingest-agent/agent-42?tenant="+tenantID.String())

	id, err := parseSpiffeURL(uri)
	require.NoError(t, err)
	require.Equal(t, tenantID, id.TenantID)
	require.Equal(t, "agent-42", id.AgentID)
	require.Equal(t, uri.String(), id.URI)
}

func TestParsePeerIdentity_NoURI(t *testing.T) {
	t.Parallel()

	// Cert with no URI SAN → ParsePeerIdentity (the wrapper) returns
	// an error. We exercise the wrapper directly here so we cover the
	// "no URI SAN" branch — parseSpiffeURL would never be reached.
	cert := &x509.Certificate{}
	_, err := ParsePeerIdentity(cert)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no URI SAN")
}

func TestParsePeerIdentity_WrongScheme(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	uri := mustURL(t, "https://sociopulse/ingest-agent/agent-42?tenant="+tenantID.String())

	_, err := parseSpiffeURL(uri)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported scheme")
}

func TestParsePeerIdentity_BadTenant(t *testing.T) {
	t.Parallel()
	uri := mustURL(t, "spiffe://sociopulse/ingest-agent/agent-42?tenant=not-a-uuid")

	_, err := parseSpiffeURL(uri)
	require.Error(t, err)
	require.Contains(t, err.Error(), "tenant uuid")
}

func TestParsePeerIdentity_BadIssuer(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	uri := mustURL(t, "spiffe://other-issuer/ingest-agent/agent-42?tenant="+tenantID.String())

	_, err := parseSpiffeURL(uri)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported issuer")
}

func TestParsePeerIdentity_BadPath(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	uri := mustURL(t, "spiffe://sociopulse/api/agent-42?tenant="+tenantID.String())

	_, err := parseSpiffeURL(uri)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not start with")
}

func TestParsePeerIdentity_MissingTenant(t *testing.T) {
	t.Parallel()
	uri := mustURL(t, "spiffe://sociopulse/ingest-agent/agent-42")

	_, err := parseSpiffeURL(uri)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing tenant")
}

func TestParsePeerIdentity_FromCert(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	uri := mustURL(t, "spiffe://sociopulse/ingest-agent/agent-42?tenant="+tenantID.String())

	cert := &x509.Certificate{URIs: []*url.URL{uri}}
	id, err := ParsePeerIdentity(cert)
	require.NoError(t, err)
	require.Equal(t, tenantID, id.TenantID)
	require.Equal(t, "agent-42", id.AgentID)
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(raw, u.Scheme+"://"))
	return u
}
