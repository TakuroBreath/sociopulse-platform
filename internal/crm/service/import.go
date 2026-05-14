package service

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"go.uber.org/zap"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	"github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/pkg/eventbus"
	"github.com/sociopulse/platform/pkg/postgres"
)

// importBatchSize is the number of rows the import handler buffers
// before issuing one InsertBatch (CopyFrom) round-trip. 1000 balances
// memory (≈ a few MB per batch) against COPY round-trips (one per
// thousand rows = ~100 RTs for the 100k cap).
const importBatchSize = 1000

// importMaxRows is the absolute cap on a single import payload.
// Defense in depth: the HTTP boundary (Plan 06 Task 5) enforces a
// MaxBodyBytes equivalent; this is the second-line check inside the
// handler so a misbehaving caller cannot smuggle a multi-million-row
// CSV through a misconfigured proxy. Matches the 100 000 cap quoted
// in plan-06-crm.md § Open questions.
const importMaxRows = 100000

// importStatusTTL is how long the Redis status hash for one job stays
// live after the import finishes. 7 days is enough for ops to
// retroactively investigate a failure without making the Redis hash
// pollute storage forever.
const importStatusTTL = 7 * 24 * time.Hour

// importPayloadInlineLimit is the maximum byte size we accept on the
// asynq task payload's `Body` field today (v1, before the S3 swap in
// Plan 12). asynq enqueues over Redis; oversized payloads thrash the
// command timeout. 50 MiB is plenty for 100 000 phone rows including
// Excel's formatting overhead, and matches the HTTP-layer
// importMaxBodyBytes so a payload that passed the gateway never
// surprises the worker.
const importPayloadInlineLimit = 50 * 1024 * 1024

// ImportRow is the parsed-but-not-yet-validated representation of one
// row in a CSV/XLSX import file. The parser packages emit these on a
// channel; the import handler reads from the channel, normalises the
// phone, dedupes, and inserts.
//
// Defined in the service package (not in api/) because it is a private
// implementation detail — external modules drive imports through
// api.ImportRequest, never row-by-row.
type ImportRow struct {
	// LineNumber is the 1-indexed source line. The header row, if
	// present, is line 1; the first data row is line 2. Used for
	// surfacing "row X failed because <reason>" in the per-import
	// error list.
	LineNumber int
	// Phone is the raw phone string as the file presented it. The
	// import handler normalises it via NormalizeRussianPhone before
	// hashing.
	Phone string
	// FullName is the optional respondent display name; when present,
	// it lives inside the JSON attributes blob alongside any other
	// provided columns (see Attributes).
	FullName string
	// ExternalRef is the optional caller-provided external identifier
	// (e.g. customer CRM id). Lives in attributes when set.
	ExternalRef string
	// Attributes is the bag of remaining named columns from the file,
	// minus phone/full_name/external_ref. Strings preserved as-is —
	// the parser does not interpret any cell content beyond the phone.
	Attributes map[string]any
}

// importTaskPayload is the JSON-encoded payload asynq carries from the
// enqueue site to the handler. We keep the body inline for v1; once
// Plan 12 wires Object Storage we swap Body for a BlobKey reference
// (and add a presigned-URL fetch step inside the handler).
type importTaskPayload struct {
	JobID     string            `json:"job_id"`
	TenantID  uuid.UUID         `json:"tenant_id"`
	ProjectID uuid.UUID         `json:"project_id"`
	Format    api.ImportFormat  `json:"format"`
	Source    string            `json:"source"`
	Filename  string            `json:"filename,omitempty"`
	Body      []byte            `json:"body"`
	Defaults  map[string]any    `json:"defaults,omitempty"`
	ColMap    map[string]string `json:"col_map,omitempty"`
	StartedAt time.Time         `json:"started_at"`
}

// importEnqueuer is the small slice of asynq.Client the import service
// consumes. We define the consumer-side interface rather than passing
// *asynq.Client directly so tests can substitute a record-only fake
// without booting a Redis server. Mirrors the project's pattern for
// other transactional dependencies (see projectTxRunner).
type importEnqueuer interface {
	Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error)
}

