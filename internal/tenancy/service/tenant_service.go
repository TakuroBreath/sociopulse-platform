package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// TxRunner is the cross-tenant transaction owner the service uses to
// co-locate state changes with transactional-outbox writes. *postgres.Pool
// satisfies this interface via its BypassRLS method; tests substitute an
// in-memory implementation that runs fn with a zero postgres.Tx.
type TxRunner interface {
	BypassRLS(ctx context.Context, fn func(postgres.Tx) error) error
}

// TenantService implements api.TenantService.
//
// All cross-tenant CRUD flows through this struct. Mutating operations open
// a single tenancy_admin transaction (via TxRunner.BypassRLS), perform the
// row write, append the lifecycle event to the transactional outbox, and
// log an audit row — all atomically. Direct NATS publishing remains for
// best-effort cache invalidations on read paths (Plan 04 Tasks 4+); the
// outbox is the durable path.
type TenantService struct {
	logger     *zap.Logger
	tx         TxRunner
	store      api.Store
	kms        api.KMSClient
	bucketProv api.BucketProvisioner
	pub        api.SettingsPublisher
	outbox     outbox.Writer
	auditWrite auditWriteFunc
}

// auditWriteFunc is the in-process audit hook. Until internal/audit/service
// exists (Plan 03 Task 7 / dedicated plan), tenancy logs audit rows via the
// process logger so the audit trail is observable without taking on a
// premature dependency. The signature mirrors the eventual audit.Logger
// interface so swapping is mechanical.
type auditWriteFunc func(ctx context.Context, tx postgres.Tx, ev auditEntry) error

// auditEntry is the in-tenancy audit-log shape. It mirrors the eventual
// internal/audit/api.Event but stays internal so we don't pin the audit
// surface from this distance.
type auditEntry struct {
	TenantID  uuid.UUID
	Action    string
	Target    string
	ActorKind string
	Payload   map[string]any
}

// Compile-time assertion: TenantService must satisfy api.TenantService.
var _ api.TenantService = (*TenantService)(nil)

// NewTenantService constructs a TenantService from already-built dependencies.
//
// The caller owns the lifecycle of every dependency; this service holds
// references only and does not close them on shutdown.
//
// bucketProv may be nil while a deployment hasn't wired Object Storage yet
// (e.g. early dev). Create logs a warning and proceeds without bucket
// provisioning when nil — the tenant lands in an "active, no recording bucket"
// state that the operator promotes via /admin/tenants/{id}/repair once
// storage is wired. In production wiring (module.go) the provisioner is
// always non-nil; the nil-tolerant constructor only exists for tests that
// don't exercise the bucket path.
func NewTenantService(
	logger *zap.Logger,
	tx TxRunner,
	store api.Store,
	kms api.KMSClient,
	bucketProv api.BucketProvisioner,
	pub api.SettingsPublisher,
	outboxWriter outbox.Writer,
) *TenantService {
	if outboxWriter == nil {
		outboxWriter = outbox.NewPostgresWriter()
	}
	return &TenantService{
		logger:     logger,
		tx:         tx,
		store:      store,
		kms:        kms,
		bucketProv: bucketProv,
		pub:        pub,
		outbox:     outboxWriter,
		auditWrite: stubAuditLog(logger),
	}
}

// stubAuditLog returns the default audit hook used until
// internal/audit/service lands. It logs at info level on the given logger;
// no row is written. The function intentionally accepts and ignores the
// postgres.Tx so the call-site already passes the right argument when the
// real audit Writer arrives.
func stubAuditLog(logger *zap.Logger) auditWriteFunc {
	l := logger.Named("audit-stub")
	return func(_ context.Context, _ postgres.Tx, ev auditEntry) error {
		l.Info("audit",
			zap.Stringer("tenant_id", ev.TenantID),
			zap.String("action", ev.Action),
			zap.String("target", ev.Target),
			zap.String("actor_kind", ev.ActorKind),
			zap.Any("payload", ev.Payload),
		)
		return nil
	}
}

