package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	surveysapi "github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/internal/surveys/schemavalidator"
	transporthttp "github.com/sociopulse/platform/internal/surveys/transport/http"
)

// =============================================================================
// Fakes
// =============================================================================

// fakeSurveyService records calls and returns canned values. Hand-
// rolled rather than gomock-generated because Plan 05/06 lessons
// learned § 8 strongly prefer hand-rolled fakes for transport tests.
type fakeSurveyService struct {
	createIn   surveysapi.CreateSurveyInput
	createID   uuid.UUID
	createErr  error
	getIDs     []uuid.UUID
	getRet     surveysapi.Survey
	getErr     error
	listIn     surveysapi.ListFilter
	listRet    []surveysapi.Survey
	listErr    error
	updateID   uuid.UUID
	updateIn   surveysapi.UpdateSurveyInput
	updateErr  error
	archiveID  uuid.UUID
	archiveErr error
	saveSurvey uuid.UUID
	saveSchema []byte
	saveMinor  bool
	saveRet    surveysapi.Version
	saveErr    error
	activateS  uuid.UUID
	activateV  uuid.UUID
	activErr   error
	activeID   uuid.UUID
	activeRet  surveysapi.Version
	activeErr  error
	listVerID  uuid.UUID
	listVerRet []surveysapi.Version
	listVerErr error
}

func (f *fakeSurveyService) Create(_ context.Context, in surveysapi.CreateSurveyInput) (uuid.UUID, error) {
	f.createIn = in
	if f.createErr != nil {
		return uuid.Nil, f.createErr
	}
	return f.createID, nil
}

func (f *fakeSurveyService) Get(_ context.Context, id uuid.UUID) (surveysapi.Survey, error) {
	f.getIDs = append(f.getIDs, id)
	if f.getErr != nil {
		return surveysapi.Survey{}, f.getErr
	}
	return f.getRet, nil
}

func (f *fakeSurveyService) List(_ context.Context, filter surveysapi.ListFilter) ([]surveysapi.Survey, error) {
	f.listIn = filter
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listRet, nil
}

func (f *fakeSurveyService) Update(_ context.Context, id uuid.UUID, in surveysapi.UpdateSurveyInput) error {
	f.updateID = id
	f.updateIn = in
	return f.updateErr
}

func (f *fakeSurveyService) Archive(_ context.Context, id uuid.UUID) error {
	f.archiveID = id
	return f.archiveErr
}

func (f *fakeSurveyService) SaveVersion(_ context.Context, surveyID uuid.UUID, schema []byte, minor bool) (surveysapi.Version, error) {
	f.saveSurvey = surveyID
	f.saveSchema = schema
	f.saveMinor = minor
	if f.saveErr != nil {
		return surveysapi.Version{}, f.saveErr
	}
	return f.saveRet, nil
}

func (f *fakeSurveyService) Activate(_ context.Context, surveyID, versionID uuid.UUID) error {
	f.activateS = surveyID
	f.activateV = versionID
	return f.activErr
}

func (f *fakeSurveyService) GetActiveVersion(_ context.Context, surveyID uuid.UUID) (surveysapi.Version, error) {
	f.activeID = surveyID
	if f.activeErr != nil {
		return surveysapi.Version{}, f.activeErr
	}
	return f.activeRet, nil
}

func (f *fakeSurveyService) ListVersions(_ context.Context, surveyID uuid.UUID) ([]surveysapi.Version, error) {
	f.listVerID = surveyID
	if f.listVerErr != nil {
		return nil, f.listVerErr
	}
	return f.listVerRet, nil
}

// fakeRuntime records preview calls and returns canned values.
type fakeRuntime struct {
	nextSchema []byte
	nextNode   string
	nextAns    map[string]surveysapi.Answer
	nextRet    surveysapi.NodeResult
	nextErr    error

	validateRet error
	progressRet float64
	progressErr error
}

func (f *fakeRuntime) NextNode(schema []byte, currentNodeID string, ans map[string]surveysapi.Answer) (surveysapi.NodeResult, error) {
	f.nextSchema = schema
	f.nextNode = currentNodeID
	f.nextAns = ans
	if f.nextErr != nil {
		return surveysapi.NodeResult{}, f.nextErr
	}
	return f.nextRet, nil
}

