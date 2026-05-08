//go:build integration

// Integration tests for crm/store.RespondentStore against a real
// Postgres 16 instance booted via testcontainers-go. The tests apply
// the project's full migration set (through 000006 — which adds the
// unique constraint Insert relies on) and exercise the store's CRUD via
// *postgres.Pool.WithTenant so the RLS policy is in effect for every
// read/write.
//
// Run: go test -tags=integration -count=1 -timeout 5m -run Respondent ./internal/crm/store/...

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	crmapi "github.com/sociopulse/platform/internal/crm/api"
	"github.com/sociopulse/platform/internal/crm/store"
	"github.com/sociopulse/platform/pkg/postgres"
)

// seedProject inserts a projects row inside the supplied tenant via a
// per-tenant tx so RLS is satisfied. Returns the id of the new row.
// Mirrors the helper in project_store_test.go but kept local so the
// respondent integration tests don't take a hidden dependency on the
// project test file.
func seedProject(t *testing.T, ctx context.Context, pool *postgres.Pool, tenantID uuid.UUID, code string) uuid.UUID {
	t.Helper()
	const q = `
		INSERT INTO projects (tenant_id, code, name, status, target_count)
		VALUES ($1, $2, $3, 'active', 100)
		RETURNING id`

	var id uuid.UUID
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, q, tenantID, code, "Project "+code).Scan(&id)
	}))
	return id
}

// insertRespondent runs s.Insert inside a per-tenant tx and returns
// the saved row. Mirrors insertProject in the project tests.
func insertRespondent(t *testing.T, ctx context.Context, pool *postgres.Pool, s *store.RespondentStore, in crmapi.Respondent) crmapi.Respondent {
	t.Helper()
	var out crmapi.Respondent
	require.NoError(t, pool.WithTenant(ctx, in.TenantID, func(tx postgres.Tx) error {
		var err error
		out, err = s.Insert(ctx, tx, in)
		return err
	}))
	return out
}

// seedDNCEntry inserts a project_dnc row inside the supplied tenant's
// per-tenant tx (so RLS is satisfied). The `app` user has DML on
// project_dnc; the BypassRLS-via-tenancy_admin path does NOT (the role
// is granted DML only on tenants / tenant_settings per 000001_init).
// Tenant-wide rows (project_id NULL) still pass the RLS WITH CHECK
// because the policy only constrains tenant_id, not project_id.
func seedDNCEntry(t *testing.T, ctx context.Context, pool *postgres.Pool, tenantID uuid.UUID, projectID *uuid.UUID, phoneHash []byte, source string) {
	t.Helper()
	const q = `
		INSERT INTO project_dnc (tenant_id, project_id, phone_hash, source)
		VALUES ($1, $2, $3, $4)`

	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx, q, tenantID, projectID, phoneHash, source)
		return err
	}))
}

func TestRespondentStore_Insert_RoundTrip(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-RESP-1")
	projectID := seedProject(t, ctx, pool, tenantID, "RESP-PROJ-1")
	s := store.NewRespondentStore(pool)

	in := crmapi.Respondent{
		TenantID:       tenantID,
		ProjectID:      projectID,
		PhoneEncrypted: []byte{0x01, 0x02, 0x03, 0x04},
		PhoneHash:      []byte{0xaa, 0xbb, 0xcc, 0xdd},
		RegionCode:     "RU",
		Attributes:     map[string]any{"name": "Иванов"},
		Status:         crmapi.RespPending,
		Source:         crmapi.SourceImported,
	}

	saved := insertRespondent(t, ctx, pool, s, in)

	require.NotEqual(t, uuid.Nil, saved.ID)
	require.Equal(t, tenantID, saved.TenantID)
	require.Equal(t, projectID, saved.ProjectID)
	require.Equal(t, []byte{0x01, 0x02, 0x03, 0x04}, saved.PhoneEncrypted)
	require.Equal(t, []byte{0xaa, 0xbb, 0xcc, 0xdd}, saved.PhoneHash)
	require.Equal(t, "RU", saved.RegionCode)
	require.Equal(t, crmapi.RespPending, saved.Status)
	require.Equal(t, crmapi.SourceImported, saved.Source)
	require.False(t, saved.CreatedAt.IsZero())

	// GetByID returns the same row.
	var got crmapi.Respondent
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		got, err = s.GetByID(ctx, tx, saved.ID)
		return err
	}))
	require.Equal(t, saved.ID, got.ID)
	require.Equal(t, []byte{0xaa, 0xbb, 0xcc, 0xdd}, got.PhoneHash)
	require.Equal(t, "Иванов", got.Attributes["name"])
}

