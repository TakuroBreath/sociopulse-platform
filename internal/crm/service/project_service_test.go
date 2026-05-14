package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// fakeTxRunner runs every fn synchronously with a zero postgres.Tx —
// the store fakes never read from it, so we don't need to spin up a
// real database. It records the supplied tenant ids so tests can
// confirm the service picked the correct one for each call.
type fakeTxRunner struct {
	mu                sync.Mutex
	withTenantTenants []uuid.UUID
	bypassCount       int
}

func (f *fakeTxRunner) WithTenant(_ context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error {
	f.mu.Lock()
	f.withTenantTenants = append(f.withTenantTenants, tenantID)
	f.mu.Unlock()
	return fn(postgres.Tx{})
}

func (f *fakeTxRunner) BypassRLS(_ context.Context, fn func(postgres.Tx) error) error {
	f.mu.Lock()
	f.bypassCount++
	f.mu.Unlock()
	return fn(postgres.Tx{})
}

// fakeProjectStore is a hand-rolled api.ProjectStorePort fake. We avoid
// gomock to keep the dependency surface tight and the test code
// readable — each method returns the canned response set by the test.
type fakeProjectStore struct {
	mu sync.Mutex

	// In-memory state.
	projects  map[uuid.UUID]crmapi.Project
	codeIndex map[string]uuid.UUID // (tenantID + lower(code)) -> id
	// members[projectID] is the slice of currently-assigned operators
	// in insertion order, mirroring the real store's `ORDER BY
	// assigned_at ASC` semantic.
	members map[uuid.UUID][]crmapi.ProjectMember
	// progress[projectID] is the canned progress snapshot the next
	// AggregateProgress call returns. When unset, falls back to a
	// derived snapshot (target_count from project + zero counters).
	progress map[uuid.UUID]crmapi.ProjectProgress

	// Programmable error injection: when not nil for a method, the next
	// call returns it (and clears the slot).
	insertErr      error
	getByIDErr     error
	listErr        error
	updateErr      error
	statusErr      error
	progressErr    error
	assignErr      error
	unassignErr    error
	listMembersErr error
}

func newFakeProjectStore() *fakeProjectStore {
	return &fakeProjectStore{
		projects:  make(map[uuid.UUID]crmapi.Project),
		codeIndex: make(map[string]uuid.UUID),
		members:   make(map[uuid.UUID][]crmapi.ProjectMember),
		progress:  make(map[uuid.UUID]crmapi.ProjectProgress),
	}
}

func codeKey(tenantID uuid.UUID, code string) string {
	return tenantID.String() + "|" + strings.ToLower(code)
}

// seed inserts a project directly into the fake store for tests that
// need pre-existing rows (e.g. duplicate-code, archived-list).
func (s *fakeProjectStore) seed(p crmapi.Project) crmapi.Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = p.CreatedAt
	}
	if p.Status == "" {
		p.Status = crmapi.StatusActive
	}
	s.projects[p.ID] = p
	s.codeIndex[codeKey(p.TenantID, p.Code)] = p.ID
	return p
}

func (s *fakeProjectStore) Insert(_ context.Context, _ postgres.Tx, p crmapi.Project) (crmapi.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertErr != nil {
		err := s.insertErr
		s.insertErr = nil
		return crmapi.Project{}, err
	}
	if _, exists := s.codeIndex[codeKey(p.TenantID, p.Code)]; exists {
		return crmapi.Project{}, crmapi.ErrProjectCodeTaken
	}
	p.ID = uuid.New()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	p.CreatedAt = now
	p.UpdatedAt = now
	if p.Status == "" {
		p.Status = crmapi.StatusActive
	}
	s.projects[p.ID] = p
	s.codeIndex[codeKey(p.TenantID, p.Code)] = p.ID
	return p, nil
}

func (s *fakeProjectStore) GetByID(_ context.Context, _ postgres.Tx, id uuid.UUID) (crmapi.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getByIDErr != nil {
		err := s.getByIDErr
		s.getByIDErr = nil
		return crmapi.Project{}, err
	}
	p, ok := s.projects[id]
	if !ok {
		return crmapi.Project{}, crmapi.ErrProjectNotFound
	}
	return p, nil
}

func (s *fakeProjectStore) GetByCode(_ context.Context, _ postgres.Tx, tenantID uuid.UUID, code string) (crmapi.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.codeIndex[codeKey(tenantID, code)]
	if !ok {
		return crmapi.Project{}, crmapi.ErrProjectNotFound
	}
	return s.projects[id], nil
}

func (s *fakeProjectStore) List(_ context.Context, _ postgres.Tx, f crmapi.ListProjectsFilter) ([]crmapi.Project, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		err := s.listErr
		s.listErr = nil
		return nil, 0, err
	}
	out := make([]crmapi.Project, 0, len(s.projects))
	for _, p := range s.projects {
		if p.TenantID != f.TenantID {
			continue
		}
		if !f.IncludeArchived && p.ArchivedAt != nil {
			continue
		}
		if f.Status != nil && p.Status != *f.Status {
			continue
		}
		out = append(out, p)
	}
	return out, int64(len(out)), nil
}

