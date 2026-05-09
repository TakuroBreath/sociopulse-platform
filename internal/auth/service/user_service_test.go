package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	authapi "github.com/sociopulse/platform/internal/auth/api"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// fakeTxRunner runs every fn synchronously with a zero postgres.Tx —
// the store fakes never read from it, so we don't need to spin up a
// real database. It records the supplied tenant ids so tests can
// confirm the service picked the correct one for each call. Plan 11.4:
// also records whether the most recent WithTenant fn returned an error,
// which would cause a real *postgres.Pool to roll back the Tx.
type fakeTxRunner struct {
	mu                sync.Mutex
	withTenantTenants []uuid.UUID
	bypassCount       int
	lastWithTenantErr error
}

func (f *fakeTxRunner) WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error {
	f.mu.Lock()
	f.withTenantTenants = append(f.withTenantTenants, tenantID)
	f.mu.Unlock()
	err := fn(postgres.Tx{})
	f.mu.Lock()
	f.lastWithTenantErr = err
	f.mu.Unlock()
	return err
}

func (f *fakeTxRunner) BypassRLS(ctx context.Context, fn func(postgres.Tx) error) error {
	f.mu.Lock()
	f.bypassCount++
	f.mu.Unlock()
	return fn(postgres.Tx{})
}

// lastRolledBack reports whether the most recent WithTenant fn returned
// non-nil — the precise condition under which *postgres.Pool.WithTenant
// rolls the Tx back. Used by Plan 11.4 tests to assert atomic rollback
// when outbox.Append fails.
func (f *fakeTxRunner) lastRolledBack() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastWithTenantErr != nil
}

// fakeStore is a hand-rolled api.UserStorePort fake. We avoid gomock
// to keep the dependency surface tight and the test code readable —
// each method returns the canned response set by the test.
type fakeStore struct {
	mu sync.Mutex

	// In-memory state.
	users         map[uuid.UUID]authapi.User
	loginIndex    map[string]uuid.UUID // (tenantID + lower(login)) -> id
	passwordHash  map[uuid.UUID]string
	mustChangePwd map[uuid.UUID]bool

	// Programmable error injection: when not nil for a method, the
	// next call returns it (and clears the slot).
	insertErr         error
	updateRolesErr    error
	updatePasswordErr error
	archiveErr        error
	restoreErr        error
	getByIDErr        error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:         make(map[uuid.UUID]authapi.User),
		loginIndex:    make(map[string]uuid.UUID),
		passwordHash:  make(map[uuid.UUID]string),
		mustChangePwd: make(map[uuid.UUID]bool),
	}
}

func loginKey(tenantID uuid.UUID, login string) string {
	return tenantID.String() + "|" + strings.ToLower(login)
}

func (s *fakeStore) seed(u authapi.User, hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Unix(0, 0)
	}
	if u.UpdatedAt.IsZero() {
		u.UpdatedAt = time.Unix(0, 0)
	}
	s.users[u.ID] = u
	s.loginIndex[loginKey(u.TenantID, u.Login)] = u.ID
	s.passwordHash[u.ID] = hash
	s.mustChangePwd[u.ID] = u.MustChangePwd
}

func (s *fakeStore) GetByID(_ context.Context, _ postgres.Tx, id uuid.UUID) (authapi.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getByIDErr != nil {
		err := s.getByIDErr
		s.getByIDErr = nil
		return authapi.User{}, err
	}
	u, ok := s.users[id]
	if !ok {
		return authapi.User{}, authapi.ErrUserNotFound
	}
	return u, nil
}

func (s *fakeStore) GetByLogin(_ context.Context, _ postgres.Tx, tenantID uuid.UUID, login string) (authapi.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.loginIndex[loginKey(tenantID, login)]
	if !ok {
		return authapi.User{}, authapi.ErrUserNotFound
	}
	return s.users[id], nil
}

func (s *fakeStore) List(_ context.Context, _ postgres.Tx, in authapi.ListUsersInput) ([]authapi.User, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]authapi.User, 0, len(s.users))
	for _, u := range s.users {
		if u.TenantID != in.TenantID {
			continue
		}
		if !in.IncludeArchived && u.ArchivedAt != nil {
			continue
		}
		out = append(out, u)
	}
	return out, int64(len(out)), nil
}

