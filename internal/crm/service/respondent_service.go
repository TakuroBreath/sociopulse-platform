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
// RespondentService uses for write paths. *postgres.Pool satisfies
// this interface via its WithTenant method; tests substitute an in-
// memory implementation that invokes fn with a zero postgres.Tx.
//
// Defined here at the consumer per project convention (see
// project_service.go projectTxRunner). RespondentService never opens
// a BypassRLS tx — every write is per-tenant and the service derives
// the tenant id from the caller's input.
type respondentTxRunner interface {
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error
}

// RespondentService implements api.RespondentService for the Create
// path (Plan 06 Task 3). The remaining methods (Get/GetWithPhone/
// Search/Delete/Import/GetImportStatus) are stubbed to return
// ErrUnimplemented; Tasks 4-5 fill those in.
//
// Security boundary: every write goes through KMSResolver.Encrypt for
// the at-rest phone ciphertext and PhoneHasher.Hash for the
// indexed-lookup hash. The plaintext phone NEVER lands in the audit
// payload, the response DTO's Phone field, or any zap logger field —
// this is enforced by tests (json.Marshal-then-Contains check on the
// audit row) and by reviewer scrutiny.
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

// errRespondentUnimplemented is returned by every method that Plan 06
// Task 3 doesn't implement (Get/GetWithPhone/Search/Delete/Import/
// GetImportStatus). Tasks 4-5 replace the stub bodies with real ones;
// until then the service surface stays usable for the Create path
// without panicking on an unrelated call.
var errRespondentUnimplemented = errors.New("crm/service: respondent method not implemented in Task 3")

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

	ciphertext, eerr := s.kms.Encrypt(ctx, p.in.TenantID, []byte(p.e164))
	if eerr != nil {
		return api.Respondent{}, fmt.Errorf("crm/service: encrypt phone: %w", eerr)
	}

	saved, serr := s.store.Insert(ctx, tx, api.Respondent{
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

// Get is stubbed pending Task 4.
func (s *RespondentService) Get(_ context.Context, _ uuid.UUID) (*api.Respondent, error) {
	return nil, errRespondentUnimplemented
}

// GetWithPhone is stubbed pending Task 4.
func (s *RespondentService) GetWithPhone(_ context.Context, _ uuid.UUID) (*api.Respondent, error) {
	return nil, errRespondentUnimplemented
}

// Search is stubbed pending Task 4.
func (s *RespondentService) Search(_ context.Context, _ api.SearchRespondentsFilter) (*api.SearchRespondentsResult, error) {
	return nil, errRespondentUnimplemented
}

// Delete is stubbed pending Task 5.
func (s *RespondentService) Delete(_ context.Context, _ uuid.UUID) (*api.DeletionRequest, error) {
	return nil, errRespondentUnimplemented
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
