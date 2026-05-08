package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/google/uuid"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	"github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/internal/surveys/schemavalidator"
	"github.com/sociopulse/platform/pkg/eventbus"
	"github.com/sociopulse/platform/pkg/postgres"
)

// surveyTxRunner is the cross-tenant transaction owner the
// SurveyService uses for write paths. *postgres.Pool satisfies this
// interface via WithTenant + BypassRLS; tests substitute an in-memory
// implementation that invokes fn with a zero postgres.Tx.
//
// Defined here at the consumer per project convention (07-go-coding-
// standards § Interfaces): the producer (*postgres.Pool) returns a
// concrete struct, the consumer narrows it to the methods it actually
// needs.
type surveyTxRunner interface {
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error
	BypassRLS(ctx context.Context, fn func(postgres.Tx) error) error
}

// schemaValidator is the narrow interface the service consumes from
// schemavalidator.SchemaValidator. Defining it here lets the test
// suite supply a fake (returning canned reports) without booting the
// real JSON-Schema compiler at every test.
type schemaValidator interface {
	Validate(ctx context.Context, schemaJSON []byte) schemavalidator.ValidationReport
}

// Service field length caps. Name has a 200-char ceiling so dashboard
// UIs render cleanly and the (lower(name)) index footprint stays
// bounded. Description is a UI-only field and gets a generous 4096-
// char cap to leave room for short HTML snippets without blowing up
// the row width.
const (
	maxNameLength        = 200
	maxDescriptionLength = 4096
	defaultListLimit     = 50
	maxListLimit         = 500
)

// SurveyService implements api.SurveyService.
//
// Mutating methods open a per-tenant transaction (Pool.WithTenant),
// run the store write, and emit an audit row inside the same
// transaction so the audit log is durable iff the row write committed.
// Get/List use BypassRLS / WithTenant depending on whether the tenant
// is known up front (admin lookups by id use BypassRLS; per-tenant
// list pages use WithTenant).
//
// The Activate flow is the most subtle: it acquires a transaction-
// level advisory lock (`pg_advisory_xact_lock`) keyed on the survey
// id so concurrent Activate calls for the same survey are serialised
// at the database level, then runs DeactivateAll → Activate →
// SetCurrentVersion in one commit. The partial unique index
// `survey_versions_active_one` guarantees at most one is_active=true
// row per survey at all visibility horizons.
type SurveyService struct {
	tx        surveyTxRunner
	surveys   api.SurveyStorePort
	versions  api.VersionStorePort
	validator schemaValidator
	audit     auditapi.Logger
	// events is the optional NATS publisher. Plan 07 declares the
	// `surveys.version.{saved,activated}` subjects in api/events.go;
	// Plan 11 owns the real NATS wire-up. Until then the composition
	// root passes nil and we skip publishing silently. Once Plan 11
	// lands, every state-changing SurveyService method emits a typed
	// event without further code changes.
	events eventbus.Publisher
	clock  func() time.Time
	// acquireLock acquires the per-survey advisory lock used by
	// Activate. Overridable so unit tests can substitute a no-op
	// (the zero postgres.Tx in the test runner has a nil pgx.Tx, so
	// real Exec calls panic). Defaults to acquireAdvisoryLock.
	acquireLock func(ctx context.Context, tx postgres.Tx, surveyID uuid.UUID) error
}

// Compile-time assertion: *SurveyService must satisfy api.SurveyService.
var _ api.SurveyService = (*SurveyService)(nil)

