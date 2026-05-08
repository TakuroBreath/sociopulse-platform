package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	"github.com/sociopulse/platform/internal/crm/api"
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
	clock func() time.Time
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
func NewProjectService(
	pool projectTxRunner,
	store api.ProjectStorePort,
	auditLogger auditapi.Logger,
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
		tx:    pool,
		store: store,
		audit: auditLogger,
		clock: clock,
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

// Update / Pause / Resume / Archive / GetProgress / Assign / Unassign /
// ListMembers — Plan 06 Task 2+ implements these. Stub them out so
// *ProjectService still satisfies api.ProjectService at compile time.

// Update implements api.ProjectService.Update — Plan 06 Task 2 fills it in.
func (s *ProjectService) Update(_ context.Context, _ uuid.UUID, _ api.UpdateProjectInput) (*api.Project, error) {
	return nil, fmt.Errorf("crm/service: update: %w", errNotImplemented)
}

// Pause implements api.ProjectService.Pause — Plan 06 Task 2 fills it in.
func (s *ProjectService) Pause(_ context.Context, _ uuid.UUID) error {
	return fmt.Errorf("crm/service: pause: %w", errNotImplemented)
}

// Resume implements api.ProjectService.Resume — Plan 06 Task 2 fills it in.
func (s *ProjectService) Resume(_ context.Context, _ uuid.UUID) error {
	return fmt.Errorf("crm/service: resume: %w", errNotImplemented)
}

// Archive implements api.ProjectService.Archive — Plan 06 Task 2 fills it in.
func (s *ProjectService) Archive(_ context.Context, _ uuid.UUID) error {
	return fmt.Errorf("crm/service: archive: %w", errNotImplemented)
}

// GetProgress implements api.ProjectService.GetProgress — Plan 06 Task 2 fills it in.
func (s *ProjectService) GetProgress(_ context.Context, _ uuid.UUID) (*api.ProjectProgress, error) {
	return nil, fmt.Errorf("crm/service: get progress: %w", errNotImplemented)
}

// Assign implements api.ProjectService.Assign — Plan 06 Task 2 fills it in.
func (s *ProjectService) Assign(_ context.Context, _ uuid.UUID, _ []uuid.UUID) error {
	return fmt.Errorf("crm/service: assign: %w", errNotImplemented)
}

// Unassign implements api.ProjectService.Unassign — Plan 06 Task 2 fills it in.
func (s *ProjectService) Unassign(_ context.Context, _ uuid.UUID, _ uuid.UUID) error {
	return fmt.Errorf("crm/service: unassign: %w", errNotImplemented)
}

// ListMembers implements api.ProjectService.ListMembers — Plan 06 Task 2 fills it in.
func (s *ProjectService) ListMembers(_ context.Context, _ uuid.UUID) ([]api.ProjectMember, error) {
	return nil, fmt.Errorf("crm/service: list members: %w", errNotImplemented)
}

// errNotImplemented marks the deferred lifecycle methods. Returned as
// the wrapped sentinel so callers can errors.Is for graceful UI
// fall-back during the multi-task rollout.
var errNotImplemented = errors.New("not implemented in Plan 06 Task 1")