// Create implements api.TenantService.Create.
//
// Order of operations:
//  1. Validate request (org_code, name).
//  2. Fast-fail on duplicate org_code BEFORE any external side effect.
//  3. Provision a per-tenant KEK in KMS (external; not in the DB tx).
//  4. Generate a 32-byte phone-hash pepper from crypto/rand.
//  5. Open a tenancy_admin transaction (BypassRLS):
//     - Insert tenant row.
//     - Append tenant.<id>.created to event_outbox.
//     - Write the audit row (currently a logger stub).
//     - Commit (BypassRLS commits on nil error).
//  6. Provision the per-tenant Object Storage bucket using the new tenant ID.
//     A failure here does NOT roll back the tenant: the tenant is left in a
//     "pending" state with ErrBucketProvisionPending wrapping the storage
//     error; operators retry via /admin/tenants/{id}/repair.
//  7. Persist the bucket name in tenant_settings (idempotent UPSERT) inside
//     a second BypassRLS tx so a subsequent storage-side rename is a one-row
//     update without re-provisioning.
//
// If step 5 fails after step 3 succeeded, the KEK is orphaned. We log a
// warning so an operator can clean up via the runbook; the request still
// surfaces an error.
func (s *TenantService) Create(ctx context.Context, req api.CreateTenantRequest) (api.Tenant, error) {
	if req.OrgCode == "" {
		return api.Tenant{}, fmt.Errorf("%w: org_code must be non-empty", api.ErrInvalidArgument)
	}
	if req.Name == "" {
		return api.Tenant{}, fmt.Errorf("%w: name must be non-empty", api.ErrInvalidArgument)
	}

	// Fast-fail on duplicate org_code (before creating a KMS KEK).
	existing, err := s.store.GetByOrgCode(ctx, req.OrgCode)
	switch {
	case errors.Is(err, api.ErrNotFound):
		// fall through to create
	case err != nil:
		return api.Tenant{}, fmt.Errorf("tenancy/service: get-by-org-code: %w", err)
	default:
		_ = existing
		return api.Tenant{}, fmt.Errorf("%w: org_code=%q", api.ErrAlreadyExists, req.OrgCode)
	}

	// Provision per-tenant KEK in Yandex KMS (external side effect; happens
	// before the DB tx because KMS.CreateKey is not transactional).
	kekName := "sociopulse-tenant-" + req.OrgCode
	kekDesc := "Per-tenant KEK for " + req.Name
	kekID, err := s.kms.CreateKey(ctx, kekName, kekDesc)
	if err != nil {
		s.logger.Warn("kms create-key failed",
			zap.String("org_code", req.OrgCode),
			zap.Error(err),
		)
		return api.Tenant{}, fmt.Errorf("%w: kms create-key: %w", api.ErrKMSUnavailable, err)
	}

	// Generate the per-tenant phone-hash pepper.
	pepper := make([]byte, 32)
	if _, err := rand.Read(pepper); err != nil {
		s.logger.Warn("orphan KEK created — manual cleanup required",
			zap.String("kek_id", kekID),
			zap.String("org_code", req.OrgCode),
			zap.Error(err),
		)
		return api.Tenant{}, fmt.Errorf("tenancy/service: rand pepper: %w", err)
	}

	t := api.Tenant{
		OrgCode:         req.OrgCode,
		Name:            req.Name,
		Status:          api.TenantStatusActive,
		KMSKEKID:        kekID,
		PhoneHashPepper: pepper,
	}
	if err := t.Validate(); err != nil {
		return api.Tenant{}, fmt.Errorf("tenancy/service: validate: %w", err)
	}

	var saved api.Tenant
	err = s.tx.BypassRLS(ctx, func(tx postgres.Tx) error {
		var err error
		saved, err = s.store.Insert(ctx, tx, t)
		if err != nil {
			return fmt.Errorf("insert tenant: %w", err)
		}
		if err := s.appendCreatedToOutbox(ctx, tx, saved); err != nil {
			return fmt.Errorf("outbox append: %w", err)
		}
		if err := s.auditWrite(ctx, tx, auditEntry{
			TenantID:  saved.ID,
			Action:    "tenancy.tenant.created",
			Target:    "tenant:" + saved.ID.String(),
			ActorKind: "service-owner",
			Payload: map[string]any{
				"org_code":   saved.OrgCode,
				"name":       saved.Name,
				"kms_kek_id": saved.KMSKEKID,
			},
		}); err != nil {
			return fmt.Errorf("audit log: %w", err)
		}
		return nil
	})
	if err != nil {
		s.logger.Warn("orphan KEK created — manual cleanup required",
			zap.String("kek_id", kekID),
			zap.String("org_code", req.OrgCode),
			zap.Error(err),
		)
		return api.Tenant{}, fmt.Errorf("tenancy/service: create tx: %w", err)
	}

	// Best-effort cache-invalidation publish OUTSIDE the tx (durability is
	// already covered by the outbox row). A failure here is logged but does
	// not unwind the database write.
	if pubErr := s.pub.PublishCreated(ctx, saved); pubErr != nil {
		s.logger.Warn("publish tenant.created failed",
			zap.Stringer("tenant_id", saved.ID),
			zap.Error(pubErr),
		)
	}

	bucketName, bucketErr := s.provisionAndPersistBucket(ctx, saved)
	saved.RecordingBucket = bucketName

	s.logger.Info("tenant created",
		zap.Stringer("tenant_id", saved.ID),
		zap.String("org_code", saved.OrgCode),
		zap.String("kek_id", saved.KMSKEKID),
		zap.String("recording_bucket", saved.RecordingBucket),
	)
	if bucketErr != nil {
		return saved, bucketErr
	}
	return saved, nil
}