// NewSurveyService constructs a SurveyService from already-built
// deps. The caller (the module composition root) owns the lifecycle
// of every dependency. clock may be nil — the constructor falls back
// to time.Now so callers do not have to repeat that boilerplate.
//
// auditLogger MUST NOT be nil: every state-changing method emits an
// audit row inside the same transaction as the data write, and a
// misconfigured composition root that registered nil would silently
// drop those rows. Tests that genuinely don't care about the audit
// trail must inject a no-op fake logger explicitly (Plan 05 lessons
// learned § 10).
//
// publisher may be nil — when nil, calls to publishEvent are no-ops
// (see Plan 11 deferral note on the events field).
func NewSurveyService(
	pool surveyTxRunner,
	surveys api.SurveyStorePort,
	versions api.VersionStorePort,
	validator schemaValidator,
	auditLogger auditapi.Logger,
	publisher eventbus.Publisher,
	clock func() time.Time,
) *SurveyService {
	if pool == nil {
		panic("surveys/service: NewSurveyService: pool is required")
	}
	if surveys == nil {
		panic("surveys/service: NewSurveyService: surveys store is required")
	}
	if versions == nil {
		panic("surveys/service: NewSurveyService: versions store is required")
	}
	if validator == nil {
		panic("surveys/service: NewSurveyService: validator is required")
	}
	if auditLogger == nil {
		panic("surveys/service: NewSurveyService: auditLogger is required (use a no-op fake in tests, never nil)")
	}
	if clock == nil {
		clock = time.Now
	}
	return &SurveyService{
		tx:          pool,
		surveys:     surveys,
		versions:    versions,
		validator:   validator,
		audit:       auditLogger,
		events:      publisher,
		clock:       clock,
		acquireLock: acquireAdvisoryLock,
	}
}

// publishEvent fan-outs a typed event payload to the configured NATS
// publisher. nil events field (Plan 11 not yet wired) → no-op. Marshal
// failures are logged via the audit context but NOT returned, because
// the parent call already committed the DB row + audit; an event-side
// failure must not surface as user-visible "save failed". This matches
// the at-least-once + outbox-retry posture established by pkg/outbox.
func (s *SurveyService) publishEvent(ctx context.Context, subject string, payload any) {
	if s.events == nil {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		_ = s.writeAudit(ctx, auditapi.Event{
			Action:  "surveys.event.publish_marshal_error",
			Payload: map[string]any{"subject": subject, "error": err.Error()},
		})
		return
	}
	if err := s.events.Publish(ctx, subject, body); err != nil {
		_ = s.writeAudit(ctx, auditapi.Event{
			Action:  "surveys.event.publish_error",
			Payload: map[string]any{"subject": subject, "error": err.Error()},
		})
	}
}

// Create implements api.SurveyService.Create. Inserts a fresh surveys
// row with status=active and emits a `surveys.created` audit row
// inside the same transaction as the row write. The row's
// PrimaryMode must be one of the documented values; an empty string
// defaults to ModeForm.
func (s *SurveyService) Create(ctx context.Context, in api.CreateSurveyInput) (uuid.UUID, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	if err := validateCreateInput(in); err != nil {
		return uuid.Nil, err
	}

	mode := in.PrimaryMode
	if mode == "" {
		mode = api.ModeForm
	}
	if mode != api.ModeForm && mode != api.ModeFlow {
		return uuid.Nil, fmt.Errorf("surveys/service: create: %w: primary_mode %q", api.ErrInvalidArgument, mode)
	}

	candidate := api.Survey{
		TenantID:    tenantID,
		Name:        in.Name,
		Description: in.Description,
		PrimaryMode: mode,
		Status:      api.StatusActive,
	}
	if actor := actorIDFromContext(ctx); actor != nil {
		candidate.CreatedBy = *actor
	}

	var saved api.Survey
	err = s.tx.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		saved, err = s.surveys.Insert(ctx, tx, candidate)
		if err != nil {
			return err
		}
		return s.writeAudit(ctx, auditapi.Event{
			TenantID: saved.TenantID,
			Action:   "surveys.created",
			Target:   "survey:" + saved.ID.String(),
			Payload: map[string]any{
				"name":         saved.Name,
				"description":  saved.Description,
				"primary_mode": string(saved.PrimaryMode),
			},
		})
	})
	if err != nil {
		if errors.Is(err, api.ErrNameTaken) {
			return uuid.Nil, err
		}
		return uuid.Nil, fmt.Errorf("surveys/service: create: %w", err)
	}
	return saved.ID, nil
}

