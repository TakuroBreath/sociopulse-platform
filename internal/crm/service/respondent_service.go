package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	"github.com/sociopulse/platform/internal/crm/api"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/pkg/eventbus"
	"github.com/sociopulse/platform/pkg/postgres"
)

// respondentTxRunner is the cross-tenant transaction owner the
// RespondentService uses. *postgres.Pool satisfies this interface
// via its WithTenant + BypassRLS methods; tests substitute an in-
// memory implementation that invokes fn with a zero postgres.Tx.
//
// Defined here at the consumer per project convention (see
// project_service.go projectTxRunner). BypassRLS is used by the
// Get/GetWithPhone/Delete entry points where the caller supplies an
// id but not necessarily the tenant — the service then resolves the
// tenant from the row via a BypassRLS GetByID, then runs the actual
// per-tenant work through WithTenant so RLS still scopes any joined
// reads.
type respondentTxRunner interface {
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error
	BypassRLS(ctx context.Context, fn func(postgres.Tx) error) error
}

// RespondentService implements api.RespondentService.
//
// Plan 06 Task 3 wired the Create path (sync single-add). Task 4 added
// the async CSV/XLSX import. Task 5 (this commit) fills in
// Get/GetWithPhone/Search/Delete with the 152-ФЗ subject-rights
// semantics: 30-day soft-delete grace + admin-only PII reveal.
//
// Security boundary: every write goes through KMSResolver.Encrypt for
// the at-rest phone ciphertext and PhoneHasher.Hash for the
// indexed-lookup hash. The plaintext phone NEVER lands in the audit
// payload, the response DTO's Phone field (except via the
// admin-gated GetWithPhone which itself audits the access), or any
// zap logger field — this is enforced by tests (json.Marshal-then-
// Contains check on the audit row) and by reviewer scrutiny.
type RespondentService struct {
	tx     respondentTxRunner
	store  api.RespondentStorePort
	kms    tenancyapi.KMSResolver
	hasher tenancyapi.PhoneHasher
	audit  auditapi.Logger
	clock  func() time.Time
	// Optional, attached via With* setters by the composition root.
	// Tests inject hand-rolled fakes per behaviour-under-test.
	enqueuer importEnqueuer
	progress progressTracker
	events   eventbus.Publisher
	logger   *zap.Logger
}

// Compile-time assertion: *RespondentService must satisfy api.RespondentService.
var _ api.RespondentService = (*RespondentService)(nil)

// deletionGracePeriod is the canonical 152-ФЗ §21 grace window
// between Delete (soft-delete) and PurgeOlderThan (hard-delete). 30
// days lets operators reverse an accidental delete; the purge worker
// runs once a day and removes rows whose deleted_at is past this
// window. The constant is exported as a package-level so the worker,
// the service, and tests share one source of truth.
const deletionGracePeriod = 30 * 24 * time.Hour

// respondentPhoneAADScope is the AAD scope passed to KMSResolver
// Encrypt/Decrypt for respondent phone ciphertexts (Plan 13.2.5 Task 6).
// Bound into the AEAD authentication tag so a phone ciphertext cannot be
// spliced across rows / columns even when the per-tenant DEK is shared.
const respondentPhoneAADScope = "crm.respondent.phone"

// defaultSearchPageSize / maxSearchPageSize are the pagination clamps
// the Search service applies before calling the store. The values
// match the Plan 05 conventions for List endpoints.
const (
	defaultSearchPageSize = 50
	maxSearchPageSize     = 500
)