// provisionAndPersistBucket runs the post-tenant-insert bucket flow:
// Provision the per-tenant Object Storage bucket, then persist the bucket
// name into tenant_settings. Both steps are best-effort: a failure leaves
// the tenant in the "pending" state with ErrBucketProvisionPending wrapping
// the provider error so /admin/tenants/{id}/repair can retry idempotently.
//
// Returns the bucket name (empty when provisioning failed) and the
// pending-state error, if any. When BucketProvisioner is nil (early-stage
// deployments without Object Storage wired), the function logs a warning
// and returns ("", nil) so the caller can return the tenant cleanly.
func (s *TenantService) provisionAndPersistBucket(ctx context.Context, saved api.Tenant) (string, error) {
	if s.bucketProv == nil {
		s.logger.Warn("bucket provisioner not wired; tenant created without recording bucket",
			zap.Stringer("tenant_id", saved.ID),
		)
		return "", nil
	}

	bucketName, err := s.bucketProv.Provision(ctx, saved.ID, saved.KMSKEKID)
	if err != nil {
		s.logger.Error("bucket provisioning failed; tenant left in pending",
			zap.Stringer("tenant_id", saved.ID),
			zap.String("kek_id", saved.KMSKEKID),
			zap.Error(err),
		)
		return "", fmt.Errorf("%w: %w", api.ErrBucketProvisionPending, err)
	}

	// Persist bucket name in tenant_settings (idempotent insert). A failure
	// here is the same kind of degraded state as a Provision failure — the
	// bucket exists but the platform cannot find it via the canonical
	// lookup. Surface ErrBucketProvisionPending so the operator retries
	// via /admin/tenants/{id}/repair.
	if err := s.tx.BypassRLS(ctx, func(tx postgres.Tx) error {
		return s.store.UpdateBucket(ctx, tx, saved.ID, bucketName)
	}); err != nil {
		s.logger.Error("persist bucket name failed; tenant left in pending",
			zap.Stringer("tenant_id", saved.ID),
			zap.String("bucket_name", bucketName),
			zap.Error(err),
		)
		return bucketName, fmt.Errorf("%w: persist bucket name: %w", api.ErrBucketProvisionPending, err)
	}
	return bucketName, nil
}