// progressTracker is the small interface the import handler uses to
// publish status updates to Redis and NATS. The concrete
// *ProgressTracker (import_progress.go) implements it; tests inject a
// recording fake.
type progressTracker interface {
	Init(ctx context.Context, jobID string, tenantID uuid.UUID, total int) error
	Update(ctx context.Context, jobID string, tenantID uuid.UUID, processed, inserted, skipped int) error
	Finish(ctx context.Context, jobID string, tenantID uuid.UUID, total, inserted, skipped int) error
	Fail(ctx context.Context, jobID string, tenantID uuid.UUID, errMsg string) error
	Status(ctx context.Context, jobID string) (*api.ImportStatus, error)
}

// configureImport is a private setter used by NewRespondentService's
// optional-deps companions. The two new fields (enqueuer, tracker) are
// passed via dedicated WithEnqueuer/WithProgress functional options
// rather than expanding the constructor signature; that keeps the
// existing callers intact and isolates Task 4 wiring from Task 3.

// WithEnqueuer attaches an asynq client (or a fake) to the
// RespondentService. When unset, Import returns ErrImportNotFound on
// every call — useful for unit tests that exercise Get/Search and
// don't care about the import path.
func (s *RespondentService) WithEnqueuer(enq importEnqueuer) *RespondentService {
	s.enqueuer = enq
	return s
}

// WithProgress attaches a progress tracker. When unset, Import returns
// an error on every call. Tests inject a fake to capture Init/Finish/
// Fail without booting Redis.
func (s *RespondentService) WithProgress(p progressTracker) *RespondentService {
	s.progress = p
	return s
}

// WithLogger attaches a zap logger so the import handler can emit
// structured progress / failure logs. Optional; nil falls back to
// zap.NewNop().
func (s *RespondentService) WithLogger(log *zap.Logger) *RespondentService {
	if log == nil {
		log = zap.NewNop()
	}
	s.logger = log
	return s
}

// WithEventBus attaches a NATS publisher for the import.* progress
// events. Optional; nil disables event publishing — Redis status hash
// remains the source of truth.
func (s *RespondentService) WithEventBus(p eventbus.Publisher) *RespondentService {
	s.events = p
	return s
}