func TestRespondentStore_Insert_DuplicatePhoneHashReturnsErrDuplicate(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-RESP-DUP")
	projectID := seedProject(t, ctx, pool, tenantID, "RESP-PROJ-DUP")
	s := store.NewRespondentStore(pool)

	hash := []byte{0xde, 0xad, 0xbe, 0xef}

	insertRespondent(t, ctx, pool, s, crmapi.Respondent{
		TenantID:       tenantID,
		ProjectID:      projectID,
		PhoneEncrypted: []byte{0x11},
		PhoneHash:      hash,
		RegionCode:     "RU",
		Source:         crmapi.SourceImported,
	})

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.Insert(ctx, tx, crmapi.Respondent{
			TenantID:       tenantID,
			ProjectID:      projectID,
			PhoneEncrypted: []byte{0x22},
			PhoneHash:      hash,
			RegionCode:     "RU",
			Source:         crmapi.SourceImported,
		})
		return err
	})
	require.ErrorIs(t, err, crmapi.ErrDuplicateRespondent)
}

func TestRespondentStore_Insert_DuplicatePhoneHashAcrossProjectsAllowed(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-RESP-XPROJ")
	projA := seedProject(t, ctx, pool, tenantID, "RESP-PROJ-A")
	projB := seedProject(t, ctx, pool, tenantID, "RESP-PROJ-B")
	s := store.NewRespondentStore(pool)

	hash := []byte{0xab, 0xcd, 0xef, 0x01}

	// Same phone in two different projects within the same tenant — the
	// uniqueness key is (tenant_id, project_id, phone_hash), so this
	// MUST succeed for both inserts.
	insertRespondent(t, ctx, pool, s, crmapi.Respondent{
		TenantID:       tenantID,
		ProjectID:      projA,
		PhoneEncrypted: []byte{0xff},
		PhoneHash:      hash,
		RegionCode:     "RU",
		Source:         crmapi.SourceImported,
	})
	insertRespondent(t, ctx, pool, s, crmapi.Respondent{
		TenantID:       tenantID,
		ProjectID:      projB,
		PhoneEncrypted: []byte{0xff},
		PhoneHash:      hash,
		RegionCode:     "RU",
		Source:         crmapi.SourceImported,
	})
}

func TestRespondentStore_GetByID_MissingReturnsErrRespondentNotFound(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-RESP-MISS")
	s := store.NewRespondentStore(pool)

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.GetByID(ctx, tx, uuid.New())
		return err
	})
	require.ErrorIs(t, err, crmapi.ErrRespondentNotFound)
}

func TestRespondentStore_GetByHash_HappyPath(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-RESP-HASH")
	projectID := seedProject(t, ctx, pool, tenantID, "RESP-PROJ-H")
	s := store.NewRespondentStore(pool)

	hash := []byte{0x10, 0x20, 0x30, 0x40}
	saved := insertRespondent(t, ctx, pool, s, crmapi.Respondent{
		TenantID:       tenantID,
		ProjectID:      projectID,
		PhoneEncrypted: []byte{0x99},
		PhoneHash:      hash,
		RegionCode:     "RU",
		Source:         crmapi.SourceImported,
	})

	var got crmapi.Respondent
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		got, err = s.GetByHash(ctx, tx, tenantID, projectID, hash)
		return err
	}))
	require.Equal(t, saved.ID, got.ID)
}

func TestRespondentStore_GetByHash_MissingReturnsErrRespondentNotFound(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-RESP-HMISS")
	projectID := seedProject(t, ctx, pool, tenantID, "RESP-PROJ-HMISS")
	s := store.NewRespondentStore(pool)

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, err := s.GetByHash(ctx, tx, tenantID, projectID, []byte{0x00, 0x00})
		return err
	})
	require.ErrorIs(t, err, crmapi.ErrRespondentNotFound)
}

func TestRespondentStore_IsBlockedDNC_ProjectScopedHit(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-DNC-PROJ")
	projectID := seedProject(t, ctx, pool, tenantID, "DNC-PROJ-1")
	s := store.NewRespondentStore(pool)

	hash := []byte{0xbe, 0xef}
	pid := projectID
	seedDNCEntry(t, ctx, pool, tenantID, &pid, hash, "manual")

	var blocked bool
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		blocked, err = s.IsBlockedDNC(ctx, tx, tenantID, projectID, hash)
		return err
	}))
	require.True(t, blocked, "project-scoped DNC entry must be reported as blocked")
}

func TestRespondentStore_IsBlockedDNC_TenantWideHit(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-DNC-WIDE")
	projectID := seedProject(t, ctx, pool, tenantID, "DNC-PROJ-W")
	s := store.NewRespondentStore(pool)

	hash := []byte{0xfe, 0xed}
	// Tenant-wide entry — project_id NULL.
	seedDNCEntry(t, ctx, pool, tenantID, nil, hash, "tenant-wide")

	var blocked bool
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		blocked, err = s.IsBlockedDNC(ctx, tx, tenantID, projectID, hash)
		return err
	}))
	require.True(t, blocked, "tenant-wide DNC entry must apply to every project in the tenant")
}