// appendCreatedToOutbox writes the tenant.<id>.created event to the
// transactional outbox using the caller's tx. The marshalled payload is the
// public api.Tenant DTO minus PhoneHashPepper (json:"-" handles that).
func (s *TenantService) appendCreatedToOutbox(ctx context.Context, tx postgres.Tx, t api.Tenant) error {
	payload, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("marshal tenant: %w", err)
	}
	tenantID := t.ID
	return s.outbox.Append(ctx, tx, outbox.Event{
		TenantID:    &tenantID,
		AggregateID: &tenantID,
		Subject:     api.SubjectTenantCreatedFor(t.ID),
		Payload:     payload,
	})
}

// Get implements api.TenantService.Get.
func (s *TenantService) Get(ctx context.Context, id uuid.UUID) (api.Tenant, error) {
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return api.Tenant{}, fmt.Errorf("tenancy/service: get: %w", err)
	}
	return t, nil
}

// GetByOrgCode implements api.TenantService.GetByOrgCode.
func (s *TenantService) GetByOrgCode(ctx context.Context, orgCode string) (api.Tenant, error) {
	t, err := s.store.GetByOrgCode(ctx, orgCode)
	if err != nil {
		return api.Tenant{}, fmt.Errorf("tenancy/service: get-by-org-code: %w", err)
	}
	return t, nil
}