func (s *fakeProjectStore) Update(_ context.Context, _ postgres.Tx, id uuid.UUID, patch crmapi.UpdatePatch) (crmapi.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateErr != nil {
		err := s.updateErr
		s.updateErr = nil
		return crmapi.Project{}, err
	}
	p, ok := s.projects[id]
	if !ok || p.ArchivedAt != nil {
		return crmapi.Project{}, crmapi.ErrProjectNotFound
	}
	if patch.Name != nil {
		p.Name = *patch.Name
	}
	if patch.Customer != nil {
		p.Customer = *patch.Customer
	}
	if patch.TargetCount != nil {
		p.TargetCount = *patch.TargetCount
	}
	if patch.PeriodFrom != nil {
		from := *patch.PeriodFrom
		p.PeriodFrom = &from
	}
	if patch.PeriodTo != nil {
		to := *patch.PeriodTo
		p.PeriodTo = &to
	}
	if patch.SurveyID != nil {
		sid := *patch.SurveyID
		p.SurveyID = &sid
	}
	p.UpdatedAt = time.Date(2026, 5, 8, 12, 0, 1, 0, time.UTC)
	s.projects[id] = p
	return p, nil
}

func (s *fakeProjectStore) UpdateStatus(_ context.Context, _ postgres.Tx, id uuid.UUID, newStatus crmapi.ProjectStatus, archivedAt *time.Time) (crmapi.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusErr != nil {
		err := s.statusErr
		s.statusErr = nil
		return crmapi.Project{}, err
	}
	p, ok := s.projects[id]
	if !ok {
		return crmapi.Project{}, crmapi.ErrProjectNotFound
	}
	p.Status = newStatus
	if archivedAt != nil {
		stamp := *archivedAt
		p.ArchivedAt = &stamp
	}
	p.UpdatedAt = time.Date(2026, 5, 8, 12, 0, 1, 0, time.UTC)
	s.projects[id] = p
	return p, nil
}

func (s *fakeProjectStore) AggregateProgress(_ context.Context, _ postgres.Tx, projectID uuid.UUID) (crmapi.ProjectProgress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.progressErr != nil {
		err := s.progressErr
		s.progressErr = nil
		return crmapi.ProjectProgress{}, err
	}
	p, ok := s.projects[projectID]
	if !ok {
		return crmapi.ProjectProgress{}, crmapi.ErrProjectNotFound
	}
	if pre, ok := s.progress[projectID]; ok {
		pre.ProjectID = projectID
		if pre.TargetCount == 0 {
			pre.TargetCount = p.TargetCount
		}
		return pre, nil
	}
	return crmapi.ProjectProgress{
		ProjectID:   projectID,
		TargetCount: p.TargetCount,
	}, nil
}

func (s *fakeProjectStore) AssignOperators(_ context.Context, _ postgres.Tx, projectID uuid.UUID, operatorIDs []uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.assignErr != nil {
		err := s.assignErr
		s.assignErr = nil
		return 0, err
	}
	if _, ok := s.projects[projectID]; !ok {
		return 0, crmapi.ErrProjectNotFound
	}
	current := s.members[projectID]
	existing := make(map[uuid.UUID]struct{}, len(current))
	for _, m := range current {
		existing[m.OperatorID] = struct{}{}
	}
	added := 0
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	for _, op := range operatorIDs {
		if _, ok := existing[op]; ok {
			continue
		}
		current = append(current, crmapi.ProjectMember{
			OperatorID: op,
			AssignedAt: now,
			Login:      "op-" + op.String()[:8],
			FullName:   "Operator " + op.String()[:8],
		})
		existing[op] = struct{}{}
		added++
	}
	s.members[projectID] = current
	return added, nil
}

func (s *fakeProjectStore) UnassignOperator(_ context.Context, _ postgres.Tx, projectID uuid.UUID, operatorID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unassignErr != nil {
		err := s.unassignErr
		s.unassignErr = nil
		return false, err
	}
	if _, ok := s.projects[projectID]; !ok {
		return false, crmapi.ErrProjectNotFound
	}
	current := s.members[projectID]
	for i, m := range current {
		if m.OperatorID == operatorID {
			s.members[projectID] = append(current[:i], current[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (s *fakeProjectStore) ListMembers(_ context.Context, _ postgres.Tx, projectID uuid.UUID) ([]crmapi.ProjectMember, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listMembersErr != nil {
		err := s.listMembersErr
		s.listMembersErr = nil
		return nil, err
	}
	current := s.members[projectID]
	out := make([]crmapi.ProjectMember, len(current))
	copy(out, current)
	return out, nil
}

// fakeAudit captures every Write call.
type fakeAudit struct {
	mu     sync.Mutex
	events []auditapi.Event
}

func (a *fakeAudit) Write(_ context.Context, ev auditapi.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
	return nil
}

func (a *fakeAudit) snapshot() []auditapi.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]auditapi.Event, len(a.events))
	copy(out, a.events)
	return out
}

// newSvc builds a ProjectService backed by hand-rolled fakes. The
// returned references are owned by the caller so tests can inspect
// recorded state directly.
func newSvc(t *testing.T) (*ProjectService, *fakeProjectStore, *fakeAudit) {
	t.Helper()
	tx := &fakeTxRunner{}
	store := newFakeProjectStore()
	audit := &fakeAudit{}
	clock := func() time.Time { return time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC) }
	svc := NewProjectService(tx, store, audit, nil /* events: Plan 11 owns */, clock)
	return svc, store, audit
}

func TestProjectService_Create_HappyPath(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	tenantID := uuid.New()
	actor := uuid.New()
	ctx := WithActorID(context.Background(), actor)

	got, err := svc.Create(ctx, crmapi.CreateProjectInput{
		TenantID:    tenantID,
		Code:        "P-001",
		Name:        "Pilot",
		Customer:    "ВЦИОМ",
		TargetCount: 1000,
	})
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotEqual(t, uuid.Nil, got.ID)
	require.Equal(t, tenantID, got.TenantID)
	require.Equal(t, "P-001", got.Code)
	require.Equal(t, "Pilot", got.Name)
	require.Equal(t, "ВЦИОМ", got.Customer)
	require.Equal(t, crmapi.StatusActive, got.Status)
	require.Equal(t, 1000, got.TargetCount)
	require.False(t, got.IsAdvertising)
	require.NotNil(t, got.CreatedBy)
	require.Equal(t, actor, *got.CreatedBy)

	// Audit row emitted with action crm.project.created.
	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "crm.project.created", events[0].Action)
	require.Equal(t, "project:"+got.ID.String(), events[0].Target)
	require.Equal(t, tenantID, events[0].TenantID)
	require.NotNil(t, events[0].ActorID)
	require.Equal(t, actor, *events[0].ActorID)
	require.Equal(t, auditapi.ActorUser, events[0].ActorKind)
	require.False(t, events[0].Timestamp.IsZero())
	require.Equal(t, "P-001", events[0].Payload["code"])
	require.Equal(t, "Pilot", events[0].Payload["name"])
	require.Equal(t, "ВЦИОМ", events[0].Payload["customer"])
	require.Equal(t, 1000, events[0].Payload["target_count"])

	// Store now contains the row.
	require.Contains(t, store.projects, got.ID)
}