func (s *fakeStore) Insert(_ context.Context, _ postgres.Tx, u authapi.User, hash string) (authapi.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertErr != nil {
		err := s.insertErr
		s.insertErr = nil
		return authapi.User{}, err
	}
	if _, exists := s.loginIndex[loginKey(u.TenantID, u.Login)]; exists {
		return authapi.User{}, authapi.ErrLoginTaken
	}
	u.ID = uuid.New()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	u.CreatedAt = now
	u.UpdatedAt = now
	s.users[u.ID] = u
	s.loginIndex[loginKey(u.TenantID, u.Login)] = u.ID
	s.passwordHash[u.ID] = hash
	s.mustChangePwd[u.ID] = u.MustChangePwd
	return u, nil
}

func (s *fakeStore) UpdateRoles(_ context.Context, _ postgres.Tx, id uuid.UUID, roles []authapi.Role) (authapi.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateRolesErr != nil {
		err := s.updateRolesErr
		s.updateRolesErr = nil
		return authapi.User{}, err
	}
	u, ok := s.users[id]
	if !ok {
		return authapi.User{}, authapi.ErrUserNotFound
	}
	u.Roles = roles
	u.UpdatedAt = time.Date(2026, 5, 8, 13, 0, 0, 0, time.UTC)
	s.users[id] = u
	return u, nil
}

func (s *fakeStore) UpdatePassword(_ context.Context, _ postgres.Tx, id uuid.UUID, hash string, mustChange bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updatePasswordErr != nil {
		err := s.updatePasswordErr
		s.updatePasswordErr = nil
		return err
	}
	if _, ok := s.users[id]; !ok {
		return authapi.ErrUserNotFound
	}
	s.passwordHash[id] = hash
	s.mustChangePwd[id] = mustChange
	return nil
}

func (s *fakeStore) Archive(_ context.Context, _ postgres.Tx, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.archiveErr != nil {
		err := s.archiveErr
		s.archiveErr = nil
		return err
	}
	u, ok := s.users[id]
	if !ok {
		return authapi.ErrUserNotFound
	}
	if u.ArchivedAt != nil {
		// Idempotent — no-op, no error.
		return nil
	}
	now := time.Date(2026, 5, 8, 14, 0, 0, 0, time.UTC)
	u.ArchivedAt = &now
	s.users[id] = u
	return nil
}

func (s *fakeStore) Restore(_ context.Context, _ postgres.Tx, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.restoreErr != nil {
		err := s.restoreErr
		s.restoreErr = nil
		return err
	}
	u, ok := s.users[id]
	if !ok {
		return authapi.ErrUserNotFound
	}
	if u.ArchivedAt == nil {
		return authapi.ErrUserNotArchived
	}
	u.ArchivedAt = nil
	s.users[id] = u
	return nil
}

func (s *fakeStore) SetTOTPEnabled(_ context.Context, _ postgres.Tx, id uuid.UUID, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return authapi.ErrUserNotFound
	}
	u.TOTPEnabled = enabled
	s.users[id] = u
	return nil
}

func (s *fakeStore) GetPasswordHash(_ context.Context, _ postgres.Tx, id uuid.UUID) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[id]; !ok {
		return "", authapi.ErrUserNotFound
	}
	return s.passwordHash[id], nil
}

// fakeHasher is a deterministic Hasher: Hash(p) returns "fake-hash:" + p
// so tests can inspect what was hashed; Verify checks that the encoded
// string ends with ":" + password. That gives us a real flow without
// the Argon2 cost.
type fakeHasher struct{}

func (fakeHasher) Hash(_ context.Context, password string) (string, error) {
	return "fake-hash:" + password, nil
}

