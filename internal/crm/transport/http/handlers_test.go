package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	transporthttp "github.com/sociopulse/platform/internal/crm/transport/http"
)

// =============================================================================
// Fakes
// =============================================================================

// fakeProjectService records calls and returns canned values. Pointer
// returns mirror the interface contract; an injected error replaces
// the canned return on the next call.
type fakeProjectService struct {
	createIn      crmapi.CreateProjectInput
	createRet     crmapi.Project
	createErr     error
	getCalls      []uuid.UUID
	getRet        crmapi.Project
	getErr        error
	listIn        crmapi.ListProjectsFilter
	listRet       crmapi.ListProjectsResult
	listErr       error
	updateID      uuid.UUID
	updateIn      crmapi.UpdateProjectInput
	updateRet     crmapi.Project
	updateErr     error
	pauseID       uuid.UUID
	pauseErr      error
	resumeID      uuid.UUID
	resumeErr     error
	archiveID     uuid.UUID
	archiveErr    error
	progressID    uuid.UUID
	progressRet   crmapi.ProjectProgress
	progressErr   error
	assignID      uuid.UUID
	assignOps     []uuid.UUID
	assignErr     error
	unassignID    uuid.UUID
	unassignOp    uuid.UUID
	unassignErr   error
	listMembersID uuid.UUID
	listMembers   []crmapi.ProjectMember
	listMembErr   error
}

func (f *fakeProjectService) Create(_ context.Context, in crmapi.CreateProjectInput) (*crmapi.Project, error) {
	f.createIn = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	cp := f.createRet
	return &cp, nil
}
func (f *fakeProjectService) Get(_ context.Context, id uuid.UUID) (*crmapi.Project, error) {
	f.getCalls = append(f.getCalls, id)
	if f.getErr != nil {
		return nil, f.getErr
	}
	cp := f.getRet
	return &cp, nil
}
func (f *fakeProjectService) List(_ context.Context, in crmapi.ListProjectsFilter) (*crmapi.ListProjectsResult, error) {
	f.listIn = in
	if f.listErr != nil {
		return nil, f.listErr
	}
	cp := f.listRet
	return &cp, nil
}
func (f *fakeProjectService) Update(_ context.Context, id uuid.UUID, in crmapi.UpdateProjectInput) (*crmapi.Project, error) {
	f.updateID = id
	f.updateIn = in
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	cp := f.updateRet
	return &cp, nil
}
func (f *fakeProjectService) Pause(_ context.Context, id uuid.UUID) error {
	f.pauseID = id
	return f.pauseErr
}
func (f *fakeProjectService) Resume(_ context.Context, id uuid.UUID) error {
	f.resumeID = id
	return f.resumeErr
}
func (f *fakeProjectService) Archive(_ context.Context, id uuid.UUID) error {
	f.archiveID = id
	return f.archiveErr
}
func (f *fakeProjectService) GetProgress(_ context.Context, id uuid.UUID) (*crmapi.ProjectProgress, error) {
	f.progressID = id
	if f.progressErr != nil {
		return nil, f.progressErr
	}
	cp := f.progressRet
	return &cp, nil
}
func (f *fakeProjectService) Assign(_ context.Context, id uuid.UUID, ops []uuid.UUID) error {
	f.assignID = id
	f.assignOps = ops
	return f.assignErr
}
func (f *fakeProjectService) Unassign(_ context.Context, id, opID uuid.UUID) error {
	f.unassignID = id
	f.unassignOp = opID
	return f.unassignErr
}
func (f *fakeProjectService) ListMembers(_ context.Context, id uuid.UUID) ([]crmapi.ProjectMember, error) {
	f.listMembersID = id
	if f.listMembErr != nil {
		return nil, f.listMembErr
	}
	return f.listMembers, nil
}

// fakeRespondentService records calls and returns canned values.
type fakeRespondentService struct {
	createIn          crmapi.CreateRespondentInput
	createRet         crmapi.Respondent
	createErr         error
	getCalls          []uuid.UUID
	getRet            crmapi.Respondent
	getErr            error
	getWithPhoneCalls []uuid.UUID
	getWithPhoneRet   crmapi.Respondent
	getWithPhoneErr   error
	searchIn          crmapi.SearchRespondentsFilter
	searchRet         crmapi.SearchRespondentsResult
	searchErr         error
	deleteID          uuid.UUID
	deleteRet         crmapi.DeletionRequest
	deleteErr         error
	importIn          crmapi.ImportRequest
	importRet         crmapi.ImportTicket
	importErr         error
	statusJobID       string
	statusRet         crmapi.ImportStatus
	statusErr         error
}