func TestProjectService_Create_RejectsAdvertising(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	tenantID := uuid.New()

	_, err := svc.Create(context.Background(), crmapi.CreateProjectInput{
		TenantID:      tenantID,
		Code:          "AD-001",
		Name:          "Advertising",
		IsAdvertising: true,
	})
	require.ErrorIs(t, err, crmapi.ErrAdvertisingRejected)

	// No row written, no audit row emitted.
	require.Empty(t, store.projects)
	require.Empty(t, audit.snapshot())
}

func TestProjectService_Create_DuplicateCodeReturnsErrProjectCodeTaken(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	tenantID := uuid.New()
	store.seed(crmapi.Project{
		TenantID: tenantID,
		Code:     "DUP-1",
		Name:     "First",
		Status:   crmapi.StatusActive,
	})

	_, err := svc.Create(context.Background(), crmapi.CreateProjectInput{
		TenantID: tenantID,
		Code:     "DUP-1",
		Name:     "Second",
	})
	require.ErrorIs(t, err, crmapi.ErrProjectCodeTaken)

	// Failed Create still must NOT have written an audit row.
	require.Empty(t, audit.snapshot())
}

func TestProjectService_Create_ValidationGuards(t *testing.T) {
	t.Parallel()

	svc, _, _ := newSvc(t)
	tenantID := uuid.New()

	// Missing tenant.
	_, err := svc.Create(context.Background(), crmapi.CreateProjectInput{
		Code: "X", Name: "X",
	})
	require.Error(t, err)

	// Missing code.
	_, err = svc.Create(context.Background(), crmapi.CreateProjectInput{
		TenantID: tenantID, Name: "X",
	})
	require.Error(t, err)

	// Missing name.
	_, err = svc.Create(context.Background(), crmapi.CreateProjectInput{
		TenantID: tenantID, Code: "X",
	})
	require.Error(t, err)

	// Code too long.
	_, err = svc.Create(context.Background(), crmapi.CreateProjectInput{
		TenantID: tenantID,
		Code:     strings.Repeat("a", 65),
		Name:     "x",
	})
	require.Error(t, err)

	// Name too long.
	_, err = svc.Create(context.Background(), crmapi.CreateProjectInput{
		TenantID: tenantID,
		Code:     "x",
		Name:     strings.Repeat("a", 201),
	})
	require.Error(t, err)

	// Negative target count.
	_, err = svc.Create(context.Background(), crmapi.CreateProjectInput{
		TenantID:    tenantID,
		Code:        "x",
		Name:        "x",
		TargetCount: -1,
	})
	require.Error(t, err)

	// Reverse period.
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	_, err = svc.Create(context.Background(), crmapi.CreateProjectInput{
		TenantID:   tenantID,
		Code:       "x",
		Name:       "x",
		PeriodFrom: &from,
		PeriodTo:   &to,
	})
	require.Error(t, err)
}

func TestProjectService_Create_AuditFallsBackToSystemActor(t *testing.T) {
	t.Parallel()

	svc, _, audit := newSvc(t)

	_, err := svc.Create(context.Background(), crmapi.CreateProjectInput{
		TenantID: uuid.New(),
		Code:     "SYS-1",
		Name:     "System Bootstrap",
	})
	require.NoError(t, err)

	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Nil(t, events[0].ActorID)
	require.Equal(t, auditapi.ActorSystem, events[0].ActorKind)
}

func TestProjectService_Create_PropagatesGenericStoreError(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	store.insertErr = errors.New("disk full")

	_, err := svc.Create(context.Background(), crmapi.CreateProjectInput{
		TenantID: uuid.New(),
		Code:     "ERR-1",
		Name:     "Generic Error",
	})
	require.ErrorContains(t, err, "disk full")
}

func TestProjectService_Get_HappyPath(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	tenantID := uuid.New()
	seeded := store.seed(crmapi.Project{
		TenantID:    tenantID,
		Code:        "G-1",
		Name:        "Get Happy",
		Status:      crmapi.StatusActive,
		TargetCount: 500,
	})

	got, err := svc.Get(context.Background(), tenantID, seeded.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, seeded.ID, got.ID)
	require.Equal(t, "G-1", got.Code)
	require.Equal(t, "Get Happy", got.Name)
}

func TestProjectService_Get_NotFound(t *testing.T) {
	t.Parallel()

	svc, _, _ := newSvc(t)
	_, err := svc.Get(context.Background(), uuid.New(), uuid.New())
	require.ErrorIs(t, err, crmapi.ErrProjectNotFound)
}

func TestProjectService_Get_RejectsNilID(t *testing.T) {
	t.Parallel()

	svc, _, _ := newSvc(t)
	_, err := svc.Get(context.Background(), uuid.New(), uuid.Nil)
	require.Error(t, err)
}