// Get implements api.SurveyService.Get. Uses BypassRLS so admin
// tooling can resolve a survey id to its tenant before any per-tenant
// flow.
func (s *SurveyService) Get(ctx context.Context, id uuid.UUID) (api.Survey, error) {
	if id == uuid.Nil {
		return api.Survey{}, fmt.Errorf("surveys/service: get: %w", api.ErrInvalidArgument)
	}
	var out api.Survey
	err := s.tx.BypassRLS(ctx, func(tx postgres.Tx) error {
		var err error
		out, err = s.surveys.GetByID(ctx, tx, id)
		return err
	})
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			return api.Survey{}, err
		}
		return api.Survey{}, fmt.Errorf("surveys/service: get: %w", err)
	}
	return out, nil
}

// List implements api.SurveyService.List. The tenant scope comes from
// the supplied ctx (tenantIDFromContext) so callers cannot accidentally
// list another tenant's surveys.
func (s *SurveyService) List(ctx context.Context, filter api.ListFilter) ([]api.Survey, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if filter.Limit <= 0 {
		filter.Limit = defaultListLimit
	}
	if filter.Limit > maxListLimit {
		filter.Limit = maxListLimit
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}

	var rows []api.Survey
	err = s.tx.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		rows, _, err = s.surveys.List(ctx, tx, filter)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("surveys/service: list: %w", err)
	}
	return rows, nil
}

// Update implements api.SurveyService.Update. Resolves the survey's
// tenant via a BypassRLS GetByID, then opens a per-tenant transaction
// (RLS in effect) and runs the partial-update.
func (s *SurveyService) Update(ctx context.Context, id uuid.UUID, in api.UpdateSurveyInput) error {
	if id == uuid.Nil {
		return fmt.Errorf("surveys/service: update: %w", api.ErrInvalidArgument)
	}
	if err := validateUpdateInput(in); err != nil {
		return err
	}
	patch := api.SurveyPatch{
		Name:        in.Name,
		Description: in.Description,
		PrimaryMode: in.PrimaryMode,
	}

	current, err := s.lookupSurvey(ctx, id)
	if err != nil {
		return err
	}
	if current.Status == api.StatusArchived {
		return api.ErrSurveyArchived
	}
	if patch.IsEmpty() {
		return nil
	}

	err = s.tx.WithTenant(ctx, current.TenantID, func(tx postgres.Tx) error {
		saved, err := s.surveys.Update(ctx, tx, id, patch)
		if err != nil {
			return err
		}
		return s.writeAudit(ctx, auditapi.Event{
			TenantID: saved.TenantID,
			Action:   "surveys.updated",
			Target:   "survey:" + saved.ID.String(),
			Payload:  buildUpdatePayload(in),
		})
	})
	if err != nil {
		if errors.Is(err, api.ErrNotFound) || errors.Is(err, api.ErrSurveyArchived) {
			return err
		}
		return fmt.Errorf("surveys/service: update: %w", err)
	}
	return nil
}

// Archive implements api.SurveyService.Archive.
//
// Active|<empty> → Archived commits/audits. Archived → Archived is a
// silent no-op (terminal idempotency). archived_at is stamped at the
// service clock so the timestamp matches the audit row exactly.
func (s *SurveyService) Archive(ctx context.Context, id uuid.UUID) error {
	if id == uuid.Nil {
		return fmt.Errorf("surveys/service: archive: %w", api.ErrInvalidArgument)
	}
	current, err := s.lookupSurvey(ctx, id)
	if err != nil {
		return err
	}
	// Idempotent: already archived → silent no-op.
	if current.Status == api.StatusArchived {
		return nil
	}

	at := s.clock().UTC()
	err = s.tx.WithTenant(ctx, current.TenantID, func(tx postgres.Tx) error {
		if err := s.surveys.Archive(ctx, tx, id, at); err != nil {
			return err
		}
		return s.writeAudit(ctx, auditapi.Event{
			TenantID: current.TenantID,
			Action:   "surveys.archived",
			Target:   "survey:" + id.String(),
			Payload: map[string]any{
				"from": string(current.Status),
				"to":   string(api.StatusArchived),
			},
		})
	})
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			return err
		}
		return fmt.Errorf("surveys/service: archive: %w", err)
	}
	return nil
}

