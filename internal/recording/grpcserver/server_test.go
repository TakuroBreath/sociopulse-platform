package grpcserver_test

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/grpcserver"
	rpb "github.com/sociopulse/platform/internal/recording/proto/v1"
)

// bufconnBufSize matches the default used elsewhere in the repo for
// bufconn-driven tests; the value is informational — the buffer
// drains synchronously on each Read.
const bufconnBufSize = 1 << 20

// TestServer_Commit_DelegatesToService is the canonical happy-path
// flow: bufconn-dialled gRPC client → fake service → asserted reply.
// Verifies that the proto fields land on rapi.CommitInput and that
// rapi.CommitOutput round-trips back into CommitResponse with
// timestamppb wrapping.
func TestServer_Commit_DelegatesToService(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	committedAt := time.Date(2026, 5, 9, 12, 30, 0, 0, time.UTC)
	recordingID := uuid.New()

	fake := &fakeRecordingService{
		commitOut: rapi.CommitOutput{
			RecordingID:      recordingID,
			CommittedAt:      committedAt,
			IdempotentReplay: false,
		},
	}
	client := bufconnClient(t, fake, tenantID)

	req := validProtoCommit(tenantID)
	resp, err := client.Commit(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, recordingID.String(), resp.GetRecordingId())
	require.False(t, resp.GetIdempotentReplay())
	require.Equal(t, committedAt.Unix(), resp.GetCommittedAt().AsTime().Unix())

	require.Equal(t, int32(1), atomic.LoadInt32(&fake.commitCalls))
	// IngestAgentID is overridden from the cert — the test agent is
	// "test-agent" per NewForTest's fake interceptor.
	require.Equal(t, "test-agent", fake.lastCommit.IngestAgentID)
	// Tenant + call ids round-trip from proto strings to uuid.UUID.
	require.Equal(t, tenantID, fake.lastCommit.TenantID)
}

// TestServer_Commit_TenantMismatch checks that a request whose
// tenant_id differs from the SPIFFE identity tenant is rejected
// with PERMISSION_DENIED — the gRPC half of cross-tenant isolation.
func TestServer_Commit_TenantMismatch(t *testing.T) {
	t.Parallel()

	identityTenant := uuid.New()
	requestTenant := uuid.New()
	require.NotEqual(t, identityTenant, requestTenant)

	fake := &fakeRecordingService{}
	client := bufconnClient(t, fake, identityTenant)

	req := validProtoCommit(requestTenant)
	_, err := client.Commit(t.Context(), req)
	require.Error(t, err)
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// The handler MUST short-circuit before the service call — fake
	// records zero invocations.
	require.Equal(t, int32(0), atomic.LoadInt32(&fake.commitCalls))
}

// TestServer_Commit_BadCallID checks that a non-uuid call_id maps to
// INVALID_ARGUMENT. tenant_id is valid + matches the identity, so we
// test only the call_id branch.
func TestServer_Commit_BadCallID(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	fake := &fakeRecordingService{}
	client := bufconnClient(t, fake, tenantID)

	req := validProtoCommit(tenantID)
	req.CallId = "not-a-uuid"
	_, err := client.Commit(t.Context(), req)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, int32(0), atomic.LoadInt32(&fake.commitCalls))
}

// TestServer_Commit_ServiceCallNotFound checks that
// rapi.ErrCallNotFound from the service layer maps to
// FAILED_PRECONDITION (per the proto contract).
func TestServer_Commit_ServiceCallNotFound(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	fake := &fakeRecordingService{
		commitErr: rapi.ErrCallNotFound,
	}
	client := bufconnClient(t, fake, tenantID)

	req := validProtoCommit(tenantID)
	_, err := client.Commit(t.Context(), req)
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Equal(t, int32(1), atomic.LoadInt32(&fake.commitCalls))
}

// TestServer_Commit_ServiceInvalidInput checks that
// rapi.ErrInvalidInput from the service layer maps to
// INVALID_ARGUMENT — distinct from the call-id parse failure
// above.
func TestServer_Commit_ServiceInvalidInput(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	fake := &fakeRecordingService{
		commitErr: rapi.ErrInvalidInput,
	}
	client := bufconnClient(t, fake, tenantID)

	req := validProtoCommit(tenantID)
	_, err := client.Commit(t.Context(), req)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, int32(1), atomic.LoadInt32(&fake.commitCalls))
}