func TestProjectService_Get_PropagatesGenericStoreError(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	store.getByIDErr = errors.New("connection refused")

	_, err := svc.Get(context.Background(), uuid.New(), uuid.New())
	require.ErrorContains(t, err, "connection refused")
}

func TestProjectService_List_HappyPath(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	tenantID := uuid.New()
	store.seed(crmapi.Project{TenantID: tenantID, Code: "A", Name: "A", Status: crmapi.StatusActive})
	store.seed(crmapi.Project{TenantID: tenantID, Code: "B", Name: "B", Status: crmapi.StatusActive})

	got, err := svc.List(context.Background(), crmapi.ListProjectsFilter{TenantID: tenantID})
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Items, 2)
	require.EqualValues(t, 2, got.TotalCount)
}

func TestProjectService_List_RequiresTenant(t *testing.T) {
	t.Parallel()

	svc, _, _ := newSvc(t)
	_, err := svc.List(context.Background(), crmapi.ListProjectsFilter{})
	require.Error(t, err)
}

func TestProjectService_List_DefaultsAndClamping(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()

	// Limit defaults to 50 when zero.
	svc, _, _ := newSvc(t)
	_, err := svc.List(context.Background(), crmapi.ListProjectsFilter{TenantID: tenantID})
	require.NoError(t, err)

	// Negative limit/offset clamped to default/zero.
	_, err = svc.List(context.Background(), crmapi.ListProjectsFilter{
		TenantID: tenantID,
		Limit:    -10,
		Offset:   -1,
	})
	require.NoError(t, err)

	// Limit > 500 clamped to 500.
	_, err = svc.List(context.Background(), crmapi.ListProjectsFilter{
		TenantID: tenantID,
		Limit:    9999,
	})
	require.NoError(t, err)
}

func TestProjectService_List_ExcludesArchivedByDefault(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	tenantID := uuid.New()
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	store.seed(crmapi.Project{TenantID: tenantID, Code: "ACT", Name: "Active", Status: crmapi.StatusActive})
	store.seed(crmapi.Project{TenantID: tenantID, Code: "ARC", Name: "Archived", Status: crmapi.StatusArchived, ArchivedAt: &now})

	got, err := svc.List(context.Background(), crmapi.ListProjectsFilter{TenantID: tenantID})
	require.NoError(t, err)
	require.Len(t, got.Items, 1)
	require.EqualValues(t, 1, got.TotalCount)
	require.Equal(t, "ACT", got.Items[0].Code)
}

func TestProjectService_List_IncludeArchived(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	tenantID := uuid.New()
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	store.seed(crmapi.Project{TenantID: tenantID, Code: "ACT", Name: "Active", Status: crmapi.StatusActive})
	store.seed(crmapi.Project{TenantID: tenantID, Code: "ARC", Name: "Archived", Status: crmapi.StatusArchived, ArchivedAt: &now})

	got, err := svc.List(context.Background(), crmapi.ListProjectsFilter{
		TenantID:        tenantID,
		IncludeArchived: true,
	})
	require.NoError(t, err)
	require.Len(t, got.Items, 2)
	require.EqualValues(t, 2, got.TotalCount)
}

func TestProjectService_List_StatusFilter(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	tenantID := uuid.New()
	store.seed(crmapi.Project{TenantID: tenantID, Code: "ACT", Name: "A", Status: crmapi.StatusActive})
	store.seed(crmapi.Project{TenantID: tenantID, Code: "PAU", Name: "P", Status: crmapi.StatusPaused})

	paused := crmapi.StatusPaused
	got, err := svc.List(context.Background(), crmapi.ListProjectsFilter{
		TenantID: tenantID,
		Status:   &paused,
	})
	require.NoError(t, err)
	require.Len(t, got.Items, 1)
	require.Equal(t, "PAU", got.Items[0].Code)
}

func TestProjectService_List_PropagatesGenericStoreError(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	store.listErr = errors.New("query timeout")

	_, err := svc.List(context.Background(), crmapi.ListProjectsFilter{TenantID: uuid.New()})
	require.ErrorContains(t, err, "query timeout")
}

func TestNewProjectService_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()

	store := newFakeProjectStore()
	audit := &fakeAudit{}
	pool := &fakeTxRunner{}

	cases := []struct {
		name string
		fn   func()
	}{
		{"nil pool", func() { _ = NewProjectService(nil, store, audit, nil, nil) }},
		{"nil store", func() { _ = NewProjectService(pool, nil, audit, nil, nil) }},
		{"nil audit logger", func() { _ = NewProjectService(pool, store, nil, nil, nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Panics(t, tc.fn, "constructor must panic on nil dep")
		})
	}
}

func TestNewProjectService_NilClockDefaultsToTimeNow(t *testing.T) {
	t.Parallel()

	tx := &fakeTxRunner{}
	store := newFakeProjectStore()
	audit := &fakeAudit{}
	svc := NewProjectService(tx, store, audit, nil, nil)
	require.NotNil(t, svc.clock)

	// Ask the service to emit an audit row and verify the timestamp was set.
	_, err := svc.Create(context.Background(), crmapi.CreateProjectInput{
		TenantID: uuid.New(),
		Code:     "clock-default",
		Name:     "Clock Default",
	})
	require.NoError(t, err)
	events := audit.snapshot()
	require.Len(t, events, 1)
	require.False(t, events[0].Timestamp.IsZero())
}

