// Package grpcserver implements the mTLS-fronted gRPC façade over
// internal/recording/api.RecordingService. It is consumed by
// cmd/recording-uploader (Plan 08 / future) and by integration tests.
//
// The server lives in a separate package — not under
// internal/recording/transport/grpc — so the import edge stays
// recording/grpcserver → recording/api → recording/service. The
// service package is NEVER imported here; the handlers receive the
// rapi.RecordingService interface directly.
//
// Plan 12.1 Task 5 wires:
//
//   - mTLS (TLS 1.3, RequireAndVerifyClientCert) with a CA-rooted
//     client cert chain.
//   - Per-call SPIFFE identity extraction (peer_identity.go) stashed
//     on ctx by an interceptor.
//   - Two unary handlers: Commit + Get (commit_handler.go).
//
// The constructor wiring is intentionally lenient: missing cert
// paths produce Enabled()==false and the caller (Module.Register)
// skips the listener entirely. Production boots load the certs and
// fail loudly via tls.LoadX509KeyPair errors.
package grpcserver

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	rpb "github.com/sociopulse/platform/internal/recording/proto/v1"
)

// Defaults keep the Config zero-value usable for tests. Production
// supplies non-zero values via cfg.Recording from pkg/config.
const (
	defaultMaxRecvBytes = 4 * 1024 * 1024 // 4 MiB; CommitRequest is ~kb-scale
	defaultTimeout      = 30 * time.Second

	// keepaliveTime is how often the server pings idle connections.
	// 30s mirrors gRPC's recommended default and is well below most
	// k8s ingress idle timeouts.
	keepaliveTime    = 30 * time.Second
	keepaliveTimeout = 10 * time.Second
)

// Config groups the construction-time parameters of the gRPC server.
// Zero-valued fields fall back to safe defaults for MaxRecvBytes and
// Timeout. ListenAddr + the three TLS paths are required for
// production — Enabled() is the caller's gate.
type Config struct {
	// ListenAddr is the host:port to bind. Empty => disabled.
	ListenAddr string

	// TLS material. mTLS is required: the server presents
	// (TLSCertFile, TLSKeyFile) and verifies clients against TLSCAFile.
	TLSCertFile string
	TLSKeyFile  string
	TLSCAFile   string

	// MaxRecvBytes caps per-message size (default 4 MiB).
	MaxRecvBytes int

	// Timeout caps the per-call wall time (default 30s). Currently
	// informational — hooked up via context.WithTimeout in the handlers
	// in a future hardening pass; today the gRPC keepalive + the
	// caller's deadline carry the load.
	Timeout time.Duration
}

// Enabled reports whether the config has every required field
// populated. False means cmd/api should skip the listener — typical
// in dev/test where TLS material isn't provisioned.
func (c Config) Enabled() bool {
	return c.ListenAddr != "" && c.TLSCertFile != "" && c.TLSKeyFile != "" && c.TLSCAFile != ""
}

// Server is the gRPC façade in front of rapi.RecordingService.
// Holds a *grpc.Server plus the listening socket; both are owned by
// Module.Start in production.
type Server struct {
	rpb.UnimplementedRecordingServiceServer

	cfg     Config
	svc     rapi.RecordingService
	logger  *zap.Logger
	grpc    *grpc.Server
	timeout time.Duration

	mu       sync.Mutex
	listener net.Listener // non-nil between Serve / GracefulStop
}

// New constructs a Server from a real Config — including loading the
// X.509 cert and CA pool. Returns a non-nil error if any TLS file
// can't be read; the caller (Module.Register) decides whether that's
// fatal-to-boot or a degraded log + skip.
//
// svc and logger are the per-process singletons; logger may be nil —
// New replaces it with zap.NewNop().
func New(cfg Config, svc rapi.RecordingService, logger *zap.Logger) (*Server, error) {
	if svc == nil {
		return nil, errors.New("grpcserver: RecordingService is required")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if cfg.ListenAddr == "" {
		return nil, errors.New("grpcserver: ListenAddr is required")
	}

	creds, err := loadServerCreds(cfg)
	if err != nil {
		return nil, fmt.Errorf("grpcserver: load TLS: %w", err)
	}

	maxRecv := cfg.MaxRecvBytes
	if maxRecv <= 0 {
		maxRecv = defaultMaxRecvBytes
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	gs := grpc.NewServer(
		grpc.Creds(creds),
		grpc.MaxRecvMsgSize(maxRecv),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    keepaliveTime,
			Timeout: keepaliveTimeout,
		}),
		grpc.ChainUnaryInterceptor(peerTenantInterceptor()),
	)

	s := &Server{
		cfg:     cfg,
		svc:     svc,
		logger:  logger,
		grpc:    gs,
		timeout: timeout,
	}
	rpb.RegisterRecordingServiceServer(gs, s)
	return s, nil
}

