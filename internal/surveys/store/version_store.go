package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// VersionStore is the Postgres-backed implementation of
// api.VersionStorePort.
//
// All methods accept a postgres.Tx so the surveys/service layer can
// co-locate the row writes with audit + outbox writes in one commit.
// The Activate flow in particular requires DeactivateAll + Activate +
// SetCurrentVersion to share a single transaction so the partial
// unique index (survey_versions_active_one) never observes two
// is_active=true rows for the same survey at any visibility horizon.
//
// Cross-module callers MUST import from internal/surveys/api only;
// depguard's module-boundaries rule rejects direct imports of this
// package from outside the surveys module.
type VersionStore struct {
	pool *postgres.Pool
}

// Compile-time assertion that *VersionStore satisfies
// api.VersionStorePort.
var _ api.VersionStorePort = (*VersionStore)(nil)

// NewVersionStore constructs a VersionStore. The pool reference is held
// for symmetry with the other stores — current methods all operate on
// the supplied Tx.
func NewVersionStore(pool *postgres.Pool) *VersionStore {
	return &VersionStore{pool: pool}
}

// versionColumns is the canonical projection used by every read query.
// version_label is intentionally NOT selected: the api.Version DTO
// projects from (major, minor) and the legacy column is kept only for
// backward compatibility with seed/test data.
const versionColumns = `id, tenant_id, survey_id, major, minor, schema,
	is_active, created_at, created_by, activated_at`

// versionRowScanner abstracts pgx.Row and a single pgx.Rows step so
// scanVersion can be reused.
type versionRowScanner interface {
	Scan(dest ...any) error
}

// scanVersion fills an api.Version from a single row. created_by is
// nullable in the DB (FK to users(id)); we scan into a *uuid.UUID and
// project nil to uuid.Nil so the DTO stays non-pointer. activated_at
// is nullable too — we keep it as *time.Time on the DTO.
//
// The api.Version DTO does not carry tenant_id, so the column read out
// of the row goes into a discard variable here.
func scanVersion(r versionRowScanner) (api.Version, error) {
	var (
		v         api.Version
		discardTI uuid.UUID
		createdBy *uuid.UUID
	)
	err := r.Scan(
		&v.ID, &discardTI, &v.SurveyID, &v.Major, &v.Minor, &v.Schema,
		&v.IsActive, &v.CreatedAt, &createdBy, &v.ActivatedAt,
	)
	if err != nil {
		return api.Version{}, err
	}
	if createdBy != nil {
		v.CreatedBy = *createdBy
	}
	return v, nil
}

// translateVersionErr maps pgx.ErrNoRows -> ErrVersionNotFound. Other
// errors flow through unchanged so the caller sees the raw pg error.
func translateVersionErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ErrVersionNotFound
	}
	return err
}

// Insert implements api.VersionStorePort.Insert. The supplied
// Version.ID is ignored — Postgres mints a fresh id. The version_label
// column (legacy, NOT NULL in 000001_init) is computed here from
// (major, minor) so the new + old representations stay in sync.
//
// IsActive is honored from the input. Most callers pass false and
// flip it later via Activate; tests that pre-seed an active row pass
// true directly.
//
// created_by is nullable: when v.CreatedBy is uuid.Nil we write NULL
// so the FK to users(id) is not violated by a synthetic zero UUID.
func (s *VersionStore) Insert(ctx context.Context, tx postgres.Tx, v api.Version) (api.Version, error) {
	const q = `
		INSERT INTO survey_versions (
			tenant_id, survey_id, major, minor, version_label,
			schema, is_active, created_by, activated_at
		) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9)
		RETURNING ` + versionColumns

	tenantID, err := s.tenantIDForSurvey(ctx, tx, v.SurveyID)
	if err != nil {
		return api.Version{}, err
	}
	label := strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor)

	var createdBy any
	if v.CreatedBy != uuid.Nil {
		createdBy = v.CreatedBy
	}

	saved, err := scanVersion(tx.QueryRow(ctx, q,
		tenantID,
		v.SurveyID,
		v.Major,
		v.Minor,
		label,
		string(v.Schema),
		v.IsActive,
		createdBy,
		v.ActivatedAt,
	))
	if err != nil {
		return api.Version{}, fmt.Errorf("surveys/store: insert version: %w", err)
	}
	return saved, nil
}

// tenantIDForSurvey reads the tenant_id of surveyID. Inside a per-
// tenant tx (Pool.WithTenant) RLS already restricts visibility to one
// tenant, so the read is just a denormalisation lookup so we can
// satisfy survey_versions.tenant_id NOT NULL.
func (s *VersionStore) tenantIDForSurvey(ctx context.Context, tx postgres.Tx, surveyID uuid.UUID) (uuid.UUID, error) {
	const q = `SELECT tenant_id FROM surveys WHERE id = $1`
	var tenantID uuid.UUID
	if err := tx.QueryRow(ctx, q, surveyID).Scan(&tenantID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, api.ErrNotFound
		}
		return uuid.Nil, fmt.Errorf("surveys/store: lookup survey tenant: %w", err)
	}
	return tenantID, nil
}

