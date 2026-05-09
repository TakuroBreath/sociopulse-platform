// Package service implements the recording module's RecordingService.
// Plan 12.1 (Foundation): Commit + Get only. Search / OpenAudioStream /
// VerifyChecksum return wrapped api.ErrInvalidInput with the marker
// "not implemented in foundation phase" until Plan 12.2 / 12.3.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/metrics"
	"github.com/sociopulse/platform/internal/recording/store"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// Sentinel re-exports keep callers in this package idiomatic without a hop
// through `rapi.`. External callers should still errors.Is against the rapi
// sentinels — the aliases below preserve identity.
var (
	ErrInvalidInput   = rapi.ErrInvalidInput
	ErrCallNotFound   = rapi.ErrCallNotFound
	ErrNotFound       = rapi.ErrNotFound
	ErrTenantMismatch = rapi.ErrTenantMismatch
)

const (
	sha256HexLen         = 64
	maxEncryptedDEKBytes = 4 * 1024
	auditActorKindIngest = "service"
)

// Deps wires the service. Pool + Store are required. Logger and Metrics
// may be nil — the implementation is nil-safe.
type Deps struct {
	Pool    *postgres.Pool
	Store   *store.PostgresStore
	Outbox  outbox.Writer
	Logger  *zap.Logger
	Metrics *metrics.RecordingMetrics
}

type svc struct {
	pool    *postgres.Pool
	store   *store.PostgresStore
	outbox  outbox.Writer
	logger  *zap.Logger
	metrics *metrics.RecordingMetrics
}

// Compile-time interface check — guards against contract drift.
var _ rapi.RecordingService = (*svc)(nil)

// New constructs the service. Returns a nil-safe instance even if
// Logger / Metrics are not provided.
func New(d Deps) rapi.RecordingService {
	if d.Logger == nil {
		d.Logger = zap.NewNop()
	}
	if d.Outbox == nil {
		d.Outbox = outbox.NewPostgresWriter()
	}
	return &svc{
		pool:    d.Pool,
		store:   d.Store,
		outbox:  d.Outbox,
		logger:  d.Logger,
		metrics: d.Metrics,
	}
}

// Commit performs the full end-to-end commit flow:
//  1. Validate input.
//  2. Begin Tx via WithTenant (RLS-aware; recording rows live in the tenant
//     namespace; outbox table is owned by tenancy_admin but app has CRUD).
//  3. INSERT (idempotent on call_id) → returns row + replay flag.
//  4. On fresh insert: append audit row + outbox event in same Tx.
//  5. Commit Tx.
//  6. Tick metrics.
//
// The outbox-relay drains rows to JetStream asynchronously; the caller does
// NOT block on NATS ack. Replay path skips audit + outbox so downstream
// subscribers see exactly one event per recording.
func (s *svc) Commit(ctx context.Context, in rapi.CommitInput) (rapi.CommitOutput, error) {
	// Single time.Now() reading: `now` doubles as the metrics-timer base AND
	// as the row's committed_at. Splitting these into separate Now() calls
	// would let the test-asserted CommitOutput.CommittedAt drift away from
	// the metrics-tagged duration baseline by a few microseconds — a latent
	// hazard for tests that compare them.
	now := time.Now()
	tenantLabel := in.TenantID.String()

	if err := validateCommit(in); err != nil {
		s.metrics.ObserveCommit(tenantLabel, "invalid", time.Since(now).Seconds())
		return rapi.CommitOutput{}, fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
	}

	row := storeRowFromInput(in, now.UTC())

	var (
		out    rapi.CommitOutput
		replay bool
	)
	err := s.pool.WithTenant(ctx, in.TenantID, func(tx postgres.Tx) error {
		inserted, didReplay, err := s.store.InsertRecordingIdempotent(ctx, tx, row)
		if err != nil {
			return err
		}
		replay = didReplay
		out = rapi.CommitOutput{
			RecordingID:      inserted.ID,
			CallID:           inserted.CallID,
			CommittedAt:      inserted.CommittedAt,
			IdempotentReplay: didReplay,
		}

		if didReplay {
			return nil // skip audit + outbox so downstream sees exactly one event
		}

		if err := writeAuditRow(ctx, tx, inserted); err != nil {
			return fmt.Errorf("audit insert: %w", err)
		}

		ev, err := buildOutboxEvent(inserted)
		if err != nil {
			return fmt.Errorf("build outbox event: %w", err)
		}
		if err := s.outbox.Append(ctx, tx, ev); err != nil {
			return fmt.Errorf("outbox append: %w", err)
		}
		return nil
	})

	dur := time.Since(now).Seconds()
	switch {
	case errors.Is(err, store.ErrCallNotFound):
		s.metrics.ObserveCommit(tenantLabel, "call_not_found", dur)
		return rapi.CommitOutput{}, ErrCallNotFound
	case err != nil:
		s.metrics.ObserveCommit(tenantLabel, "error", dur)
		return rapi.CommitOutput{}, fmt.Errorf("recording.commit: %w", err)
	}

	if replay {
		s.metrics.ObserveCommit(tenantLabel, "replay", dur)
		s.logger.Info("recording commit idempotent replay",
			zap.String("tenant_id", tenantLabel),
			zap.String("call_id", in.CallID.String()),
			zap.String("recording_id", out.RecordingID.String()))
	} else {
		s.metrics.ObserveCommit(tenantLabel, "ok", dur)
		s.metrics.AddStorageBytes(tenantLabel, in.BytesSize)
		s.logger.Info("recording committed",
			zap.String("tenant_id", tenantLabel),
			zap.String("call_id", in.CallID.String()),
			zap.String("recording_id", out.RecordingID.String()),
			zap.Int64("bytes", in.BytesSize),
			zap.String("sha256", in.SHA256Hex))
	}
	return out, nil
}