// SaveVersion implements api.SurveyService.SaveVersion.
//
// The flow:
//  1. Validate the schema via schemavalidator. On invalid: return
//     *api.ValidationError carrying the report (errors.Is(err,
//     ErrValidation) still matches via Unwrap).
//  2. Resolve tenant + reject archived survey.
//  3. Compute next major.minor: minor=true bumps minor of the latest
//     major; minor=false bumps to (latestMajor+1, 0).
//  4. Insert the row in a per-tenant tx and audit `surveys.version_saved`.
//
// SaveVersion does NOT auto-activate the new version. Callers wire a
// separate Activate call.
func (s *SurveyService) SaveVersion(ctx context.Context, surveyID uuid.UUID, schemaJSON []byte, minor bool) (api.Version, error) {
	if surveyID == uuid.Nil {
		return api.Version{}, fmt.Errorf("surveys/service: save version: %w", api.ErrInvalidArgument)
	}
	if len(schemaJSON) == 0 {
		return api.Version{}, fmt.Errorf("surveys/service: save version: empty schema: %w", api.ErrInvalidArgument)
	}

	report := s.validator.Validate(ctx, schemaJSON)
	if !report.Valid {
		return api.Version{}, &api.ValidationError{Report: convertReport(report)}
	}

	current, err := s.lookupSurvey(ctx, surveyID)
	if err != nil {
		return api.Version{}, err
	}
	if current.Status == api.StatusArchived {
		return api.Version{}, api.ErrSurveyArchived
	}

	var saved api.Version
	err = s.tx.WithTenant(ctx, current.TenantID, func(tx postgres.Tx) error {
		next, err := s.computeNextVersion(ctx, tx, surveyID, minor)
		if err != nil {
			return err
		}
		candidate := api.Version{
			SurveyID: surveyID,
			Major:    next.major,
			Minor:    next.minor,
			Schema:   schemaJSON,
		}
		if actor := actorIDFromContext(ctx); actor != nil {
			candidate.CreatedBy = *actor
		}
		saved, err = s.versions.Insert(ctx, tx, candidate)
		if err != nil {
			return err
		}
		return s.writeAudit(ctx, auditapi.Event{
			TenantID: current.TenantID,
			Action:   "surveys.version_saved",
			Target:   "version:" + saved.ID.String(),
			Payload: map[string]any{
				"survey_id": surveyID,
				"major":     saved.Major,
				"minor":     saved.Minor,
			},
		})
	})
	if err != nil {
		if errors.Is(err, api.ErrNotFound) || errors.Is(err, api.ErrSurveyArchived) {
			return api.Version{}, err
		}
		return api.Version{}, fmt.Errorf("surveys/service: save version: %w", err)
	}

	s.publishEvent(ctx, api.SubjectVersionSavedFor(current.TenantID), api.VersionSavedEvent{
		SurveyID:  surveyID,
		VersionID: saved.ID,
		Major:     saved.Major,
		Minor:     saved.Minor,
	})
	return saved, nil
}

// nextVersion is the (major, minor) tuple computed by
// computeNextVersion.
type nextVersion struct {
	major int
	minor int
}

// computeNextVersion picks the next (major, minor) for SaveVersion.
// minor=false → (latestMajor+1, 0); minor=true → (latestMajor,
// latestMinor+1) when the survey already has at least one version,
// or (1, 0) when it has none yet (so a "minor on empty" still
// produces a valid first version rather than an error).
func (s *SurveyService) computeNextVersion(ctx context.Context, tx postgres.Tx, surveyID uuid.UUID, minor bool) (nextVersion, error) {
	latestMajor, err := s.versions.LatestMajor(ctx, tx, surveyID)
	if err != nil {
		return nextVersion{}, err
	}
	if !minor || latestMajor == 0 {
		// Major bump (or first version regardless of the minor flag):
		// always 1 when the survey has no versions; otherwise
		// latestMajor+1.
		return nextVersion{major: latestMajor + 1, minor: 0}, nil
	}
	latestMinor, err := s.versions.LatestMinor(ctx, tx, surveyID, latestMajor)
	if err != nil {
		return nextVersion{}, err
	}
	if latestMinor < 0 {
		// Defensive: shouldn't happen because LatestMajor returned
		// >0, but if the row vanished between the two reads we still
		// produce a valid number.
		return nextVersion{major: latestMajor, minor: 0}, nil
	}
	return nextVersion{major: latestMajor, minor: latestMinor + 1}, nil
}