// NewRespondentService constructs a RespondentService.
//
// All six service-typed dependencies are mandatory — a misconfigured
// composition root that registered nil would silently drop audit rows
// and bypass encryption, both of which violate 152-ФЗ. We fail loudly
// at construction time rather than at first call.
//
// clock is optional: nil falls back to time.Now. Tests inject a frozen
// clock so timestamps are deterministic.
func NewRespondentService(
	pool respondentTxRunner,
	store api.RespondentStorePort,
	kms tenancyapi.KMSResolver,
	hasher tenancyapi.PhoneHasher,
	auditLogger auditapi.Logger,
	clock func() time.Time,
) *RespondentService {
	if pool == nil {
		panic("crm/service: NewRespondentService: pool is required")
	}
	if store == nil {
		panic("crm/service: NewRespondentService: store is required")
	}
	if kms == nil {
		panic("crm/service: NewRespondentService: kms is required (use a no-op fake in tests, never nil)")
	}
	if hasher == nil {
		panic("crm/service: NewRespondentService: hasher is required (use a no-op fake in tests, never nil)")
	}
	if auditLogger == nil {
		panic("crm/service: NewRespondentService: auditLogger is required (use a no-op fake in tests, never nil)")
	}
	if clock == nil {
		clock = time.Now
	}
	return &RespondentService{
		tx:     pool,
		store:  store,
		kms:    kms,
		hasher: hasher,
		audit:  auditLogger,
		clock:  clock,
		logger: zap.NewNop(),
	}
}

// createRespondentParams bundles everything Create's inner-tx closure
// needs. Extracted so Create stays under gocognit's complexity ceiling
// (Plan 05 lessons § 7) — the public method becomes a simple
// validate-normalise-hash chain that hands an immutable params struct
// to applyRespondentCreate.
type createRespondentParams struct {
	in         api.CreateRespondentInput
	e164       string
	phoneHash  []byte
	regionCode string
	source     string
}

