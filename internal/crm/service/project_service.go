package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	"github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/pkg/eventbus"
	"github.com/sociopulse/platform/pkg/postgres"
)

// projectTxRunner is the cross-tenant transaction owner ProjectService
// uses for write paths. *postgres.Pool satisfies this interface via its
// WithTenant + BypassRLS methods; tests substitute an in-memory
// implementation that invokes fn with a zero postgres.Tx.
//
// Defined here at the consumer per project convention (07-go-coding
// -standards § Interfaces): the producer (*postgres.Pool) returns a
// concrete struct, the consumer narrows it to the methods it actually
// needs.
type projectTxRunner interface {
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error
	BypassRLS(ctx context.Context, fn func(postgres.Tx) error) error
}

// Pagination defaults applied by ProjectService.List when the caller
// supplies non-positive values. The 500-row ceiling matches the
// auth/service.UserService pattern so admin tooling has a single mental
// model for "max page size".
const (
	defaultListLimit = 50
	maxListLimit     = 500
)

// Project field length caps. Code must fit in the unique constraint
// without bloating the index; Name is a UI-display string and is capped
// at 200 to keep dashboards readable. The DB columns are unconstrained
// `text`, so the service layer is the only enforcer — the caps live
// here, not in the DDL.
const (
	maxCodeLength = 64
	maxNameLength = 200
)

// ProjectService implements api.ProjectService.
//
// Mutating methods open a per-tenant transaction (Pool.WithTenant), run
// the store write, and emit an audit row inside the same transaction so
// the audit log is durable iff the row write committed. Get opens a
// BypassRLS transaction because the caller has not (necessarily)
// supplied a tenant context — admin tooling routinely needs to resolve
// a project id to its tenant before any per-tenant flow.
//
// 152-ФЗ note: Customer / Name are stored as plaintext in the DB.
// Plan 06+ may flip the columns to bytea + envelope encrypt them once
// the project-wide PII pattern is established; the DTO surface stays
// string-typed.
type ProjectService struct {
	tx    projectTxRunner
	store api.ProjectStorePort
	audit auditapi.Logger
	// events is the optional NATS publisher. Plan 06 declares the
	// `crm.project.{created,updated,status_changed}` subjects in
	// internal/crm/api/events.go but Plan 11 owns the real NATS wire-
	// up; until then the composition root passes nil and we skip
	// publishing silently. Once Plan 11 lands, modulo wiring, every
	// state-changing ProjectService method will emit a typed event
	// without further code changes.
	events eventbus.Publisher
	clock  func() time.Time
}

// Compile-time assertion: *ProjectService must satisfy api.ProjectService.
var _ api.ProjectService = (*ProjectService)(nil)

// NewProjectService constructs a ProjectService from already-built deps.
// The caller (the module composition root) owns the lifecycle of every
// dependency. clock may be nil — the constructor falls back to
// time.Now so callers do not have to repeat that boilerplate.
//
// auditLogger MUST NOT be nil: every state-changing ProjectService
// method emits an audit row inside the same transaction as the data
// write, and a misconfigured composition root that registered nil
// would silently drop those rows. Tests that genuinely don't care
// about the audit trail must inject a no-op fake logger explicitly
// (see Plan 05 lessons learned § 10).
// publisher may be nil — when nil, all calls to publishEvent are no-ops
// (see Plan 11 deferral note on the events field). Tests that don't
// care about events pass nil; tests that DO care pass a fake.
func NewProjectService(
	pool projectTxRunner,
	store api.ProjectStorePort,
	auditLogger auditapi.Logger,
	publisher eventbus.Publisher,
	clock func() time.Time,
) *ProjectService {
	if pool == nil {
		panic("crm/service: NewProjectService: pool is required")
	}
	if store == nil {
		panic("crm/service: NewProjectService: store is required")
	}
	if auditLogger == nil {
		panic("crm/service: NewProjectService: auditLogger is required (use a no-op fake in tests, never nil)")
	}
	if clock == nil {
		clock = time.Now
	}
	return &ProjectService{
		tx:     pool,
		store:  store,
		audit:  auditLogger,
		events: publisher, // nil-tolerant; see field doc
		clock:  clock,
	}
}