// GetByID implements api.VersionStorePort.GetByID.
func (s *VersionStore) GetByID(ctx context.Context, tx postgres.Tx, id uuid.UUID) (api.Version, error) {
	const q = `SELECT ` + versionColumns + ` FROM survey_versions WHERE id = $1`

	v, err := scanVersion(tx.QueryRow(ctx, q, id))
	if err != nil {
		if terr := translateVersionErr(err); errors.Is(terr, api.ErrVersionNotFound) {
			return api.Version{}, terr
		}
		return api.Version{}, fmt.Errorf("surveys/store: get version: %w", err)
	}
	return v, nil
}

// GetActive implements api.VersionStorePort.GetActive. Returns
// ErrNoActiveVersion when no row in survey_versions has
// is_active=true for surveyID.
func (s *VersionStore) GetActive(ctx context.Context, tx postgres.Tx, surveyID uuid.UUID) (api.Version, error) {
	const q = `
		SELECT ` + versionColumns + `
		FROM survey_versions
		WHERE survey_id = $1 AND is_active = true`

	v, err := scanVersion(tx.QueryRow(ctx, q, surveyID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return api.Version{}, api.ErrNoActiveVersion
		}
		return api.Version{}, fmt.Errorf("surveys/store: get active version: %w", err)
	}
	return v, nil
}

// List implements api.VersionStorePort.List. Versions are returned
// newest first by (major, minor) and then by created_at so callers see
// the most recently saved version at the top of the slice.
func (s *VersionStore) List(ctx context.Context, tx postgres.Tx, surveyID uuid.UUID) ([]api.Version, error) {
	const q = `
		SELECT ` + versionColumns + `
		FROM survey_versions
		WHERE survey_id = $1
		ORDER BY major DESC, minor DESC, created_at DESC`

	rows, err := tx.Query(ctx, q, surveyID)
	if err != nil {
		return nil, fmt.Errorf("surveys/store: list versions query: %w", err)
	}
	defer rows.Close()

	out := make([]api.Version, 0)
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("surveys/store: list versions scan: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("surveys/store: list versions iterate: %w", err)
	}
	return out, nil
}

// LatestMajor implements api.VersionStorePort.LatestMajor. Returns 0
// when no version exists yet (the first SaveVersion bumps to 1).
func (s *VersionStore) LatestMajor(ctx context.Context, tx postgres.Tx, surveyID uuid.UUID) (int, error) {
	const q = `
		SELECT COALESCE(max(major), 0)
		FROM survey_versions
		WHERE survey_id = $1`

	var max int
	if err := tx.QueryRow(ctx, q, surveyID).Scan(&max); err != nil {
		return 0, fmt.Errorf("surveys/store: latest major: %w", err)
	}
	return max, nil
}

// LatestMinor implements api.VersionStorePort.LatestMinor. Returns -1
// when no version exists for the supplied (surveyID, major) so the
// caller can distinguish "no minor yet" (first minor bump → 0) from
// "minor 0 already exists" (next is 1). Today both callers are inside
// SaveVersion which always increments by 1; the clear sentinel matters
// when LatestMinor grows new consumers.
func (s *VersionStore) LatestMinor(ctx context.Context, tx postgres.Tx, surveyID uuid.UUID, major int) (int, error) {
	const q = `
		SELECT COALESCE(max(minor), -1)
		FROM survey_versions
		WHERE survey_id = $1 AND major = $2`

	var max int
	if err := tx.QueryRow(ctx, q, surveyID, major).Scan(&max); err != nil {
		return 0, fmt.Errorf("surveys/store: latest minor: %w", err)
	}
	return max, nil
}

// DeactivateAll implements api.VersionStorePort.DeactivateAll. Sets
// is_active=false on every row of surveyID. Combined with the
// partial unique index `survey_versions_active_one`, this is the
// "step 1" of the Activate flow: deactivate first so the subsequent
// Activate INSERT/UPDATE doesn't violate the partial unique.
func (s *VersionStore) DeactivateAll(ctx context.Context, tx postgres.Tx, surveyID uuid.UUID) error {
	const q = `
		UPDATE survey_versions SET
			is_active = false
		WHERE survey_id = $1 AND is_active = true`

	if _, err := tx.Exec(ctx, q, surveyID); err != nil {
		return fmt.Errorf("surveys/store: deactivate all versions: %w", err)
	}
	return nil
}

// Activate implements api.VersionStorePort.Activate. Sets is_active=
// true on versionID and stamps activated_at=at. The caller MUST have
// already deactivated every other version of the survey (via
// DeactivateAll) in the same transaction; otherwise the partial unique
// index `survey_versions_active_one` raises 23505.
func (s *VersionStore) Activate(ctx context.Context, tx postgres.Tx, versionID uuid.UUID, at time.Time) error {
	const q = `
		UPDATE survey_versions SET
			is_active    = true,
			activated_at = $2
		WHERE id = $1`

	tag, err := tx.Exec(ctx, q, versionID, at)
	if err != nil {
		return fmt.Errorf("surveys/store: activate version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return api.ErrVersionNotFound
	}
	return nil
}