func (f *fakeRuntime) ValidateAnswer(_ []byte, _ string, _ surveysapi.Answer) error {
	return f.validateRet
}

func (f *fakeRuntime) CalculateProgress(_ []byte, _ string) (float64, error) {
	if f.progressErr != nil {
		return 0, f.progressErr
	}
	return f.progressRet, nil
}

// fakeValidator records calls and returns canned reports.
type fakeValidator struct {
	called  int
	gotBody []byte
	ret     schemavalidator.ValidationReport
}

func (f *fakeValidator) Validate(_ context.Context, body []byte) schemavalidator.ValidationReport {
	f.called++
	f.gotBody = append([]byte(nil), body...)
	return f.ret
}

// fakeRBAC always allows; tests that need a deny override Check.
type fakeRBAC struct {
	denyAll bool
}

func (f *fakeRBAC) Check(_ context.Context, _ authapi.Claims, _ authapi.Action, _ authapi.Resource) error {
	if f.denyAll {
		return authapi.ErrInsufficientRole
	}
	return nil
}

// fakeAuthValidator returns canned Claims for any token.
type fakeAuthValidator struct {
	claims authapi.Claims
	err    error
}

func (f *fakeAuthValidator) Validate(_ context.Context, _ string) (authapi.Claims, error) {
	return f.claims, f.err
}

// =============================================================================
// Test fixture
// =============================================================================

type fixture struct {
	router    *gin.Engine
	surveys   *fakeSurveyService
	runtime   *fakeRuntime
	validator *fakeValidator
	rbac      *fakeRBAC
	authV     *fakeAuthValidator

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
		router:    r,
		surveys:   &fakeSurveyService{},
		runtime:   &fakeRuntime{},
		validator: &fakeValidator{ret: schemavalidator.ValidationReport{Valid: true}},
		rbac:      &fakeRBAC{},
		authV: &fakeAuthValidator{
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
		Logger:    nil,
		Surveys:   f.surveys,
		Runtime:   f.runtime,
		Validator: f.validator,
		Auth:      f.authV,
		RBAC:      f.rbac,
	})
	return f
}

func (f *fixture) setRoles(rs ...authapi.Role) {
	f.authV.claims.Roles = rs
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

// doAuthRaw issues an authenticated POST with a raw body (for the
// validate endpoint which accepts arbitrary JSON bytes).
func (f *fixture) doAuthRaw(t *testing.T, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(stdhttp.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer dummy.token")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// doNoAuth issues a request without an Authorization header.
func (f *fixture) doNoAuth(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
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
// Mount nil-deps
// =============================================================================

func TestMount_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	require.Panics(t, func() {
		transporthttp.Mount(r.Group("/api"), transporthttp.Deps{})
	})
}

// =============================================================================
// CreateSurvey
// =============================================================================

func TestCreateSurvey_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	f.surveys.createID = id

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/surveys", transporthttp.CreateSurveyRequest{
		Name:        "Q1 2026",
		Description: "Pilot survey",
		PrimaryMode: "form",
	})
	require.Equal(t, stdhttp.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.CreateSurveyResponse](t, rec)
	assert.Equal(t, id.String(), resp.ID)
	assert.Equal(t, "Q1 2026", f.surveys.createIn.Name)
	assert.Equal(t, surveysapi.ModeForm, f.surveys.createIn.PrimaryMode)
}

func TestCreateSurvey_DefaultsModeWhenAbsent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.surveys.createID = uuid.New()

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/surveys", transporthttp.CreateSurveyRequest{
		Name: "X",
	})
	require.Equal(t, stdhttp.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, surveysapi.ModeForm, f.surveys.createIn.PrimaryMode)
}

func TestCreateSurvey_BadRequest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/surveys", map[string]string{
		"description": "nameless",
	})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "surveys.bad_request", body.Error)
}