// publishEvent fan-outs a typed event payload to the configured NATS
// publisher. nil events field (Plan 11 not yet wired) → no-op. Marshal
// failures are logged via the audit context but NOT returned, because
// the parent call already committed the DB row + audit; an event-side
// failure must not surface as user-visible "save failed". This matches
// the at-least-once + outbox-retry posture established by pkg/outbox.
func (s *ProjectService) publishEvent(ctx context.Context, subject string, payload any) {
	if s.events == nil {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		// Best-effort observability — the row write already succeeded.
		_ = s.writeAudit(ctx, auditapi.Event{
			Action:  "crm.event.publish_marshal_error",
			Payload: map[string]any{"subject": subject, "error": err.Error()},
		})
		return
	}
	if err := s.events.Publish(ctx, subject, body); err != nil {
		_ = s.writeAudit(ctx, auditapi.Event{
			Action:  "crm.event.publish_error",
			Payload: map[string]any{"subject": subject, "error": err.Error()},
		})
	}
}

// Create implements api.ProjectService.Create. Inserts a fresh project
// row with status=active and emits a "crm.project.created" audit row
// inside the same transaction as the row write. Rejects the
// is_advertising=true case up front (152-ФЗ scope, not 38-ФЗ).
func (s *ProjectService) Create(ctx context.Context, in api.CreateProjectInput) (*api.Project, error) {
	if err := validateCreateInput(in); err != nil {
		return nil, err
	}
	if in.IsAdvertising {
		// Reject before opening any tx so the early-exit doesn't pay a
		// pool round-trip for invalid input.
		return nil, api.ErrAdvertisingRejected
	}

	candidate := api.Project{
		TenantID:      in.TenantID,
		Code:          in.Code,
		Name:          in.Name,
		Customer:      in.Customer,
		Status:        api.StatusActive,
		TargetCount:   in.TargetCount,
		PeriodFrom:    in.PeriodFrom,
		PeriodTo:      in.PeriodTo,
		SurveyID:      in.SurveyID,
		IsAdvertising: false, // explicit: rejected case handled above
		CreatedBy:     actorIDFromContext(ctx),
	}

	var saved api.Project
	err := s.tx.WithTenant(ctx, in.TenantID, func(tx postgres.Tx) error {
		var err error
		saved, err = s.store.Insert(ctx, tx, candidate)
		if err != nil {
			return err
		}
		return s.writeAudit(ctx, auditapi.Event{
			TenantID: saved.TenantID,
			Action:   "crm.project.created",
			Target:   "project:" + saved.ID.String(),
			Payload: map[string]any{
				"code":         saved.Code,
				"name":         saved.Name,
				"customer":     saved.Customer,
				"target_count": saved.TargetCount,
			},
		})
	})
	if err != nil {
		// Bubble the sentinel as-is when it's a known error so callers
		// can errors.Is without losing the kind.
		if errors.Is(err, api.ErrProjectCodeTaken) {
			return nil, err
		}
		return nil, fmt.Errorf("crm/service: create project: %w", err)
	}
	s.publishEvent(ctx, api.SubjectProjectCreatedFor(saved.TenantID), api.ProjectCreatedEvent{
		ProjectID:   saved.ID,
		TenantID:    saved.TenantID,
		Code:        saved.Code,
		Name:        saved.Name,
		Customer:    saved.Customer,
		TargetCount: saved.TargetCount,
		CreatedAt:   saved.CreatedAt,
	})
	return &saved, nil
}