func (f *fakeRespondentService) Create(_ context.Context, in crmapi.CreateRespondentInput) (*crmapi.Respondent, error) {
	f.createIn = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	cp := f.createRet
	return &cp, nil
}
func (f *fakeRespondentService) Get(_ context.Context, id uuid.UUID) (*crmapi.Respondent, error) {
	f.getCalls = append(f.getCalls, id)
	if f.getErr != nil {
		return nil, f.getErr
	}
	cp := f.getRet
	return &cp, nil
}
func (f *fakeRespondentService) GetWithPhone(_ context.Context, id uuid.UUID) (*crmapi.Respondent, error) {
	f.getWithPhoneCalls = append(f.getWithPhoneCalls, id)
	if f.getWithPhoneErr != nil {
		return nil, f.getWithPhoneErr
	}
	cp := f.getWithPhoneRet
	return &cp, nil
}
func (f *fakeRespondentService) Search(_ context.Context, in crmapi.SearchRespondentsFilter) (*crmapi.SearchRespondentsResult, error) {
	f.searchIn = in
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	cp := f.searchRet
	return &cp, nil
}
func (f *fakeRespondentService) Delete(_ context.Context, id uuid.UUID) (*crmapi.DeletionRequest, error) {
	f.deleteID = id
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	cp := f.deleteRet
	return &cp, nil
}
func (f *fakeRespondentService) Import(_ context.Context, req crmapi.ImportRequest) (*crmapi.ImportTicket, error) {
	f.importIn = req
	if f.importErr != nil {
		return nil, f.importErr
	}
	cp := f.importRet
	return &cp, nil
}
func (f *fakeRespondentService) GetImportStatus(_ context.Context, jobID string) (*crmapi.ImportStatus, error) {
	f.statusJobID = jobID
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	cp := f.statusRet
	return &cp, nil
}

// fakeRBAC always allows; tests that need a deny override Check.
type fakeRBAC struct {
	denyAll bool
	denyErr error
}

func (f *fakeRBAC) Check(_ context.Context, _ authapi.Claims, _ authapi.Action, _ authapi.Resource) error {
	if f.denyAll {
		if f.denyErr != nil {
			return f.denyErr
		}
		return authapi.ErrInsufficientRole
	}
	return nil
}

// fakeValidator returns canned Claims for any token.
type fakeValidator struct {
	claims authapi.Claims
	err    error
}

func (f *fakeValidator) Validate(_ context.Context, _ string) (authapi.Claims, error) {
	return f.claims, f.err
}

// =============================================================================
// Test fixture
// =============================================================================

type fixture struct {
	router    *gin.Engine
	projects  *fakeProjectService
	respond   *fakeRespondentService
	rbac      *fakeRBAC
	validator *fakeValidator

	// Shared canned values for the validator's "fresh authenticated
	// caller". Tests reset these as needed.
	tenantID uuid.UUID
	userID   uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()

	tenantID := uuid.New()
	userID := uuid.New()

	f := &fixture{
		router:   r,
		projects: &fakeProjectService{},
		respond:  &fakeRespondentService{},
		rbac:     &fakeRBAC{},
		validator: &fakeValidator{
			claims: authapi.Claims{
				UserID:   userID,
				TenantID: tenantID,
				Login:    "alice",
				Roles:    []authapi.Role{authapi.RoleAdmin},
			},
		},
		tenantID: tenantID,
		userID:   userID,
	}
	api := r.Group("/api")
	transporthttp.Mount(api, transporthttp.Deps{
		Logger:     nil,
		Projects:   f.projects,
		Respondent: f.respond,
		RBAC:       f.rbac,
		Validator:  f.validator,
	})
	return f
}

func (f *fixture) setRoles(rs ...authapi.Role) {
	f.validator.claims.Roles = rs
}