func TestCreateSurvey_InvalidArgument(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.surveys.createErr = surveysapi.ErrInvalidArgument

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/surveys", transporthttp.CreateSurveyRequest{
		Name: "X",
	})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "surveys.invalid_argument", body.Error)
}

func TestCreateSurvey_OperatorForbidden(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/surveys", transporthttp.CreateSurveyRequest{
		Name: "X",
	})
	require.Equal(t, stdhttp.StatusForbidden, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "auth.insufficient_role", body.Error)
}

func TestCreateSurvey_NoAuth401(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doNoAuth(t, stdhttp.MethodPost, "/api/surveys", transporthttp.CreateSurveyRequest{
		Name: "X",
	})
	require.Equal(t, stdhttp.StatusUnauthorized, rec.Code)
}

// =============================================================================
// ListSurveys
// =============================================================================

func TestListSurveys_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)
	f.surveys.listRet = []surveysapi.Survey{
		{ID: uuid.New(), TenantID: f.tenantID, Name: "S1", Status: surveysapi.StatusActive, PrimaryMode: surveysapi.ModeForm},
		{ID: uuid.New(), TenantID: f.tenantID, Name: "S2", Status: surveysapi.StatusActive, PrimaryMode: surveysapi.ModeFlow},
	}

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/surveys?limit=10&offset=5&status=active&search=foo", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.ListSurveysResponse](t, rec)
	assert.Equal(t, 2, resp.Total)
	assert.Len(t, resp.Surveys, 2)
	assert.Equal(t, 10, f.surveys.listIn.Limit)
	assert.Equal(t, 5, f.surveys.listIn.Offset)
	assert.Equal(t, surveysapi.StatusActive, f.surveys.listIn.Status)
	assert.Equal(t, "foo", f.surveys.listIn.Search)
}

func TestListSurveys_DefaultsWhenNoQuery(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/surveys", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code)
	assert.Equal(t, 50, f.surveys.listIn.Limit)
	assert.Equal(t, 0, f.surveys.listIn.Offset)
}

// =============================================================================
// GetSurvey
// =============================================================================

func TestGetSurvey_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)
	id := uuid.New()
	f.surveys.getRet = surveysapi.Survey{
		ID: id, TenantID: f.tenantID, Name: "X", Status: surveysapi.StatusActive,
		PrimaryMode: surveysapi.ModeForm, CreatedAt: time.Now().UTC(),
	}

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/surveys/"+id.String(), nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.SurveyDTO](t, rec)
	assert.Equal(t, id.String(), resp.ID)
	assert.Equal(t, "X", resp.Name)
}

func TestGetSurvey_NotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)
	f.surveys.getErr = surveysapi.ErrNotFound

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/surveys/"+uuid.New().String(), nil)
	require.Equal(t, stdhttp.StatusNotFound, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "surveys.not_found", body.Error)
}

func TestGetSurvey_BadID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/surveys/not-a-uuid", nil)
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

// =============================================================================
// UpdateSurvey
// =============================================================================

func TestUpdateSurvey_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	name := "Renamed"

	rec := f.doAuth(t, stdhttp.MethodPatch, "/api/surveys/"+id.String(),
		transporthttp.UpdateSurveyRequest{Name: &name})
	require.Equal(t, stdhttp.StatusNoContent, rec.Code, "body=%s", rec.Body.String())
	require.NotNil(t, f.surveys.updateIn.Name)
	assert.Equal(t, "Renamed", *f.surveys.updateIn.Name)
	assert.Equal(t, id, f.surveys.updateID)
}

func TestUpdateSurvey_Archived(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.surveys.updateErr = surveysapi.ErrSurveyArchived

	rec := f.doAuth(t, stdhttp.MethodPatch, "/api/surveys/"+uuid.New().String(),
		transporthttp.UpdateSurveyRequest{})
	require.Equal(t, stdhttp.StatusForbidden, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "surveys.archived", body.Error)
}

// =============================================================================
// ArchiveSurvey
// =============================================================================

func TestArchiveSurvey_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/surveys/"+id.String()+"/archive", nil)
	require.Equal(t, stdhttp.StatusNoContent, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, id, f.surveys.archiveID)
}