// Activate implements api.SurveyService.Activate.
//
// Atomicity strategy (lifted from plan-07-surveys.md and locked into
// the task spec):
//   - Open a per-tenant tx.
//   - Acquire a transaction-level advisory lock keyed on the survey
//     id (`pg_advisory_xact_lock(hashtext(survey_id::text))`). This
//     serialises concurrent Activates for the same survey at the
//     database level — the partial unique index alone wouldn't
//     prevent two Activates landing in the same instant.
//   - DeactivateAll → Activate → SetCurrentVersion in one commit.
//     The partial unique index `survey_versions_active_one`
//     guarantees the visibility horizon never observes two active
//     rows.
//
// Audit row carries {previous_version_id, new_version_id} so the
// event is reversible by inspection. Plan 11 will wire the NATS
// publish; today the slot is no-op.
func (s *SurveyService) Activate(ctx context.Context, surveyID, versionID uuid.UUID) error {
	if surveyID == uuid.Nil || versionID == uuid.Nil {
		return fmt.Errorf("surveys/service: activate: %w", api.ErrInvalidArgument)
	}
	current, err := s.lookupSurvey(ctx, surveyID)
	if err != nil {
		return err
	}
	if current.Status == api.StatusArchived {
		return api.ErrSurveyArchived
	}

	at := s.clock().UTC()
	err = s.tx.WithTenant(ctx, current.TenantID, func(tx postgres.Tx) error {
		return s.applyActivate(ctx, tx, current.TenantID, surveyID, versionID, at)
	})
	if errors.Is(err, errAlreadyActive) {
		return nil
	}
	if err != nil {
		if errors.Is(err, api.ErrNotFound) || errors.Is(err, api.ErrSurveyArchived) ||
			errors.Is(err, api.ErrVersionNotFound) || errors.Is(err, api.ErrNoActiveVersion) {
			return err
		}
		return fmt.Errorf("surveys/service: activate: %w", err)
	}
	s.publishEvent(ctx, api.SubjectVersionActivatedFor(current.TenantID), api.VersionActivatedEvent{
		SurveyID:  surveyID,
		VersionID: versionID,
	})
	return nil
}

// applyActivate is the inner-tx Activate worker. Extracted so the
// public method stays under gocognit's complexity ceiling (Plan
// 05/06 lessons learned). Acquires the advisory lock, verifies the
// target version belongs to surveyID, captures the previous active
// id for audit, then performs DeactivateAll → Activate →
// SetCurrentVersion → writeAudit in one tx.
func (s *SurveyService) applyActivate(ctx context.Context, tx postgres.Tx, tenantID, surveyID, versionID uuid.UUID, at time.Time) error {
	if err := s.acquireLock(ctx, tx, surveyID); err != nil {
		return err
	}
	target, err := s.versions.GetByID(ctx, tx, versionID)
	if err != nil {
		return err
	}
	if target.SurveyID != surveyID {
		return api.ErrVersionNotFound
	}
	previousVersionID, err := s.captureCurrentActive(ctx, tx, surveyID, versionID)
	if err != nil {
		return err
	}
	if err := s.versions.DeactivateAll(ctx, tx, surveyID); err != nil {
		return err
	}
	if err := s.versions.Activate(ctx, tx, versionID, at); err != nil {
		return err
	}
	if err := s.surveys.SetCurrentVersion(ctx, tx, surveyID, versionID); err != nil {
		return err
	}
	return s.writeAudit(ctx, auditapi.Event{
		TenantID: tenantID,
		Action:   "surveys.version_activated",
		Target:   "version:" + versionID.String(),
		Payload: map[string]any{
			"survey_id":           surveyID,
			"new_version_id":      versionID,
			"previous_version_id": previousVersionID,
		},
	})
}