// Get is a thin pass-through to store.GetByCallID with row→DTO mapping.
func (s *svc) Get(ctx context.Context, tenantID, callID uuid.UUID) (rapi.RecordingMetadata, error) {
	r, err := s.store.GetByCallID(ctx, tenantID, callID)
	if errors.Is(err, store.ErrCallNotFound) {
		return rapi.RecordingMetadata{}, ErrNotFound
	}
	if err != nil {
		return rapi.RecordingMetadata{}, fmt.Errorf("recording.get: %w", err)
	}

	// store.RecordingRow.DeleteAt is *time.Time; api.RecordingMetadata.DeleteAt
	// is time.Time. Plan 12.1 always commits with non-nil DeleteAt, but be
	// defensive: nil → zero time (callers can treat zero as "no deletion").
	var deleteAt time.Time
	if r.DeleteAt != nil {
		deleteAt = *r.DeleteAt
	}

	return rapi.RecordingMetadata{
		RecordingID:    r.ID,
		CallID:         r.CallID,
		TenantID:       r.TenantID,
		S3Bucket:       r.S3Bucket,
		AudioObjectKey: r.AudioObjectKey,
		BytesSize:      r.BytesSize,
		Duration:       time.Duration(r.DurationMS) * time.Millisecond,
		SHA256Hex:      r.SHA256Hex,
		Status:         r.Status,
		CommittedAt:    r.CommittedAt,
		DeleteAt:       deleteAt,
		ColdAt:         r.ColdAt,
		VerifiedAt:     r.VerifiedAt,
	}, nil
}

func (s *svc) Search(_ context.Context, _ uuid.UUID, _ rapi.SearchQuery) (rapi.SearchResult, error) {
	return rapi.SearchResult{}, fmt.Errorf("%w: Search not implemented in foundation phase", ErrInvalidInput)
}

func (s *svc) OpenAudioStream(_ context.Context, _, _ uuid.UUID, _ *rapi.ByteRange) (rapi.AudioStream, error) {
	return rapi.AudioStream{}, fmt.Errorf("%w: OpenAudioStream not implemented in foundation phase", ErrInvalidInput)
}

func (s *svc) VerifyChecksum(_ context.Context, _, _ uuid.UUID) (rapi.VerifyResult, error) {
	return rapi.VerifyResult{}, fmt.Errorf("%w: VerifyChecksum not implemented in foundation phase", ErrInvalidInput)
}

// ----- helpers -----

func validateCommit(in rapi.CommitInput) error {
	switch {
	case in.TenantID == uuid.Nil:
		return errors.New("tenant_id required")
	case in.CallID == uuid.Nil:
		return errors.New("call_id required")
	case in.S3Bucket == "":
		return errors.New("s3_bucket required")
	case in.AudioObjectKey == "":
		return errors.New("audio_object_key required")
	case in.KMSKeyID == "":
		return errors.New("kms_key_id required")
	case len(in.EncryptedDEK) == 0:
		return errors.New("encrypted_dek required")
	case len(in.EncryptedDEK) > maxEncryptedDEKBytes:
		return fmt.Errorf("encrypted_dek too large: max %d bytes", maxEncryptedDEKBytes)
	case in.BytesSize <= 0:
		return errors.New("bytes_size must be > 0")
	case in.Duration <= 0:
		return errors.New("duration must be > 0")
	case len(in.SHA256Hex) != sha256HexLen:
		return fmt.Errorf("sha256 length: want %d hex chars, got %d", sha256HexLen, len(in.SHA256Hex))
	case in.Codec == "":
		return errors.New("codec required")
	case in.SampleRate <= 0:
		return errors.New("sample_rate must be > 0")
	case in.DeleteAt.IsZero():
		return errors.New("delete_at required (retention plan must be resolved)")
	case in.ColdAt.IsZero():
		return errors.New("cold_at required")
	case in.RecordedAt.IsZero():
		return errors.New("recorded_at required")
	}
	return nil
}