func TestArchiveSurvey_NotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.surveys.archiveErr = surveysapi.ErrNotFound

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/surveys/"+uuid.New().String()+"/archive", nil)
	require.Equal(t, stdhttp.StatusNotFound, rec.Code)
}

// =============================================================================
// SaveVersion
// =============================================================================

func TestSaveVersion_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	verID := uuid.New()
	schema := json.RawMessage(`{"version":"1.0","primary_mode":"flow","nodes":[]}`)
	f.surveys.saveRet = surveysapi.Version{
		ID: verID, SurveyID: id, Major: 1, Minor: 0, Schema: []byte(schema),
		CreatedAt: time.Now().UTC(),
	}

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/surveys/"+id.String()+"/versions",
		transporthttp.SaveVersionRequest{Schema: schema, Minor: false})
	require.Equal(t, stdhttp.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.VersionDTO](t, rec)
	assert.Equal(t, verID.String(), resp.ID)
	assert.Equal(t, 1, resp.Major)
	assert.False(t, f.surveys.saveMinor)
	assert.JSONEq(t, string(schema), string(f.surveys.saveSchema))
}

func TestSaveVersion_MinorBumpFlag(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	f.surveys.saveRet = surveysapi.Version{ID: uuid.New(), SurveyID: id, Major: 1, Minor: 1}

	schema := json.RawMessage(`{"a":1}`)
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/surveys/"+id.String()+"/versions",
		transporthttp.SaveVersionRequest{Schema: schema, Minor: true})
	require.Equal(t, stdhttp.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	assert.True(t, f.surveys.saveMinor)
}

func TestSaveVersion_ValidationError_422_WithReport(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	f.surveys.saveErr = &surveysapi.ValidationError{
		Report: surveysapi.Report{
			Issues: []surveysapi.Issue{
				{Code: "graph.cycle-no-exit", NodeID: "q3", Message: "cycle reachable"},
			},
		},
	}

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/surveys/"+id.String()+"/versions",
		transporthttp.SaveVersionRequest{Schema: json.RawMessage(`{"a":1}`)})
	require.Equal(t, stdhttp.StatusUnprocessableEntity, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.ValidationReportDTO](t, rec)
	assert.False(t, resp.Valid)
	require.Len(t, resp.Issues, 1)
	assert.Equal(t, "graph.cycle-no-exit", resp.Issues[0].Code)
	assert.Equal(t, "q3", resp.Issues[0].Path)
}

func TestSaveVersion_EmptyBodyRejected(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := uuid.New()
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/surveys/"+id.String()+"/versions",
		transporthttp.SaveVersionRequest{Minor: true})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestSaveVersion_OperatorForbidden(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/surveys/"+uuid.New().String()+"/versions",
		transporthttp.SaveVersionRequest{Schema: json.RawMessage(`{"a":1}`)})
	require.Equal(t, stdhttp.StatusForbidden, rec.Code)
}

func TestSaveVersion_NotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.surveys.saveErr = surveysapi.ErrNotFound

	rec := f.doAuth(t, stdhttp.MethodPost, "/api/surveys/"+uuid.New().String()+"/versions",
		transporthttp.SaveVersionRequest{Schema: json.RawMessage(`{"a":1}`)})
	require.Equal(t, stdhttp.StatusNotFound, rec.Code)
}

// =============================================================================
// Activate
// =============================================================================

func TestActivateVersion_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	sid := uuid.New()
	vid := uuid.New()

	rec := f.doAuth(t, stdhttp.MethodPost,
		"/api/surveys/"+sid.String()+"/versions/"+vid.String()+"/activate", nil)
	require.Equal(t, stdhttp.StatusNoContent, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, sid, f.surveys.activateS)
	assert.Equal(t, vid, f.surveys.activateV)
}

func TestActivateVersion_VersionNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.surveys.activErr = surveysapi.ErrVersionNotFound

	rec := f.doAuth(t, stdhttp.MethodPost,
		"/api/surveys/"+uuid.New().String()+"/versions/"+uuid.New().String()+"/activate", nil)
	require.Equal(t, stdhttp.StatusNotFound, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "surveys.version_not_found", body.Error)
}