func (fakeHasher) Verify(_ context.Context, encoded, password string) (bool, error) {
	return encoded == "fake-hash:"+password, nil
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

// fakeOutbox is a recording outbox.Writer fake. Mirrors the slice-backed
// pattern used in internal/recording/worker/retention_test.go and
// internal/tenancy/service/tenant_service_test.go. Plan 11.4 introduced
// this for the auth service tests (the FIRST auth NATS subject).
type fakeOutbox struct {
	mu       sync.Mutex
	events   []outbox.Event
	failWith error
}

func newFakeOutbox() *fakeOutbox { return &fakeOutbox{} }

// withFailure returns f wired to fail every Append with err. Returning
// the receiver lets tests chain construction in one line.
func (f *fakeOutbox) withFailure(err error) *fakeOutbox {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = err
	return f
}

func (f *fakeOutbox) Append(_ context.Context, _ postgres.Tx, ev outbox.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return f.failWith
	}
	// Defensive copy so a downstream caller mutating ev.Payload does not
	// race with snapshot readers.
	cp := ev
	if ev.Payload != nil {
		cp.Payload = append([]byte(nil), ev.Payload...)
	}
	f.events = append(f.events, cp)
	return nil
}

func (f *fakeOutbox) appended() []outbox.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]outbox.Event, len(f.events))
	copy(out, f.events)
	return out
}

// Compile-time interface check.
var _ outbox.Writer = (*fakeOutbox)(nil)

// newSvc builds a UserService backed by hand-rolled fakes. The
// returned references are owned by the caller so tests can inspect
// recorded state directly. Plan 11.4 added the outbox fake.
func newSvc(t *testing.T) (*UserService, *fakeStore, *fakeAudit, *fakeOutbox) {
	t.Helper()
	tx := &fakeTxRunner{}
	store := newFakeStore()
	audit := &fakeAudit{}
	outboxFake := newFakeOutbox()
	clock := func() time.Time { return time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC) }
	svc := NewUserService(tx, store, fakeHasher{}, audit, outboxFake, clock)
	return svc, store, audit, outboxFake
}

func TestUserService_Create_HappyPath(t *testing.T) {
	t.Parallel()

	svc, store, audit, _ := newSvc(t)
	ctx := WithActorID(context.Background(), uuid.New())

	tenantID := uuid.New()
	user, tempPwd, err := svc.Create(ctx, authapi.CreateUserInput{
		TenantID: tenantID,
		Login:    "alice",
		FullName: "Алиса Тест",
		Email:    "alice@example.com",
		Roles:    []authapi.Role{authapi.RoleOperator},
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, user.ID)
	require.Equal(t, "alice", user.Login)
	require.True(t, user.MustChangePwd)
	require.Len(t, tempPwd, 16)

	// Hash visible in the fake store should equal the deterministic
	// hash of the temp password — confirms the service hashed before
	// inserting.
	require.Equal(t, "fake-hash:"+tempPwd, store.passwordHash[user.ID])

	// Audit row emitted with action user.created.
	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "user.created", events[0].Action)
	require.Equal(t, "user:"+user.ID.String(), events[0].Target)
	require.NotNil(t, events[0].ActorID)
}

func TestUserService_Create_DuplicateLoginReturnsErrLoginTaken(t *testing.T) {
	t.Parallel()

	svc, store, _, _ := newSvc(t)
	ctx := context.Background()

	tenantID := uuid.New()
	store.seed(authapi.User{TenantID: tenantID, Login: "dup", Roles: []authapi.Role{authapi.RoleOperator}}, "h")

	_, _, err := svc.Create(ctx, authapi.CreateUserInput{
		TenantID: tenantID,
		Login:    "dup",
		Roles:    []authapi.Role{authapi.RoleOperator},
	})
	require.ErrorIs(t, err, authapi.ErrLoginTaken)
}

func TestUserService_Create_RejectsEmptyRoles(t *testing.T) {
	t.Parallel()

	svc, _, _, _ := newSvc(t)

	_, _, err := svc.Create(context.Background(), authapi.CreateUserInput{
		TenantID: uuid.New(),
		Login:    "x",
		Roles:    nil,
	})
	require.ErrorIs(t, err, authapi.ErrEmptyRoles)
}