func TestProjectService_TxRunnerSelection(t *testing.T) {
	t.Parallel()

	tx := &fakeTxRunner{}
	store := newFakeProjectStore()
	audit := &fakeAudit{}
	svc := NewProjectService(tx, store, audit, nil, nil)

	tenantID := uuid.New()
	_, err := svc.Create(context.Background(), crmapi.CreateProjectInput{
		TenantID: tenantID, Code: "TX-1", Name: "Tx Selection",
	})
	require.NoError(t, err)
	require.Len(t, tx.withTenantTenants, 1)
	require.Equal(t, tenantID, tx.withTenantTenants[0])
	require.Equal(t, 0, tx.bypassCount)

	// Get now uses WithTenant under callerTenantID's scope (Plan 13.2.5
	// Task 1 — the cross-tenant guard middleware does the BypassRLS
	// resolve at the front door via ResolveTenant; the service method
	// itself runs RLS-scoped).
	seededProj := store.seed(crmapi.Project{TenantID: tenantID, Code: "G-X", Name: "Get Tx", Status: crmapi.StatusActive})
	_, err = svc.Get(context.Background(), tenantID, seededProj.ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(tx.withTenantTenants), 2,
		"Get now uses WithTenant; expect at least Create+Get tenant scopes")

	// ResolveTenant is the only sanctioned BypassRLS path; calling it
	// from the test exercises that branch.
	_, err = svc.ResolveTenant(context.Background(), seededProj.ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, tx.bypassCount, 1,
		"ResolveTenant must use BypassRLS for the cross-tenant lookup")

	// List uses WithTenant.
	_, err = svc.List(context.Background(), crmapi.ListProjectsFilter{TenantID: tenantID})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(tx.withTenantTenants), 3)
}

// strPtr / intPtr return short-lived pointers for the patch-fields in
// UpdateProjectInput tests; saves the awkward inline `name := "x";
// in.Name = &name` boilerplate at every call site.
func strPtr(v string) *string { return &v }
func intPtr(v int) *int       { return &v }

// ─── Update ────────────────────────────────────────────────────────────────

func TestProjectService_Update_HappyPathOnlyName(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	tenantID := uuid.New()
	seeded := store.seed(crmapi.Project{
		TenantID: tenantID,
		Code:     "U-1",
		Name:     "Original",
		Status:   crmapi.StatusActive,
	})

	got, err := svc.Update(context.Background(), tenantID, seeded.ID, crmapi.UpdateProjectInput{
		Name: strPtr("Renamed"),
	})
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "Renamed", got.Name)
	require.Equal(t, "U-1", got.Code, "code is immutable")

	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "crm.project.updated", events[0].Action)
	require.Equal(t, "project:"+seeded.ID.String(), events[0].Target)
	require.Equal(t, tenantID, events[0].TenantID)
	require.Equal(t, "Renamed", events[0].Payload["name"])
	// Only Name was patched — no other keys leak into the audit payload.
	require.NotContains(t, events[0].Payload, "customer")
	require.NotContains(t, events[0].Payload, "target_count")
}

func TestProjectService_Update_NoFieldsIsNoOp(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	tenantID := uuid.New()
	seeded := store.seed(crmapi.Project{
		TenantID: tenantID,
		Code:     "U-2",
		Name:     "Untouched",
		Status:   crmapi.StatusActive,
	})

	got, err := svc.Update(context.Background(), tenantID, seeded.ID, crmapi.UpdateProjectInput{})
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "Untouched", got.Name)

	// Empty patch must not produce an audit row — there's no change.
	require.Empty(t, audit.snapshot(), "no-op Update must not emit audit")
}

func TestProjectService_Update_RejectsArchived(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	tenantID := uuid.New()
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	seeded := store.seed(crmapi.Project{
		TenantID:   tenantID,
		Code:       "U-3",
		Name:       "Archived",
		Status:     crmapi.StatusArchived,
		ArchivedAt: &now,
	})

	_, err := svc.Update(context.Background(), tenantID, seeded.ID, crmapi.UpdateProjectInput{
		Name: strPtr("Cannot"),
	})
	require.ErrorIs(t, err, crmapi.ErrProjectArchived)
	require.Empty(t, audit.snapshot())
}

func TestProjectService_Update_NotFound(t *testing.T) {
	t.Parallel()

	svc, _, audit := newSvc(t)
	_, err := svc.Update(context.Background(), uuid.New(), uuid.New(), crmapi.UpdateProjectInput{
		Name: strPtr("Ghost"),
	})
	require.ErrorIs(t, err, crmapi.ErrProjectNotFound)
	require.Empty(t, audit.snapshot())
}

func TestProjectService_Update_RejectsNilID(t *testing.T) {
	t.Parallel()

	svc, _, _ := newSvc(t)
	_, err := svc.Update(context.Background(), uuid.New(), uuid.Nil, crmapi.UpdateProjectInput{
		Name: strPtr("X"),
	})
	require.ErrorIs(t, err, crmapi.ErrInvalidArgument)
}

func TestProjectService_Update_PropagatesGenericStoreError(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	tenantID := uuid.New()
	seeded := store.seed(crmapi.Project{
		TenantID: tenantID,
		Code:     "U-ERR",
		Name:     "Err",
		Status:   crmapi.StatusActive,
	})
	store.updateErr = errors.New("disk full")

	_, err := svc.Update(context.Background(), tenantID, seeded.ID, crmapi.UpdateProjectInput{
		Name: strPtr("X"),
	})
	require.ErrorContains(t, err, "disk full")
}

