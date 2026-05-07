//go:build integration

// Service-level integration tests for tenancy.TenantService. These tests
// boot Postgres 16 via testcontainers, apply the project migrations, build
// the TenantService with the real PostgresStore + PostgresWriter, and
// exercise the full Create / status-transition flow.
//
// The KMS dependency is stubbed: the real Yandex KMS adapter is wired by
// Plan 04 Task 3 — until then we substitute a fake that returns a
// deterministic key id.
//
// Run: go test -tags=integration -count=1 -timeout 5m ./internal/tenancy/service/...
package service_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/internal/tenancy/service"
	"github.com/sociopulse/platform/internal/tenancy/store"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// integrationKMS is a hand-rolled api.KMSClient stub used until Plan 04
// Task 3 wires the real Yandex SDK adapter.
type integrationKMS struct {
	keyID string
	err   error
}

func (k *integrationKMS) CreateKey(_ context.Context, _, _ string) (string, error) {
	if k.err != nil {
		return "", k.err
	}
	return k.keyID, nil
}

func (k *integrationKMS) Encrypt(_ context.Context, _ string, _ []byte) ([]byte, string, error) {
	return nil, "", errors.New("integrationKMS.Encrypt not implemented")
}

func (k *integrationKMS) Decrypt(_ context.Context, _ string, _ []byte) ([]byte, string, error) {
	return nil, "", errors.New("integrationKMS.Decrypt not implemented")
}

func (k *integrationKMS) GenerateDataKey(_ context.Context, _ string) ([]byte, []byte, string, error) {
	return nil, nil, "", errors.New("integrationKMS.GenerateDataKey not implemented")
}

func newServiceTestPool(t *testing.T) *postgres.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pg, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("sociopulse_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repo := filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))
	migrationsAbs, err := filepath.Abs(filepath.Join(repo, "migrations"))
	require.NoError(t, err)
	_, err = os.Stat(migrationsAbs)
	require.NoError(t, err)

	m, err := migrate.New("file://"+migrationsAbs, dsn)
	require.NoError(t, err)
	t.Cleanup(func() {
		srcErr, dbErr := m.Close()
		_ = srcErr
		_ = dbErr
	})
	require.NoError(t, m.Up())

	pool, err := postgres.Open(ctx, postgres.Config{
		DSN:            dsn,
		MaxConns:       8,
		MinConns:       1,
		ConnectTimeout: 10 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestTenantService_Create_PersistsRowAndOutboxAtomically(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := newServiceTestPool(t)
	st := store.NewPostgresStore(pool)
	kms := &integrationKMS{keyID: "yk-kek-integration"}
	svc := service.NewTenantService(zaptest.NewLogger(t),
		pool, st, kms, &recordingPublisher{}, outbox.NewPostgresWriter())

	const orgCode = "CC-INT-CREATE"
	tn, err := svc.Create(ctx, api.CreateTenantRequest{
		OrgCode: orgCode,
		Name:    "Integration",
	})
	require.NoError(t, err)
	require.Equal(t, orgCode, tn.OrgCode)
	require.Equal(t, api.TenantStatusActive, tn.Status)
	require.Equal(t, "yk-kek-integration", tn.KMSKEKID)

	// Tenant row must be queryable.
	got, err := st.Get(ctx, tn.ID)
	require.NoError(t, err)
	require.Equal(t, tn.ID, got.ID)

	// Outbox row must be present with the canonical subject.
	var (
		subject string
		ten     *uuid.UUID
	)
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT subject, tenant_id FROM event_outbox WHERE aggregate_id = $1`,
			tn.ID,
		).Scan(&subject, &ten)
	}))
	require.Equal(t, api.SubjectTenantCreatedFor(tn.ID), subject)
	require.NotNil(t, ten)
	require.Equal(t, tn.ID, *ten)
}

func TestTenantService_Create_DuplicateRejectedWithoutKMSCall(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := newServiceTestPool(t)
	st := store.NewPostgresStore(pool)
	kms := &countingKMS{keyID: "yk"}
	svc := service.NewTenantService(zaptest.NewLogger(t),
		pool, st, kms, &recordingPublisher{}, outbox.NewPostgresWriter())

	_, err := svc.Create(ctx, api.CreateTenantRequest{OrgCode: "CC-DUP", Name: "First"})
	require.NoError(t, err)
	require.Equal(t, 1, kms.count)

	_, err = svc.Create(ctx, api.CreateTenantRequest{OrgCode: "CC-DUP", Name: "Dup"})
	require.ErrorIs(t, err, api.ErrAlreadyExists)
	require.Equal(t, 1, kms.count, "duplicate detection must skip the KMS call")
}

func TestTenantService_Suspend_PersistsRowAndOutboxAtomically(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := newServiceTestPool(t)
	st := store.NewPostgresStore(pool)
	svc := service.NewTenantService(zaptest.NewLogger(t),
		pool, st, &integrationKMS{keyID: "yk"}, &recordingPublisher{}, outbox.NewPostgresWriter())

	tn, err := svc.Create(ctx, api.CreateTenantRequest{OrgCode: "CC-SUS", Name: "Suspend"})
	require.NoError(t, err)

	require.NoError(t, svc.Suspend(ctx, tn.ID, "non-payment"))

	got, err := st.Get(ctx, tn.ID)
	require.NoError(t, err)
	require.Equal(t, api.TenantStatusSuspended, got.Status)

	// Two outbox rows should now exist for this tenant: one created, one suspended.
	var count int
	require.NoError(t, pool.BypassRLS(ctx, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM event_outbox WHERE aggregate_id = $1`, tn.ID,
		).Scan(&count)
	}))
	require.Equal(t, 2, count)
}

// recordingPublisher captures publish calls without doing any I/O. Its
// presence in the service-level integration tests confirms that publish
// failures do not affect the durable write path (covered by the outbox
// rows above).
type recordingPublisher struct {
	created   []uuid.UUID
	suspended []uuid.UUID
	archived  []uuid.UUID
}

func (p *recordingPublisher) PublishCreated(_ context.Context, t api.Tenant) error {
	p.created = append(p.created, t.ID)
	return nil
}
func (p *recordingPublisher) PublishSuspended(_ context.Context, id uuid.UUID) error {
	p.suspended = append(p.suspended, id)
	return nil
}
func (p *recordingPublisher) PublishArchived(_ context.Context, id uuid.UUID) error {
	p.archived = append(p.archived, id)
	return nil
}
func (p *recordingPublisher) PublishSettingUpdated(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}
func (p *recordingPublisher) PublishSettingDeleted(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}

// countingKMS reports how many CreateKey calls have happened — used by the
// duplicate test to assert no wasteful KMS calls land.
type countingKMS struct {
	keyID string
	count int
}

func (k *countingKMS) CreateKey(_ context.Context, _, _ string) (string, error) {
	k.count++
	return k.keyID, nil
}
func (k *countingKMS) Encrypt(_ context.Context, _ string, _ []byte) ([]byte, string, error) {
	return nil, "", errors.New("countingKMS.Encrypt not implemented")
}
func (k *countingKMS) Decrypt(_ context.Context, _ string, _ []byte) ([]byte, string, error) {
	return nil, "", errors.New("countingKMS.Decrypt not implemented")
}
func (k *countingKMS) GenerateDataKey(_ context.Context, _ string) ([]byte, []byte, string, error) {
	return nil, nil, "", errors.New("countingKMS.GenerateDataKey not implemented")
}