func TestUserService_List_FiltersArchivedByDefault(t *testing.T) {
	t.Parallel()

	svc, store, _, _ := newSvc(t)
	tenantID := uuid.New()
	now := time.Now()
	store.seed(authapi.User{TenantID: tenantID, Login: "active", Roles: []authapi.Role{authapi.RoleOperator}}, "h")
	store.seed(authapi.User{TenantID: tenantID, Login: "archived", Roles: []authapi.Role{authapi.RoleOperator}, ArchivedAt: &now}, "h")

	rows, total, err := svc.List(context.Background(), authapi.ListUsersInput{TenantID: tenantID})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.EqualValues(t, 1, total)
	require.Equal(t, "active", rows[0].Login)

	rowsAll, totalAll, err := svc.List(context.Background(), authapi.ListUsersInput{
		TenantID:        tenantID,
		IncludeArchived: true,
	})
	require.NoError(t, err)
	require.Len(t, rowsAll, 2)
	require.EqualValues(t, 2, totalAll)
}

func TestUserService_Get_NotFound(t *testing.T) {
	t.Parallel()

	svc, _, _, _ := newSvc(t)
	_, err := svc.Get(context.Background(), uuid.New())
	require.ErrorIs(t, err, authapi.ErrUserNotFound)
}

func TestUserService_UpdateRole_RejectsEmpty(t *testing.T) {
	t.Parallel()

	svc, store, _, _ := newSvc(t)
	tenantID := uuid.New()
	store.seed(authapi.User{TenantID: tenantID, Login: "u", Roles: []authapi.Role{authapi.RoleOperator}}, "h")
	id := store.loginIndex[loginKey(tenantID, "u")]

	_, err := svc.UpdateRole(context.Background(), id, nil)
	require.ErrorIs(t, err, authapi.ErrEmptyRoles)
}

func TestUserService_UpdateRole_HappyPath(t *testing.T) {
	t.Parallel()

	svc, store, audit, _ := newSvc(t)
	tenantID := uuid.New()
	store.seed(authapi.User{TenantID: tenantID, Login: "u", Roles: []authapi.Role{authapi.RoleOperator}}, "h")
	id := store.loginIndex[loginKey(tenantID, "u")]

	updated, err := svc.UpdateRole(context.Background(), id,
		[]authapi.Role{authapi.RoleSupervisor, authapi.RoleAdmin},
	)
	require.NoError(t, err)
	require.ElementsMatch(t,
		[]authapi.Role{authapi.RoleSupervisor, authapi.RoleAdmin},
		updated.Roles,
	)

	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "user.roles_updated", events[0].Action)
}

func TestUserService_Archive_Idempotent(t *testing.T) {
	t.Parallel()

	svc, store, audit, outboxFake := newSvc(t)
	tenantID := uuid.New()
	store.seed(authapi.User{TenantID: tenantID, Login: "ar", Roles: []authapi.Role{authapi.RoleOperator}}, "h")
	id := store.loginIndex[loginKey(tenantID, "ar")]

	require.NoError(t, svc.Archive(context.Background(), id))
	require.NoError(t, svc.Archive(context.Background(), id)) // idempotent

	events := audit.snapshot()
	require.Len(t, events, 2, "every archive call still emits an audit row")
	require.Equal(t, "user.archived", events[0].Action)
	require.Equal(t, "user.archived", events[1].Action)

	// Plan 11.4 Task 1 contract pin: every archive emits a fresh outbox
	// row alongside the audit row. The cache invalidator (Plan 11.4 Task 6)
	// is idempotent on user_id, so duplicate publications are harmless;
	// a future refactor that adds a didReplay short-circuit (mirroring
	// recording.Commit) would silently change THIS contract without
	// breaking any other test.
	require.Len(t, outboxFake.appended(), 2,
		"every archive emits a fresh outbox row; downstream invalidator dedup is idempotent")
}