// captureCurrentActive returns the currently-active version id for
// surveyID (or uuid.Nil when none) and signals already-active by
// returning errAlreadyActive when the prior active matches versionID.
// ErrNoActiveVersion is treated as "fine, no prior active" — the
// caller observes uuid.Nil and proceeds.
func (s *SurveyService) captureCurrentActive(ctx context.Context, tx postgres.Tx, surveyID, versionID uuid.UUID) (uuid.UUID, error) {
	prev, perr := s.versions.GetActive(ctx, tx, surveyID)
	if perr == nil {
		if prev.ID == versionID {
			return uuid.Nil, errAlreadyActive
		}
		return prev.ID, nil
	}
	if errors.Is(perr, api.ErrNoActiveVersion) {
		return uuid.Nil, nil
	}
	return uuid.Nil, perr
}

// errAlreadyActive is the internal sentinel computeNextVersion uses
// to signal "this is a no-op, don't roll back the tx, just exit
// cleanly". Activate translates it to a nil return at the boundary.
var errAlreadyActive = errors.New("surveys/service: version already active (idempotent)")

// GetActiveVersion implements api.SurveyService.GetActiveVersion.
func (s *SurveyService) GetActiveVersion(ctx context.Context, surveyID uuid.UUID) (api.Version, error) {
	if surveyID == uuid.Nil {
		return api.Version{}, fmt.Errorf("surveys/service: get active version: %w", api.ErrInvalidArgument)
	}
	current, err := s.lookupSurvey(ctx, surveyID)
	if err != nil {
		return api.Version{}, err
	}

	var v api.Version
	err = s.tx.WithTenant(ctx, current.TenantID, func(tx postgres.Tx) error {
		var err error
		v, err = s.versions.GetActive(ctx, tx, surveyID)
		return err
	})
	if err != nil {
		if errors.Is(err, api.ErrNoActiveVersion) {
			return api.Version{}, err
		}
		return api.Version{}, fmt.Errorf("surveys/service: get active version: %w", err)
	}
	return v, nil
}

// ListVersions implements api.SurveyService.ListVersions.
func (s *SurveyService) ListVersions(ctx context.Context, surveyID uuid.UUID) ([]api.Version, error) {
	if surveyID == uuid.Nil {
		return nil, fmt.Errorf("surveys/service: list versions: %w", api.ErrInvalidArgument)
	}
	current, err := s.lookupSurvey(ctx, surveyID)
	if err != nil {
		return nil, err
	}

	var rows []api.Version
	err = s.tx.WithTenant(ctx, current.TenantID, func(tx postgres.Tx) error {
		var err error
		rows, err = s.versions.List(ctx, tx, surveyID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("surveys/service: list versions: %w", err)
	}
	return rows, nil
}

// lookupSurvey reads the row by id via BypassRLS so the caller does
// not have to know the tenant up front. Returns api.ErrNotFound on a
// missing row.
func (s *SurveyService) lookupSurvey(ctx context.Context, id uuid.UUID) (api.Survey, error) {
	var sv api.Survey
	err := s.tx.BypassRLS(ctx, func(tx postgres.Tx) error {
		var err error
		sv, err = s.surveys.GetByID(ctx, tx, id)
		return err
	})
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			return api.Survey{}, err
		}
		return api.Survey{}, fmt.Errorf("surveys/service: lookup survey: %w", err)
	}
	return sv, nil
}

// acquireAdvisoryLock acquires a transaction-level advisory lock keyed
// on surveyID. Pg's advisory locks take a bigint; we map the UUID
// through fnv-1a to keep the key stable + deterministic. The lock is
// released automatically when the tx commits or rolls back.
func acquireAdvisoryLock(ctx context.Context, tx postgres.Tx, surveyID uuid.UUID) error {
	h := fnv.New64a()
	_, _ = h.Write(surveyID[:])
	// The cast to int64 is intentional: pg_advisory_xact_lock takes a
	// signed bigint, and we want the bit pattern preserved.
	key := int64(h.Sum64()) //nolint:gosec // intentional: bit-pattern preserved for advisory lock key
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", key); err != nil {
		return fmt.Errorf("surveys/service: acquire advisory lock: %w", err)
	}
	return nil
}