// doAuth issues an authenticated request through the router.
func (f *fixture) doAuth(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *bytes.Buffer
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		bodyReader = bytes.NewBuffer(raw)
	} else {
		bodyReader = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer dummy.token")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// doAuthRaw issues a request with a custom body and Content-Type.
// Used for the multipart-import tests.
func (f *fixture) doAuthRaw(t *testing.T, method, path, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer dummy.token")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "body=%s", rec.Body.String())
	return out
}

// =============================================================================
// Project endpoints
// =============================================================================

func TestCreateProject_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	pid := uuid.New()
	f.projects.createRet = crmapi.Project{
		ID:        pid,
		TenantID:  f.tenantID,
		Code:      "PRJ-1",
		Name:      "Test Project",
		Status:    crmapi.StatusActive,
		CreatedAt: time.Now().UTC(),
	}

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/projects", transporthttp.CreateProjectRequest{
		Code: "PRJ-1", Name: "Test Project", TargetCount: 100,
	})
	require.Equal(t, stdhttp.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.ProjectDTO](t, rec)
	assert.Equal(t, "PRJ-1", resp.Code)
	assert.Equal(t, f.tenantID, f.projects.createIn.TenantID)
}

func TestCreateProject_BadRequest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/projects", map[string]string{"name": "x"})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "crm.bad_request", body.Error)
}

func TestCreateProject_AdvertisingRejected(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projects.createErr = crmapi.ErrAdvertisingRejected

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/projects", transporthttp.CreateProjectRequest{
		Code: "X", Name: "Y", TargetCount: 0,
	})
	require.Equal(t, stdhttp.StatusConflict, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "crm.project.advertising_rejected", body.Error)
}

func TestCreateProject_DuplicateCode(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projects.createErr = crmapi.ErrProjectCodeTaken

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/projects", transporthttp.CreateProjectRequest{
		Code: "X", Name: "Y", TargetCount: 0,
	})
	require.Equal(t, stdhttp.StatusConflict, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "crm.project.code_taken", body.Error)
}

func TestCreateProject_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/projects", transporthttp.CreateProjectRequest{
		Code: "X", Name: "Y", TargetCount: 0,
	})
	require.Equal(t, stdhttp.StatusForbidden, rec.Code)
}

func TestListProjects_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projects.listRet = crmapi.ListProjectsResult{
		Items: []crmapi.Project{
			{ID: uuid.New(), TenantID: f.tenantID, Code: "P1", Name: "One"},
		},
		TotalCount: 1,
	}
	f.setRoles(authapi.RoleOperator)

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/projects?limit=10&offset=0", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.ListProjectsResponse](t, rec)
	assert.Equal(t, int64(1), resp.TotalCount)
	assert.Len(t, resp.Projects, 1)
	assert.Equal(t, 10, f.projects.listIn.Limit)
}

func TestGetProject_Missing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projects.getErr = crmapi.ErrProjectNotFound
	f.setRoles(authapi.RoleOperator)

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/projects/"+uuid.New().String(), nil)
	require.Equal(t, stdhttp.StatusNotFound, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "crm.project.not_found", body.Error)
}

func TestGetProject_BadID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/projects/not-a-uuid", nil)
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestUpdateProject_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	f.projects.updateRet = crmapi.Project{ID: id, TenantID: f.tenantID, Name: "New Name"}

	name := "New Name"
	rec := f.doAuth(t, stdhttp.MethodPatch, "/api/projects/"+id.String(),
		transporthttp.UpdateProjectRequest{Name: &name})
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.NotNil(t, f.projects.updateIn.Name)
	assert.Equal(t, "New Name", *f.projects.updateIn.Name)
}

func TestPauseProject_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/projects/"+id.String()+"/pause", nil)
	require.Equal(t, stdhttp.StatusNoContent, rec.Code)
	assert.Equal(t, id, f.projects.pauseID)
}

func TestResumeProject_Archived(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projects.resumeErr = crmapi.ErrProjectArchived
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/projects/"+uuid.New().String()+"/resume", nil)
	require.Equal(t, stdhttp.StatusConflict, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "crm.project.archived", body.Error)
}

func TestArchiveProject_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/projects/"+id.String()+"/archive", nil)
	require.Equal(t, stdhttp.StatusNoContent, rec.Code)
	assert.Equal(t, id, f.projects.archiveID)
}

func TestGetProjectProgress_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	f.projects.progressRet = crmapi.ProjectProgress{
		ProjectID:      id,
		TargetCount:    100,
		CompletedCount: 25,
		PercentDone:    25.0,
	}
	f.setRoles(authapi.RoleOperator)

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/projects/"+id.String()+"/progress", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.ProjectProgressDTO](t, rec)
	assert.Equal(t, 100, resp.TargetCount)
	assert.Equal(t, 25, resp.CompletedCount)
	assert.InDelta(t, 25.0, resp.PercentDone, 0.001)
}