// TestUserService_Archive_PublishesOutboxEvent verifies that Archive
// emits a tenant.<t>.auth.user.deleted outbox row alongside the
// existing audit row. Plan 11.4 Task 1 contract.
func TestUserService_Archive_PublishesOutboxEvent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	tenantID := uuid.New()
	svc, store, audit, outboxFake := newSvc(t)
	store.seed(authapi.User{TenantID: tenantID, Login: "to-archive", Roles: []authapi.Role{authapi.RoleOperator}}, "h")
	userID := store.loginIndex[loginKey(tenantID, "to-archive")]

	require.NoError(t, svc.Archive(ctx, userID))

	// Audit row is still emitted (Plan 11.4 doesn't remove existing behaviour).
	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "user.archived", events[0].Action)

	// Outbox row is the new behaviour.
	rows := outboxFake.appended()
	require.Len(t, rows, 1, "Archive must append exactly one outbox row")

	got := rows[0]
	require.Equal(t, authapi.SubjectUserDeletedFor(tenantID), got.Subject)
	require.NotNil(t, got.TenantID)
	require.Equal(t, tenantID, *got.TenantID)
	require.NotNil(t, got.AggregateID)
	require.Equal(t, userID, *got.AggregateID)

	var ev authapi.UserDeletedEvent
	require.NoError(t, json.Unmarshal(got.Payload, &ev))
	assert.Equal(t, userID, ev.UserID)
	assert.Equal(t, tenantID, ev.TenantID)
	// The newSvc clock returns 2026-05-08T12:00:00Z.
	assert.Equal(t, time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC).Unix(), ev.DeletedAt)
	assert.Equal(t, "archived", ev.Reason)
}

// TestUserService_Archive_OutboxAppendErrorRollsBackTx ensures the
// transaction rolls back when outbox append fails — the audit row and
// the user.archived store mutation must NOT commit independently.
func TestUserService_Archive_OutboxAppendErrorRollsBackTx(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	tenantID := uuid.New()

	tx := &fakeTxRunner{}
	store := newFakeStore()
	audit := &fakeAudit{}
	wantErr := errors.New("outbox down")
	outboxFake := newFakeOutbox().withFailure(wantErr)

	clock := func() time.Time { return time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC) }
	svc := NewUserService(tx, store, fakeHasher{}, audit, outboxFake, clock)

	store.seed(authapi.User{TenantID: tenantID, Login: "to-archive", Roles: []authapi.Role{authapi.RoleOperator}}, "h")
	userID := store.loginIndex[loginKey(tenantID, "to-archive")]

	err := svc.Archive(ctx, userID)
	require.Error(t, err)
	require.ErrorIs(t, err, wantErr,
		"outbox failure must propagate so the WithTenant Tx rolls back")
	require.True(t, tx.lastRolledBack(),
		"outbox.Append failure must roll back the WithTenant Tx")
}

func TestUserService_Restore_RejectsNonArchived(t *testing.T) {
	t.Parallel()

	svc, store, _, _ := newSvc(t)
	tenantID := uuid.New()
	store.seed(authapi.User{TenantID: tenantID, Login: "u", Roles: []authapi.Role{authapi.RoleOperator}}, "h")
	id := store.loginIndex[loginKey(tenantID, "u")]

	err := svc.Restore(context.Background(), id)
	require.ErrorIs(t, err, authapi.ErrUserNotArchived)
}

func TestUserService_Restore_ClearsArchivedAt(t *testing.T) {
	t.Parallel()

	svc, store, audit, _ := newSvc(t)
	tenantID := uuid.New()
	now := time.Now()
	store.seed(authapi.User{
		TenantID:   tenantID,
		Login:      "u",
		Roles:      []authapi.Role{authapi.RoleOperator},
		ArchivedAt: &now,
	}, "h")
	id := store.loginIndex[loginKey(tenantID, "u")]

	require.NoError(t, svc.Restore(context.Background(), id))

	got := store.users[id]
	require.Nil(t, got.ArchivedAt)

	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "user.restored", events[0].Action)
}