// List implements api.TenantService.List, applying the documented
// limit/offset clamps before delegating to the store.
func (s *TenantService) List(ctx context.Context, filter api.ListTenantsFilter) ([]api.Tenant, error) {
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if filter.Limit > 500 {
		filter.Limit = 500
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	out, err := s.store.List(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("tenancy/service: list: %w", err)
	}
	return out, nil
}

// Suspend implements api.TenantService.Suspend. The status update, outbox
// append, and audit row commit atomically inside one tenancy_admin tx.
func (s *TenantService) Suspend(ctx context.Context, id uuid.UUID, reason string) error {
	return s.transitionStatus(ctx, id, api.TenantStatusSuspended,
		api.SubjectTenantSuspendedFor(id),
		api.TenantSuspendedEvent{TenantID: id, Reason: reason},
		"tenancy.tenant.suspended",
		map[string]any{"reason": reason},
		func(ctx context.Context) error { return s.pub.PublishSuspended(ctx, id) },
	)
}

// Resume implements api.TenantService.Resume.
func (s *TenantService) Resume(ctx context.Context, id uuid.UUID) error {
	return s.transitionStatus(ctx, id, api.TenantStatusActive,
		api.SubjectTenantResumedFor(id),
		api.TenantResumedEvent{TenantID: id},
		"tenancy.tenant.resumed",
		nil,
		nil, // SettingsPublisher has no PublishResumed; the outbox row covers durability.
	)
}

// Archive implements api.TenantService.Archive.
func (s *TenantService) Archive(ctx context.Context, id uuid.UUID) error {
	return s.transitionStatus(ctx, id, api.TenantStatusArchived,
		api.SubjectTenantArchivedFor(id),
		api.TenantArchivedEvent{TenantID: id},
		"tenancy.tenant.archived",
		nil,
		func(ctx context.Context) error { return s.pub.PublishArchived(ctx, id) },
	)
}

// tenantServiceWithKMS extends *TenantService with a KMSResolver hook so
// Suspend/Archive transitions invalidate the DEK cache atomically with the
// status change. The base service handles the DB write + outbox + audit; the
// wrapper layers the cache invalidation on top after a successful tx.
//
// Cache invalidation is best-effort and post-tx: a failure to drop the
// cache entry does not unwind the tenant status change. The cache will
// age out via TTL even if the call is missed.
type tenantServiceWithKMS struct {
	*TenantService
	kmsResolver api.KMSResolver
}

// NewTenantServiceWithKMS returns a TenantService that additionally calls
// KMSResolver.InvalidateCache after Suspend/Archive succeed. cmd/api should
// prefer this constructor when a resolver is available so a suspended
// tenant's DEKs leave the resolver's hot path immediately.
//
// Resume is intentionally NOT instrumented — the cache contents are still
// useful when a tenant returns to active. Create handles its own KEK
// provisioning via the embedded KMSClient and does not need cache hooks.
func NewTenantServiceWithKMS(
	logger *zap.Logger,
	tx TxRunner,
	store api.Store,
	kms api.KMSClient,
	bucketProv api.BucketProvisioner,
	pub api.SettingsPublisher,
	outboxWriter outbox.Writer,
	resolver api.KMSResolver,
) api.TenantService {
	base := NewTenantService(logger, tx, store, kms, bucketProv, pub, outboxWriter)
	if resolver == nil {
		return base
	}
	return &tenantServiceWithKMS{TenantService: base, kmsResolver: resolver}
}

// Suspend wraps the base Suspend and invalidates the DEK cache on success.
func (s *tenantServiceWithKMS) Suspend(ctx context.Context, id uuid.UUID, reason string) error {
	if err := s.TenantService.Suspend(ctx, id, reason); err != nil {
		return err
	}
	s.kmsResolver.InvalidateCache(id)
	return nil
}

// Archive wraps the base Archive and invalidates the DEK cache on success.
func (s *tenantServiceWithKMS) Archive(ctx context.Context, id uuid.UUID) error {
	if err := s.TenantService.Archive(ctx, id); err != nil {
		return err
	}
	s.kmsResolver.InvalidateCache(id)
	return nil
}

// Compile-time assertion: tenantServiceWithKMS must satisfy api.TenantService.
var _ api.TenantService = (*tenantServiceWithKMS)(nil)

// transitionStatus is the shared write path for Suspend/Resume/Archive.
// It opens one tenancy_admin tx that updates tenants.status, appends the
// canonical lifecycle event to the outbox, and writes an audit row. After
// the tx commits, it publishes a best-effort cache-invalidation event.
//
// pubFn may be nil when the SettingsPublisher has no matching method
// (e.g. Resume — peers consume the outbox-derived NATS event for durability).
func (s *TenantService) transitionStatus(
	ctx context.Context,
	id uuid.UUID,
	status api.TenantStatus,
	subject string,
	event any,
	auditAction string,
	auditPayload map[string]any,
	pubFn func(context.Context) error,
) error {
	err := s.tx.BypassRLS(ctx, func(tx postgres.Tx) error {
		if err := s.store.UpdateStatus(ctx, tx, id, status); err != nil {
			return fmt.Errorf("update status %s: %w", status, err)
		}

		payload, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("marshal event: %w", err)
		}
		tenantID := id
		if err := s.outbox.Append(ctx, tx, outbox.Event{
			TenantID:    &tenantID,
			AggregateID: &tenantID,
			Subject:     subject,
			Payload:     payload,
		}); err != nil {
			return fmt.Errorf("outbox append: %w", err)
		}
		if err := s.auditWrite(ctx, tx, auditEntry{
			TenantID:  id,
			Action:    auditAction,
			Target:    "tenant:" + id.String(),
			ActorKind: "service-owner",
			Payload:   auditPayload,
		}); err != nil {
			return fmt.Errorf("audit log: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("tenancy/service: transition status: %w", err)
	}

	if pubFn != nil {
		if err := pubFn(ctx); err != nil {
			s.logger.Warn("publish lifecycle event failed",
				zap.Stringer("tenant_id", id),
				zap.String("subject", subject),
				zap.Error(err),
			)
		}
	}
	s.logger.Info("tenant status transitioned",
		zap.Stringer("tenant_id", id),
		zap.String("status", string(status)),
	)
	return nil
}