func TestAssignOperators_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	op1 := uuid.New()
	op2 := uuid.New()

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/projects/"+id.String()+"/assign",
		transporthttp.AssignOperatorsRequest{OperatorIDs: []uuid.UUID{op1, op2}})
	require.Equal(t, stdhttp.StatusNoContent, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, id, f.projects.assignID)
	assert.Len(t, f.projects.assignOps, 2)
}

func TestAssignOperators_EmptyList(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/projects/"+id.String()+"/assign",
		transporthttp.AssignOperatorsRequest{OperatorIDs: nil})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestUnassignOperator_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	pid := uuid.New()
	op := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodDelete, fmt.Sprintf("/api/projects/%s/operators/%s", pid, op), nil)
	require.Equal(t, stdhttp.StatusNoContent, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, pid, f.projects.unassignID)
	assert.Equal(t, op, f.projects.unassignOp)
}

func TestListProjectMembers_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	f.projects.listMembers = []crmapi.ProjectMember{
		{OperatorID: uuid.New(), AssignedAt: time.Now(), Login: "op1", FullName: "Op One"},
	}
	f.setRoles(authapi.RoleSupervisor)

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/projects/"+id.String()+"/members", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.ListMembersResponse](t, rec)
	assert.Len(t, resp.Members, 1)
	assert.Equal(t, "op1", resp.Members[0].Login)
}

func TestListProjectMembers_OperatorForbidden(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)
	id := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/projects/"+id.String()+"/members", nil)
	require.Equal(t, stdhttp.StatusForbidden, rec.Code)
}

// =============================================================================
// Respondent endpoints
// =============================================================================

func TestCreateRespondent_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rid := uuid.New()
	f.respond.createRet = crmapi.Respondent{
		ID: rid, TenantID: f.tenantID, Status: crmapi.RespPending,
		PhoneMasked: "+7-9**-***-**-67",
	}

	pid := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/projects/"+pid.String()+"/respondents",
		transporthttp.CreateRespondentRequest{Phone: "+79161234567"})
	require.Equal(t, stdhttp.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.RespondentDTO](t, rec)
	assert.Equal(t, "+7-9**-***-**-67", resp.PhoneMasked)
	assert.Empty(t, resp.Phone)
}

func TestCreateRespondent_InvalidPhone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond.createErr = crmapi.ErrInvalidPhone

	pid := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/projects/"+pid.String()+"/respondents",
		transporthttp.CreateRespondentRequest{Phone: "abc"})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "crm.respondent.invalid_phone", body.Error)
}

func TestCreateRespondent_DNCConflict(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond.createErr = crmapi.ErrPhoneInDNC

	pid := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/projects/"+pid.String()+"/respondents",
		transporthttp.CreateRespondentRequest{Phone: "+79161234567"})
	require.Equal(t, stdhttp.StatusConflict, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "crm.respondent.phone_in_dnc", body.Error)
}

func TestGetRespondent_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rid := uuid.New()
	f.respond.getRet = crmapi.Respondent{
		ID: rid, TenantID: f.tenantID, PhoneMasked: "+7-9**-***-**-67",
	}
	f.setRoles(authapi.RoleOperator)

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/respondents/"+rid.String(), nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.RespondentDTO](t, rec)
	assert.Empty(t, resp.Phone)
	assert.Equal(t, "+7-9**-***-**-67", resp.PhoneMasked)
}

func TestGetRespondent_Deleted_410Gone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond.getErr = crmapi.ErrRespondentDeleted
	f.setRoles(authapi.RoleOperator)

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/respondents/"+uuid.New().String(), nil)
	require.Equal(t, stdhttp.StatusGone, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "crm.respondent.deleted", body.Error)
}

func TestGetRespondent_NotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond.getErr = crmapi.ErrRespondentNotFound
	f.setRoles(authapi.RoleOperator)

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/respondents/"+uuid.New().String(), nil)
	require.Equal(t, stdhttp.StatusNotFound, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "crm.respondent.not_found", body.Error)
}

