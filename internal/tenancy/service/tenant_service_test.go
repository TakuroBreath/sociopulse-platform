package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/service"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// fakeStore is a hand-rolled in-memory api.Store double used by the unit
// tests. The behaviour mimics the Postgres store: GetByOrgCode / Get return
// api.ErrNotFound when missing; Insert returns api.ErrAlreadyExists on a
// second insert with the same OrgCode.
//
// The Tx parameter is ignored by the fakes — service-level tests run
// without a real database; the value is wired by the test harness via
// service.NewTenantService's TxRunner indirection.
type fakeStore struct {
	mu sync.Mutex

	insertFn         func(ctx context.Context, tx postgres.Tx, t api.Tenant) (api.Tenant, error)
	getByOrgCodeFn   func(ctx context.Context, orgCode string) (api.Tenant, error)
	getFn            func(ctx context.Context, id uuid.UUID) (api.Tenant, error)
	listFn           func(ctx context.Context, f api.ListTenantsFilter) ([]api.Tenant, error)
	updateStatusFn   func(ctx context.Context, tx postgres.Tx, id uuid.UUID, s api.TenantStatus) error
	getPepperFn      func(ctx context.Context, tenantID uuid.UUID) ([]byte, error)
	getSettingFn     func(ctx context.Context, tenantID uuid.UUID, key string) (api.SettingValue, error)
	getAllSettingsFn func(ctx context.Context, tenantID uuid.UUID) (map[string]api.SettingValue, error)
	upsertSettingFn  func(ctx context.Context, tx postgres.Tx, tenantID uuid.UUID, key string, value api.SettingValue) error
	deleteSettingFn  func(ctx context.Context, tx postgres.Tx, tenantID uuid.UUID, key string) error
}

func (f *fakeStore) Insert(ctx context.Context, tx postgres.Tx, t api.Tenant) (api.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertFn != nil {
		return f.insertFn(ctx, tx, t)
	}
	return api.Tenant{}, errors.New("fakeStore.Insert not configured")
}

func (f *fakeStore) Get(ctx context.Context, id uuid.UUID) (api.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getFn != nil {
		return f.getFn(ctx, id)
	}
	return api.Tenant{}, errors.New("fakeStore.Get not configured")
}

func (f *fakeStore) GetByOrgCode(ctx context.Context, orgCode string) (api.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getByOrgCodeFn != nil {
		return f.getByOrgCodeFn(ctx, orgCode)
	}
	return api.Tenant{}, errors.New("fakeStore.GetByOrgCode not configured")
}

func (f *fakeStore) List(ctx context.Context, filter api.ListTenantsFilter) ([]api.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listFn != nil {
		return f.listFn(ctx, filter)
	}
	return nil, errors.New("fakeStore.List not configured")
}

func (f *fakeStore) UpdateStatus(ctx context.Context, tx postgres.Tx, id uuid.UUID, status api.TenantStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateStatusFn != nil {
		return f.updateStatusFn(ctx, tx, id, status)
	}
	return errors.New("fakeStore.UpdateStatus not configured")
}

func (f *fakeStore) GetPhoneHashPepper(ctx context.Context, tenantID uuid.UUID) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getPepperFn != nil {
		return f.getPepperFn(ctx, tenantID)
	}
	return nil, errors.New("fakeStore.GetPhoneHashPepper not configured")
}

func (f *fakeStore) GetSetting(ctx context.Context, tenantID uuid.UUID, key string) (api.SettingValue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getSettingFn != nil {
		return f.getSettingFn(ctx, tenantID, key)
	}
	return api.SettingValue{}, errors.New("fakeStore.GetSetting not configured")
}

func (f *fakeStore) GetAllSettings(ctx context.Context, tenantID uuid.UUID) (map[string]api.SettingValue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getAllSettingsFn != nil {
		return f.getAllSettingsFn(ctx, tenantID)
	}
	return nil, errors.New("fakeStore.GetAllSettings not configured")
}

func (f *fakeStore) UpsertSetting(ctx context.Context, tx postgres.Tx, tenantID uuid.UUID, key string, value api.SettingValue) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertSettingFn != nil {
		return f.upsertSettingFn(ctx, tx, tenantID, key, value)
	}
	return errors.New("fakeStore.UpsertSetting not configured")
}

