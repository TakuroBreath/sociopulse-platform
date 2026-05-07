package api

import (
	"context"

	"github.com/google/uuid"
)

// ProjectService is the public CRUD surface for projects.
type ProjectService interface {
	// Create allocates a new project row and any initial quotas/members.
	Create(ctx context.Context, in CreateProjectInput) (*Project, error)
	// Get returns the project with the given ID, or ErrProjectNotFound.
	Get(ctx context.Context, id uuid.UUID) (*Project, error)
	// List returns one page of projects matching f, plus the total count.
	List(ctx context.Context, f ListProjectsFilter) (*ListProjectsResult, error)
	// Update applies the patch fields in in to the project.
	Update(ctx context.Context, id uuid.UUID, in UpdateProjectInput) (*Project, error)
	// Pause transitions the project to StatusPaused.
	Pause(ctx context.Context, id uuid.UUID) error
	// Resume transitions the project from StatusPaused back to StatusActive.
	Resume(ctx context.Context, id uuid.UUID) error
	// Archive transitions the project to StatusArchived (terminal).
	Archive(ctx context.Context, id uuid.UUID) error
	// GetProgress returns live counters for the dashboard widget.
	GetProgress(ctx context.Context, id uuid.UUID) (*ProjectProgress, error)
	// Assign attaches operators to a project.
	Assign(ctx context.Context, id uuid.UUID, operatorIDs []uuid.UUID) error
	// Unassign detaches an operator from a project.
	Unassign(ctx context.Context, id uuid.UUID, operatorID uuid.UUID) error
	// ListMembers returns the current operator assignments.
	ListMembers(ctx context.Context, id uuid.UUID) ([]ProjectMember, error)
}

// RespondentService is the public surface for respondent CRUD + async import.
type RespondentService interface {
	// Create inserts a respondent. Phone is normalised and hashed; if the
	// phone is on DNC the call returns ErrPhoneInDNC.
	Create(ctx context.Context, in CreateRespondentInput) (*Respondent, error)
	// Get returns a respondent with the masked phone (operator-safe).
	Get(ctx context.Context, id uuid.UUID) (*Respondent, error)
	// GetWithPhone returns a respondent with the raw phone populated.
	// Restricted to admin role; the call is mirrored to the audit log.
	GetWithPhone(ctx context.Context, id uuid.UUID) (*Respondent, error)
	// Search returns one page of respondents matching f, plus the total count.
	Search(ctx context.Context, f SearchRespondentsFilter) (*SearchRespondentsResult, error)
	// Delete soft-deletes the respondent and schedules the 30-day purge.
	Delete(ctx context.Context, id uuid.UUID) (*DeletionRequest, error)
	// Import enqueues a CSV/XLSX import. Returns a ticket; progress is
	// observed via GetImportStatus and the import.* NATS events.
	Import(ctx context.Context, req ImportRequest) (*ImportTicket, error)
	// GetImportStatus returns the current state of an in-flight or finished import.
	GetImportStatus(ctx context.Context, jobID string) (*ImportStatus, error)
}

// QuotaTracker exposes the live quota counter store. It is hot-path: every
// dialer attempt calls IsFull, every successful survey calls Increment.
type QuotaTracker interface {
	// IsFull returns true when the quota cell identified by dims is at target.
	IsFull(ctx context.Context, projectID uuid.UUID, dims map[string]string) (bool, error)
	// Increment atomically bumps the Done counter for the dims cell.
	Increment(ctx context.Context, projectID uuid.UUID, dims map[string]string) error
	// GetProgress returns one snapshot row per quota cell.
	GetProgress(ctx context.Context, projectID uuid.UUID) ([]QuotaSnapshot, error)
}

// DNCManager owns the project-scoped and tenant-wide do-not-call list.
type DNCManager interface {
	// IsBlocked returns true when phone is on the DNC list for the given
	// project. ProjectID is used to scope per-project DNC; pass the
	// tenant-wide project sentinel when checking globally.
	IsBlocked(ctx context.Context, projectID uuid.UUID, phone string) (bool, error)
	// Add inserts an entry. Pass projectID=nil for a tenant-wide entry.
	Add(ctx context.Context, projectID *uuid.UUID, phone, source string) error
	// Remove deletes an entry. Pass projectID=nil for a tenant-wide entry.
	Remove(ctx context.Context, projectID *uuid.UUID, phone string) error
	// Import bulk-loads CSV bytes.
	Import(ctx context.Context, projectID *uuid.UUID, csv []byte) (added int, err error)
	// List returns one page of DNC entries; PhoneMasked is the only phone field returned.
	List(ctx context.Context, projectID *uuid.UUID, page, pageSize int) ([]DNCEntry, int, error)
}