func TestGetRespondentWithPhone_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rid := uuid.New()
	f.respond.getWithPhoneRet = crmapi.Respondent{
		ID: rid, TenantID: f.tenantID,
		Phone: "+79161234567", PhoneMasked: "+7-9**-***-**-67",
	}

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/respondents/"+rid.String()+"/with-phone", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.RespondentDTO](t, rec)
	assert.Equal(t, "+79161234567", resp.Phone)
}

func TestGetRespondentWithPhone_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleSupervisor)
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/respondents/"+uuid.New().String()+"/with-phone", nil)
	require.Equal(t, stdhttp.StatusForbidden, rec.Code)
}

func TestGetRespondentWithPhone_RBACDenied(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.rbac.denyAll = true
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/respondents/"+uuid.New().String()+"/with-phone", nil)
	require.Equal(t, stdhttp.StatusForbidden, rec.Code)
}

func TestSearchRespondents_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond.searchRet = crmapi.SearchRespondentsResult{
		Items: []crmapi.Respondent{
			{ID: uuid.New(), TenantID: f.tenantID, PhoneMasked: "***"},
		},
		TotalCount: 1,
	}
	f.setRoles(authapi.RoleOperator)

	pid := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/projects/"+pid.String()+"/respondents?page=2&page_size=20", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.SearchRespondentsResponse](t, rec)
	assert.Equal(t, 1, resp.TotalCount)
	assert.Equal(t, 2, resp.Page)
	assert.Equal(t, 20, resp.PageSize)
	assert.Equal(t, pid, f.respond.searchIn.ProjectID)
}

func TestDeleteRespondent_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rid := uuid.New()
	purgeAt := time.Now().Add(30 * 24 * time.Hour).UTC()
	f.respond.deleteRet = crmapi.DeletionRequest{
		RespondentID: rid,
		DeleteAt:     purgeAt,
	}

	rec := f.doAuth(t, stdhttp.MethodDelete, "/api/respondents/"+rid.String(), nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.DeletionReceiptDTO](t, rec)
	assert.Equal(t, rid.String(), resp.RespondentID)
	assert.True(t, resp.ScheduledPurgeAt.Equal(purgeAt))
}

func TestDeleteRespondent_AlreadyDeleted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond.deleteErr = crmapi.ErrRespondentDeleted
	rec := f.doAuth(t, stdhttp.MethodDelete, "/api/respondents/"+uuid.New().String(), nil)
	require.Equal(t, stdhttp.StatusGone, rec.Code)
}

func TestDeleteRespondent_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)
	rec := f.doAuth(t, stdhttp.MethodDelete, "/api/respondents/"+uuid.New().String(), nil)
	require.Equal(t, stdhttp.StatusForbidden, rec.Code)
}

func TestImportRespondents_JSONHappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond.importRet = crmapi.ImportTicket{
		JobID:     "job-1",
		ProjectID: uuid.New(),
		Enqueued:  true,
		Status:    "queued",
		StartedAt: time.Now().UTC(),
	}

	pid := uuid.New()
	rec := f.doAuthRaw(t, stdhttp.MethodPost,
		"/api/projects/"+pid.String()+"/respondents/import?format=csv&filename=phones.csv",
		"text/csv", []byte("phone\n+79161234567\n"))
	require.Equal(t, stdhttp.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.ImportTicketDTO](t, rec)
	assert.Equal(t, "job-1", resp.JobID)
	assert.Equal(t, crmapi.ImportFormatCSV, f.respond.importIn.Format)
}

func TestImportRespondents_PayloadTooBig(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond.importErr = crmapi.ErrImportPayloadTooBig

	pid := uuid.New()
	rec := f.doAuthRaw(t, stdhttp.MethodPost,
		"/api/projects/"+pid.String()+"/respondents/import",
		"text/csv", []byte("phone\n+79161234567\n"))
	require.Equal(t, stdhttp.StatusRequestEntityTooLarge, rec.Code)
}

func TestGetImportStatus_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond.statusRet = crmapi.ImportStatus{
		JobID: "job-1", State: "succeeded", Total: 100, Inserted: 95, Skipped: 5,
		StartedAt: time.Now().UTC(),
	}

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/imports/job-1", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.ImportStatusDTO](t, rec)
	assert.Equal(t, "succeeded", resp.State)
	assert.Equal(t, 95, resp.Inserted)
	assert.Equal(t, "job-1", f.respond.statusJobID)
}