func TestProjectService_Update_MultiFieldPatchAuditsAllKeys(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	tenantID := uuid.New()
	seeded := store.seed(crmapi.Project{
		TenantID:    tenantID,
		Code:        "U-MULTI",
		Name:        "Original",
		Status:      crmapi.StatusActive,
		TargetCount: 100,
	})

	got, err := svc.Update(context.Background(), tenantID, seeded.ID, crmapi.UpdateProjectInput{
		Name:        strPtr("Renamed"),
		Customer:    strPtr("New Customer"),
		TargetCount: intPtr(2000),
	})
	require.NoError(t, err)
	require.Equal(t, "Renamed", got.Name)
	require.Equal(t, "New Customer", got.Customer)
	require.Equal(t, 2000, got.TargetCount)

	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "Renamed", events[0].Payload["name"])
	require.Equal(t, "New Customer", events[0].Payload["customer"])
	require.Equal(t, 2000, events[0].Payload["target_count"])
}

// ─── Pause ─────────────────────────────────────────────────────────────────

func TestProjectService_Pause_ActiveTransitions(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	tenantID := uuid.New()
	seeded := store.seed(crmapi.Project{
		TenantID: tenantID,
		Code:     "P-A",
		Name:     "Pause Me",
		Status:   crmapi.StatusActive,
	})

	require.NoError(t, svc.Pause(context.Background(), tenantID, seeded.ID))

	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "crm.project.paused", events[0].Action)
	require.Equal(t, "project:"+seeded.ID.String(), events[0].Target)
	require.Equal(t, "active", events[0].Payload["from"])
	require.Equal(t, "paused", events[0].Payload["to"])

	// Underlying row is now Paused.
	got := store.projects[seeded.ID]
	require.Equal(t, crmapi.StatusPaused, got.Status)
}

func TestProjectService_Pause_AlreadyPausedIsNoOp(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "P-P",
		Name:     "Already Paused",
		Status:   crmapi.StatusPaused,
	})

	require.NoError(t, svc.Pause(context.Background(), seeded.TenantID, seeded.ID))
	require.Empty(t, audit.snapshot(), "idempotent Pause must not emit audit")
}

func TestProjectService_Pause_OnArchivedRejects(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	seeded := store.seed(crmapi.Project{
		TenantID:   uuid.New(),
		Code:       "P-AR",
		Name:       "Archived",
		Status:     crmapi.StatusArchived,
		ArchivedAt: &now,
	})

	err := svc.Pause(context.Background(), seeded.TenantID, seeded.ID)
	require.ErrorIs(t, err, crmapi.ErrProjectArchived)
	require.Empty(t, audit.snapshot())
}

// ─── Resume ────────────────────────────────────────────────────────────────

func TestProjectService_Resume_PausedTransitions(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "R-P",
		Name:     "Resume Me",
		Status:   crmapi.StatusPaused,
	})

	require.NoError(t, svc.Resume(context.Background(), seeded.TenantID, seeded.ID))

	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "crm.project.resumed", events[0].Action)
	require.Equal(t, "paused", events[0].Payload["from"])
	require.Equal(t, "active", events[0].Payload["to"])

	got := store.projects[seeded.ID]
	require.Equal(t, crmapi.StatusActive, got.Status)
}

func TestProjectService_Resume_AlreadyActiveIsNoOp(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "R-A",
		Name:     "Already Active",
		Status:   crmapi.StatusActive,
	})

	require.NoError(t, svc.Resume(context.Background(), seeded.TenantID, seeded.ID))
	require.Empty(t, audit.snapshot())
}

func TestProjectService_Resume_OnArchivedRejects(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	seeded := store.seed(crmapi.Project{
		TenantID:   uuid.New(),
		Code:       "R-AR",
		Name:       "Archived",
		Status:     crmapi.StatusArchived,
		ArchivedAt: &now,
	})
	err := svc.Resume(context.Background(), seeded.TenantID, seeded.ID)
	require.ErrorIs(t, err, crmapi.ErrProjectArchived)
}

// ─── Archive ───────────────────────────────────────────────────────────────

func TestProjectService_Archive_ActiveTransitions(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "A-A",
		Name:     "Archive From Active",
		Status:   crmapi.StatusActive,
	})

	require.NoError(t, svc.Archive(context.Background(), seeded.TenantID, seeded.ID))

	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "crm.project.archived", events[0].Action)
	require.Equal(t, "active", events[0].Payload["from"])
	require.Equal(t, "archived", events[0].Payload["to"])

	got := store.projects[seeded.ID]
	require.Equal(t, crmapi.StatusArchived, got.Status)
	require.NotNil(t, got.ArchivedAt, "Archive must stamp archived_at")
	// archived_at uses the service clock (frozen at 2026-05-08).
	require.Equal(t, time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC), got.ArchivedAt.UTC())
}

func TestProjectService_Archive_PausedTransitions(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "A-P",
		Name:     "Archive From Paused",
		Status:   crmapi.StatusPaused,
	})

	require.NoError(t, svc.Archive(context.Background(), seeded.TenantID, seeded.ID))

	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "paused", events[0].Payload["from"])
	require.Equal(t, "archived", events[0].Payload["to"])
}

func TestProjectService_Archive_AlreadyArchivedIsNoOp(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	seeded := store.seed(crmapi.Project{
		TenantID:   uuid.New(),
		Code:       "A-AR",
		Name:       "Already Archived",
		Status:     crmapi.StatusArchived,
		ArchivedAt: &now,
	})

	require.NoError(t, svc.Archive(context.Background(), seeded.TenantID, seeded.ID), "idempotent on archived")
	require.Empty(t, audit.snapshot(), "no-op Archive must not emit audit")
}

// ─── GetProgress ───────────────────────────────────────────────────────────

