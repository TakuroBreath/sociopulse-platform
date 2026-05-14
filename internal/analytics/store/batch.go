package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	apianalytics "github.com/sociopulse/platform/internal/analytics/api"
	recordingapi "github.com/sociopulse/platform/internal/recording/api"
)

// ErrNilConn is returned when one of the Insert* helpers is called
// with a nil *Conn. The empty-slice fast path runs BEFORE this check
// so test code can drive empty flushes without bootstrapping a real
// connection — that pattern matters for the ingest pipeline's "drain
// on shutdown" path, where the buffer may legitimately be empty.
var ErrNilConn = errors.New("analytics/store: nil conn")

// InsertCalls binds the supplied AnalyticsCallEventPayload rows into a
// single PrepareBatch against events_calls and Sends it. The column
// list mirrors migrations/clickhouse/000001_events_calls.up.sql tuple
// order (13 columns); changing the migration without updating this
// statement is caught by the schema-shape test in
// batch_integration_test.go.
//
// Contracts:
//   - rows == nil or len(rows) == 0 → returns nil without touching the
//     driver. The ingest pipeline calls this on every flush boundary,
//     including post-drain ones where the buffer is empty.
//   - conn == nil after the empty fast path → ErrNilConn. This guards
//     a programmer error in test code or wiring.
//   - PrepareBatch / Append / Send errors are wrapped with a
//     descriptive %w-chain so callers can errors.Is against the
//     underlying clickhouse-go classification.
//
// defer batch.Close() reaches the resource cleanup path regardless of
// whether Send succeeds — see clickhouse-go README "Batch insert".
func InsertCalls(ctx context.Context, conn *Conn, rows []apianalytics.AnalyticsCallEventPayload) error {
	if len(rows) == 0 {
		return nil
	}
	if conn == nil {
		return ErrNilConn
	}

	const stmt = `INSERT INTO events_calls (
		date, ts, tenant_id, project_id, operator_id, call_id, status,
		duration_sec, hangup_cause, region_code, attempt_no, trunk_used, event_id
	)`

	batch, err := conn.Driver().PrepareBatch(ctx, stmt)
	if err != nil {
		return fmt.Errorf("analytics/store: prepare batch calls: %w", err)
	}
	defer batch.Close()

	for i, r := range rows {
		if err := batch.Append(
			r.Date,
			r.TS,
			r.TenantID,
			r.ProjectID,
			r.OperatorID,
			r.CallID,
			r.Status,
			r.DurationSec,
			r.HangupCause,
			r.RegionCode,
			r.AttemptNo,
			r.TrunkUsed,
			r.EventID,
		); err != nil {
			return fmt.Errorf("analytics/store: append calls row %d: %w", i, err)
		}
	}

	if err := batch.Send(); err != nil {
		return fmt.Errorf("analytics/store: send calls batch: %w", err)
	}
	return nil
}

// InsertOperatorStates binds the supplied
// AnalyticsOperatorStateEventPayload rows into a single PrepareBatch
// against events_operator_state and Sends it. The column list mirrors
// migrations/clickhouse/000002_events_operator_state.up.sql tuple
// order (8 columns).
//
// ProjectID is *uuid.UUID because the CH column is Nullable(UUID) —
// transitions to / from `offline` may carry no project context. The
// clickhouse-go native driver accepts *uuid.UUID for Nullable(UUID)
// columns natively; a nil pointer becomes a SQL NULL.
func InsertOperatorStates(ctx context.Context, conn *Conn, rows []apianalytics.AnalyticsOperatorStateEventPayload) error {
	if len(rows) == 0 {
		return nil
	}
	if conn == nil {
		return ErrNilConn
	}

	const stmt = `INSERT INTO events_operator_state (
		date, ts, tenant_id, user_id, state, duration_in_state_sec, project_id, event_id
	)`

	batch, err := conn.Driver().PrepareBatch(ctx, stmt)
	if err != nil {
		return fmt.Errorf("analytics/store: prepare batch operator_state: %w", err)
	}
	defer batch.Close()

	for i, r := range rows {
		if err := batch.Append(
			r.Date,
			r.TS,
			r.TenantID,
			r.UserID,
			r.State,
			r.DurationInStateSec,
			r.ProjectID,
			r.EventID,
		); err != nil {
			return fmt.Errorf("analytics/store: append operator_state row %d: %w", i, err)
		}
	}

	if err := batch.Send(); err != nil {
		return fmt.Errorf("analytics/store: send operator_state batch: %w", err)
	}
	return nil
}

// InsertRecordingsUploaded binds the supplied RecordingUploadedEvent
// rows into a single PrepareBatch against events_recording_uploaded
// and Sends it. The column list mirrors
// migrations/clickhouse/000003_events_recording_uploaded.up.sql tuple
// order (11 columns excluding _inserted_at).
//
// Two derivations happen here that the producer payload does not
// supply:
//   - date / ts are derived from CommittedAt (unix seconds). CH
//     accepts time.Time for both Date and DateTime64 columns; the
//     driver handles the formatting.
//   - duration_sec is converted from int32 to uint32. Task 1 already
//     clamps DurationSec ≥ 0 at producer time, but a defensive
//     re-clamp here ensures the cast cannot wrap negative values to
//     huge uint32. max() over int32(0) is the modern idiom (Go 1.21+).
func InsertRecordingsUploaded(ctx context.Context, conn *Conn, rows []recordingapi.RecordingUploadedEvent) error {
	if len(rows) == 0 {
		return nil
	}
	if conn == nil {
		return ErrNilConn
	}

	const stmt = `INSERT INTO events_recording_uploaded (
		date, ts, tenant_id, project_id, call_id, fs_node, s3_key,
		size_bytes, duration_sec, encryption_key_alias, event_id
	)`

	batch, err := conn.Driver().PrepareBatch(ctx, stmt)
	if err != nil {
		return fmt.Errorf("analytics/store: prepare batch recordings_uploaded: %w", err)
	}
	defer batch.Close()

	for i, r := range rows {
		ts := time.Unix(r.CommittedAt, 0).UTC()
		date := ts.Format("2006-01-02")

		// Defense-in-depth clamp: Task 1's producer-side invariant is
		// DurationSec ≥ 0, but a hostile re-encoding could violate it.
		// max() pins the floor at 0 before the uint32 cast — the cast
		// itself is safe because the floor guarantees a non-negative
		// int32 (≤ MaxInt32 < MaxUint32). gosec G115 can't see the
		// max() guard; the suppression below is justified by the
		// surrounding clamp.
		durationSec := uint32(max(int32(0), r.DurationSec)) //nolint:gosec // clamped to [0, MaxInt32] by max() above.

		// BytesSize is int64 by Plan 12.1 contract (file sizes are
		// non-negative). The same max() floor + nolint pattern applies.
		bytesSize := uint64(max(int64(0), r.BytesSize)) //nolint:gosec // clamped to [0, MaxInt64] by max() above.

		if err := batch.Append(
			date,
			ts,
			r.TenantID,
			r.ProjectID,
			r.CallID,
			r.FSNode,
			r.S3Key,
			bytesSize,
			durationSec,
			r.EncryptionKeyAlias,
			r.EventID,
		); err != nil {
			return fmt.Errorf("analytics/store: append recordings_uploaded row %d: %w", i, err)
		}
	}

	if err := batch.Send(); err != nil {
		return fmt.Errorf("analytics/store: send recordings_uploaded batch: %w", err)
	}
	return nil
}