func (f *fakeStore) DeleteSetting(ctx context.Context, tx postgres.Tx, tenantID uuid.UUID, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteSettingFn != nil {
		return f.deleteSettingFn(ctx, tx, tenantID, key)
	}
	return errors.New("fakeStore.DeleteSetting not configured")
}

// fakeKMS is a minimal api.KMSClient double. Only CreateKey is exercised by
// the unit tests; the other methods return an error when invoked so a
// drift between expectation and behaviour would surface immediately.
type fakeKMS struct {
	mu             sync.Mutex
	createKeyFn    func(ctx context.Context, name, description string) (string, error)
	createKeyCalls int
}

func (f *fakeKMS) CreateKey(ctx context.Context, name, description string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createKeyCalls++
	if f.createKeyFn != nil {
		return f.createKeyFn(ctx, name, description)
	}
	return "", errors.New("fakeKMS.CreateKey not configured")
}

func (f *fakeKMS) Encrypt(_ context.Context, _ string, _ []byte) ([]byte, string, error) {
	return nil, "", errors.New("fakeKMS.Encrypt not configured")
}

func (f *fakeKMS) Decrypt(_ context.Context, _ string, _ []byte) ([]byte, string, error) {
	return nil, "", errors.New("fakeKMS.Decrypt not configured")
}

func (f *fakeKMS) GenerateDataKey(_ context.Context, _ string) ([]byte, []byte, string, error) {
	return nil, nil, "", errors.New("fakeKMS.GenerateDataKey not configured")
}

// fakeTxRunner satisfies service.TxRunner without a real database. It
// invokes fn with a zero postgres.Tx and surfaces fn's error verbatim. The
// fakes the test wires never actually call methods on the Tx (they ignore
// it), so the zero value is safe.
type fakeTxRunner struct {
	mu       sync.Mutex
	calls    int
	errOn    error
	beforeFn func()
}

func (f *fakeTxRunner) BypassRLS(ctx context.Context, fn func(postgres.Tx) error) error {
	f.mu.Lock()
	if f.beforeFn != nil {
		f.beforeFn()
	}
	f.calls++
	if f.errOn != nil {
		f.mu.Unlock()
		return f.errOn
	}
	f.mu.Unlock()
	return fn(postgres.Tx{})
}

// fakeOutbox captures every Append call so tests can assert the canonical
// subject + payload shape.
type fakeOutbox struct {
	mu    sync.Mutex
	calls []outboxCall
	errOn error
}

type outboxCall struct {
	tenantID    *uuid.UUID
	aggregateID *uuid.UUID
	subject     string
	payload     []byte
}

func (f *fakeOutbox) Append(_ context.Context, _ postgres.Tx, ev outbox.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errOn != nil {
		return f.errOn
	}
	f.calls = append(f.calls, outboxCall{
		tenantID:    ev.TenantID,
		aggregateID: ev.AggregateID,
		subject:     ev.Subject,
		payload:     append([]byte(nil), ev.Payload...),
	})
	return nil
}

func (f *fakeOutbox) snapshot() []outboxCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]outboxCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakePublisher is a minimal api.SettingsPublisher double.
type fakePublisher struct {
	mu                  sync.Mutex
	publishCreatedFn    func(ctx context.Context, t api.Tenant) error
	publishSuspendedFn  func(ctx context.Context, tenantID uuid.UUID) error
	publishArchivedFn   func(ctx context.Context, tenantID uuid.UUID) error
	publishCreatedFor   []uuid.UUID
	publishSuspendedFor []uuid.UUID
	publishArchivedFor  []uuid.UUID
}

func (f *fakePublisher) PublishCreated(ctx context.Context, t api.Tenant) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishCreatedFor = append(f.publishCreatedFor, t.ID)
	if f.publishCreatedFn != nil {
		return f.publishCreatedFn(ctx, t)
	}
	return nil
}

func (f *fakePublisher) PublishSuspended(ctx context.Context, tenantID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishSuspendedFor = append(f.publishSuspendedFor, tenantID)
	if f.publishSuspendedFn != nil {
		return f.publishSuspendedFn(ctx, tenantID)
	}
	return nil
}