func storeRowFromInput(in rapi.CommitInput, committedAt time.Time) store.RecordingRow {
	var dekKey *string
	if in.DEKObjectKey != "" {
		k := in.DEKObjectKey
		dekKey = &k
	}
	deleteAt := in.DeleteAt
	return store.RecordingRow{
		ID:             uuid.Must(uuid.NewV7()),
		CallID:         in.CallID,
		TenantID:       in.TenantID,
		S3Bucket:       in.S3Bucket,
		AudioObjectKey: in.AudioObjectKey,
		DEKObjectKey:   dekKey,
		KMSKeyID:       in.KMSKeyID,
		EncryptedDEK:   in.EncryptedDEK,
		BytesSize:      in.BytesSize,
		DurationMS:     in.Duration.Milliseconds(),
		SHA256Hex:      in.SHA256Hex,
		Codec:          in.Codec,
		SampleRate:     in.SampleRate, // already int32 in api.CommitInput — no cast needed
		Status:         "stored",
		CommittedAt:    committedAt,
		DeleteAt:       &deleteAt, // validation guaranteed non-zero
		ColdAt:         in.ColdAt,
		RecordedAt:     in.RecordedAt,
		IngestAgentID:  in.IngestAgentID,
	}
}

// writeAuditRow inserts one audit_log row inside tx.
//
// Actual audit_log schema (migrations/000001_init.up.sql):
//
//	id bigserial primary key  (db-assigned — omitted from INSERT)
//	tenant_id uuid not null
//	actor_kind text not null check (actor_kind in ('user','system','service'))
//	actor_user_id uuid        (nullable — nil for service actors)
//	action text not null
//	target_kind text not null
//	target_id text            (nullable text, not uuid)
//	payload jsonb
//	ts timestamptz not null default now()
//	ip text
//	user_agent text
func writeAuditRow(ctx context.Context, tx postgres.Tx, r store.RecordingRow) error {
	payload, err := json.Marshal(map[string]any{
		"recording_id":     r.ID,
		"call_id":          r.CallID,
		"sha256":           r.SHA256Hex,
		"bytes_size":       r.BytesSize,
		"kms_key_id":       r.KMSKeyID,
		"audio_object_key": r.AudioObjectKey,
		"ingest_agent_id":  r.IngestAgentID,
	})
	if err != nil {
		return fmt.Errorf("marshal audit payload: %w", err)
	}

	// actor_user_id is uuid nullable — use nil for service actors.
	// target_id is text — pass the recording UUID as a string.
	// ts has a default but we pass the committed_at so audit and commit share
	// the same timestamp; ts default now() is a fallback if we omit it.
	const q = `
INSERT INTO audit_log (tenant_id, actor_kind, actor_user_id, action, target_kind, target_id, payload, ts)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
`
	_, err = tx.Exec(ctx, q,
		r.TenantID,
		auditActorKindIngest,
		nil, // actor_user_id — service actor has no user uuid
		rapi.AuditActionCommitted,
		"recording",
		r.ID.String(), // target_id is text
		payload,
		r.CommittedAt,
	)
	return err
}

func buildOutboxEvent(r store.RecordingRow) (outbox.Event, error) {
	payload, err := json.Marshal(rapi.RecordingUploadedEvent{
		RecordingID: r.ID,
		CallID:      r.CallID,
		TenantID:    r.TenantID,
		BytesSize:   r.BytesSize,
		DurationMS:  r.DurationMS,
		SHA256Hex:   r.SHA256Hex,
		Status:      r.Status,
		CommittedAt: r.CommittedAt.Unix(),
	})
	if err != nil {
		return outbox.Event{}, fmt.Errorf("marshal outbox payload: %w", err)
	}
	tenantID := r.TenantID
	callID := r.CallID
	return outbox.Event{
		TenantID:    &tenantID,
		AggregateID: &callID,
		Subject:     rapi.SubjectRecordingUploadedFor(r.TenantID),
		Payload:     payload,
	}, nil
}