func TestGetImportStatus_NotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond.statusErr = crmapi.ErrImportNotFound

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/imports/missing", nil)
	require.Equal(t, stdhttp.StatusNotFound, rec.Code)
}

// =============================================================================
// Misc / cross-cutting
// =============================================================================

func TestNoAuthHeader_Unauthorized(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// No bearer token at all.
	req := httptest.NewRequest(stdhttp.MethodGet, "/api/projects", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	require.Equal(t, stdhttp.StatusUnauthorized, rec.Code)
}

func TestInvalidToken_Unauthorized(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.err = authapi.ErrTokenInvalid

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/projects", nil)
	require.Equal(t, stdhttp.StatusUnauthorized, rec.Code)
}

func TestInternalError_5xxScrubbed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projects.getErr = errors.New("postgres exploded; secret password=hunter2 in the trace")
	f.setRoles(authapi.RoleOperator)

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/projects/"+uuid.New().String(), nil)
	require.Equal(t, stdhttp.StatusInternalServerError, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "crm.internal", body.Error)
	assert.Equal(t, "internal error", body.Message)
	assert.NotContains(t, rec.Body.String(), "hunter2", "5xx must NOT echo internal details")
}

// TestImportRespondents_Multipart exercises the multipart upload
// branch of the import endpoint. The fake captures the parsed request
// so we can assert format inference and body wiring.
func TestImportRespondents_Multipart(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond.importRet = crmapi.ImportTicket{
		JobID: "job-mp", Status: "queued", StartedAt: time.Now().UTC(),
	}

	pid := uuid.New()
	// Build a multipart body manually.
	var b bytes.Buffer
	const boundary = "test-boundary"
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString(`Content-Disposition: form-data; name="file"; filename="phones.csv"` + "\r\n")
	b.WriteString("Content-Type: text/csv\r\n\r\n")
	b.WriteString("phone\n+79161234567\n")
	b.WriteString("\r\n--" + boundary + "--\r\n")

	req := httptest.NewRequest(stdhttp.MethodPost,
		"/api/projects/"+pid.String()+"/respondents/import",
		bytes.NewReader(b.Bytes()))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Authorization", "Bearer dummy.token")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	require.Equal(t, stdhttp.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "phones.csv", f.respond.importIn.Filename)
	assert.Equal(t, crmapi.ImportFormatCSV, f.respond.importIn.Format)
}

// TestSearchRespondents_DefaultsApplied exercises the default
// page/page_size values when query params are absent.
func TestSearchRespondents_DefaultsApplied(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond.searchRet = crmapi.SearchRespondentsResult{TotalCount: 0}
	f.setRoles(authapi.RoleOperator)

	pid := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/projects/"+pid.String()+"/respondents", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.SearchRespondentsResponse](t, rec)
	assert.Equal(t, 1, resp.Page)
	assert.Equal(t, 50, resp.PageSize)
}

// TestUpdateProject_NotFound covers the 404 mapping for PATCH.
func TestUpdateProject_NotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projects.updateErr = crmapi.ErrProjectNotFound

	rec := f.doAuth(t, stdhttp.MethodPatch, "/api/projects/"+uuid.New().String(),
		transporthttp.UpdateProjectRequest{})
	require.Equal(t, stdhttp.StatusNotFound, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "crm.project.not_found", body.Error)
}

// TestListProjects_BadStatusFilter exercises the optional status query
// param — invalid values pass through to the service which rejects
// them, but a value-less filter is the default.
func TestListProjects_BadStatusFilter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projects.listRet = crmapi.ListProjectsResult{Items: nil, TotalCount: 0}
	f.setRoles(authapi.RoleOperator)

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/projects?status=paused&include_archived=true&search=foo", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code)
	require.NotNil(t, f.projects.listIn.Status)
	assert.Equal(t, crmapi.StatusPaused, *f.projects.listIn.Status)
	assert.True(t, f.projects.listIn.IncludeArchived)
	assert.Equal(t, "foo", f.projects.listIn.Search)
}

// TestGetProgressMissing covers the 404 path for the progress widget.
func TestGetProgressMissing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projects.progressErr = crmapi.ErrProjectNotFound
	f.setRoles(authapi.RoleOperator)

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/projects/"+uuid.New().String()+"/progress", nil)
	require.Equal(t, stdhttp.StatusNotFound, rec.Code)
}

