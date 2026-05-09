// Package service implements the recording module's RecordingService.
// Plan 12.1 (Foundation): Commit + Get implemented; Search / VerifyChecksum
// return wrapped api.ErrInvalidInput with the marker "not implemented in
// foundation phase" until Plan 12.3.
// Plan 12.2 Task 4: OpenAudioStream is wired to the crypto + storage ports
// (AudioDecryptor, DEKUnwrapper, ObjectStore) and writes a recording.accessed
// audit row + access metrics on every call.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	rapi "github.com/sociopulse/platform/internal/recording/api"
	"github.com/sociopulse/platform/internal/recording/crypto"
	"github.com/sociopulse/platform/internal/recording/metrics"
	"github.com/sociopulse/platform/internal/recording/storage"
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
	ErrAlreadyDeleted = rapi.ErrAlreadyDeleted
)

const (
	sha256HexLen         = 64
	maxEncryptedDEKBytes = 4 * 1024
	auditActorKindIngest = "service"
)

// Deps wires the service. Pool + Store are required. Logger and Metrics
// may be nil — the implementation is nil-safe.
//
// Plan 12.2 Task 4 added the crypto + storage ports:
//   - Decryptor defaults to crypto.NewAESGCMDecryptor() when nil.
//   - KMS and Objects have no defaults; OpenAudioStream returns
//     ErrInvalidInput ("not wired") if either is nil. The Commit / Get
//     paths do not need them.
type Deps struct {
	Pool      *postgres.Pool
	Store     *store.PostgresStore
	Outbox    outbox.Writer
	Logger    *zap.Logger
	Metrics   *metrics.RecordingMetrics
	Decryptor crypto.AudioDecryptor
	KMS       crypto.DEKUnwrapper
	Objects   storage.ObjectStore
}

