package analytics

// Internal (same-package) test that exercises lookupCrmProjectService
// directly, bypassing Register's DSN gate. The external
// TestModule_LocatorCrmFallbacks in module_test.go can only test paths
// downstream of the DSN gate; this file pins the locator-lookup
// resolution semantics in isolation.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/internal/modules"
)

// fakeLocator is a tiny in-process ServiceLocator for the lookup tests.
// Used in place of modules.NewMapLocator because the lookup tests want
// fine-grained control over the value returned for the
// "crm.ProjectService" key (nil locator, missing entry, wrong type).
type fakeLocator struct {
	entries map[string]any
}

func (l *fakeLocator) Register(name string, svc any) {
	if l.entries == nil {
		l.entries = map[string]any{}
	}
	l.entries[name] = svc
}

func (l *fakeLocator) Lookup(name string) (any, bool) {
	if l == nil {
		return nil, false
	}
	v, ok := l.entries[name]
	return v, ok
}

// fakeProjectService satisfies crmapi.ProjectService just enough for
// the type assertion inside lookupCrmProjectService to succeed. Every
// method panics — the helper is allowed to be invoked but the lookup
// test never calls through it.
type fakeProjectService struct{}

func (fakeProjectService) Create(context.Context, crmapi.CreateProjectInput) (*crmapi.Project, error) {
	panic("not invoked")
}
func (fakeProjectService) Get(context.Context, uuid.UUID) (*crmapi.Project, error) {
	panic("not invoked")
}
func (fakeProjectService) List(context.Context, crmapi.ListProjectsFilter) (*crmapi.ListProjectsResult, error) {
	panic("not invoked")
}
func (fakeProjectService) Update(context.Context, uuid.UUID, crmapi.UpdateProjectInput) (*crmapi.Project, error) {
	panic("not invoked")
}
func (fakeProjectService) Pause(context.Context, uuid.UUID) error  { panic("not invoked") }
func (fakeProjectService) Resume(context.Context, uuid.UUID) error { panic("not invoked") }
func (fakeProjectService) Archive(context.Context, uuid.UUID) error {
	panic("not invoked")
}
func (fakeProjectService) GetProgress(context.Context, uuid.UUID) (*crmapi.ProjectProgress, error) {
	panic("not invoked")
}
func (fakeProjectService) Assign(context.Context, uuid.UUID, []uuid.UUID) error {
	panic("not invoked")
}
func (fakeProjectService) Unassign(context.Context, uuid.UUID, uuid.UUID) error {
	panic("not invoked")
}
func (fakeProjectService) ListMembers(context.Context, uuid.UUID) ([]crmapi.ProjectMember, error) {
	panic("not invoked")
}

// Compile-time check: fakeProjectService satisfies crmapi.ProjectService.
// If this fails, the upstream interface has drifted — update the fake.
var _ crmapi.ProjectService = fakeProjectService{}

// TestLookupCrmProjectService_NilLocator returns nil + WARN per Q12.
func TestLookupCrmProjectService_NilLocator(t *testing.T) {
	t.Parallel()

	got := lookupCrmProjectService(nil, zap.NewNop())
	require.Nil(t, got, "nil locator must yield nil CrmReader (Q12)")
}

// TestLookupCrmProjectService_MissingEntry returns nil per Q12 when
// the locator has no crm.ProjectService registration.
func TestLookupCrmProjectService_MissingEntry(t *testing.T) {
	t.Parallel()

	loc := &fakeLocator{}
	got := lookupCrmProjectService(loc, zap.NewNop())
	require.Nil(t, got, "missing locator entry must yield nil CrmReader (Q12)")
}

// TestLookupCrmProjectService_WrongType returns nil per Q12 when the
// locator value is registered with the right name but the wrong type.
func TestLookupCrmProjectService_WrongType(t *testing.T) {
	t.Parallel()

	loc := &fakeLocator{}
	loc.Register("crm.ProjectService", "not-a-project-service")
	got := lookupCrmProjectService(loc, zap.NewNop())
	require.Nil(t, got, "wrong-type locator value must yield nil CrmReader (Q12)")
}

// TestLookupCrmProjectService_HappyPath returns the registered service
// when the locator carries a value satisfying crmapi.ProjectService.
func TestLookupCrmProjectService_HappyPath(t *testing.T) {
	t.Parallel()

	loc := &fakeLocator{}
	loc.Register("crm.ProjectService", fakeProjectService{})
	got := lookupCrmProjectService(loc, zap.NewNop())
	require.NotNil(t, got, "happy path must return non-nil CrmReader")
}

// Internal sanity: fakeLocator satisfies the modules.ServiceLocator
// contract. Catches drift in the interface at compile time.
var _ modules.ServiceLocator = (*fakeLocator)(nil)