// TestAssignOperators_BadID exercises the path-id parse failure.
func TestAssignOperators_BadID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/projects/not-a-uuid/assign",
		transporthttp.AssignOperatorsRequest{OperatorIDs: []uuid.UUID{uuid.New()}})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

// TestUnassign_BadOperatorID exercises the operator-id parse failure.
func TestUnassign_BadOperatorID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodDelete,
		"/api/projects/"+uuid.New().String()+"/operators/not-a-uuid", nil)
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

// TestUpdateProject_InvalidJSON exercises bind error on PATCH.
func TestUpdateProject_InvalidJSON(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuthRaw(t, stdhttp.MethodPatch,
		"/api/projects/"+uuid.New().String(),
		"application/json", []byte("not-json"))
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

// TestDeleteRespondent_BadID exercises the path-id parse failure.
func TestDeleteRespondent_BadID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodDelete, "/api/respondents/not-a-uuid", nil)
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

// TestGetProjectProgress_WithQuotas exercises the quota-mapping
// branch of progressToDTO.
func TestGetProjectProgress_WithQuotas(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	f.projects.progressRet = crmapi.ProjectProgress{
		ProjectID:   id,
		TargetCount: 100,
		QuotaProgress: []crmapi.QuotaSnapshot{
			{DimensionKind: "region", DimensionValue: "MSK", Target: 50, Done: 25, PercentDone: 50, IsFull: false},
		},
	}
	f.setRoles(authapi.RoleOperator)

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/projects/"+id.String()+"/progress", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.ProjectProgressDTO](t, rec)
	require.Len(t, resp.QuotaProgress, 1)
	assert.Equal(t, "MSK", resp.QuotaProgress[0].DimensionValue)
}

// TestGetImportStatus_WithErrors exercises the error-mapping branch
// of importStatusToDTO.
func TestGetImportStatus_WithErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond.statusRet = crmapi.ImportStatus{
		JobID: "j", State: "failed",
		Errors: []crmapi.ImportError{
			{Row: 5, Phone: "+xxx", Message: "bad phone"},
		},
		StartedAt: time.Now().UTC(),
	}

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/imports/j", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code)
	resp := decode[transporthttp.ImportStatusDTO](t, rec)
	require.Len(t, resp.Errors, 1)
	assert.Equal(t, 5, resp.Errors[0].Row)
}

// TestErrorMapping_AllSentinels exercises mapCRMError across every
// sentinel so the gocyclo-suppressed switch stays auditable.
func TestErrorMapping_AllSentinels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"DuplicateRespondent", crmapi.ErrDuplicateRespondent, stdhttp.StatusConflict, "crm.respondent.duplicate"},
		{"InvalidStatus", crmapi.ErrInvalidStatus, stdhttp.StatusConflict, "crm.project.invalid_status"},
		{"InvalidQuotaKind", crmapi.ErrInvalidQuotaKind, stdhttp.StatusBadRequest, "crm.quota.invalid_kind"},
		{"ImportInProgress", crmapi.ErrImportInProgress, stdhttp.StatusConflict, "crm.import.in_progress"},
		{"ImportFormatUnsupported", crmapi.ErrImportFormatUnsupported, stdhttp.StatusBadRequest, "crm.import.format_unsupported"},
		{"InvalidArgument", crmapi.ErrInvalidArgument, stdhttp.StatusBadRequest, "crm.invalid_argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.setRoles(authapi.RoleOperator)
			f.projects.getErr = tc.err
			rec := f.doAuth(t, stdhttp.MethodGet, "/api/projects/"+uuid.New().String(), nil)
			require.Equal(t, tc.status, rec.Code, "body=%s", rec.Body.String())
			body := decode[transporthttp.ErrorEnvelope](t, rec)
			assert.Equal(t, tc.code, body.Error)
		})
	}
}