func TestProjectService_GetProgress_HappyPath(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID:    uuid.New(),
		Code:        "GP-1",
		Name:        "Progress",
		Status:      crmapi.StatusActive,
		TargetCount: 1000,
	})
	store.progress[seeded.ID] = crmapi.ProjectProgress{
		TargetCount:     1000,
		CompletedCount:  250,
		InProgressCount: 5,
		PendingCount:    745,
	}

	got, err := svc.GetProgress(context.Background(), seeded.TenantID, seeded.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, seeded.ID, got.ProjectID)
	require.Equal(t, 1000, got.TargetCount)
	require.Equal(t, 250, got.CompletedCount)
	require.Equal(t, 5, got.InProgressCount)
	require.Equal(t, 745, got.PendingCount)
	require.InDelta(t, 25.0, got.PercentDone, 0.01)

	// Reads do not emit audit rows.
	require.Empty(t, audit.snapshot())
}

func TestProjectService_GetProgress_NotFound(t *testing.T) {
	t.Parallel()

	svc, _, _ := newSvc(t)
	_, err := svc.GetProgress(context.Background(), uuid.New(), uuid.New())
	require.ErrorIs(t, err, crmapi.ErrProjectNotFound)
}

func TestProjectService_GetProgress_ZeroTargetGivesZeroPercent(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID:    uuid.New(),
		Code:        "GP-0",
		Name:        "No Target",
		Status:      crmapi.StatusActive,
		TargetCount: 0,
	})
	store.progress[seeded.ID] = crmapi.ProjectProgress{
		TargetCount:    0,
		CompletedCount: 0,
	}

	got, err := svc.GetProgress(context.Background(), seeded.TenantID, seeded.ID)
	require.NoError(t, err)
	require.Zero(t, got.PercentDone, "no target -> 0%, not divide-by-zero")
}

// ─── Assign ────────────────────────────────────────────────────────────────

func TestProjectService_Assign_HappyPathThreeOperators(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "AS-1",
		Name:     "Assign 3",
		Status:   crmapi.StatusActive,
	})
	op1, op2, op3 := uuid.New(), uuid.New(), uuid.New()

	require.NoError(t, svc.Assign(context.Background(), seeded.TenantID, seeded.ID,
		[]uuid.UUID{op1, op2, op3}))

	// One audit row PER added operator.
	events := audit.snapshot()
	require.Len(t, events, 3)
	for _, ev := range events {
		require.Equal(t, "crm.project.member_assigned", ev.Action)
		require.Equal(t, seeded.TenantID, ev.TenantID)
	}
	require.Len(t, store.members[seeded.ID], 3)
}

func TestProjectService_Assign_MergeSemanticsPartialOverlap(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "AS-2",
		Name:     "Assign Merge",
		Status:   crmapi.StatusActive,
	})
	existing := uuid.New()
	store.members[seeded.ID] = []crmapi.ProjectMember{{
		OperatorID: existing,
		AssignedAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
	}}
	new1, new2 := uuid.New(), uuid.New()

	require.NoError(t, svc.Assign(context.Background(), seeded.TenantID, seeded.ID,
		[]uuid.UUID{existing, new1, new2}))

	events := audit.snapshot()
	require.Len(t, events, 2, "audit only the two newly-added operators")
	gotIDs := []string{
		events[0].Target,
		events[1].Target,
	}
	require.NotContains(t, gotIDs, "user:"+existing.String(),
		"already-assigned operator must NOT be audited again")
}

func TestProjectService_Assign_DeduplicatesInput(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "AS-3",
		Name:     "Assign Dup Input",
		Status:   crmapi.StatusActive,
	})
	op := uuid.New()

	require.NoError(t, svc.Assign(context.Background(), seeded.TenantID, seeded.ID,
		[]uuid.UUID{op, op, op}))
	require.Len(t, audit.snapshot(), 1, "duplicate input dedups to one add + one audit")
	require.Len(t, store.members[seeded.ID], 1)
}

func TestProjectService_Assign_EmptyOperatorIDsRejected(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "AS-E",
		Name:     "Assign Empty",
		Status:   crmapi.StatusActive,
	})

	err := svc.Assign(context.Background(), seeded.TenantID, seeded.ID, []uuid.UUID{})
	require.ErrorIs(t, err, crmapi.ErrInvalidArgument)
	require.Empty(t, audit.snapshot())
}

func TestProjectService_Assign_AllNilIDsRejected(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "AS-N",
		Name:     "Assign Nil",
		Status:   crmapi.StatusActive,
	})

	err := svc.Assign(context.Background(), seeded.TenantID, seeded.ID, []uuid.UUID{uuid.Nil, uuid.Nil})
	require.ErrorIs(t, err, crmapi.ErrInvalidArgument)
}

func TestProjectService_Assign_OnArchivedRejects(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	seeded := store.seed(crmapi.Project{
		TenantID:   uuid.New(),
		Code:       "AS-AR",
		Name:       "Archived",
		Status:     crmapi.StatusArchived,
		ArchivedAt: &now,
	})
	err := svc.Assign(context.Background(), seeded.TenantID, seeded.ID, []uuid.UUID{uuid.New()})
	require.ErrorIs(t, err, crmapi.ErrProjectArchived)
}

// ─── Unassign ──────────────────────────────────────────────────────────────

func TestProjectService_Unassign_HappyPath(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "UN-1",
		Name:     "Unassign",
		Status:   crmapi.StatusActive,
	})
	op := uuid.New()
	store.members[seeded.ID] = []crmapi.ProjectMember{{
		OperatorID: op,
		AssignedAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
	}}

	require.NoError(t, svc.Unassign(context.Background(), seeded.TenantID, seeded.ID, op))

	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "crm.project.member_unassigned", events[0].Action)
	require.Equal(t, "user:"+op.String(), events[0].Target)
	require.Empty(t, store.members[seeded.ID])
}

func TestProjectService_Unassign_NonMemberIsSilentNoOp(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "UN-2",
		Name:     "Unassign Stranger",
		Status:   crmapi.StatusActive,
	})

	require.NoError(t, svc.Unassign(context.Background(), seeded.TenantID, seeded.ID, uuid.New()),
		"unassign of non-member must not error")
	require.Empty(t, audit.snapshot(), "no audit for no-op unassign")
}

