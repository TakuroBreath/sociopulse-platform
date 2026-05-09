package grpcserver

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	rpb "github.com/sociopulse/platform/internal/recording/proto/v1"
)

// Commit is the proto→service translator for RecordingService.Commit.
//
// Wire path:
//
//  1. Resolve the verified peer identity (interceptor stamps it on ctx).
//  2. Parse req.tenant_id; reject when missing/invalid.
//  3. Cross-check the request tenant against the cert tenant — this
//     is the gRPC-layer half of cross-tenant isolation; the database
//     RLS row filter is the second half.
//  4. Translate proto fields into rapi.CommitInput. The IngestAgentID
//     is OVERRIDDEN from the cert so a misbehaving caller cannot
//     forge audit-log provenance.
//  5. Delegate to svc.Commit and map sentinel errors back to gRPC
//     status codes.
func (s *Server) Commit(ctx context.Context, req *rpb.CommitRequest) (*rpb.CommitResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	identity, ok := peerIdentityFromCtx(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "peer identity missing from context")
	}

	tenantID, err := uuid.Parse(req.GetTenantId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "tenant_id: %v", err)
	}
	if tenantID != identity.TenantID {
		return nil, status.Errorf(codes.PermissionDenied,
			"tenant_id %s does not match SPIFFE identity tenant %s",
			tenantID, identity.TenantID)
	}

	callID, err := uuid.Parse(req.GetCallId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "call_id: %v", err)
	}

	// proto Duration / Timestamp pointers may be nil — service
	// validation rejects zero values, but we still want a
	// codes.InvalidArgument (not codes.Internal) for that path.
	in := rapi.CommitInput{
		TenantID:       tenantID,
		CallID:         callID,
		S3Bucket:       req.GetS3Bucket(),
		AudioObjectKey: req.GetAudioObjectKey(),
		DEKObjectKey:   req.GetDekObjectKey(),
		KMSKeyID:       req.GetKmsKeyId(),
		EncryptedDEK:   req.GetEncryptedDek(),
		BytesSize:      req.GetBytesSize(),
		Duration:       req.GetDuration().AsDuration(),
		SHA256Hex:      req.GetSha256(),
		Codec:          req.GetCodec(),
		SampleRate:     req.GetSampleRate(),
		DeleteAt:       req.GetDeleteAt().AsTime(),
		ColdAt:         req.GetColdAt().AsTime(),
		// Override agent id from the verified cert — the wire field is
		// informational/legacy; the cert is the source of truth.
		IngestAgentID: identity.AgentID,
		RecordedAt:    req.GetRecordedAt().AsTime(),
	}

	out, err := s.svc.Commit(ctx, in)
	if err != nil {
		return nil, mapCommitError(err)
	}

	if s.logger != nil {
		s.logger.Debug("recording.Commit ok",
			zap.String("tenant_id", tenantID.String()),
			zap.String("call_id", callID.String()),
			zap.String("recording_id", out.RecordingID.String()),
			zap.Bool("idempotent_replay", out.IdempotentReplay),
		)
	}

	return &rpb.CommitResponse{
		RecordingId:      out.RecordingID.String(),
		CallId:           out.CallID.String(),
		CommittedAt:      timestamppb.New(out.CommittedAt),
		IdempotentReplay: out.IdempotentReplay,
	}, nil
}

// Get is the proto→service translator for RecordingService.Get.
// Same identity-check flow as Commit; on success the
// rapi.RecordingMetadata is mapped to a GetResponse with
// duration/timestamp wrappers.
func (s *Server) Get(ctx context.Context, req *rpb.GetRequest) (*rpb.GetResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	identity, ok := peerIdentityFromCtx(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "peer identity missing from context")
	}

	tenantID, err := uuid.Parse(req.GetTenantId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "tenant_id: %v", err)
	}
	if tenantID != identity.TenantID {
		return nil, status.Errorf(codes.PermissionDenied,
			"tenant_id %s does not match SPIFFE identity tenant %s",
			tenantID, identity.TenantID)
	}

	callID, err := uuid.Parse(req.GetCallId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "call_id: %v", err)
	}

	md, err := s.svc.Get(ctx, tenantID, callID)
	if err != nil {
		return nil, mapGetError(err)
	}

	resp := &rpb.GetResponse{
		RecordingId:    md.RecordingID.String(),
		CallId:         md.CallID.String(),
		TenantId:       md.TenantID.String(),
		S3Bucket:       md.S3Bucket,
		AudioObjectKey: md.AudioObjectKey,
		BytesSize:      md.BytesSize,
		Duration:       durationpb.New(md.Duration),
		Sha256:         md.SHA256Hex,
		Status:         md.Status,
		CommittedAt:    timestamppb.New(md.CommittedAt),
		DeleteAt:       timestamppb.New(md.DeleteAt),
		ColdAt:         timestamppb.New(md.ColdAt),
	}
	if md.VerifiedAt != nil {
		resp.VerifiedAt = timestamppb.New(*md.VerifiedAt)
	}
	return resp, nil
}

// mapCommitError translates an rapi.* sentinel error to a gRPC
// status. errors.Is is used so wrapped errors (e.g.
// `fmt.Errorf("%w: ...", ErrInvalidInput)` in service.Commit)
// resolve correctly.
func mapCommitError(err error) error {
	switch {
	case errors.Is(err, rapi.ErrInvalidInput):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, rapi.ErrCallNotFound):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, rapi.ErrTenantMismatch):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, rapi.ErrAlreadyDeleted):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

// mapGetError translates an rapi.* sentinel error from svc.Get.
// ErrNotFound → NotFound; tenant mismatch comes through ParsePeerIdentity
// before we hit Get, so we don't need to map it here.
func mapGetError(err error) error {
	switch {
	case errors.Is(err, rapi.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, rapi.ErrTenantMismatch):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, rapi.ErrInvalidInput):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