func (f *fakePublisher) PublishArchived(ctx context.Context, tenantID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishArchivedFor = append(f.publishArchivedFor, tenantID)
	if f.publishArchivedFn != nil {
		return f.publishArchivedFn(ctx, tenantID)
	}
	return nil
}

func (f *fakePublisher) PublishSettingUpdated(ctx context.Context, tenantID uuid.UUID, key string) error {
	return nil
}

func (f *fakePublisher) PublishSettingDeleted(ctx context.Context, tenantID uuid.UUID, key string) error {
	return nil
}

// ---------- Tests ----------

func TestTenantService_Create_HappyPath(t *testing.T) {
	t.Parallel()

	const orgCode = "CC-MOSKVA-01"
	const kekID = "yk-kek-tenant-abc"
	createdID := uuid.New()

	store := &fakeStore{
		getByOrgCodeFn: func(_ context.Context, code string) (api.Tenant, error) {
			require.Equal(t, orgCode, code)
			return api.Tenant{}, api.ErrNotFound
		},
		insertFn: func(_ context.Context, _ postgres.Tx, tn api.Tenant) (api.Tenant, error) {
			require.Equal(t, orgCode, tn.OrgCode)
			require.Equal(t, kekID, tn.KMSKEKID)
			require.Equal(t, api.TenantStatusActive, tn.Status)
			require.Len(t, tn.PhoneHashPepper, 32)
			tn.ID = createdID
			return tn, nil
		},
	}
	kms := &fakeKMS{
		createKeyFn: func(_ context.Context, name, _ string) (string, error) {
			require.Contains(t, name, orgCode)
			return kekID, nil
		},
	}
	pub := &fakePublisher{}
	tx := &fakeTxRunner{}
	ob := &fakeOutbox{}

	svc := service.NewTenantService(zaptest.NewLogger(t), tx, store, kms, pub, ob)
	tn, err := svc.Create(context.Background(), api.CreateTenantRequest{
		OrgCode: orgCode,
		Name:    "ВЦИОМ-Москва",
	})
	require.NoError(t, err)
	require.Equal(t, createdID, tn.ID)
	require.Equal(t, api.TenantStatusActive, tn.Status)
	require.Equal(t, kekID, tn.KMSKEKID)
	require.Equal(t, []uuid.UUID{createdID}, pub.publishCreatedFor)
	require.Equal(t, 1, kms.createKeyCalls)

	// One outbox event should land for the created tenant.
	calls := ob.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, api.SubjectTenantCreatedFor(createdID), calls[0].subject)
	require.NotNil(t, calls[0].tenantID)
	require.Equal(t, createdID, *calls[0].tenantID)
}

func TestTenantService_Create_RejectsDuplicateOrgCode(t *testing.T) {
	t.Parallel()

	existing := api.Tenant{ID: uuid.New(), OrgCode: "CC-MOSKVA-01"}
	store := &fakeStore{
		getByOrgCodeFn: func(_ context.Context, _ string) (api.Tenant, error) {
			return existing, nil
		},
	}
	kms := &fakeKMS{} // CreateKey must NOT be called.
	pub := &fakePublisher{}
	tx := &fakeTxRunner{}
	ob := &fakeOutbox{}

	svc := service.NewTenantService(zaptest.NewLogger(t), tx, store, kms, pub, ob)
	_, err := svc.Create(context.Background(), api.CreateTenantRequest{
		OrgCode: "CC-MOSKVA-01",
		Name:    "Dup",
	})
	require.ErrorIs(t, err, api.ErrAlreadyExists)
	require.Zero(t, kms.createKeyCalls, "KMS.CreateKey must not be invoked when org_code is already taken")
	require.Empty(t, ob.snapshot(), "outbox must not record a row when the duplicate check fails")
}

func TestTenantService_Create_RejectsEmptyOrgCode(t *testing.T) {
	t.Parallel()

	svc := service.NewTenantService(zaptest.NewLogger(t),
		&fakeTxRunner{}, &fakeStore{}, &fakeKMS{}, &fakePublisher{}, &fakeOutbox{})
	_, err := svc.Create(context.Background(), api.CreateTenantRequest{
		OrgCode: "",
		Name:    "X",
	})
	require.ErrorIs(t, err, api.ErrInvalidArgument)
}