func TestActivateVersion_BadVersionID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodPost,
		"/api/surveys/"+uuid.New().String()+"/versions/not-a-uuid/activate", nil)
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

// =============================================================================
// GetActiveVersion
// =============================================================================

func TestGetActiveVersion_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)
	sid := uuid.New()
	vid := uuid.New()
	now := time.Now().UTC()
	f.surveys.activeRet = surveysapi.Version{
		ID: vid, SurveyID: sid, Major: 1, Minor: 0,
		Schema: []byte(`{"k":1}`), IsActive: true,
		CreatedAt: now, ActivatedAt: &now,
	}

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/surveys/"+sid.String()+"/versions/active", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.VersionDTO](t, rec)
	assert.Equal(t, vid.String(), resp.ID)
	assert.True(t, resp.IsActive)
	assert.JSONEq(t, `{"k":1}`, string(resp.Schema))
}

func TestGetActiveVersion_NoActive(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)
	f.surveys.activeErr = surveysapi.ErrNoActiveVersion

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/surveys/"+uuid.New().String()+"/versions/active", nil)
	require.Equal(t, stdhttp.StatusNotFound, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "surveys.no_active_version", body.Error)
}

// =============================================================================
// ListVersions
// =============================================================================

func TestListVersions_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleSupervisor)
	sid := uuid.New()
	f.surveys.listVerRet = []surveysapi.Version{
		{ID: uuid.New(), SurveyID: sid, Major: 1, Minor: 1, IsActive: true},
		{ID: uuid.New(), SurveyID: sid, Major: 1, Minor: 0},
	}

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/surveys/"+sid.String()+"/versions", nil)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.ListVersionsResponse](t, rec)
	assert.Len(t, resp.Versions, 2)
	assert.True(t, resp.Versions[0].IsActive)
}

// =============================================================================
// PreviewRun
// =============================================================================

func TestPreviewRun_HappyPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)
	f.runtime.nextRet = surveysapi.NodeResult{
		NextNodeID: "q2",
		Terminated: false,
		Progress:   0.5,
	}

	num := 42.0
	body := transporthttp.PreviewRunRequest{
		Schema:        json.RawMessage(`{"version":"1.0","nodes":[]}`),
		CurrentNodeID: "q1",
		Answers: map[string]transporthttp.AnswerPayload{
			"q1": {SingleChoice: "yes"},
			"q2": {Number: &num},
			"q3": {MultiChoice: []string{"a", "b"}},
			"q4": {Text: "free-form"},
		},
	}
	rec := f.doAuth(t, stdhttp.MethodPost,
		"/api/surveys/"+uuid.New().String()+"/preview/run", body)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.PreviewRunResponse](t, rec)
	assert.Equal(t, "q2", resp.NextNodeID)
	assert.False(t, resp.Terminated)
	assert.InDelta(t, 0.5, resp.Progress, 0.001)

	// Verify answers were converted into the api.Answer map.
	assert.Equal(t, "q1", f.runtime.nextNode)
	require.Len(t, f.runtime.nextAns, 4)
	assert.Equal(t, "yes", f.runtime.nextAns["q1"].SingleChoice)
	require.NotNil(t, f.runtime.nextAns["q2"].Number)
	assert.InDelta(t, 42.0, *f.runtime.nextAns["q2"].Number, 0.001)
	assert.Equal(t, []string{"a", "b"}, f.runtime.nextAns["q3"].MultiChoice)
	assert.Equal(t, "free-form", f.runtime.nextAns["q4"].Text)
}

func TestPreviewRun_NoMatchingEdge_422(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)
	f.runtime.nextErr = surveysapi.ErrNoMatchingEdge

	rec := f.doAuth(t, stdhttp.MethodPost,
		"/api/surveys/"+uuid.New().String()+"/preview/run",
		transporthttp.PreviewRunRequest{
			Schema:        json.RawMessage(`{"a":1}`),
			CurrentNodeID: "q1",
		})
	require.Equal(t, stdhttp.StatusUnprocessableEntity, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "surveys.no_matching_edge", body.Error)
}