// NewForTest constructs a Server suitable for bufconn-driven unit
// tests: no TLS, no listener, and a fixed PeerIdentity injected into
// every incoming RPC by a fake interceptor. tenantID becomes the
// PeerIdentity.TenantID stamped on ctx.
//
// Tests then call Serve(*bufconn.Listener) themselves.
func NewForTest(svc rapi.RecordingService, tenantID interface{ String() string }, logger *zap.Logger) *Server {
	if logger == nil {
		logger = zap.NewNop()
	}

	parsedTenantID, err := parseTenantUUIDForTest(tenantID.String())
	if err != nil {
		// Tests should pass valid UUIDs; panic surfaces the wiring bug
		// loudly rather than emitting opaque PERMISSION_DENIED later.
		panic(fmt.Sprintf("grpcserver.NewForTest: invalid tenantID: %v", err))
	}
	identity := PeerIdentity{
		TenantID: parsedTenantID,
		AgentID:  "test-agent",
		URI:      fmt.Sprintf("spiffe://sociopulse/ingest-agent/test-agent?tenant=%s", parsedTenantID),
	}

	gs := grpc.NewServer(
		grpc.MaxRecvMsgSize(defaultMaxRecvBytes),
		grpc.ChainUnaryInterceptor(fakePeerIdentityInterceptor(identity)),
	)

	s := &Server{
		cfg:     Config{},
		svc:     svc,
		logger:  logger,
		grpc:    gs,
		timeout: defaultTimeout,
	}
	rpb.RegisterRecordingServiceServer(gs, s)
	return s
}

// ServeAddr opens a TCP listener on cfg.ListenAddr and serves until
// GracefulStop. Returns nil on graceful close (grpc.ErrServerStopped),
// otherwise the listener / Serve error.
func (s *Server) ServeAddr() error {
	lis, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("grpcserver: listen %s: %w", s.cfg.ListenAddr, err)
	}
	return s.Serve(lis)
}

// Serve runs the gRPC loop on the supplied listener. Used by both
// production (TCP) and tests (bufconn).
func (s *Server) Serve(lis net.Listener) error {
	s.mu.Lock()
	s.listener = lis
	s.mu.Unlock()

	s.logger.Info("recording gRPC listener up",
		zap.String("addr", lis.Addr().String()))

	err := s.grpc.Serve(lis)
	if err == nil || errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

// GracefulStop drains in-flight RPCs and shuts down the listener.
// Safe to call before Serve — the underlying *grpc.Server treats it
// as a no-op in that case.
func (s *Server) GracefulStop() {
	s.grpc.GracefulStop()
}

// Stop is the abrupt-close variant kept for symmetry with future
// shutdown paths that need a deadline. Currently unused by Module.
func (s *Server) Stop() {
	s.grpc.Stop()
}

// loadServerCreds builds the TransportCredentials for a TLS 1.3 mTLS
// handshake. The CA pool seeds ClientCAs; ClientAuth is set to
// RequireAndVerifyClientCert so any unauthenticated connection is
// rejected at the TLS layer (well before the gRPC handler runs).
func loadServerCreds(cfg Config) (credentials.TransportCredentials, error) {
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" || cfg.TLSCAFile == "" {
		return nil, errors.New("TLSCertFile, TLSKeyFile, TLSCAFile are all required")
	}

	cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load server cert: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.TLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("CA file: no certificates parsed")
	}

	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}
	return credentials.NewTLS(tlsCfg), nil
}