func TestTenantService_Create_RejectsEmptyName(t *testing.T) {
	t.Parallel()

	svc := service.NewTenantService(zaptest.NewLogger(t),
		&fakeTxRunner{}, &fakeStore{}, &fakeKMS{}, &fakePublisher{}, &fakeOutbox{})
	_, err := svc.Create(context.Background(), api.CreateTenantRequest{
		OrgCode: "CC-X",
		Name:    "",
	})
	require.ErrorIs(t, err, api.ErrInvalidArgument)
}

func TestTenantService_Create_KMSFailureSurfacesAsKMSUnavailable(t *testing.T) {
	t.Parallel()

	store := &fakeStore{
		getByOrgCodeFn: func(_ context.Context, _ string) (api.Tenant, error) {
			return api.Tenant{}, api.ErrNotFound
		},
	}
	kms := &fakeKMS{
		createKeyFn: func(_ context.Context, _, _ string) (string, error) {
			return "", errors.New("kms timeout")
		},
	}
	svc := service.NewTenantService(zaptest.NewLogger(t),
		&fakeTxRunner{}, store, kms, &fakePublisher{}, &fakeOutbox{})
	_, err := svc.Create(context.Background(), api.CreateTenantRequest{
		OrgCode: "CC-NEW",
		Name:    "X",
	})
	require.ErrorIs(t, err, api.ErrKMSUnavailable)
}

func TestTenantService_Create_PropagatesInsertErrorAfterKEK(t *testing.T) {
	// If Insert fails after CreateKey succeeded, the KEK is orphaned. The
	// service still surfaces the error to the caller; cleanup is manual.
	t.Parallel()

	store := &fakeStore{
		getByOrgCodeFn: func(_ context.Context, _ string) (api.Tenant, error) {
			return api.Tenant{}, api.ErrNotFound
		},
		insertFn: func(_ context.Context, _ postgres.Tx, _ api.Tenant) (api.Tenant, error) {
			return api.Tenant{}, errors.New("pg: deadlock")
		},
	}
	kms := &fakeKMS{
		createKeyFn: func(_ context.Context, _, _ string) (string, error) {
			return "yk-kek-orphan", nil
		},
	}
	svc := service.NewTenantService(zaptest.NewLogger(t),
		&fakeTxRunner{}, store, kms, &fakePublisher{}, &fakeOutbox{})
	_, err := svc.Create(context.Background(), api.CreateTenantRequest{
		OrgCode: "CC-NEW",
		Name:    "X",
	})
	require.Error(t, err)
}

func TestTenantService_Create_OutboxFailureRollsBackTransaction(t *testing.T) {
	// When the outbox Append fails, the whole tx must be rolled back so the
	// tenant row never lands without its lifecycle event.
	t.Parallel()

	store := &fakeStore{
		getByOrgCodeFn: func(_ context.Context, _ string) (api.Tenant, error) {
			return api.Tenant{}, api.ErrNotFound
		},
		insertFn: func(_ context.Context, _ postgres.Tx, tn api.Tenant) (api.Tenant, error) {
			tn.ID = uuid.New()
			return tn, nil
		},
	}
	kms := &fakeKMS{
		createKeyFn: func(_ context.Context, _, _ string) (string, error) {
			return "yk-kek", nil
		},
	}
	ob := &fakeOutbox{errOn: errors.New("outbox down")}
	svc := service.NewTenantService(zaptest.NewLogger(t),
		&fakeTxRunner{}, store, kms, &fakePublisher{}, ob)

	_, err := svc.Create(context.Background(), api.CreateTenantRequest{OrgCode: "CC-X", Name: "X"})
	require.Error(t, err)
}

func TestTenantService_Get_DelegatesToStore(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	wanted := api.Tenant{ID: id, OrgCode: "CC-X", Name: "X", Status: api.TenantStatusActive}

	store := &fakeStore{
		getFn: func(_ context.Context, gotID uuid.UUID) (api.Tenant, error) {
			require.Equal(t, id, gotID)
			return wanted, nil
		},
	}
	svc := service.NewTenantService(zaptest.NewLogger(t), &fakeTxRunner{}, store, &fakeKMS{}, &fakePublisher{}, &fakeOutbox{})
	got, err := svc.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, wanted, got)
}