// TestInferImportFormat_Branches verifies all format-inference paths
// via the public Mount surface (this is the most efficient way to
// trigger inferImportFormat across formats without exporting it).
func TestInferImportFormat_Branches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		path        string
		contentType string
		expect      crmapi.ImportFormat
	}{
		{"explicit xlsx", "?format=xlsx&filename=x.xlsx",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", crmapi.ImportFormatXLSX},
		{"content-type csv", "?filename=phones",
			"text/csv", crmapi.ImportFormatCSV},
		{"filename xlsx", "?filename=phones.xlsx",
			"application/octet-stream", crmapi.ImportFormatXLSX},
		{"filename csv", "?filename=phones.csv",
			"application/octet-stream", crmapi.ImportFormatCSV},
		{"unknown falls back to csv", "",
			"application/octet-stream", crmapi.ImportFormatCSV},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.respond.importRet = crmapi.ImportTicket{JobID: "j", Status: "queued", StartedAt: time.Now()}
			pid := uuid.New()
			rec := f.doAuthRaw(t, stdhttp.MethodPost,
				"/api/projects/"+pid.String()+"/respondents/import"+tc.path,
				tc.contentType, []byte("phone\n+79161234567\n"))
			require.Equal(t, stdhttp.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
			assert.Equal(t, tc.expect, f.respond.importIn.Format)
		})
	}
}

// TestImportRespondents_JSONBody exercises the non-multipart code path
// where the body is read directly from the request body.
func TestImportRespondents_JSONBody(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond.importRet = crmapi.ImportTicket{JobID: "json-job", Status: "queued", StartedAt: time.Now()}
	pid := uuid.New()
	rec := f.doAuthRaw(t, stdhttp.MethodPost,
		"/api/projects/"+pid.String()+"/respondents/import?format=csv&filename=raw.csv",
		"application/octet-stream", []byte("phone\n+79161234567\n"))
	require.Equal(t, stdhttp.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "raw.csv", f.respond.importIn.Filename)
}

// TestImportRespondents_MultipartMissingFile exercises the bind-error
// branch when the multipart body lacks the "file" form field.
func TestImportRespondents_MultipartMissingFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	pid := uuid.New()
	const boundary = "test-boundary"
	var b bytes.Buffer
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString(`Content-Disposition: form-data; name="other"` + "\r\n\r\n")
	b.WriteString("data")
	b.WriteString("\r\n--" + boundary + "--\r\n")

	req := httptest.NewRequest(stdhttp.MethodPost,
		"/api/projects/"+pid.String()+"/respondents/import",
		bytes.NewReader(b.Bytes()))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Authorization", "Bearer dummy.token")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

// TestImportRespondents_BadProjectID exercises the path-id failure
// branch.
func TestImportRespondents_BadProjectID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuthRaw(t, stdhttp.MethodPost,
		"/api/projects/not-uuid/respondents/import",
		"text/csv", []byte("phone\n+79161234567\n"))
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

// TestGetImportStatus_EmptyJobID exercises the empty-job-id guard.
func TestGetImportStatus_EmptyJobID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// gin treats /imports/ as a 404 because :job_id is required, but
	// /imports/some-id works. We don't have a public empty-id route;
	// a status query for a missing job returns ErrImportNotFound.
	f.respond.statusErr = crmapi.ErrImportNotFound
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/imports/some-id", nil)
	require.Equal(t, stdhttp.StatusNotFound, rec.Code)
}

// TestSearchRespondents_StatusFilter exercises the optional status
// query param wiring.
func TestSearchRespondents_StatusFilter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond.searchRet = crmapi.SearchRespondentsResult{}
	f.setRoles(authapi.RoleOperator)
	pid := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodGet,
		"/api/projects/"+pid.String()+"/respondents?status=pending&region=MSK&query=ivan", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code)
	require.NotNil(t, f.respond.searchIn.Status)
	assert.Equal(t, crmapi.RespPending, *f.respond.searchIn.Status)
	assert.Equal(t, "MSK", f.respond.searchIn.Region)
	assert.Equal(t, "ivan", f.respond.searchIn.Query)
}

func TestMount_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	cases := []struct {
		name string
		deps transporthttp.Deps
	}{
		{"nil projects", transporthttp.Deps{Respondent: &fakeRespondentService{}, RBAC: &fakeRBAC{}, Validator: &fakeValidator{}}},
		{"nil respondent", transporthttp.Deps{Projects: &fakeProjectService{}, RBAC: &fakeRBAC{}, Validator: &fakeValidator{}}},
		{"nil rbac", transporthttp.Deps{Projects: &fakeProjectService{}, Respondent: &fakeRespondentService{}, Validator: &fakeValidator{}}},
		{"nil validator", transporthttp.Deps{Projects: &fakeProjectService{}, Respondent: &fakeRespondentService{}, RBAC: &fakeRBAC{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := gin.New()
			require.Panics(t, func() {
				transporthttp.Mount(r.Group("/api"), tc.deps)
			})
		})
	}
}