func TestPreviewRun_NodeNotFound_404(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)
	f.runtime.nextErr = surveysapi.ErrNodeNotFound

	rec := f.doAuth(t, stdhttp.MethodPost,
		"/api/surveys/"+uuid.New().String()+"/preview/run",
		transporthttp.PreviewRunRequest{
			Schema:        json.RawMessage(`{"a":1}`),
			CurrentNodeID: "missing",
		})
	require.Equal(t, stdhttp.StatusNotFound, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "surveys.node_not_found", body.Error)
}

func TestPreviewRun_EmptyBodyRejected(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)

	rec := f.doAuth(t, stdhttp.MethodPost,
		"/api/surveys/"+uuid.New().String()+"/preview/run",
		transporthttp.PreviewRunRequest{CurrentNodeID: "q1"})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestPreviewRun_BadSurveyID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)
	rec := f.doAuth(t, stdhttp.MethodPost,
		"/api/surveys/not-a-uuid/preview/run",
		transporthttp.PreviewRunRequest{
			Schema:        json.RawMessage(`{"a":1}`),
			CurrentNodeID: "q1",
		})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

// =============================================================================
// ValidateSchema
// =============================================================================

func TestValidateSchema_Valid_200(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.ret = schemavalidator.ValidationReport{Valid: true}
	body := []byte(`{"version":"1.0","primary_mode":"form","nodes":[]}`)

	rec := f.doAuthRaw(t,
		"/api/surveys/"+uuid.New().String()+"/validate", body)
	require.Equal(t, stdhttp.StatusOK, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.ValidationReportDTO](t, rec)
	assert.True(t, resp.Valid)
	assert.Equal(t, 1, f.validator.called)
	assert.JSONEq(t, string(body), string(f.validator.gotBody))
}

func TestValidateSchema_Invalid_422_WithReport(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.validator.ret = schemavalidator.ValidationReport{
		Valid: false,
		Issues: []schemavalidator.Issue{
			{Code: schemavalidator.CodeGraphDanglingEdge, Path: "/nodes/3/next/0/to", Message: "edge target missing"},
			{Code: schemavalidator.CodeGraphForwardRef, Path: "node:q5", Message: "q5 not yet visited"},
		},
	}

	body := []byte(`{"version":"1.0","nodes":[]}`)
	rec := f.doAuthRaw(t,
		"/api/surveys/"+uuid.New().String()+"/validate", body)
	require.Equal(t, stdhttp.StatusUnprocessableEntity, rec.Code, "body=%s", rec.Body.String())
	resp := decode[transporthttp.ValidationReportDTO](t, rec)
	assert.False(t, resp.Valid)
	require.Len(t, resp.Issues, 2)
	assert.Equal(t, schemavalidator.CodeGraphDanglingEdge, resp.Issues[0].Code)
	assert.Equal(t, "/nodes/3/next/0/to", resp.Issues[0].Path)
}

func TestValidateSchema_EmptyBody_400(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuthRaw(t,
		"/api/surveys/"+uuid.New().String()+"/validate", nil)
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestValidateSchema_OperatorForbidden(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.setRoles(authapi.RoleOperator)

	rec := f.doAuthRaw(t,
		"/api/surveys/"+uuid.New().String()+"/validate",
		[]byte(`{"a":1}`))
	require.Equal(t, stdhttp.StatusForbidden, rec.Code)
}

// =============================================================================
// Auth middleware integration
// =============================================================================

func TestEndpoints_RequireAuthMiddleware(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"create", stdhttp.MethodPost, "/api/surveys"},
		{"list", stdhttp.MethodGet, "/api/surveys"},
		{"get", stdhttp.MethodGet, "/api/surveys/" + uuid.New().String()},
		{"update", stdhttp.MethodPatch, "/api/surveys/" + uuid.New().String()},
		{"archive", stdhttp.MethodPost, "/api/surveys/" + uuid.New().String() + "/archive"},
		{"saveVersion", stdhttp.MethodPost, "/api/surveys/" + uuid.New().String() + "/versions"},
		{"activate", stdhttp.MethodPost, "/api/surveys/" + uuid.New().String() + "/versions/" + uuid.New().String() + "/activate"},
		{"getActive", stdhttp.MethodGet, "/api/surveys/" + uuid.New().String() + "/versions/active"},
		{"listVersions", stdhttp.MethodGet, "/api/surveys/" + uuid.New().String() + "/versions"},
		{"previewRun", stdhttp.MethodPost, "/api/surveys/" + uuid.New().String() + "/preview/run"},
		{"validate", stdhttp.MethodPost, "/api/surveys/" + uuid.New().String() + "/validate"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			rec := f.doNoAuth(t, tc.method, tc.path, nil)
			require.Equalf(t, stdhttp.StatusUnauthorized, rec.Code, "%s %s -> %d", tc.method, tc.path, rec.Code)
		})
	}
}