func TestTenantService_GetByOrgCode_DelegatesToStore(t *testing.T) {
	t.Parallel()

	wanted := api.Tenant{ID: uuid.New(), OrgCode: "CC-X", Name: "X", Status: api.TenantStatusActive}
	store := &fakeStore{
		getByOrgCodeFn: func(_ context.Context, code string) (api.Tenant, error) {
			require.Equal(t, "CC-X", code)
			return wanted, nil
		},
	}
	svc := service.NewTenantService(zaptest.NewLogger(t), &fakeTxRunner{}, store, &fakeKMS{}, &fakePublisher{}, &fakeOutbox{})
	got, err := svc.GetByOrgCode(context.Background(), "CC-X")
	require.NoError(t, err)
	require.Equal(t, wanted, got)
}

func TestTenantService_List_AppliesDefaultLimit(t *testing.T) {
	t.Parallel()

	var observed api.ListTenantsFilter
	store := &fakeStore{
		listFn: func(_ context.Context, f api.ListTenantsFilter) ([]api.Tenant, error) {
			observed = f
			return []api.Tenant{{ID: uuid.New()}}, nil
		},
	}
	svc := service.NewTenantService(zaptest.NewLogger(t), &fakeTxRunner{}, store, &fakeKMS{}, &fakePublisher{}, &fakeOutbox{})
	out, err := svc.List(context.Background(), api.ListTenantsFilter{}) // limit=0 → default
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, 50, observed.Limit, "default limit must be 50")
}

func TestTenantService_List_ClampsLimitTo500(t *testing.T) {
	t.Parallel()

	var observed api.ListTenantsFilter
	store := &fakeStore{
		listFn: func(_ context.Context, f api.ListTenantsFilter) ([]api.Tenant, error) {
			observed = f
			return nil, nil
		},
	}
	svc := service.NewTenantService(zaptest.NewLogger(t), &fakeTxRunner{}, store, &fakeKMS{}, &fakePublisher{}, &fakeOutbox{})
	_, err := svc.List(context.Background(), api.ListTenantsFilter{Limit: 9999})
	require.NoError(t, err)
	require.Equal(t, 500, observed.Limit, "limit must clamp to 500")
}

func TestTenantService_List_ClampsNegativeOffsetToZero(t *testing.T) {
	t.Parallel()

	var observed api.ListTenantsFilter
	store := &fakeStore{
		listFn: func(_ context.Context, f api.ListTenantsFilter) ([]api.Tenant, error) {
			observed = f
			return nil, nil
		},
	}
	svc := service.NewTenantService(zaptest.NewLogger(t), &fakeTxRunner{}, store, &fakeKMS{}, &fakePublisher{}, &fakeOutbox{})
	_, err := svc.List(context.Background(), api.ListTenantsFilter{Offset: -10})
	require.NoError(t, err)
	require.Equal(t, 0, observed.Offset)
}

func TestTenantService_Suspend_HappyPath(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	var (
		gotID     uuid.UUID
		gotStatus api.TenantStatus
	)
	store := &fakeStore{
		updateStatusFn: func(_ context.Context, _ postgres.Tx, x uuid.UUID, s api.TenantStatus) error {
			gotID = x
			gotStatus = s
			return nil
		},
	}
	pub := &fakePublisher{}
	ob := &fakeOutbox{}

	svc := service.NewTenantService(zaptest.NewLogger(t), &fakeTxRunner{}, store, &fakeKMS{}, pub, ob)
	require.NoError(t, svc.Suspend(context.Background(), id, "non-payment"))
	require.Equal(t, id, gotID)
	require.Equal(t, api.TenantStatusSuspended, gotStatus)
	require.Equal(t, []uuid.UUID{id}, pub.publishSuspendedFor)

	calls := ob.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, api.SubjectTenantSuspendedFor(id), calls[0].subject)
}