// bufconnClient wires up an in-memory grpc.ClientConn over bufconn,
// dials it, and registers t.Cleanup to gracefully tear everything
// down. svc is the fake RecordingService injected behind the gRPC
// handlers; tenantID is stamped on the fake PeerIdentity (see
// grpcserver.NewForTest).
func bufconnClient(t *testing.T, svc rapi.RecordingService, tenantID uuid.UUID) rpb.RecordingServiceClient {
	t.Helper()

	lis := bufconn.Listen(bufconnBufSize)
	srv := grpcserver.NewForTest(svc, tenantID, zaptest.NewLogger(t))

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- srv.Serve(lis) }()

	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		// Close client first so in-flight RPCs unwind, then stop the
		// server so the goroutine returns. Drain serveErrCh so goleak
		// sees the goroutine exit before TestMain's verify runs.
		require.NoError(t, conn.Close())
		srv.GracefulStop()
		_ = lis.Close()
		select {
		case err := <-serveErrCh:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) && !errors.Is(err, io.ErrClosedPipe) {
				t.Errorf("server.Serve returned unexpected error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("server.Serve did not return after GracefulStop")
		}
	})

	return rpb.NewRecordingServiceClient(conn)
}

// validProtoCommit returns a CommitRequest populated with values
// that pass both proto- and service-layer validation. Tests that
// want to exercise a specific failure mode mutate one field after
// calling this.
func validProtoCommit(tenantID uuid.UUID) *rpb.CommitRequest {
	now := time.Now().UTC()
	return &rpb.CommitRequest{
		TenantId:       tenantID.String(),
		CallId:         uuid.New().String(),
		S3Bucket:       "sociopulse-recordings-test",
		AudioObjectKey: "recordings/x/y.opus.enc",
		DekObjectKey:   "recordings/x/y.dek.enc",
		KmsKeyId:       "kms-key-1",
		EncryptedDek:   []byte("encrypted-dek-bytes"),
		BytesSize:      12345,
		Duration:       durationpb.New(15 * time.Second),
		Sha256:         "f1e2d3c4b5a697887766554433221100ffeeddccbbaa99887766554433221100",
		Codec:          "opus",
		SampleRate:     48000,
		DeleteAt:       timestamppb.New(now.Add(720 * time.Hour)),
		ColdAt:         timestamppb.New(now.Add(360 * time.Hour)),
		IngestAgentId:  "wire-claimed-agent-should-be-ignored",
		RecordedAt:     timestamppb.New(now),
	}
}

// fakeRecordingService is a hand-rolled rapi.RecordingService used
// by every server_test case — keeps the test deps zero (no testify-
// mock generation) and lets us count calls + assert on the input.
type fakeRecordingService struct {
	commitOut rapi.CommitOutput
	commitErr error

	getOut rapi.RecordingMetadata
	getErr error

	commitCalls int32
	getCalls    int32

	// lastCommit is captured on every Commit so tests can assert on
	// the input the handler synthesised.
	lastCommit rapi.CommitInput
}

var _ rapi.RecordingService = (*fakeRecordingService)(nil)

func (f *fakeRecordingService) Commit(_ context.Context, in rapi.CommitInput) (rapi.CommitOutput, error) {
	atomic.AddInt32(&f.commitCalls, 1)
	f.lastCommit = in
	if f.commitErr != nil {
		return rapi.CommitOutput{}, f.commitErr
	}
	out := f.commitOut
	if out.CallID == uuid.Nil {
		out.CallID = in.CallID
	}
	return out, nil
}

func (f *fakeRecordingService) Get(_ context.Context, _, _ uuid.UUID) (rapi.RecordingMetadata, error) {
	atomic.AddInt32(&f.getCalls, 1)
	if f.getErr != nil {
		return rapi.RecordingMetadata{}, f.getErr
	}
	return f.getOut, nil
}

func (f *fakeRecordingService) Search(_ context.Context, _ uuid.UUID, _ rapi.SearchQuery) (rapi.SearchResult, error) {
	return rapi.SearchResult{}, rapi.ErrInvalidInput
}

func (f *fakeRecordingService) OpenAudioStream(_ context.Context, _, _ uuid.UUID, _ *rapi.ByteRange) (rapi.AudioStream, error) {
	return rapi.AudioStream{}, rapi.ErrNotFound
}

func (f *fakeRecordingService) VerifyChecksum(_ context.Context, _, _ uuid.UUID) (rapi.VerifyResult, error) {
	return rapi.VerifyResult{}, rapi.ErrNotFound
}