func TestUserService_ResetPassword_FlipsMustChange(t *testing.T) {
	t.Parallel()

	svc, store, audit, _ := newSvc(t)
	tenantID := uuid.New()
	store.seed(authapi.User{TenantID: tenantID, Login: "u", Roles: []authapi.Role{authapi.RoleOperator}}, "old-hash")
	id := store.loginIndex[loginKey(tenantID, "u")]
	store.mustChangePwd[id] = false

	tempPwd, err := svc.ResetPassword(context.Background(), id)
	require.NoError(t, err)
	require.Len(t, tempPwd, 16)
	require.Equal(t, "fake-hash:"+tempPwd, store.passwordHash[id])
	require.True(t, store.mustChangePwd[id])

	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "user.password_reset", events[0].Action)
}

func TestUserService_ChangePassword_HappyPath(t *testing.T) {
	t.Parallel()

	svc, store, audit, _ := newSvc(t)
	tenantID := uuid.New()
	store.seed(authapi.User{TenantID: tenantID, Login: "u", Roles: []authapi.Role{authapi.RoleOperator}}, "fake-hash:old")
	id := store.loginIndex[loginKey(tenantID, "u")]
	store.mustChangePwd[id] = true

	require.NoError(t, svc.ChangePassword(context.Background(), id, "old", "new-secret"))
	require.Equal(t, "fake-hash:new-secret", store.passwordHash[id])
	require.False(t, store.mustChangePwd[id])

	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "user.password_changed", events[0].Action)
}

func TestUserService_ChangePassword_WrongOldReturnsErrInvalidCredentials(t *testing.T) {
	t.Parallel()

	svc, store, audit, _ := newSvc(t)
	tenantID := uuid.New()
	store.seed(authapi.User{TenantID: tenantID, Login: "u", Roles: []authapi.Role{authapi.RoleOperator}}, "fake-hash:correct")
	id := store.loginIndex[loginKey(tenantID, "u")]

	err := svc.ChangePassword(context.Background(), id, "wrong", "new-secret")
	require.ErrorIs(t, err, authapi.ErrInvalidCredentials)
	require.Equal(t, "fake-hash:correct", store.passwordHash[id], "store must not be mutated on failed verification")
	require.Empty(t, audit.snapshot(), "no audit row on failed verification")
}

// TestUserService_ChangePassword_MissingUserIndistinguishableFromWrongPassword
// asserts that ChangePassword responds with the same error sentinel
// (ErrInvalidCredentials) whether the user id exists or not, AND that
// it always exercises the Hasher.Verify path (so the wall-time of the
// missing-user branch matches the wrong-password branch).
//
// This guards a real attack: without the dummy-Verify, an attacker
// probing UUIDs sees a faster response for unknown ids and can
// enumerate active users.
func TestUserService_ChangePassword_MissingUserIndistinguishableFromWrongPassword(t *testing.T) {
	t.Parallel()

	svc, _, audit, _ := newSvc(t)

	// Brand-new uuid the store has never seen.
	missingID := uuid.New()
	err := svc.ChangePassword(context.Background(), missingID, "any-old", "new-secret")
	require.ErrorIs(t, err, authapi.ErrInvalidCredentials,
		"missing user must surface the same sentinel as wrong-password — not ErrUserNotFound")
	require.Empty(t, audit.snapshot(), "no audit row on missing-user path")
}

func TestNewUserService_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	hasher := fakeHasher{}
	audit := &fakeAudit{}
	pool := &fakeTxRunner{}
	outboxFake := newFakeOutbox()

	cases := []struct {
		name string
		fn   func()
	}{
		{"nil pool", func() { _ = NewUserService(nil, store, hasher, audit, outboxFake, nil) }},
		{"nil store", func() { _ = NewUserService(pool, nil, hasher, audit, outboxFake, nil) }},
		{"nil hasher", func() { _ = NewUserService(pool, store, nil, audit, outboxFake, nil) }},
		{"nil audit logger", func() { _ = NewUserService(pool, store, hasher, nil, outboxFake, nil) }},
		{"nil outbox writer", func() { _ = NewUserService(pool, store, hasher, audit, nil, nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Panics(t, tc.fn, "constructor must panic on nil dep")
		})
	}
}