// Import enqueues an asynq task that runs the import asynchronously.
// The synchronous response carries the job id; the actual progress is
// observed via GetImportStatus and the import.* NATS events.
//
// Idempotency: when the caller supplies a JobID that was already
// enqueued (and the original task is still within asynq.Unique TTL),
// the second enqueue is dropped silently and the existing ticket is
// returned. This lets clients implement at-least-once retry without
// double-importing.
//
// Validation:
//   - TenantID, ProjectID must be non-nil.
//   - Format must be csv or xlsx.
//   - Source, when set, must be SourceImported (RDD imports are not
//     a user-facing path; the operator UI only emits Imported).
//   - Body must be non-empty and ≤ importPayloadInlineLimit bytes.
func (s *RespondentService) Import(ctx context.Context, req api.ImportRequest) (*api.ImportTicket, error) {
	if err := validateImportRequest(req); err != nil {
		return nil, err
	}
	if s.enqueuer == nil || s.progress == nil {
		// Composition root never registered the import path. Returning
		// an explicit error is more useful to a caller than panicking
		// or silently swallowing the request.
		return nil, fmt.Errorf("crm/service: import: %w", api.ErrInvalidArgument)
	}

	jobID := req.JobID
	if jobID == "" {
		jobID = uuid.NewString()
	}
	source := req.Source
	if source == "" {
		source = api.SourceImported
	}

	startedAt := s.clock()
	if initErr := s.progress.Init(ctx, jobID, req.TenantID, 0); initErr != nil {
		return nil, fmt.Errorf("crm/service: import: init progress: %w", initErr)
	}

	payload := importTaskPayload{
		JobID:     jobID,
		TenantID:  req.TenantID,
		ProjectID: req.ProjectID,
		Format:    req.Format,
		Source:    source,
		Filename:  req.Filename,
		Body:      req.Body,
		Defaults:  req.DefaultAttrs,
		ColMap:    req.ColumnMap,
		StartedAt: startedAt,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("crm/service: import: encode payload: %w", err)
	}

	task := asynq.NewTask(api.TaskRespondentImport, encoded)
	info, err := s.enqueuer.Enqueue(task,
		asynq.TaskID(jobID),
		asynq.Queue("crm"),
		asynq.MaxRetry(3),
		asynq.Unique(time.Hour),
	)
	switch {
	case errors.Is(err, asynq.ErrDuplicateTask), errors.Is(err, asynq.ErrTaskIDConflict):
		// Idempotent retry: someone (possibly the same client) already
		// enqueued this job id; treat as success.
		return &api.ImportTicket{
			JobID:     jobID,
			ProjectID: req.ProjectID,
			Enqueued:  false,
			Status:    "queued",
			StartedAt: startedAt,
		}, nil
	case err != nil:
		// Best-effort cleanup of the Redis status hash; the user can
		// always retry, and a stale "pending" entry without a queued
		// task is recoverable.
		_ = s.progress.Fail(ctx, jobID, req.TenantID, "enqueue failed: "+err.Error())
		return nil, fmt.Errorf("crm/service: import: enqueue: %w", err)
	}

	if aerr := s.writeAudit(ctx, auditapi.Event{
		TenantID: req.TenantID,
		Action:   "crm.respondents.import.queued",
		Target:   "project:" + req.ProjectID.String(),
		Payload: map[string]any{
			"job_id":     jobID,
			"project_id": req.ProjectID,
			"format":     string(req.Format),
			"size":       len(req.Body),
		},
	}); aerr != nil {
		s.logger.Warn("audit write failed", zap.String("action", "crm.respondents.import.queued"), zap.Error(aerr))
	}

	s.logger.Info("respondent import enqueued",
		zap.String("job_id", jobID),
		zap.String("task_id", info.ID),
		zap.String("queue", info.Queue),
	)

	return &api.ImportTicket{
		JobID:     jobID,
		ProjectID: req.ProjectID,
		Enqueued:  true,
		Status:    "queued",
		StartedAt: startedAt,
	}, nil
}