func TestProjectService_Unassign_NilIDsRejected(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "UN-3",
		Name:     "x",
		Status:   crmapi.StatusActive,
	})

	require.ErrorIs(t,
		svc.Unassign(context.Background(), seeded.TenantID, uuid.Nil, uuid.New()),
		crmapi.ErrInvalidArgument)
	require.ErrorIs(t,
		svc.Unassign(context.Background(), seeded.TenantID, seeded.ID, uuid.Nil),
		crmapi.ErrInvalidArgument)
}

// ─── ListMembers ───────────────────────────────────────────────────────────

func TestProjectService_ListMembers_HappyPath(t *testing.T) {
	t.Parallel()

	svc, store, audit := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "LM-1",
		Name:     "List Members",
		Status:   crmapi.StatusActive,
	})
	op1, op2 := uuid.New(), uuid.New()
	store.members[seeded.ID] = []crmapi.ProjectMember{
		{OperatorID: op1, AssignedAt: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC), Login: "alice", FullName: "Alice"},
		{OperatorID: op2, AssignedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC), Login: "bob", FullName: "Bob"},
	}

	got, err := svc.ListMembers(context.Background(), seeded.TenantID, seeded.ID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, op1, got[0].OperatorID)
	require.Equal(t, "alice", got[0].Login)
	require.Equal(t, "Alice", got[0].FullName)
	require.Equal(t, op2, got[1].OperatorID)

	// Reads do not emit audit rows.
	require.Empty(t, audit.snapshot())
}

func TestProjectService_ListMembers_NotFound(t *testing.T) {
	t.Parallel()

	svc, _, _ := newSvc(t)
	_, err := svc.ListMembers(context.Background(), uuid.New(), uuid.New())
	require.ErrorIs(t, err, crmapi.ErrProjectNotFound)
}

func TestProjectService_ListMembers_EmptyProjectReturnsEmpty(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "LM-E",
		Name:     "Empty",
		Status:   crmapi.StatusActive,
	})

	got, err := svc.ListMembers(context.Background(), seeded.TenantID, seeded.ID)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestProjectService_ListMembers_RejectsNilID(t *testing.T) {
	t.Parallel()

	svc, _, _ := newSvc(t)
	_, err := svc.ListMembers(context.Background(), uuid.New(), uuid.Nil)
	require.ErrorIs(t, err, crmapi.ErrInvalidArgument)
}

func TestProjectService_ListMembers_PropagatesGenericStoreError(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "LM-ERR",
		Name:     "Err",
		Status:   crmapi.StatusActive,
	})
	store.listMembersErr = errors.New("query timeout")

	_, err := svc.ListMembers(context.Background(), seeded.TenantID, seeded.ID)
	require.ErrorContains(t, err, "query timeout")
}

func TestProjectService_GetProgress_PropagatesGenericStoreError(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID:    uuid.New(),
		Code:        "GP-ERR",
		Name:        "Err",
		Status:      crmapi.StatusActive,
		TargetCount: 10,
	})
	store.progressErr = errors.New("aggregation failed")

	_, err := svc.GetProgress(context.Background(), seeded.TenantID, seeded.ID)
	require.ErrorContains(t, err, "aggregation failed")
}

func TestProjectService_GetProgress_RejectsNilID(t *testing.T) {
	t.Parallel()

	svc, _, _ := newSvc(t)
	_, err := svc.GetProgress(context.Background(), uuid.New(), uuid.Nil)
	require.ErrorIs(t, err, crmapi.ErrInvalidArgument)
}

func TestProjectService_Pause_PropagatesGenericStoreError(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "PAUSE-ERR",
		Name:     "Err",
		Status:   crmapi.StatusActive,
	})
	store.statusErr = errors.New("constraint violation")

	err := svc.Pause(context.Background(), seeded.TenantID, seeded.ID)
	require.ErrorContains(t, err, "constraint violation")
}

func TestProjectService_Pause_RejectsNilID(t *testing.T) {
	t.Parallel()

	svc, _, _ := newSvc(t)
	require.ErrorIs(t,
		svc.Pause(context.Background(), uuid.New(), uuid.Nil),
		crmapi.ErrInvalidArgument)
}

func TestProjectService_Assign_PropagatesGenericStoreError(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "AS-ERR",
		Name:     "Err",
		Status:   crmapi.StatusActive,
	})
	store.assignErr = errors.New("deadlock detected")

	err := svc.Assign(context.Background(), seeded.TenantID, seeded.ID, []uuid.UUID{uuid.New()})
	require.ErrorContains(t, err, "deadlock detected")
}

func TestProjectService_Unassign_PropagatesGenericStoreError(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "UN-ERR",
		Name:     "Err",
		Status:   crmapi.StatusActive,
	})
	store.unassignErr = errors.New("connection reset")

	err := svc.Unassign(context.Background(), seeded.TenantID, seeded.ID, uuid.New())
	require.ErrorContains(t, err, "connection reset")
}

func TestProjectService_Update_PropagatesPropagatedNotFound(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	seeded := store.seed(crmapi.Project{
		TenantID: uuid.New(),
		Code:     "U-NOTFOUND",
		Name:     "Will Be Lost",
		Status:   crmapi.StatusActive,
	})
	store.updateErr = crmapi.ErrProjectNotFound

	_, err := svc.Update(context.Background(), seeded.TenantID, seeded.ID, crmapi.UpdateProjectInput{
		Name: strPtr("X"),
	})
	require.ErrorIs(t, err, crmapi.ErrProjectNotFound)
}