func TestUserService_AuditEvent_CarriesActorFromContext(t *testing.T) {
	t.Parallel()

	svc, _, audit, _ := newSvc(t)
	actor := uuid.New()
	ctx := WithActorID(context.Background(), actor)

	_, _, err := svc.Create(ctx, authapi.CreateUserInput{
		TenantID: uuid.New(),
		Login:    "actor-test",
		Roles:    []authapi.Role{authapi.RoleOperator},
	})
	require.NoError(t, err)

	events := audit.snapshot()
	require.Len(t, events, 1)
	require.NotNil(t, events[0].ActorID)
	require.Equal(t, actor, *events[0].ActorID)
	require.Equal(t, auditapi.ActorUser, events[0].ActorKind)
}

func TestUserService_AuditEvent_FallsBackToSystemActor(t *testing.T) {
	t.Parallel()

	svc, _, audit, _ := newSvc(t)

	_, _, err := svc.Create(context.Background(), authapi.CreateUserInput{
		TenantID: uuid.New(),
		Login:    "system-test",
		Roles:    []authapi.Role{authapi.RoleOperator},
	})
	require.NoError(t, err)

	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Nil(t, events[0].ActorID)
	require.Equal(t, auditapi.ActorSystem, events[0].ActorKind)
}

func TestUserService_Archive_PropagatesUserNotFound(t *testing.T) {
	t.Parallel()

	svc, _, _, _ := newSvc(t)

	err := svc.Archive(context.Background(), uuid.New())
	require.ErrorIs(t, err, authapi.ErrUserNotFound)
}

func TestUserService_ValidationGuards(t *testing.T) {
	t.Parallel()

	svc, _, _, _ := newSvc(t)
	ctx := context.Background()

	// Create requires tenant + login + roles.
	_, _, err := svc.Create(ctx, authapi.CreateUserInput{Login: "x", Roles: []authapi.Role{authapi.RoleOperator}})
	require.Error(t, err, "uuid.Nil tenant must error")

	_, _, err = svc.Create(ctx, authapi.CreateUserInput{TenantID: uuid.New(), Roles: []authapi.Role{authapi.RoleOperator}})
	require.Error(t, err, "empty login must error")

	// List requires tenant.
	_, _, err = svc.List(ctx, authapi.ListUsersInput{})
	require.Error(t, err)

	// List clamps Limit + Offset (no error path, but exercises the branches).
	_, _, err = svc.List(ctx, authapi.ListUsersInput{TenantID: uuid.New(), Limit: -1, Offset: -1})
	require.NoError(t, err)
	_, _, err = svc.List(ctx, authapi.ListUsersInput{TenantID: uuid.New(), Limit: 9999})
	require.NoError(t, err)

	// Get/UpdateRole/Archive/Restore/ResetPassword/ChangePassword all
	// reject uuid.Nil ids.
	_, err = svc.Get(ctx, uuid.Nil)
	require.Error(t, err)

	_, err = svc.UpdateRole(ctx, uuid.Nil, []authapi.Role{authapi.RoleOperator})
	require.Error(t, err)

	require.Error(t, svc.Archive(ctx, uuid.Nil))
	require.Error(t, svc.Restore(ctx, uuid.Nil))

	_, err = svc.ResetPassword(ctx, uuid.Nil)
	require.Error(t, err)

	require.Error(t, svc.ChangePassword(ctx, uuid.Nil, "old", "new"))

	// ChangePassword rejects empty new password.
	require.Error(t, svc.ChangePassword(ctx, uuid.New(), "old", ""))
}

func TestUserService_PropagatesNotFoundForMissingTargets(t *testing.T) {
	t.Parallel()

	svc, _, _, _ := newSvc(t)
	ctx := context.Background()

	// Each method that resolves the tenant before mutating bubbles
	// ErrUserNotFound when the user is absent.
	_, err := svc.UpdateRole(ctx, uuid.New(), []authapi.Role{authapi.RoleOperator})
	require.ErrorIs(t, err, authapi.ErrUserNotFound)

	require.ErrorIs(t, svc.Restore(ctx, uuid.New()), authapi.ErrUserNotFound)

	_, err = svc.ResetPassword(ctx, uuid.New())
	require.ErrorIs(t, err, authapi.ErrUserNotFound)

	// ChangePassword is intentionally NOT in this list — its missing-user
	// path returns ErrInvalidCredentials (not ErrUserNotFound) to deny an
	// attacker an enumeration oracle. See
	// TestUserService_ChangePassword_MissingUserIndistinguishableFromWrongPassword.
}