func TestTenantService_Suspend_PropagatesStoreError(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	store := &fakeStore{
		updateStatusFn: func(_ context.Context, _ postgres.Tx, _ uuid.UUID, _ api.TenantStatus) error {
			return api.ErrNotFound
		},
	}
	pub := &fakePublisher{}
	ob := &fakeOutbox{}

	svc := service.NewTenantService(zaptest.NewLogger(t), &fakeTxRunner{}, store, &fakeKMS{}, pub, ob)
	err := svc.Suspend(context.Background(), id, "")
	require.ErrorIs(t, err, api.ErrNotFound)
	require.Empty(t, pub.publishSuspendedFor, "publish must not run when store fails")
	require.Empty(t, ob.snapshot(), "outbox row must not land when store fails")
}

func TestTenantService_Resume_HappyPath(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	var gotStatus api.TenantStatus
	store := &fakeStore{
		updateStatusFn: func(_ context.Context, _ postgres.Tx, _ uuid.UUID, s api.TenantStatus) error {
			gotStatus = s
			return nil
		},
	}
	ob := &fakeOutbox{}
	svc := service.NewTenantService(zaptest.NewLogger(t), &fakeTxRunner{}, store, &fakeKMS{}, &fakePublisher{}, ob)
	require.NoError(t, svc.Resume(context.Background(), id))
	require.Equal(t, api.TenantStatusActive, gotStatus)

	calls := ob.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, api.SubjectTenantResumedFor(id), calls[0].subject)
}

func TestTenantService_Archive_HappyPath(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	var gotStatus api.TenantStatus
	store := &fakeStore{
		updateStatusFn: func(_ context.Context, _ postgres.Tx, _ uuid.UUID, s api.TenantStatus) error {
			gotStatus = s
			return nil
		},
	}
	pub := &fakePublisher{}
	ob := &fakeOutbox{}

	svc := service.NewTenantService(zaptest.NewLogger(t), &fakeTxRunner{}, store, &fakeKMS{}, pub, ob)
	require.NoError(t, svc.Archive(context.Background(), id))
	require.Equal(t, api.TenantStatusArchived, gotStatus)
	require.Equal(t, []uuid.UUID{id}, pub.publishArchivedFor)

	calls := ob.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, api.SubjectTenantArchivedFor(id), calls[0].subject)
}

func TestTenantService_ImplementsAPIInterface(t *testing.T) {
	t.Parallel()
	// Compile-time-style check: NewTenantService must return something that
	// satisfies api.TenantService. (The compile-time assertion lives next to
	// the struct in tenant_service.go; this test confirms the constructor
	// itself returns a value that can be assigned to the interface.)
	var _ api.TenantService = service.NewTenantService(zaptest.NewLogger(t),
		&fakeTxRunner{}, &fakeStore{}, &fakeKMS{}, &fakePublisher{}, &fakeOutbox{})
}

// fakeKMSResolverForCache is a minimal api.KMSResolver double that records
// InvalidateCache calls — the only method exercised by the
// TenantServiceWithKMS tests. Other methods return errors so any unexpected
// touch surfaces immediately.
type fakeKMSResolverForCache struct {
	mu          sync.Mutex
	invalidated []uuid.UUID
}

func (f *fakeKMSResolverForCache) EnsureKEK(_ context.Context, _ uuid.UUID) (string, error) {
	return "", errors.New("fakeKMSResolverForCache.EnsureKEK not configured")
}

func (f *fakeKMSResolverForCache) GenerateDataKey(_ context.Context, _ uuid.UUID) (api.DataKey, error) {
	return api.DataKey{}, errors.New("fakeKMSResolverForCache.GenerateDataKey not configured")
}

func (f *fakeKMSResolverForCache) Encrypt(_ context.Context, _ uuid.UUID, _ []byte) ([]byte, error) {
	return nil, errors.New("fakeKMSResolverForCache.Encrypt not configured")
}

func (f *fakeKMSResolverForCache) Decrypt(_ context.Context, _ uuid.UUID, _ []byte) ([]byte, error) {
	return nil, errors.New("fakeKMSResolverForCache.Decrypt not configured")
}

func (f *fakeKMSResolverForCache) InvalidateCache(tenantID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated = append(f.invalidated, tenantID)
}