// Create implements api.RespondentService.Create. Russian-phone
// normalisation, DNC pre-check, KMS encryption, and the row insert all
// happen here; on success an audit row "crm.respondent.created" is
// emitted in the same per-tenant transaction.
//
// Error contract:
//   - api.ErrInvalidArgument when TenantID/ProjectID/Source-input is
//     malformed before any I/O runs.
//   - api.ErrInvalidPhone when the phone fails normalisation.
//   - api.ErrPhoneInDNC when the phone is already on the project DNC
//     list (or tenant-wide DNC). A separate audit row
//     "crm.respondent.create_blocked_dnc" is emitted on the block path.
//   - api.ErrDuplicateRespondent when the (tenant, project, phone_hash)
//     unique constraint fires.
//   - any wrapped error from the store / KMS / hasher otherwise.
//
// The plaintext phone is NEVER recorded in the audit payload, the
// response DTO's Phone field, or any zap log message. The masked phone
// (PhoneMasked) is computed from the canonical E.164 form for display.
func (s *RespondentService) Create(ctx context.Context, in api.CreateRespondentInput) (*api.Respondent, error) {
	if err := validateCreateRespondentInput(in); err != nil {
		return nil, err
	}

	np, err := NormalizeRussianPhone(in.Phone)
	if err != nil {
		// NormalizeRussianPhone already wraps with %w on
		// api.ErrInvalidPhone; bubble as-is.
		return nil, err
	}

	phoneHash, err := s.hasher.Hash(ctx, in.TenantID, np.E164)
	if err != nil {
		return nil, fmt.Errorf("crm/service: hash phone: %w", err)
	}

	source := in.Source
	if source == "" {
		source = api.SourceImported
	}

	regionCode := strings.TrimSpace(in.RegionCode)
	if regionCode == "" {
		regionCode = np.Region
	}

	params := createRespondentParams{
		in:         in,
		e164:       np.E164,
		phoneHash:  phoneHash,
		regionCode: regionCode,
		source:     source,
	}

	var saved api.Respondent
	err = s.tx.WithTenant(ctx, in.TenantID, func(tx postgres.Tx) error {
		var ierr error
		saved, ierr = s.applyRespondentCreate(ctx, tx, params)
		return ierr
	})
	if err != nil {
		// Bubble the sentinels as-is so callers can errors.Is them
		// without losing the kind. Generic store errors get wrapped
		// with the operation context.
		if errors.Is(err, api.ErrPhoneInDNC) ||
			errors.Is(err, api.ErrDuplicateRespondent) ||
			errors.Is(err, api.ErrRespondentNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("crm/service: create respondent: %w", err)
	}

	saved.PhoneMasked = MaskPhone(np.E164)
	saved.Phone = "" // explicit: never return the plaintext in Create
	return &saved, nil
}

// applyRespondentCreate is the inner-tx Create worker. Performs the
// DNC pre-check, the dup pre-check, the KMS Encrypt round-trip, the
// row insert, and the audit write inside the supplied transaction.
//
// Order matters:
//  1. DNC pre-check first — short-circuits before any work, emits a
//     PII-free audit row on block.
//  2. GetByHash dup pre-check — saves a KMS round-trip when the
//     phone is already a respondent in this project. The unique
//     constraint remains the authoritative detector (this branch is
//     latency optimisation, not correctness).
//  3. KMS Encrypt — fail loud (it's the security boundary).
//  4. Insert + audit — together so the audit is durable iff the row
//     write committed.
func (s *RespondentService) applyRespondentCreate(ctx context.Context, tx postgres.Tx, p createRespondentParams) (api.Respondent, error) {
	blocked, derr := s.store.IsBlockedDNC(ctx, tx, p.in.TenantID, p.in.ProjectID, p.phoneHash)
	if derr != nil {
		return api.Respondent{}, derr
	}
	if blocked {
		if aerr := s.writeAudit(ctx, auditapi.Event{
			TenantID: p.in.TenantID,
			Action:   "crm.respondent.create_blocked_dnc",
			Target:   "project:" + p.in.ProjectID.String(),
			Payload: map[string]any{
				"project_id":  p.in.ProjectID,
				"region_code": p.regionCode,
				"source":      p.source,
			},
		}); aerr != nil {
			return api.Respondent{}, aerr
		}
		return api.Respondent{}, api.ErrPhoneInDNC
	}

	if _, gerr := s.store.GetByHash(ctx, tx, p.in.TenantID, p.in.ProjectID, p.phoneHash); gerr == nil {
		return api.Respondent{}, api.ErrDuplicateRespondent
	} else if !errors.Is(gerr, api.ErrRespondentNotFound) {
		return api.Respondent{}, gerr
	}

	// Plan 13.2.5 Task 6: the phone ciphertext's AAD is bound to the row
	// ID via encryption.BuildAAD. Mint the row UUID client-side BEFORE
	// Encrypt so the value bound into the AEAD tag matches what the
	// Insert SQL writes into respondents.id. Server-side gen_random_uuid()
	// would render the ciphertext undecryptable.
	respondentID := uuid.New()
	ciphertext, eerr := s.kms.Encrypt(ctx, p.in.TenantID, respondentPhoneAADScope, respondentID.String(), []byte(p.e164))
	if eerr != nil {
		return api.Respondent{}, fmt.Errorf("crm/service: encrypt phone: %w", eerr)
	}

	saved, serr := s.store.Insert(ctx, tx, api.Respondent{
		ID:             respondentID,
		TenantID:       p.in.TenantID,
		ProjectID:      p.in.ProjectID,
		PhoneEncrypted: ciphertext,
		PhoneHash:      p.phoneHash,
		RegionCode:     p.regionCode,
		Attributes:     p.in.Attributes,
		Status:         api.RespPending,
		Source:         p.source,
	})
	if serr != nil {
		return api.Respondent{}, serr
	}

	if aerr := s.writeAudit(ctx, auditapi.Event{
		TenantID: saved.TenantID,
		Action:   "crm.respondent.created",
		Target:   "respondent:" + saved.ID.String(),
		Payload: map[string]any{
			"project_id":  saved.ProjectID,
			"region_code": saved.RegionCode,
			"source":      saved.Source,
		},
	}); aerr != nil {
		return api.Respondent{}, aerr
	}
	return saved, nil
}

// Get implements api.RespondentService.Get. Returns the respondent
// with the masked phone populated; the plaintext Phone field is left
// empty so an operator-facing handler cannot accidentally leak it.
//
// Plan 13.2.5 Task 1: looks the row up under callerTenantID's RLS
// scope. A row owned by a different tenant surfaces as
// ErrRespondentNotFound (RLS hides it) — indistinguishable from
// genuine non-existence, by design. Soft-deleted rows still return
// ErrRespondentDeleted so the UI can render "pending purge" instead
// of a generic 404.
func (s *RespondentService) Get(ctx context.Context, callerTenantID, id uuid.UUID) (*api.Respondent, error) {
	if callerTenantID == uuid.Nil {
		return nil, fmt.Errorf("crm/service: get respondent: caller tenant id required: %w", api.ErrInvalidArgument)
	}
	if id == uuid.Nil {
		return nil, fmt.Errorf("crm/service: get respondent: %w", api.ErrInvalidArgument)
	}
	var row api.Respondent
	err := s.tx.WithTenant(ctx, callerTenantID, func(tx postgres.Tx) error {
		var ierr error
		row, ierr = s.store.GetByID(ctx, tx, id)
		return ierr
	})
	if err != nil {
		if errors.Is(err, api.ErrRespondentNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("crm/service: get respondent: %w", err)
	}
	if row.DeleteAt != nil {
		return nil, api.ErrRespondentDeleted
	}
	masked, derr := s.maskedPhoneFor(ctx, row)
	if derr != nil {
		// Decryption failure must NOT leak details to the caller —
		// log via audit-context downstream, return a generic wrapped
		// error here.
		return nil, fmt.Errorf("crm/service: get respondent: mask phone: %w", derr)
	}
	row.PhoneMasked = masked
	row.Phone = "" // explicit: never populate plaintext on Get
	// Strip the at-rest representations from the DTO returned to the
	// transport — operators have no business consuming bytea blobs,
	// and removing them avoids accidental leakage through naive
	// JSON-encoders.
	row.PhoneEncrypted = nil
	row.PhoneHash = nil
	return &row, nil
}

// ResolveTenant implements api.RespondentService.ResolveTenant. Returns
// the owning tenant id via a BypassRLS lookup so the transport-layer
// tenant.RequireSameTenant middleware can compare against the caller's
// claims.TenantID before the handler runs. Returns ErrRespondentNotFound
// when no row matches.
//
// This is the only sanctioned BypassRLS resolver in RespondentService
// after Plan 13.2.5 Task 1.
func (s *RespondentService) ResolveTenant(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	if id == uuid.Nil {
		return uuid.Nil, fmt.Errorf("crm/service: resolve tenant: id required")
	}
	row, err := s.lookupRespondent(ctx, id)
	if err != nil {
		return uuid.Nil, err
	}
	return row.TenantID, nil
}

// GetWithPhone implements api.RespondentService.GetWithPhone. The
// caller must already have passed an admin RBAC gate at the HTTP
// layer; this method nevertheless writes one audit row per
// invocation (action `crm.respondent.read_pii`) so the access trail
// is complete even for service-internal callers.
//
// Plan 13.2.5 Task 1: looks the row up under callerTenantID's RLS
// scope (cross-tenant rows are invisible). Returns Phone (plaintext,
// decrypted via the per-tenant KMS) and PhoneMasked. Soft-deleted
// rows return ErrRespondentDeleted.
func (s *RespondentService) GetWithPhone(ctx context.Context, callerTenantID, id uuid.UUID) (*api.Respondent, error) {
	if callerTenantID == uuid.Nil {
		return nil, fmt.Errorf("crm/service: get with phone: caller tenant id required: %w", api.ErrInvalidArgument)
	}
	if id == uuid.Nil {
		return nil, fmt.Errorf("crm/service: get with phone: %w", api.ErrInvalidArgument)
	}
	var row api.Respondent
	err := s.tx.WithTenant(ctx, callerTenantID, func(tx postgres.Tx) error {
		var ierr error
		row, ierr = s.store.GetByID(ctx, tx, id)
		return ierr
	})
	if err != nil {
		if errors.Is(err, api.ErrRespondentNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("crm/service: get with phone: %w", err)
	}
	if row.DeleteAt != nil {
		return nil, api.ErrRespondentDeleted
	}
	plaintext, derr := s.kms.Decrypt(ctx, row.TenantID, respondentPhoneAADScope, row.ID.String(), row.PhoneEncrypted)
	if derr != nil {
		return nil, fmt.Errorf("crm/service: get with phone: decrypt: %w", derr)
	}
	phone := string(plaintext)
	row.Phone = phone
	row.PhoneMasked = MaskPhone(phone)
	row.PhoneEncrypted = nil
	row.PhoneHash = nil

	if aerr := s.writeAudit(ctx, auditapi.Event{
		TenantID: row.TenantID,
		Action:   "crm.respondent.read_pii",
		Target:   "respondent:" + row.ID.String(),
		Payload: map[string]any{
			"respondent_id": row.ID,
			"project_id":    row.ProjectID,
		},
	}); aerr != nil {
		// Non-fatal — audit write failure must not gate access; surface
		// a structured log, return the result. Plan 05 lessons § 11
		// established this pattern (stubbed audit must never silently
		// drop a row, but it must also never fail a happy-path call).
		s.logger.Warn("audit write failed",
			zap.String("action", "crm.respondent.read_pii"),
			zap.Error(aerr))
	}
	return &row, nil
}

// Search implements api.RespondentService.Search. Pagination is
// clamped to the project conventions (page>=1, pageSize in
// [1, maxSearchPageSize]); soft-deleted rows are filtered out by the
// store. Returns the page slice + total count.
//
// The HTTP transport derives TenantID + ProjectID from the JWT claims
// + path parameters. The service rejects uuid.Nil up-front so a
// careless caller cannot enumerate cross-tenant rows.
func (s *RespondentService) Search(ctx context.Context, f api.SearchRespondentsFilter) (*api.SearchRespondentsResult, error) {
	if f.TenantID == uuid.Nil {
		return nil, fmt.Errorf("crm/service: search respondents: tenant id required: %w", api.ErrInvalidArgument)
	}
	if f.ProjectID == uuid.Nil {
		return nil, fmt.Errorf("crm/service: search respondents: project id required: %w", api.ErrInvalidArgument)
	}
	if f.Page <= 0 {
		f.Page = 1
	}
	if f.PageSize <= 0 {
		f.PageSize = defaultSearchPageSize
	}
	if f.PageSize > maxSearchPageSize {
		f.PageSize = maxSearchPageSize
	}

	var (
		rows  []api.Respondent
		total int64
	)
	err := s.tx.WithTenant(ctx, f.TenantID, func(tx postgres.Tx) error {
		var qerr error
		rows, total, qerr = s.store.Search(ctx, tx, f)
		return qerr
	})
	if err != nil {
		return nil, fmt.Errorf("crm/service: search respondents: %w", err)
	}

	// Mask phone + strip at-rest fields so the operator UI can
	// json-encode the slice without accidentally leaking phone hash
	// bytes (bytea would render base64 — useless to operators and a
	// minor PII vector).
	masked := make([]api.Respondent, len(rows))
	for i, r := range rows {
		m, derr := s.maskedPhoneFor(ctx, r)
		if derr != nil {
			s.logger.Warn("mask phone failed during search",
				zap.String("respondent_id", r.ID.String()),
				zap.Error(derr))
			m = MaskPhone("") // safe fallback "***"
		}
		r.PhoneMasked = m
		r.Phone = ""
		r.PhoneEncrypted = nil
		r.PhoneHash = nil
		masked[i] = r
	}
	return &api.SearchRespondentsResult{
		Items:      masked,
		TotalCount: int(total),
	}, nil
}

// Delete implements api.RespondentService.Delete with the 152-ФЗ §21
// 30-day grace window. Soft-marks deleted_at + deletion_reason so the
// purge worker can hard-delete after the grace period; emits an audit
// row "crm.respondent.deleted" inside the same transaction so the
// trail is durable iff the soft-delete committed.
//
// Plan 13.2.5 Task 1: the entire read+write runs inside one
// WithTenant(callerTenantID, ...) — RLS rejects rows owned by other
// tenants as ErrRespondentNotFound.
//
// Idempotency: a second Delete on the same id returns
// ErrRespondentDeleted — the first call already stamped deleted_at.
func (s *RespondentService) Delete(ctx context.Context, callerTenantID, id uuid.UUID) (*api.DeletionRequest, error) {
	if callerTenantID == uuid.Nil {
		return nil, fmt.Errorf("crm/service: delete respondent: caller tenant id required: %w", api.ErrInvalidArgument)
	}
	if id == uuid.Nil {
		return nil, fmt.Errorf("crm/service: delete respondent: %w", api.ErrInvalidArgument)
	}

	deleteAt := s.clock().UTC()
	scheduledPurge := deleteAt.Add(deletionGracePeriod)
	const reason = "user_request"

	var projectID uuid.UUID
	err := s.tx.WithTenant(ctx, callerTenantID, func(tx postgres.Tx) error {
		row, ierr := s.store.GetByID(ctx, tx, id)
		if ierr != nil {
			return ierr
		}
		if row.DeleteAt != nil {
			return api.ErrRespondentDeleted
		}
		projectID = row.ProjectID
		if derr := s.store.SoftDelete(ctx, tx, id, reason, deleteAt); derr != nil {
			return derr
		}
		return s.writeAudit(ctx, auditapi.Event{
			TenantID: callerTenantID,
			Action:   "crm.respondent.deleted",
			Target:   "respondent:" + id.String(),
			Payload: map[string]any{
				"respondent_id":      id,
				"project_id":         row.ProjectID,
				"reason":             reason,
				"deleted_at":         deleteAt,
				"scheduled_purge_at": scheduledPurge,
			},
		})
	})
	if err != nil {
		if errors.Is(err, api.ErrRespondentDeleted) || errors.Is(err, api.ErrRespondentNotFound) {
			// Pre-13.2.5 contract: missing → ErrRespondentNotFound;
			// already soft-deleted → ErrRespondentDeleted. We forward
			// both verbatim so existing callers' errors.Is checks keep
			// working.
			return nil, err
		}
		return nil, fmt.Errorf("crm/service: delete respondent: %w", err)
	}

	_ = projectID // reserved for future event emission (Plan 11 wire-up)
	return &api.DeletionRequest{
		RespondentID: id,
		DeleteAt:     scheduledPurge,
	}, nil
}

// lookupRespondent resolves a respondent by id via BypassRLS. Returns
// ErrRespondentNotFound on a missing row; otherwise wraps the
// underlying error. The caller is responsible for any tenant-scoped
// follow-up (we keep this read separate so admin tooling that needs
// to resolve a row id to its tenant doesn't have to know the tenant
// up front).
func (s *RespondentService) lookupRespondent(ctx context.Context, id uuid.UUID) (api.Respondent, error) {
	var row api.Respondent
	err := s.tx.BypassRLS(ctx, func(tx postgres.Tx) error {
		var qerr error
		row, qerr = s.store.GetByID(ctx, tx, id)
		return qerr
	})
	if err != nil {
		if errors.Is(err, api.ErrRespondentNotFound) {
			return api.Respondent{}, err
		}
		return api.Respondent{}, fmt.Errorf("crm/service: lookup respondent: %w", err)
	}
	return row, nil
}

// maskedPhoneFor decrypts the at-rest phone ciphertext and returns
// the display-safe mask. Returns an empty string + the wrapped KMS
// error on decrypt failure — callers decide whether to render "***"
// or surface the error.
//
// Plan 13.2.5 Task 6: the row's ID is bound into the AEAD AAD; an
// attacker who swaps the ciphertext bytes from another row fails
// decryption here.
func (s *RespondentService) maskedPhoneFor(ctx context.Context, r api.Respondent) (string, error) {
	if len(r.PhoneEncrypted) == 0 {
		return "", nil
	}
	plaintext, err := s.kms.Decrypt(ctx, r.TenantID, respondentPhoneAADScope, r.ID.String(), r.PhoneEncrypted)
	if err != nil {
		return "", err
	}
	return MaskPhone(string(plaintext)), nil
}

// Import and GetImportStatus live in import.go (Plan 06 Task 4).

// validateCreateRespondentInput checks the synchronous-rejection
// invariants on CreateRespondentInput. TenantID and ProjectID are
// mandatory (the HTTP transport derives the former from JWT claims;
// the latter is a path parameter). Phone is intentionally not checked
// here — NormalizeRussianPhone owns that branch and emits a more
// specific sentinel.
//
// We deliberately do NOT log the input phone even on validation
// failure; structured logs are written by the outermost handler
// (httputil/error_handler) which has its own redaction policy.
func validateCreateRespondentInput(in api.CreateRespondentInput) error {
	if in.TenantID == uuid.Nil {
		return fmt.Errorf("crm/service: create respondent: tenant id required: %w", api.ErrInvalidArgument)
	}
	if in.ProjectID == uuid.Nil {
		return fmt.Errorf("crm/service: create respondent: project id required: %w", api.ErrInvalidArgument)
	}
	if in.Source != "" && in.Source != api.SourceImported && in.Source != api.SourceRDD {
		return fmt.Errorf("crm/service: create respondent: invalid source %q: %w", in.Source, api.ErrInvalidArgument)
	}
	return nil
}

// writeAudit is the RespondentService-local copy of the audit helper.
// Mirrors the equivalent in project_service.go (we don't share via a
// receiver-typed helper because Go methods can't be defined on a
// helper struct without restructuring the existing ProjectService).
//
// The clock and ActorKind defaulting policy is identical: if no actor
// is present in ctx the row is logged as ActorSystem with a nil
// ActorID — typical for worker-driven actions.
func (s *RespondentService) writeAudit(ctx context.Context, ev auditapi.Event) error {
	if s.audit == nil {
		return nil
	}
	if ev.ActorKind == "" {
		ev.ActorKind = auditapi.ActorUser
	}
	if ev.ActorID == nil {
		ev.ActorID = actorIDFromContext(ctx)
		if ev.ActorID == nil {
			ev.ActorKind = auditapi.ActorSystem
		}
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = s.clock()
	}
	if err := s.audit.Write(ctx, ev); err != nil {
		return fmt.Errorf("audit write: %w", err)
	}
	return nil
}

// MaskPhone returns a UI-safe mask of an E.164 phone string. The
// canonical Russian E.164 form is "+7" + 10 digits = 12 chars; the
// mask preserves "+7", the first digit of the operator code, and the
// last two digits of the subscriber number, replacing the rest with
// asterisks. Inputs shorter than 4 chars (or non-RU format) collapse
// to "***" so logging code accidentally calling this on a malformed
// value can't leak internals.
//
// The function is intentionally NOT used inside Create's audit
// payload — we never emit a masked phone there either, since even
// the masked form leaks the trailing two digits, and 2-digit suffix
// joins are a known re-identification vector for small populations.
// MaskPhone is reserved for the response DTO surface (operator UI).
func MaskPhone(p string) string {
	if len(p) < 4 {
		return "***"
	}
	if !strings.HasPrefix(p, "+7") || len(p) != 12 {
		return "***"
	}
	digit3 := p[2:3]
	last2 := p[10:12]
	return "+7-" + digit3 + "**-***-**-" + last2
}