func TestAuthValidatorError_Yields401(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.authV.err = errors.New("token signing key rotated")

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/surveys", nil)
	require.Equal(t, stdhttp.StatusUnauthorized, rec.Code)
}

// =============================================================================
// Mount nil-deps coverage
// =============================================================================

func TestMount_PanicsOnEachMissingDep(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name string
		deps transporthttp.Deps
	}{
		{"surveys", transporthttp.Deps{Runtime: &fakeRuntime{}, Validator: &fakeValidator{}, Auth: &fakeAuthValidator{}, RBAC: &fakeRBAC{}}},
		{"runtime", transporthttp.Deps{Surveys: &fakeSurveyService{}, Validator: &fakeValidator{}, Auth: &fakeAuthValidator{}, RBAC: &fakeRBAC{}}},
		{"validator", transporthttp.Deps{Surveys: &fakeSurveyService{}, Runtime: &fakeRuntime{}, Auth: &fakeAuthValidator{}, RBAC: &fakeRBAC{}}},
		{"auth", transporthttp.Deps{Surveys: &fakeSurveyService{}, Runtime: &fakeRuntime{}, Validator: &fakeValidator{}, RBAC: &fakeRBAC{}}},
		{"rbac", transporthttp.Deps{Surveys: &fakeSurveyService{}, Runtime: &fakeRuntime{}, Validator: &fakeValidator{}, Auth: &fakeAuthValidator{}}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := gin.New()
			require.Panicsf(t, func() {
				transporthttp.Mount(r.Group("/api"), tc.deps)
			}, "missing %s should panic", tc.name)
		})
	}
}

// =============================================================================
// Internal-error path (5xx)
// =============================================================================

func TestListSurveys_InternalError_500(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.surveys.listErr = errors.New("connection lost")

	rec := f.doAuth(t, stdhttp.MethodGet, "/api/surveys", nil)
	require.Equal(t, stdhttp.StatusInternalServerError, rec.Code)
	body := decode[transporthttp.ErrorEnvelope](t, rec)
	assert.Equal(t, "surveys.internal", body.Error)
	assert.Equal(t, "internal error", body.Message)
	// Verify the underlying error message did NOT leak.
	assert.NotContains(t, body.Message, "connection lost")
}

func TestUpdateSurvey_BadRequest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodPatch, "/api/surveys/not-a-uuid",
		transporthttp.UpdateSurveyRequest{})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestUpdateSurvey_InvalidPayload(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodPatch, "/api/surveys/"+uuid.New().String(),
		map[string]string{"primary_mode": "garbage"})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestUpdateSurvey_PrimaryModeUpdate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	mode := "flow"
	rec := f.doAuth(t, stdhttp.MethodPatch, "/api/surveys/"+uuid.New().String(),
		transporthttp.UpdateSurveyRequest{PrimaryMode: &mode})
	require.Equal(t, stdhttp.StatusNoContent, rec.Code, "body=%s", rec.Body.String())
	require.NotNil(t, f.surveys.updateIn.PrimaryMode)
	assert.Equal(t, surveysapi.ModeFlow, *f.surveys.updateIn.PrimaryMode)
}