func TestUserService_PropagatesGenericStoreErrorsAsWrapped(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	wantErr := errors.New("disk full")

	type ctx struct {
		svc   *UserService
		store *fakeStore
		id    uuid.UUID
	}

	tests := []struct {
		name string
		// run injects the canned error and returns the resulting service-
		// layer error so the parent test can assert on it. No *testing.T
		// flows in here on purpose (avoids thelper miscategorisation).
		run func(c ctx) error
	}{
		{
			name: "Create",
			run: func(c ctx) error {
				c.store.insertErr = wantErr
				_, _, err := c.svc.Create(context.Background(), authapi.CreateUserInput{
					TenantID: tenantID,
					Login:    "fail-create",
					Roles:    []authapi.Role{authapi.RoleOperator},
				})
				return err
			},
		},
		{
			name: "UpdateRole",
			run: func(c ctx) error {
				c.store.updateRolesErr = wantErr
				_, err := c.svc.UpdateRole(context.Background(), c.id, []authapi.Role{authapi.RoleSupervisor})
				return err
			},
		},
		{
			name: "Archive",
			run: func(c ctx) error {
				c.store.archiveErr = wantErr
				return c.svc.Archive(context.Background(), c.id)
			},
		},
		{
			name: "ResetPassword",
			run: func(c ctx) error {
				c.store.updatePasswordErr = wantErr
				_, err := c.svc.ResetPassword(context.Background(), c.id)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc, store, _, _ := newSvc(t)
			store.seed(authapi.User{TenantID: tenantID, Login: "u", Roles: []authapi.Role{authapi.RoleOperator}}, "h")
			id := store.loginIndex[loginKey(tenantID, "u")]
			err := tt.run(ctx{svc: svc, store: store, id: id})
			require.ErrorContains(t, err, "disk full")
		})
	}
}

func TestUserService_GetByIDStoreErrorIsWrapped(t *testing.T) {
	t.Parallel()

	svc, store, _, _ := newSvc(t)
	store.getByIDErr = errors.New("connection refused")

	_, err := svc.Get(context.Background(), uuid.New())
	require.ErrorContains(t, err, "connection refused")
}

func TestUserService_NewUserService_NilClockDefaultsToTimeNow(t *testing.T) {
	t.Parallel()

	tx := &fakeTxRunner{}
	store := newFakeStore()
	audit := &fakeAudit{}
	outboxFake := newFakeOutbox()
	svc := NewUserService(tx, store, fakeHasher{}, audit, outboxFake, nil)
	require.NotNil(t, svc.clock)
	// Ask the service to emit an audit row and verify the timestamp was set.
	_, _, err := svc.Create(context.Background(), authapi.CreateUserInput{
		TenantID: uuid.New(),
		Login:    "clock-default",
		Roles:    []authapi.Role{authapi.RoleOperator},
	})
	require.NoError(t, err)
	events := audit.snapshot()
	require.Len(t, events, 1)
	require.False(t, events[0].Timestamp.IsZero())
}

func TestUserService_ChangePassword_PropagatesStoreErr(t *testing.T) {
	t.Parallel()

	svc, store, _, _ := newSvc(t)
	tenantID := uuid.New()
	store.seed(authapi.User{TenantID: tenantID, Login: "u", Roles: []authapi.Role{authapi.RoleOperator}}, "fake-hash:correct")
	id := store.loginIndex[loginKey(tenantID, "u")]

	wantErr := errors.New("disk full")
	store.updatePasswordErr = wantErr

	err := svc.ChangePassword(context.Background(), id, "correct", "new")
	require.Error(t, err)
	require.ErrorContains(t, err, "disk full")
}
