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

	// Programmable error injection: when not nil for a method, the next
	// call returns it (and clears the slot).
	insertErr  error
	getByIDErr error
	listErr    error
}

func newFakeProjectStore() *fakeProjectStore {
	return &fakeProjectStore{
		projects:  make(map[uuid.UUID]crmapi.Project),
		codeIndex: make(map[string]uuid.UUID),
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
	svc := NewProjectService(tx, store, audit, clock)
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

	got, err := svc.Get(context.Background(), seeded.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, seeded.ID, got.ID)
	require.Equal(t, "G-1", got.Code)
	require.Equal(t, "Get Happy", got.Name)
}

func TestProjectService_Get_NotFound(t *testing.T) {
	t.Parallel()

	svc, _, _ := newSvc(t)
	_, err := svc.Get(context.Background(), uuid.New())
	require.ErrorIs(t, err, crmapi.ErrProjectNotFound)
}

func TestProjectService_Get_RejectsNilID(t *testing.T) {
	t.Parallel()

	svc, _, _ := newSvc(t)
	_, err := svc.Get(context.Background(), uuid.Nil)
	require.Error(t, err)
}

func TestProjectService_Get_PropagatesGenericStoreError(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSvc(t)
	store.getByIDErr = errors.New("connection refused")

	_, err := svc.Get(context.Background(), uuid.New())
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
		{"nil pool", func() { _ = NewProjectService(nil, store, audit, nil) }},
		{"nil store", func() { _ = NewProjectService(pool, nil, audit, nil) }},
		{"nil audit logger", func() { _ = NewProjectService(pool, store, nil, nil) }},
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
	svc := NewProjectService(tx, store, audit, nil)
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

func TestProjectService_DeferredMethodsReturnNotImplemented(t *testing.T) {
	t.Parallel()

	svc, _, _ := newSvc(t)
	id := uuid.New()
	ctx := context.Background()

	_, err := svc.Update(ctx, id, crmapi.UpdateProjectInput{})
	require.Error(t, err)

	require.Error(t, svc.Pause(ctx, id))
	require.Error(t, svc.Resume(ctx, id))
	require.Error(t, svc.Archive(ctx, id))

	_, err = svc.GetProgress(ctx, id)
	require.Error(t, err)

	require.Error(t, svc.Assign(ctx, id, []uuid.UUID{uuid.New()}))
	require.Error(t, svc.Unassign(ctx, id, uuid.New()))

	_, err = svc.ListMembers(ctx, id)
	require.Error(t, err)
}

func TestProjectService_TxRunnerSelection(t *testing.T) {
	t.Parallel()

	tx := &fakeTxRunner{}
	store := newFakeProjectStore()
	audit := &fakeAudit{}
	svc := NewProjectService(tx, store, audit, nil)

	tenantID := uuid.New()
	_, err := svc.Create(context.Background(), crmapi.CreateProjectInput{
		TenantID: tenantID, Code: "TX-1", Name: "Tx Selection",
	})
	require.NoError(t, err)
	require.Len(t, tx.withTenantTenants, 1)
	require.Equal(t, tenantID, tx.withTenantTenants[0])
	require.Equal(t, 0, tx.bypassCount)

	// Get uses BypassRLS — admin path.
	store.seed(crmapi.Project{TenantID: uuid.New(), Code: "G-X", Name: "Get Tx", Status: crmapi.StatusActive})
	id := uuid.Nil
	for k := range store.projects {
		id = k
		break
	}
	_, err = svc.Get(context.Background(), id)
	require.NoError(t, err)
	require.GreaterOrEqual(t, tx.bypassCount, 1)

	// List uses WithTenant.
	_, err = svc.List(context.Background(), crmapi.ListProjectsFilter{TenantID: tenantID})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(tx.withTenantTenants), 2)
}