// GetImportStatus reads the Redis status hash for jobID and returns the
// canonical ImportStatus DTO. Returns ErrImportNotFound when the job
// is unknown (TTL elapsed, never enqueued).
func (s *RespondentService) GetImportStatus(ctx context.Context, jobID string) (*api.ImportStatus, error) {
	if s.progress == nil {
		return nil, fmt.Errorf("crm/service: get import status: %w", api.ErrInvalidArgument)
	}
	if strings.TrimSpace(jobID) == "" {
		return nil, fmt.Errorf("crm/service: get import status: empty job id: %w", api.ErrInvalidArgument)
	}
	st, err := s.progress.Status(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// HandleImportTask is the asynq.Handler-shaped entry point for the
// crm:respondent.import task. The composition root (module.go)
// registers it on a ServeMux. Exported so tests can invoke it directly
// and assert behaviour without a full asynq server.
func (s *RespondentService) HandleImportTask(ctx context.Context, t *asynq.Task) error {
	var p importTaskPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("crm/service: handle import: decode payload: %w", err)
	}
	if err := validateTaskPayload(p); err != nil {
		// Invalid payload — return SkipRetry so asynq won't keep
		// poking at it. The handler emits a "failed" status so
		// observability surfaces the bad payload.
		_ = s.progress.Fail(ctx, p.JobID, p.TenantID, "invalid payload: "+err.Error())
		return fmt.Errorf("crm/service: handle import: %w (skip retry: %w)", err, asynq.SkipRetry)
	}

	logger := s.logger.With(
		zap.String("job_id", p.JobID),
		zap.String("project_id", p.ProjectID.String()),
		zap.String("format", string(p.Format)),
	)
	logger.Info("import handler started")

	rows, parseErr := parseImportPayload(p)
	if parseErr != nil {
		_ = s.progress.Fail(ctx, p.JobID, p.TenantID, "parse failed: "+parseErr.Error())
		return fmt.Errorf("crm/service: handle import: parse: %w (skip retry: %w)", parseErr, asynq.SkipRetry)
	}

	totals := importTotals{}
	totals.total = len(rows)
	if err := s.progress.Init(ctx, p.JobID, p.TenantID, totals.total); err != nil {
		logger.Warn("progress init failed", zap.Error(err))
	}

	for batchStart := 0; batchStart < len(rows); batchStart += importBatchSize {
		select {
		case <-ctx.Done():
			_ = s.progress.Fail(ctx, p.JobID, p.TenantID, "cancelled: "+ctx.Err().Error())
			return fmt.Errorf("crm/service: handle import: context cancelled: %w", ctx.Err())
		default:
		}
		batchEnd := batchStart + importBatchSize
		if batchEnd > len(rows) {
			batchEnd = len(rows)
		}
		batch := rows[batchStart:batchEnd]
		batchInserted, batchSkipped, batchErr := s.processBatch(ctx, p, batch)
		if batchErr != nil {
			_ = s.progress.Fail(ctx, p.JobID, p.TenantID, batchErr.Error())
			return fmt.Errorf("crm/service: handle import: batch starting at %d: %w", batchStart, batchErr)
		}
		totals.processed += len(batch)
		totals.inserted += batchInserted
		totals.skipped += batchSkipped

		if perr := s.progress.Update(ctx, p.JobID, p.TenantID, totals.processed, totals.inserted, totals.skipped); perr != nil {
			logger.Warn("progress update failed", zap.Error(perr))
		}
	}

	if err := s.progress.Finish(ctx, p.JobID, p.TenantID, totals.total, totals.inserted, totals.skipped); err != nil {
		logger.Warn("progress finish failed", zap.Error(err))
	}
	logger.Info("import handler finished",
		zap.Int("total", totals.total),
		zap.Int("inserted", totals.inserted),
		zap.Int("skipped", totals.skipped),
	)
	return nil
}

// importTotals tracks the running counts across batches inside one
// HandleImportTask invocation.
type importTotals struct {
	total     int
	processed int
	inserted  int
	skipped   int
}

// stagedRow bundles the per-row state computed in stage 1 (normalise +
// hash + intra-batch dedupe) so stage 2 (tx-scoped persistence) can
// consume it without re-running expensive work.
type stagedRow struct {
	e164    string
	hash    []byte
	region  string
	attrs   map[string]any
	hashKey string
}

// processBatch normalises, dedupes, encrypts, and bulk-inserts one
// batch of ImportRows. Returns (inserted, skipped, error). Errors at
// this level are infrastructural (DB down, KMS unavailable) and bubble
// up so asynq retries the task; per-row failures (bad phone, DNC, dup)
// count toward the skipped total and don't abort the batch.
func (s *RespondentService) processBatch(ctx context.Context, p importTaskPayload, batch []ImportRow) (int, int, error) {
	if len(batch) == 0 {
		return 0, 0, nil
	}
	staged, skipped, err := s.stageBatch(ctx, p, batch)
	if err != nil {
		return 0, 0, err
	}
	if len(staged) == 0 {
		return 0, skipped, nil
	}

	hashes := make([][]byte, len(staged))
	for i := range staged {
		hashes[i] = staged[i].hash
	}

	var inserted int
	err = s.tx.WithTenant(ctx, p.TenantID, func(tx postgres.Tx) error {
		insertable, dbSkipped, ferr := s.filterAgainstDB(ctx, tx, p, staged, hashes)
		if ferr != nil {
			return ferr
		}
		skipped += dbSkipped
		if len(insertable) == 0 {
			return nil
		}
		n, perr := s.persistBatch(ctx, tx, p, insertable)
		if perr != nil {
			return perr
		}
		inserted += n
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return inserted, skipped, nil
}

// stageBatch performs the in-memory stage 1 work: phone normalisation,
// hashing, and within-file dedup. Returns the staged slice + the count
// of rows skipped at this stage. Hasher errors are infrastructural and
// bubble up; bad-phone / intra-batch-dup count as skipped.
func (s *RespondentService) stageBatch(ctx context.Context, p importTaskPayload, batch []ImportRow) ([]stagedRow, int, error) {
	staged := make([]stagedRow, 0, len(batch))
	seenHash := make(map[string]struct{}, len(batch))
	skipped := 0
	for _, r := range batch {
		np, perr := NormalizeRussianPhone(r.Phone)
		if perr != nil {
			skipped++
			continue
		}
		hash, herr := s.hasher.Hash(ctx, p.TenantID, np.E164)
		if herr != nil {
			return nil, 0, fmt.Errorf("hash phone: %w", herr)
		}
		key := bytesToHex(hash)
		if _, dup := seenHash[key]; dup {
			skipped++
			continue
		}
		seenHash[key] = struct{}{}
		region := np.Region
		if rc, ok := r.Attributes["region_code"].(string); ok && strings.TrimSpace(rc) != "" {
			region = strings.TrimSpace(rc)
		}
		staged = append(staged, stagedRow{
			e164:    np.E164,
			hash:    hash,
			region:  region,
			attrs:   buildAttributes(r, p.Defaults),
			hashKey: key,
		})
	}
	return staged, skipped, nil
}

// filterAgainstDB runs the existing-hash dedup and DNC check inside the
// caller's tx. Returns the rows that survive both filters plus the
// count of additional skips.
func (s *RespondentService) filterAgainstDB(ctx context.Context, tx postgres.Tx, p importTaskPayload, staged []stagedRow, hashes [][]byte) ([]stagedRow, int, error) {
	existing, eerr := s.store.ExistingHashes(ctx, tx, p.TenantID, p.ProjectID, hashes)
	if eerr != nil {
		return nil, 0, fmt.Errorf("existing hashes: %w", eerr)
	}
	exists := make(map[string]struct{}, len(existing))
	for _, h := range existing {
		exists[bytesToHex(h)] = struct{}{}
	}

	insertable := make([]stagedRow, 0, len(staged))
	skipped := 0
	for _, sr := range staged {
		if _, dup := exists[sr.hashKey]; dup {
			skipped++
			continue
		}
		blocked, derr := s.store.IsBlockedDNC(ctx, tx, p.TenantID, p.ProjectID, sr.hash)
		if derr != nil {
			return nil, 0, fmt.Errorf("dnc check: %w", derr)
		}
		if blocked {
			skipped++
			continue
		}
		insertable = append(insertable, sr)
	}
	return insertable, skipped, nil
}

// persistBatch encrypts the surviving rows and runs the COPY insert.
// Returns the count of inserted rows.
//
// Plan 13.2.5 Task 6: the per-row UUID is minted client-side BEFORE the
// KMS Encrypt call so the AAD bound into the ciphertext reproduces at
// decrypt time. The COPY then writes the same ID into respondents.id.
func (s *RespondentService) persistBatch(ctx context.Context, tx postgres.Tx, p importTaskPayload, insertable []stagedRow) (int, error) {
	respondents := make([]api.Respondent, 0, len(insertable))
	for _, sr := range insertable {
		respondentID := uuid.New()
		ciphertext, eerr := s.kms.Encrypt(ctx, p.TenantID, respondentPhoneAADScope, respondentID.String(), []byte(sr.e164))
		if eerr != nil {
			return 0, fmt.Errorf("encrypt phone: %w", eerr)
		}
		respondents = append(respondents, api.Respondent{
			ID:             respondentID,
			TenantID:       p.TenantID,
			ProjectID:      p.ProjectID,
			PhoneEncrypted: ciphertext,
			PhoneHash:      sr.hash,
			RegionCode:     sr.region,
			Attributes:     sr.attrs,
			Status:         api.RespPending,
			Source:         p.Source,
		})
	}
	n, ierr := s.store.InsertBatch(ctx, tx, respondents)
	if ierr != nil {
		return 0, fmt.Errorf("insert batch: %w", ierr)
	}
	return n, nil
}

// buildAttributes merges the row's parsed attributes with the request
// defaults. Per-row keys win on collision (the operator's CSV is the
// authoritative source). Always returns non-nil so the JSONB column
// stays NOT NULL.
func buildAttributes(r ImportRow, defaults map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range r.Attributes {
		out[k] = v
	}
	if r.FullName != "" {
		out["full_name"] = r.FullName
	}
	if r.ExternalRef != "" {
		out["external_ref"] = r.ExternalRef
	}
	return out
}

// bytesToHex returns a stable string key for an in-memory dedup map.
// We use encoding/hex directly — its allocation profile for a single
// short hash is identical to a hand-rolled strings.Builder loop, but
// the stdlib version is more obviously correct and one less thing for
// future readers to second-guess.
func bytesToHex(b []byte) string {
	return hex.EncodeToString(b)
}

// validateImportRequest enforces the synchronous-rejection invariants
// on ImportRequest. Callers receive ErrInvalidArgument /
// ErrImportFormatUnsupported / ErrImportPayloadTooBig before any work
// is enqueued.
func validateImportRequest(req api.ImportRequest) error {
	if req.TenantID == uuid.Nil {
		return fmt.Errorf("crm/service: import: tenant id required: %w", api.ErrInvalidArgument)
	}
	if req.ProjectID == uuid.Nil {
		return fmt.Errorf("crm/service: import: project id required: %w", api.ErrInvalidArgument)
	}
	switch req.Format {
	case api.ImportFormatCSV, api.ImportFormatXLSX:
		// ok
	default:
		return fmt.Errorf("crm/service: import: format %q: %w", req.Format, api.ErrImportFormatUnsupported)
	}
	if req.Source != "" && req.Source != api.SourceImported {
		return fmt.Errorf("crm/service: import: invalid source %q: %w", req.Source, api.ErrInvalidArgument)
	}
	if len(req.Body) == 0 {
		return fmt.Errorf("crm/service: import: empty body: %w", api.ErrInvalidArgument)
	}
	if len(req.Body) > importPayloadInlineLimit {
		return fmt.Errorf("crm/service: import: body %d bytes exceeds limit %d: %w",
			len(req.Body), importPayloadInlineLimit, api.ErrImportPayloadTooBig)
	}
	return nil
}

// validateTaskPayload sanity-checks the asynq payload after JSON
// decode. Mirrors validateImportRequest but is permissive about
// JobID/Source defaults — those are filled in at Import-time, never
// inside the worker.
func validateTaskPayload(p importTaskPayload) error {
	if p.JobID == "" {
		return errors.New("missing job id")
	}
	if p.TenantID == uuid.Nil {
		return errors.New("missing tenant id")
	}
	if p.ProjectID == uuid.Nil {
		return errors.New("missing project id")
	}
	switch p.Format {
	case api.ImportFormatCSV, api.ImportFormatXLSX:
		// ok
	default:
		return fmt.Errorf("unsupported format %q", p.Format)
	}
	if len(p.Body) == 0 {
		return errors.New("empty body")
	}
	return nil
}

// parseImportPayload dispatches to the format-specific parser and
// returns the materialised ImportRow slice. We materialise (rather
// than streaming row-by-row to the handler) so the per-import 100 000
// row cap can be checked once, before any DB work, and so the batch
// loop can re-slice in fixed-size groups.
func parseImportPayload(p importTaskPayload) ([]ImportRow, error) {
	if len(p.Body) == 0 {
		return nil, errors.New("empty payload body")
	}
	switch p.Format {
	case api.ImportFormatCSV:
		return parseCSV(bytes.NewReader(p.Body))
	case api.ImportFormatXLSX:
		return parseXLSX(bytes.NewReader(p.Body))
	default:
		return nil, fmt.Errorf("unsupported format %q", p.Format)
	}
}