func TestRespondentStore_IsBlockedDNC_NoMatchReturnsFalse(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-DNC-NONE")
	projectID := seedProject(t, ctx, pool, tenantID, "DNC-PROJ-N")
	s := store.NewRespondentStore(pool)

	var blocked bool
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		blocked, err = s.IsBlockedDNC(ctx, tx, tenantID, projectID, []byte{0x00})
		return err
	}))
	require.False(t, blocked)
}

func TestRespondentStore_InsertBatch_RoundTrip(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-RESP-COPY")
	projectID := seedProject(t, ctx, pool, tenantID, "RESP-PROJ-COPY")
	s := store.NewRespondentStore(pool)

	// Build 1500 respondents — exercises a batch larger than the
	// service-layer batch size to confirm pgx.CopyFrom handles it.
	const rowCount = 1500
	rows := make([]crmapi.Respondent, rowCount)
	for i := range rows {
		hash := make([]byte, 32)
		// distinct deterministic hashes
		hash[0] = byte(i & 0xff)
		hash[1] = byte((i >> 8) & 0xff)
		rows[i] = crmapi.Respondent{
			TenantID:       tenantID,
			ProjectID:      projectID,
			PhoneEncrypted: []byte{0xee, 0xee},
			PhoneHash:      hash,
			RegionCode:     "RU",
			Source:         crmapi.SourceImported,
		}
	}

	var inserted int
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		inserted, err = s.InsertBatch(ctx, tx, rows)
		return err
	}))
	require.Equal(t, rowCount, inserted)

	// ExistingHashes returns all the rows we just inserted.
	hashes := make([][]byte, rowCount)
	for i, r := range rows {
		hashes[i] = r.PhoneHash
	}
	var existing [][]byte
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		existing, err = s.ExistingHashes(ctx, tx, tenantID, projectID, hashes)
		return err
	}))
	require.Len(t, existing, rowCount)
}

func TestRespondentStore_InsertBatch_DuplicateInBatchFails(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-RESP-COPY-DUP")
	projectID := seedProject(t, ctx, pool, tenantID, "RESP-PROJ-COPY-DUP")
	s := store.NewRespondentStore(pool)

	hash := []byte{0xfe, 0xfe, 0xfe, 0xfe}
	rows := []crmapi.Respondent{
		{TenantID: tenantID, ProjectID: projectID, PhoneEncrypted: []byte{0x01}, PhoneHash: hash, RegionCode: "RU", Source: crmapi.SourceImported},
		{TenantID: tenantID, ProjectID: projectID, PhoneEncrypted: []byte{0x02}, PhoneHash: hash, RegionCode: "RU", Source: crmapi.SourceImported},
	}

	err := pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		_, ierr := s.InsertBatch(ctx, tx, rows)
		return ierr
	})
	require.ErrorIs(t, err, crmapi.ErrDuplicateRespondent)
}

func TestRespondentStore_ExistingHashes_ReturnsOnlyMatching(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-RESP-EH")
	projectID := seedProject(t, ctx, pool, tenantID, "RESP-PROJ-EH")
	s := store.NewRespondentStore(pool)

	present := []byte{0xa1, 0xa2}
	missing := []byte{0xb1, 0xb2}
	insertRespondent(t, ctx, pool, s, crmapi.Respondent{
		TenantID: tenantID, ProjectID: projectID, PhoneEncrypted: []byte{0x01}, PhoneHash: present, RegionCode: "RU", Source: crmapi.SourceImported,
	})

	var existing [][]byte
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		existing, err = s.ExistingHashes(ctx, tx, tenantID, projectID, [][]byte{present, missing})
		return err
	}))
	require.Len(t, existing, 1)
	require.Equal(t, present, existing[0])
}

func TestRespondentStore_ExistingHashes_EmptyInputReturnsNil(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-RESP-EH-EMPTY")
	projectID := seedProject(t, ctx, pool, tenantID, "RESP-PROJ-EH-EMPTY")
	s := store.NewRespondentStore(pool)

	var existing [][]byte
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		existing, err = s.ExistingHashes(ctx, tx, tenantID, projectID, nil)
		return err
	}))
	require.Empty(t, existing)
}

func TestRespondentStore_IsBlockedDNC_OtherProjectDoesNotBlock(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := newCRMTestPool(t)
	tenantID := seedTenant(t, ctx, pool, "CC-DNC-XPROJ")
	projA := seedProject(t, ctx, pool, tenantID, "DNC-X-A")
	projB := seedProject(t, ctx, pool, tenantID, "DNC-X-B")
	s := store.NewRespondentStore(pool)

	hash := []byte{0x55}
	pid := projA
	// Entry scoped to project A — must NOT block project B.
	seedDNCEntry(t, ctx, pool, tenantID, &pid, hash, "manual")

	var blocked bool
	require.NoError(t, pool.WithTenant(ctx, tenantID, func(tx postgres.Tx) error {
		var err error
		blocked, err = s.IsBlockedDNC(ctx, tx, tenantID, projB, hash)
		return err
	}))
	require.False(t, blocked, "project-A scoped DNC entry must not affect project B")
}