// validateCreateInput checks the synchronous-rejection invariants on
// CreateSurveyInput. Name is mandatory; description is optional but
// length-capped.
func validateCreateInput(in api.CreateSurveyInput) error {
	if strings.TrimSpace(in.Name) == "" {
		return fmt.Errorf("surveys/service: create: %w: name required", api.ErrInvalidArgument)
	}
	if len(in.Name) > maxNameLength {
		return fmt.Errorf("surveys/service: create: %w: name exceeds %d chars", api.ErrInvalidArgument, maxNameLength)
	}
	if len(in.Description) > maxDescriptionLength {
		return fmt.Errorf("surveys/service: create: %w: description exceeds %d chars", api.ErrInvalidArgument, maxDescriptionLength)
	}
	return nil
}

// validateUpdateInput checks the synchronous-rejection invariants on
// UpdateSurveyInput. Empty patch is allowed (short-circuit later);
// non-empty fields must respect length caps.
func validateUpdateInput(in api.UpdateSurveyInput) error {
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" {
			return fmt.Errorf("surveys/service: update: %w: name required", api.ErrInvalidArgument)
		}
		if len(*in.Name) > maxNameLength {
			return fmt.Errorf("surveys/service: update: %w: name exceeds %d chars", api.ErrInvalidArgument, maxNameLength)
		}
	}
	if in.Description != nil && len(*in.Description) > maxDescriptionLength {
		return fmt.Errorf("surveys/service: update: %w: description exceeds %d chars", api.ErrInvalidArgument, maxDescriptionLength)
	}
	if in.PrimaryMode != nil {
		mode := *in.PrimaryMode
		if mode != api.ModeForm && mode != api.ModeFlow {
			return fmt.Errorf("surveys/service: update: %w: primary_mode %q", api.ErrInvalidArgument, mode)
		}
	}
	return nil
}

// buildUpdatePayload renders the audit payload for "surveys.updated"
// — only the keys the caller actually patched.
func buildUpdatePayload(in api.UpdateSurveyInput) map[string]any {
	out := make(map[string]any, 3)
	if in.Name != nil {
		out["name"] = *in.Name
	}
	if in.Description != nil {
		out["description"] = *in.Description
	}
	if in.PrimaryMode != nil {
		out["primary_mode"] = string(*in.PrimaryMode)
	}
	return out
}

// convertReport translates a schemavalidator.ValidationReport into
// the api.Report shape used by api.ValidationError.
func convertReport(in schemavalidator.ValidationReport) api.Report {
	out := api.Report{Issues: make([]api.Issue, len(in.Issues))}
	for i, iss := range in.Issues {
		out.Issues[i] = api.Issue{
			Code:    iss.Code,
			NodeID:  iss.Path,
			Message: iss.Message,
		}
	}
	return out
}

// tenantIDFromContext pulls the tenant id off ctx via the tenancy
// package's well-known context key. It returns ErrInvalidArgument
// when the key is absent so callers see a structured error rather
// than a panic.
//
// The key is co-located here (not imported from internal/tenancy) to
// keep depguard's module-boundaries rule satisfied. The HTTP
// transport (Plan 07 Task 6) populates the key from the JWT claims.
func tenantIDFromContext(ctx context.Context) (uuid.UUID, error) {
	v := ctx.Value(tenantIDContextKey{})
	id, ok := v.(uuid.UUID)
	if !ok || id == uuid.Nil {
		return uuid.Nil, fmt.Errorf("surveys/service: %w: tenant id missing from context", api.ErrInvalidArgument)
	}
	return id, nil
}

// tenantIDContextKey is the unexported context key surveys services
// use to pull the tenant id. The HTTP transport (Plan 07 Task 6)
// populates it from the JWT claims; tests inject the tenant via
// WithTenantID directly.
type tenantIDContextKey struct{}

// WithTenantID returns a context that carries the supplied tenant id.
// Surveys services inspect the context for this value when scoping
// list/create operations to a tenant.
func WithTenantID(ctx context.Context, tenantID uuid.UUID) context.Context {
	return context.WithValue(ctx, tenantIDContextKey{}, tenantID)
}