func (f *fakeKMSResolverForCache) snapshot() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uuid.UUID(nil), f.invalidated...)
}

func TestTenantServiceWithKMS_Suspend_InvalidatesCache(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	store := &fakeStore{
		updateStatusFn: func(_ context.Context, _ postgres.Tx, _ uuid.UUID, _ api.TenantStatus) error {
			return nil
		},
	}
	resolver := &fakeKMSResolverForCache{}
	svc := service.NewTenantServiceWithKMS(zaptest.NewLogger(t),
		&fakeTxRunner{}, store, &fakeKMS{}, &fakePublisher{}, &fakeOutbox{}, resolver)

	require.NoError(t, svc.Suspend(context.Background(), id, "test"))
	require.Equal(t, []uuid.UUID{id}, resolver.snapshot(),
		"Suspend on the KMS-aware variant must call InvalidateCache for the tenant")
}

func TestTenantServiceWithKMS_Archive_InvalidatesCache(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	store := &fakeStore{
		updateStatusFn: func(_ context.Context, _ postgres.Tx, _ uuid.UUID, _ api.TenantStatus) error {
			return nil
		},
	}
	resolver := &fakeKMSResolverForCache{}
	svc := service.NewTenantServiceWithKMS(zaptest.NewLogger(t),
		&fakeTxRunner{}, store, &fakeKMS{}, &fakePublisher{}, &fakeOutbox{}, resolver)

	require.NoError(t, svc.Archive(context.Background(), id))
	require.Equal(t, []uuid.UUID{id}, resolver.snapshot(),
		"Archive on the KMS-aware variant must call InvalidateCache for the tenant")
}

func TestTenantServiceWithKMS_Suspend_DoesNotInvalidateOnFailure(t *testing.T) {
	// When the underlying Suspend fails, the cache must NOT be invalidated:
	// the tenant is still in its previous state, and dropping the DEK would
	// force unnecessary KMS round-trips on the next Encrypt.
	t.Parallel()

	id := uuid.New()
	store := &fakeStore{
		updateStatusFn: func(_ context.Context, _ postgres.Tx, _ uuid.UUID, _ api.TenantStatus) error {
			return api.ErrNotFound
		},
	}
	resolver := &fakeKMSResolverForCache{}
	svc := service.NewTenantServiceWithKMS(zaptest.NewLogger(t),
		&fakeTxRunner{}, store, &fakeKMS{}, &fakePublisher{}, &fakeOutbox{}, resolver)

	err := svc.Suspend(context.Background(), id, "")
	require.ErrorIs(t, err, api.ErrNotFound)
	require.Empty(t, resolver.snapshot(),
		"a failed Suspend must not invalidate the DEK cache")
}

func TestTenantServiceWithKMS_Resume_DoesNotInvalidate(t *testing.T) {
	// Resume restores a tenant to active; the DEK cache, if anything, is
	// still useful. We expect no InvalidateCache call.
	t.Parallel()

	id := uuid.New()
	store := &fakeStore{
		updateStatusFn: func(_ context.Context, _ postgres.Tx, _ uuid.UUID, _ api.TenantStatus) error {
			return nil
		},
	}
	resolver := &fakeKMSResolverForCache{}
	svc := service.NewTenantServiceWithKMS(zaptest.NewLogger(t),
		&fakeTxRunner{}, store, &fakeKMS{}, &fakePublisher{}, &fakeOutbox{}, resolver)

	require.NoError(t, svc.Resume(context.Background(), id))
	require.Empty(t, resolver.snapshot(),
		"Resume must not invalidate the DEK cache (tenant remains usable)")
}

func TestTenantServiceWithKMS_ImplementsAPIInterface(t *testing.T) {
	t.Parallel()
	var _ api.TenantService = service.NewTenantServiceWithKMS(zaptest.NewLogger(t),
		&fakeTxRunner{}, &fakeStore{}, &fakeKMS{}, &fakePublisher{}, &fakeOutbox{},
		&fakeKMSResolverForCache{})
}

// outbox.Writer is an interface; tests pass a fake. Keep a compile-time
// assertion to make schema drift surface immediately if Writer changes.
var _ outbox.Writer = (*fakeOutbox)(nil)