func TestArchiveSurvey_BadID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/surveys/not-a-uuid/archive", nil)
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestSaveVersion_BadID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodPost, "/api/surveys/not-a-uuid/versions",
		transporthttp.SaveVersionRequest{Schema: json.RawMessage(`{"a":1}`)})
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestActivateVersion_BadSurveyID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodPost,
		"/api/surveys/not-a-uuid/versions/"+uuid.New().String()+"/activate", nil)
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestGetActiveVersion_BadID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/surveys/not-a-uuid/versions/active", nil)
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestListVersions_BadID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/surveys/not-a-uuid/versions", nil)
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

func TestListVersions_Internal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.surveys.listVerErr = errors.New("db down")
	rec := f.doAuth(t, stdhttp.MethodGet, "/api/surveys/"+uuid.New().String()+"/versions", nil)
	require.Equal(t, stdhttp.StatusInternalServerError, rec.Code)
}

func TestValidateSchema_BadID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.doAuthRaw(t, "/api/surveys/not-a-uuid/validate", []byte(`{"a":1}`))
	require.Equal(t, stdhttp.StatusBadRequest, rec.Code)
}

// =============================================================================
// mapSurveyError coverage (sentinel matrix)
// =============================================================================

func TestErrorMapping_Coverage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		injectErr error
		wantCode  int
		wantSlug  string
	}{
		{"InvalidArgument", surveysapi.ErrInvalidArgument, stdhttp.StatusBadRequest, "surveys.invalid_argument"},
		{"NoActiveVersion", surveysapi.ErrNoActiveVersion, stdhttp.StatusNotFound, "surveys.no_active_version"},
		{"NodeNotFound", surveysapi.ErrNodeNotFound, stdhttp.StatusNotFound, "surveys.node_not_found"},
		{"NotFound", surveysapi.ErrNotFound, stdhttp.StatusNotFound, "surveys.not_found"},
		{"VersionNotFound", surveysapi.ErrVersionNotFound, stdhttp.StatusNotFound, "surveys.version_not_found"},
		{"Archived", surveysapi.ErrSurveyArchived, stdhttp.StatusForbidden, "surveys.archived"},
		{"Schema", surveysapi.ErrSchema, stdhttp.StatusUnprocessableEntity, "surveys.schema_invalid"},
		{"Validation", surveysapi.ErrValidation, stdhttp.StatusUnprocessableEntity, "surveys.validation_failed"},
		{"Cycle", surveysapi.ErrCycle, stdhttp.StatusUnprocessableEntity, "surveys.graph_invalid"},
		{"Unreachable", surveysapi.ErrUnreachable, stdhttp.StatusUnprocessableEntity, "surveys.graph_invalid"},
		{"Dangling", surveysapi.ErrDanglingEdge, stdhttp.StatusUnprocessableEntity, "surveys.graph_invalid"},
		{"ForwardRef", surveysapi.ErrForwardRef, stdhttp.StatusUnprocessableEntity, "surveys.graph_invalid"},
		{"NoMatchingEdge", surveysapi.ErrNoMatchingEdge, stdhttp.StatusUnprocessableEntity, "surveys.no_matching_edge"},
		{"BadAnswer", surveysapi.ErrBadAnswer, stdhttp.StatusBadRequest, "surveys.bad_answer"},
		{"InsufficientRoleEcho", authapi.ErrInsufficientRole, stdhttp.StatusForbidden, "auth.insufficient_role"},
		{"TokenInvalidEcho", authapi.ErrTokenInvalid, stdhttp.StatusUnauthorized, "auth.token_invalid"},
		{"TokenRevokedEcho", authapi.ErrTokenRevoked, stdhttp.StatusUnauthorized, "auth.token_revoked"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.surveys.archiveErr = tc.injectErr
			rec := f.doAuth(t, stdhttp.MethodPost,
				"/api/surveys/"+uuid.New().String()+"/archive", nil)
			require.Equalf(t, tc.wantCode, rec.Code, "body=%s", rec.Body.String())
			body := decode[transporthttp.ErrorEnvelope](t, rec)
			assert.Equal(t, tc.wantSlug, body.Error)
		})
	}
}