type svc struct {
	pool      *postgres.Pool
	store     *store.PostgresStore
	outbox    outbox.Writer
	logger    *zap.Logger
	metrics   *metrics.RecordingMetrics
	decryptor crypto.AudioDecryptor
	kms       crypto.DEKUnwrapper
	objects   storage.ObjectStore
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
	if d.Decryptor == nil {
		d.Decryptor = crypto.NewAESGCMDecryptor()
	}
	return &svc{
		pool:      d.Pool,
		store:     d.Store,
		outbox:    d.Outbox,
		logger:    d.Logger,
		metrics:   d.Metrics,
		decryptor: d.Decryptor,
		kms:       d.KMS,
		objects:   d.Objects,
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

// OpenAudioStream returns a streamed, decrypted reader for the audio.
// byteRange is IGNORED in v1 — Plan 12.3 sets Accept-Ranges: none.
// Future v2 chunked-envelope format will support ranges natively.
//
// Pipeline:
//  1. Lookup row by (tenantID, callID).
//  2. Bail if status == 'deleted'.
//  3. Unwrap DEK via the KMS port, AAD = tenant_id bytes.
//  4. GET ciphertext stream from object store.
//  5. AES-GCM decrypt with AAD = tenant_id bytes (full-buffer for v1).
//  6. Write recording.accessed audit row (failure → log + metric, NOT a hard fail).
//  7. Return AudioStream with bytes.Reader over plaintext.
func (s *svc) OpenAudioStream(ctx context.Context, tenantID, callID uuid.UUID, _ *rapi.ByteRange) (rapi.AudioStream, error) {
	if s.kms == nil || s.objects == nil {
		return rapi.AudioStream{}, fmt.Errorf("%w: recording crypto/storage not wired", ErrInvalidInput)
	}

	start := time.Now()
	tenantLabel := tenantID.String()

	row, err := s.store.GetByCallID(ctx, tenantID, callID)
	if errors.Is(err, store.ErrCallNotFound) {
		s.metrics.ObserveAccess(tenantLabel, "not_found", time.Since(start).Seconds())
		return rapi.AudioStream{}, ErrNotFound
	}
	if err != nil {
		s.metrics.ObserveAccess(tenantLabel, "error", time.Since(start).Seconds())
		return rapi.AudioStream{}, fmt.Errorf("recording.open_audio: %w", err)
	}
	if row.Status == "deleted" {
		s.metrics.ObserveAccess(tenantLabel, "deleted", time.Since(start).Seconds())
		return rapi.AudioStream{}, ErrAlreadyDeleted
	}

	aad := []byte(row.TenantID.String())

	dekPlain, err := s.kms.DecryptDEK(ctx, row.KMSKeyID, row.EncryptedDEK, aad)
	if err != nil {
		s.metrics.ObserveAccess(tenantLabel, "kms_error", time.Since(start).Seconds())
		return rapi.AudioStream{}, fmt.Errorf("recording.open_audio.kms: %w", err)
	}
	defer zeroBytes(dekPlain)

	rc, err := s.objects.Get(ctx, row.S3Bucket, row.AudioObjectKey)
	if err != nil {
		s.metrics.ObserveAccess(tenantLabel, "object_error", time.Since(start).Seconds())
		if errors.Is(err, storage.ErrObjectNotFound) {
			return rapi.AudioStream{}, ErrNotFound // hide storage shape from API consumers
		}
		return rapi.AudioStream{}, fmt.Errorf("recording.open_audio.object: %w", err)
	}
	defer rc.Close()

	plain, err := s.decryptor.Decrypt(ctx, dekPlain, rc, row.BytesSize, aad)
	if err != nil {
		s.metrics.ObserveAccess(tenantLabel, "decrypt_error", time.Since(start).Seconds())
		return rapi.AudioStream{}, fmt.Errorf("recording.open_audio.decrypt: %w", err)
	}

	if err := s.writeAccessAudit(ctx, row); err != nil {
		// Audit failure must NOT block playback — log + tick + continue.
		s.logger.Warn("recording access audit failed",
			zap.String("tenant_id", tenantLabel),
			zap.String("call_id", callID.String()),
			zap.Error(err))
		s.metrics.ObserveAccess(tenantLabel, "audit_failed", time.Since(start).Seconds())
	} else {
		s.metrics.ObserveAccess(tenantLabel, "ok", time.Since(start).Seconds())
	}

	return rapi.AudioStream{
		Reader:        io.NopCloser(bytes.NewReader(plain)),
		ContentType:   contentTypeForCodec(row.Codec),
		ContentLength: int64(len(plain)),
		StartOffset:   0,
		EndOffset:     int64(len(plain)) - 1,
	}, nil
}

func (s *svc) VerifyChecksum(_ context.Context, _, _ uuid.UUID) (rapi.VerifyResult, error) {
	return rapi.VerifyResult{}, fmt.Errorf("%w: VerifyChecksum not implemented in foundation phase", ErrInvalidInput)
}

// ----- helpers -----

// validateCommit composes three sub-validators to keep cyclomatic complexity
// below the project's gocyclo cap (15). The split is by concern: identity
// fields (tenant/call), provenance fields (S3 + KMS + DEK), and content
// fields (sizes + timestamps + codec). Returns the FIRST failure to mirror
// the original switch's short-circuit behaviour.
func validateCommit(in rapi.CommitInput) error {
	if err := validateCommitIdentity(in); err != nil {
		return err
	}
	if err := validateCommitProvenance(in); err != nil {
		return err
	}
	return validateCommitContent(in)
}

// validateCommitIdentity rejects malformed tenant/call references — the
// surface area that drives RLS + the call_recordings UNIQUE constraint.
func validateCommitIdentity(in rapi.CommitInput) error {
	switch {
	case in.TenantID == uuid.Nil:
		return errors.New("tenant_id required")
	case in.CallID == uuid.Nil:
		return errors.New("call_id required")
	}
	return nil
}

// validateCommitProvenance rejects malformed S3 + KMS + DEK fields.
func validateCommitProvenance(in rapi.CommitInput) error {
	switch {
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
	}
	return nil
}

// validateCommitContent rejects malformed audio-content fields (sizes, codec,
// retention plan timestamps).
func validateCommitContent(in rapi.CommitInput) error {
	switch {
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

// contentTypeForCodec returns the canonical MIME for our supported codecs.
// "opus" / "opus-32" → audio/ogg (OGG container is the in-storage wrapping
// the ingest-uploader produces). Anything else falls back to a generic
// octet-stream so an HTTP layer doesn't fabricate a misleading Content-Type.
func contentTypeForCodec(codec string) string {
	switch codec {
	case "opus", "opus-32":
		return "audio/ogg"
	default:
		return "application/octet-stream"
	}
}

// zeroBytes overwrites the buffer with zeros. Best-effort hygiene against
// the DEK plaintext lingering in heap memory longer than necessary. Go's
// GC may still hold a copy; cryptographic claims here are weak.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// writeAccessAudit appends an audit_log row recording who fetched what.
// Single-statement INSERT inside WithTenant — caller doesn't need
// rollback semantics (playback proceeds even on audit failure).
//
// Mirrors the column order in writeAuditRow above so the schema-shape
// (verified during Plan 12.1 Task 4) stays consistent across both
// audit emitters from this service.
//   - actor_kind = 'service' (service actor, not a user)
//   - actor_user_id = nil (service actor has no user uuid)
//   - target_id is text — pass r.ID.String()
//   - ts = now() server-side so the audit row's stamp is the time the
//     audit was committed, not the time the recording was originally made.
func (s *svc) writeAccessAudit(ctx context.Context, r store.RecordingRow) error {
	payload, err := json.Marshal(map[string]any{
		"recording_id":     r.ID,
		"call_id":          r.CallID,
		"audio_object_key": r.AudioObjectKey,
		"bytes_size":       r.BytesSize,
		"sha256":           r.SHA256Hex,
	})
	if err != nil {
		return fmt.Errorf("marshal audit payload: %w", err)
	}
	return s.pool.WithTenant(ctx, r.TenantID, func(tx postgres.Tx) error {
		const q = `
INSERT INTO audit_log (tenant_id, actor_kind, actor_user_id, action, target_kind, target_id, payload, ts)
VALUES ($1, $2, $3, $4, $5, $6, $7, now())
`
		_, err := tx.Exec(ctx, q,
			r.TenantID,
			auditActorKindIngest, // 'service'
			nil,                  // actor_user_id — service actor has no user uuid
			rapi.AuditActionAccessed,
			"recording",
			r.ID.String(), // target_id is text
			payload,
		)
		return err
	})
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