// Get implements api.ProjectService.Get. The lookup uses a BypassRLS
// transaction because the caller has not (necessarily) supplied a
// tenant context — admin tooling routinely needs to resolve a project
// id to its tenant before any per-tenant flow. This mirrors
// UserService.Get from auth/service.
//
// Quotas / Assignments are NOT populated by this method in Task 1;
// Plan 06 Task 2 fills those slices. For now Get returns the row-level
// fields only.
func (s *ProjectService) Get(ctx context.Context, id uuid.UUID) (*api.Project, error) {
	if id == uuid.Nil {
		return nil, fmt.Errorf("crm/service: get project: id required")
	}
	var p api.Project
	err := s.tx.BypassRLS(ctx, func(tx postgres.Tx) error {
		var err error
		p, err = s.store.GetByID(ctx, tx, id)
		return err
	})
	if err != nil {
		if errors.Is(err, api.ErrProjectNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("crm/service: get project: %w", err)
	}
	return &p, nil
}

// List implements api.ProjectService.List. Limit/Offset are clamped to
// the documented 50/500 bounds and 0-floor before the store call so a
// careless caller does not request millions of rows in one shot.
//
// IncludeArchived defaults to false (the zero value) — admin lists hide
// soft-deleted rows by default; the dedicated "show all" view passes
// IncludeArchived=true.
func (s *ProjectService) List(ctx context.Context, f api.ListProjectsFilter) (*api.ListProjectsResult, error) {
	if f.TenantID == uuid.Nil {
		return nil, fmt.Errorf("crm/service: list projects: tenant id required")
	}
	if f.Limit <= 0 {
		f.Limit = defaultListLimit
	}
	if f.Limit > maxListLimit {
		f.Limit = maxListLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	var (
		rows  []api.Project
		total int64
	)
	err := s.tx.WithTenant(ctx, f.TenantID, func(tx postgres.Tx) error {
		var err error
		rows, total, err = s.store.List(ctx, tx, f)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("crm/service: list projects: %w", err)
	}
	return &api.ListProjectsResult{
		Items:      rows,
		TotalCount: total,
	}, nil
}

// validateCreateInput checks the synchronous-rejection invariants on
// CreateProjectInput. TenantID, Code, and Name are mandatory; Code/Name
// have length caps so the index footprint stays bounded.
func validateCreateInput(in api.CreateProjectInput) error {
	if in.TenantID == uuid.Nil {
		return fmt.Errorf("crm/service: create project: tenant id required")
	}
	if in.Code == "" {
		return fmt.Errorf("crm/service: create project: code required")
	}
	if len(in.Code) > maxCodeLength {
		return fmt.Errorf("crm/service: create project: code exceeds %d chars", maxCodeLength)
	}
	if in.Name == "" {
		return fmt.Errorf("crm/service: create project: name required")
	}
	if len(in.Name) > maxNameLength {
		return fmt.Errorf("crm/service: create project: name exceeds %d chars", maxNameLength)
	}
	if in.TargetCount < 0 {
		return fmt.Errorf("crm/service: create project: target_count must be >= 0")
	}
	if in.PeriodFrom != nil && in.PeriodTo != nil && in.PeriodTo.Before(*in.PeriodFrom) {
		return fmt.Errorf("crm/service: create project: period_to must be >= period_from")
	}
	return nil
}

// Update implements api.ProjectService.Update.
//
// Resolves the project's tenant via a BypassRLS GetByID, then opens a
// per-tenant transaction (RLS in effect) and runs the partial-update.
// An empty patch is a true no-op: the service short-circuits before
// even opening the transaction so the audit trail stays clean (no
// "updated" row when nothing changed).
//
// Archived projects are rejected with ErrProjectArchived so callers
// don't accidentally mutate a soft-deleted row. The Update SQL itself
// also excludes archived rows; the up-front check exists to surface a
// clearer sentinel than a generic "not found".
//
// One audit row "crm.project.updated" is emitted on success carrying
// the diff payload (the keys that were actually patched). The
// transaction commits the row write and the audit row together — the
// service inherits that durability guarantee from writeAudit.
func (s *ProjectService) Update(ctx context.Context, id uuid.UUID, in api.UpdateProjectInput) (*api.Project, error) {
	if id == uuid.Nil {
		return nil, fmt.Errorf("crm/service: update project: %w", api.ErrInvalidArgument)
	}
	patch := api.UpdatePatch{
		Name:        in.Name,
		Customer:    in.Customer,
		TargetCount: in.TargetCount,
		PeriodFrom:  in.PeriodFrom,
		PeriodTo:    in.PeriodTo,
		SurveyID:    in.SurveyID,
	}

	current, err := s.lookupProject(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.ArchivedAt != nil {
		return nil, api.ErrProjectArchived
	}
	// Empty patch: short-circuit so we don't bump updated_at or audit a non-change.
	if patch.IsEmpty() {
		return &current, nil
	}

	var saved api.Project
	err = s.tx.WithTenant(ctx, current.TenantID, func(tx postgres.Tx) error {
		var err error
		saved, err = s.store.Update(ctx, tx, id, patch)
		if err != nil {
			return err
		}
		return s.writeAudit(ctx, auditapi.Event{
			TenantID: saved.TenantID,
			Action:   "crm.project.updated",
			Target:   "project:" + saved.ID.String(),
			Payload:  buildUpdatePayload(in),
		})
	})
	if err != nil {
		if errors.Is(err, api.ErrProjectNotFound) || errors.Is(err, api.ErrProjectArchived) {
			return nil, err
		}
		return nil, fmt.Errorf("crm/service: update project: %w", err)
	}
	s.publishEvent(ctx, api.SubjectProjectUpdatedFor(saved.TenantID), api.ProjectUpdatedEvent{
		ProjectID: saved.ID,
		TenantID:  saved.TenantID,
		Changed:   changedFieldNames(in),
		UpdatedAt: saved.UpdatedAt,
	})
	return &saved, nil
}

// changedFieldNames returns the json-tag names of fields whose pointer
// is non-nil in the patch. Stable order so consumers can rely on it.
func changedFieldNames(in api.UpdateProjectInput) []string {
	out := make([]string, 0, 5)
	if in.Name != nil {
		out = append(out, "name")
	}
	if in.Customer != nil {
		out = append(out, "customer")
	}
	if in.TargetCount != nil {
		out = append(out, "target_count")
	}
	if in.PeriodFrom != nil {
		out = append(out, "period_from")
	}
	if in.PeriodTo != nil {
		out = append(out, "period_to")
	}
	if in.SurveyID != nil {
		out = append(out, "survey_id")
	}
	return out
}

// Pause implements api.ProjectService.Pause: Active → Paused.
//
// State machine: Active→Paused commits and audits, Paused→Paused is a
// silent no-op (idempotent), Archived is rejected with ErrProjectArchived
// (terminal state guard).
func (s *ProjectService) Pause(ctx context.Context, id uuid.UUID) error {
	return s.transitionStatus(ctx, id, api.StatusPaused, "crm.project.paused")
}

// Resume implements api.ProjectService.Resume: Paused → Active.
//
// Symmetrical to Pause — Paused→Active commits/audits, Active→Active is
// a silent no-op, Archived is rejected.
func (s *ProjectService) Resume(ctx context.Context, id uuid.UUID) error {
	return s.transitionStatus(ctx, id, api.StatusActive, "crm.project.resumed")
}

// Archive implements api.ProjectService.Archive: terminal transition.
//
// Active|Paused → Archived commits/audits, Archived→Archived is a
// silent no-op (terminal idempotency). archived_at is stamped at the
// service clock so the timestamp matches the audit row exactly.
func (s *ProjectService) Archive(ctx context.Context, id uuid.UUID) error {
	return s.transitionStatus(ctx, id, api.StatusArchived, "crm.project.archived")
}

// transitionStatus is the shared engine for Pause/Resume/Archive. It
// resolves the project, runs the state-machine guard against the
// current status, and (when the transition is real, not a no-op) opens
// a per-tenant transaction to write the new status + audit row in one
// commit.
//
// Idempotency rules (locked in via the user prompt):
//
//	target == current         -> silent no-op (no SQL, no audit)
//	current == archived       -> ErrProjectArchived (unless target=archived,
//	                             which is the no-op above)
//	target  == archived       -> stamp archived_at at the service clock
//	any other transition path -> proceed.
func (s *ProjectService) transitionStatus(ctx context.Context, id uuid.UUID, target api.ProjectStatus, action string) error {
	if id == uuid.Nil {
		return fmt.Errorf("crm/service: %s: %w", action, api.ErrInvalidArgument)
	}
	current, err := s.lookupProject(ctx, id)
	if err != nil {
		return err
	}
	// Idempotent: same state → silent no-op.
	if current.Status == target {
		return nil
	}
	// Archived is terminal for non-Archive targets.
	if current.Status == api.StatusArchived {
		return api.ErrProjectArchived
	}

	var archivedAt *time.Time
	if target == api.StatusArchived {
		ts := s.clock().UTC()
		archivedAt = &ts
	}

	from := current.Status
	err = s.tx.WithTenant(ctx, current.TenantID, func(tx postgres.Tx) error {
		_, err := s.store.UpdateStatus(ctx, tx, id, target, archivedAt)
		if err != nil {
			return err
		}
		return s.writeAudit(ctx, auditapi.Event{
			TenantID: current.TenantID,
			Action:   action,
			Target:   "project:" + id.String(),
			Payload: map[string]any{
				"from": string(from),
				"to":   string(target),
			},
		})
	})
	if err != nil {
		if errors.Is(err, api.ErrProjectNotFound) {
			return err
		}
		return fmt.Errorf("crm/service: %s: %w", action, err)
	}
	s.publishEvent(ctx, api.SubjectProjectStatusFor(current.TenantID), api.ProjectStatusChangedEvent{
		ProjectID:  id,
		TenantID:   current.TenantID,
		OldStatus:  from,
		NewStatus:  target,
		ChangedAt:  s.clock().UTC(),
		ArchivedAt: archivedAt,
	})
	return nil
}

// GetProgress implements api.ProjectService.GetProgress.
//
// Reads only — no audit row, no event publish. Resolves tenant via
// BypassRLS GetByID, then runs AggregateProgress through a per-tenant
// tx so RLS still scopes the underlying respondents/calls reads.
//
// Derived metrics (PercentDone, PaceLast24h, ETACompletion) are
// computed here from the raw counters the store returns. The plan
// source builds these in a separate helper; we inline them since the
// math is small (3 lines) and the alternative is a one-method file.
//
// PaceLast24h and ETACompletion are stubbed in v1 — the calls table
// exists per migrations/000001 but isn't yet populated by any module
// (Plan 08+ owns the dialer). Returning 0/nil is honest — the
// dashboard renders "—" until calls flow.
func (s *ProjectService) GetProgress(ctx context.Context, id uuid.UUID) (*api.ProjectProgress, error) {
	if id == uuid.Nil {
		return nil, fmt.Errorf("crm/service: get progress: %w", api.ErrInvalidArgument)
	}
	current, err := s.lookupProject(ctx, id)
	if err != nil {
		return nil, err
	}

	var prog api.ProjectProgress
	err = s.tx.WithTenant(ctx, current.TenantID, func(tx postgres.Tx) error {
		var err error
		prog, err = s.store.AggregateProgress(ctx, tx, id)
		return err
	})
	if err != nil {
		if errors.Is(err, api.ErrProjectNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("crm/service: get progress: %w", err)
	}

	if prog.TargetCount > 0 {
		prog.PercentDone = float64(prog.CompletedCount) / float64(prog.TargetCount) * 100
	}
	return &prog, nil
}

// Assign implements api.ProjectService.Assign with MERGE semantics.
//
// Empty input → ErrInvalidArgument. Duplicates in the input slice are
// de-duplicated up front (the store would also handle them via
// ON CONFLICT, but de-duping here saves a placeholder slot per dup).
//
// One audit row per *newly* added operator (RETURNING tells us which
// were inserted) so the audit trail has a 1:1 mapping with member-
// joined events. Operators that were already members are silently
// skipped — no audit row, no error.
func (s *ProjectService) Assign(ctx context.Context, id uuid.UUID, operatorIDs []uuid.UUID) error {
	dedup, err := s.validateAssignInput(id, operatorIDs)
	if err != nil {
		return err
	}
	current, err := s.lookupProject(ctx, id)
	if err != nil {
		return err
	}
	if current.ArchivedAt != nil {
		return api.ErrProjectArchived
	}

	err = s.tx.WithTenant(ctx, current.TenantID, func(tx postgres.Tx) error {
		return s.applyAssign(ctx, tx, id, current.TenantID, dedup)
	})
	if err != nil {
		if errors.Is(err, api.ErrProjectNotFound) || errors.Is(err, api.ErrProjectArchived) {
			return err
		}
		return fmt.Errorf("crm/service: assign: %w", err)
	}
	return nil
}

// validateAssignInput pre-checks Assign inputs and returns the
// deduplicated, non-nil operator id slice. Surfaced as a helper so the
// public Assign method stays under gocognit's complexity ceiling — this
// is the trio of guards that would otherwise pile branches on the
// happy-path closure.
func (s *ProjectService) validateAssignInput(id uuid.UUID, operatorIDs []uuid.UUID) ([]uuid.UUID, error) {
	if id == uuid.Nil {
		return nil, fmt.Errorf("crm/service: assign: %w", api.ErrInvalidArgument)
	}
	if len(operatorIDs) == 0 {
		return nil, fmt.Errorf("crm/service: assign: operator ids required: %w", api.ErrInvalidArgument)
	}
	dedup := dedupNonNil(operatorIDs)
	if len(dedup) == 0 {
		return nil, fmt.Errorf("crm/service: assign: operator ids required: %w", api.ErrInvalidArgument)
	}
	return dedup, nil
}

// applyAssign is the inner-tx Assign worker. Snapshots the existing
// members so we can audit precisely the rows the store inserted
// (RETURNING from AssignOperators gives count, not ids), runs the
// MERGE insert, and emits one audit row per newly-added operator.
//
// Extracted from Assign so the public method stays under gocognit's
// complexity ceiling (Plan 05 lessons learned § 7).
func (s *ProjectService) applyAssign(ctx context.Context, tx postgres.Tx, id, tenantID uuid.UUID, ops []uuid.UUID) error {
	existing, err := s.store.ListMembers(ctx, tx, id)
	if err != nil {
		return err
	}
	existingSet := make(map[uuid.UUID]struct{}, len(existing))
	for _, m := range existing {
		existingSet[m.OperatorID] = struct{}{}
	}

	if _, err := s.store.AssignOperators(ctx, tx, id, ops); err != nil {
		return err
	}

	for _, op := range ops {
		if _, already := existingSet[op]; already {
			continue
		}
		if err := s.writeAudit(ctx, auditapi.Event{
			TenantID: tenantID,
			Action:   "crm.project.member_assigned",
			Target:   "user:" + op.String(),
			Payload: map[string]any{
				"project_id":  id,
				"operator_id": op,
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

// Unassign implements api.ProjectService.Unassign.
//
// One DELETE; one audit row when the row was actually present (the
// store returns deleted=false for unknown operators, in which case we
// silently no-op — no error, no audit). Archived projects still allow
// Unassign to support cleanup of soft-deleted projects' rosters.
func (s *ProjectService) Unassign(ctx context.Context, id uuid.UUID, operatorID uuid.UUID) error {
	if id == uuid.Nil || operatorID == uuid.Nil {
		return fmt.Errorf("crm/service: unassign: %w", api.ErrInvalidArgument)
	}
	current, err := s.lookupProject(ctx, id)
	if err != nil {
		return err
	}

	err = s.tx.WithTenant(ctx, current.TenantID, func(tx postgres.Tx) error {
		deleted, err := s.store.UnassignOperator(ctx, tx, id, operatorID)
		if err != nil {
			return err
		}
		if !deleted {
			return nil
		}
		return s.writeAudit(ctx, auditapi.Event{
			TenantID: current.TenantID,
			Action:   "crm.project.member_unassigned",
			Target:   "user:" + operatorID.String(),
			Payload: map[string]any{
				"project_id":  id,
				"operator_id": operatorID,
			},
		})
	})
	if err != nil {
		if errors.Is(err, api.ErrProjectNotFound) {
			return err
		}
		return fmt.Errorf("crm/service: unassign: %w", err)
	}
	return nil
}

// ListMembers implements api.ProjectService.ListMembers.
//
// Pure read — no audit row, no event publish. Resolves tenant via
// BypassRLS GetByID, then issues the join through a per-tenant tx so
// the users-table read is RLS-scoped (a tenant cannot enumerate
// another's users via a stale project_assignments row).
func (s *ProjectService) ListMembers(ctx context.Context, id uuid.UUID) ([]api.ProjectMember, error) {
	if id == uuid.Nil {
		return nil, fmt.Errorf("crm/service: list members: %w", api.ErrInvalidArgument)
	}
	current, err := s.lookupProject(ctx, id)
	if err != nil {
		return nil, err
	}

	var members []api.ProjectMember
	err = s.tx.WithTenant(ctx, current.TenantID, func(tx postgres.Tx) error {
		var err error
		members, err = s.store.ListMembers(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("crm/service: list members: %w", err)
	}
	return members, nil
}

// lookupProject reads the row by id via BypassRLS so the caller doesn't
// have to know the tenant up front. Returns api.ErrProjectNotFound on a
// missing row; otherwise wraps the underlying error.
func (s *ProjectService) lookupProject(ctx context.Context, id uuid.UUID) (api.Project, error) {
	var p api.Project
	err := s.tx.BypassRLS(ctx, func(tx postgres.Tx) error {
		var err error
		p, err = s.store.GetByID(ctx, tx, id)
		return err
	})
	if err != nil {
		if errors.Is(err, api.ErrProjectNotFound) {
			return api.Project{}, err
		}
		return api.Project{}, fmt.Errorf("crm/service: lookup project: %w", err)
	}
	return p, nil
}

// dedupNonNil returns a stable-ordered slice of unique non-nil UUIDs
// from src. Stable order matters for the test suite and for any future
// transactional ordering guarantees.
func dedupNonNil(src []uuid.UUID) []uuid.UUID {
	if len(src) == 0 {
		return nil
	}
	seen := make(map[uuid.UUID]struct{}, len(src))
	out := make([]uuid.UUID, 0, len(src))
	for _, id := range src {
		if id == uuid.Nil {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// buildUpdatePayload renders the audit payload for "crm.project.updated"
// — only the keys the caller actually patched, so the trail stays
// reviewable.
func buildUpdatePayload(in api.UpdateProjectInput) map[string]any {
	out := make(map[string]any, 6)
	if in.Name != nil {
		out["name"] = *in.Name
	}
	if in.Customer != nil {
		out["customer"] = *in.Customer
	}
	if in.TargetCount != nil {
		out["target_count"] = *in.TargetCount
	}
	if in.PeriodFrom != nil {
		out["period_from"] = *in.PeriodFrom
	}
	if in.PeriodTo != nil {
		out["period_to"] = *in.PeriodTo
	}
	if in.SurveyID != nil {
		out["survey_id"] = *in.SurveyID
	}
	return out
}
