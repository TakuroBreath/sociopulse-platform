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
	"github.com/stretchr/testify/require"

	auditapi "github.com/sociopulse/platform/internal/audit/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/pkg/postgres"
)

// fakeRespondentStore is a hand-rolled api.RespondentStorePort fake.
// We avoid gomock so the test code stays readable and the dependency
// surface stays tight (mirrors fakeProjectStore in
// project_service_test.go).
type fakeRespondentStore struct {
	mu sync.Mutex

	rows map[uuid.UUID]crmapi.Respondent

	// indexes used by GetByHash. key = tenantID|projectID|hex(phoneHash)
	hashIndex map[string]uuid.UUID

	// dncBlocks[tenantID|projectID|hex(phoneHash)] = true means the
	// fake reports the phone hash as blocked.
	dncBlocks map[string]bool

	// programmable error injection — when not nil, the next
	// matching call returns it (and clears the slot).
	insertErr         error
	getByIDErr        error
	getByHashErr      error
	isBlockedDNCErr   error
	insertBatchErr    error
	existingHashesErr error
	softDeleteErr     error
	purgeErr          error
	searchErr         error

	// counters so tests can confirm the service short-circuited.
	insertCalls         int
	getByHashCalls      int
	isBlockedDNCCalls   int
	insertBatchCalls    int
	existingHashesCalls int
	softDeleteCalls     int
	purgeCalls          int
	searchCalls         int
}

func newFakeRespondentStore() *fakeRespondentStore {
	return &fakeRespondentStore{
		rows:      make(map[uuid.UUID]crmapi.Respondent),
		hashIndex: make(map[string]uuid.UUID),
		dncBlocks: make(map[string]bool),
	}
}

func respondentKey(tenantID, projectID uuid.UUID, phoneHash []byte) string {
	var b strings.Builder
	b.WriteString(tenantID.String())
	b.WriteByte('|')
	b.WriteString(projectID.String())
	b.WriteByte('|')
	for _, x := range phoneHash {
		b.WriteByte("0123456789abcdef"[x>>4])
		b.WriteByte("0123456789abcdef"[x&0xf])
	}
	return b.String()
}

func (s *fakeRespondentStore) Insert(_ context.Context, _ postgres.Tx, r crmapi.Respondent) (crmapi.Respondent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertCalls++
	if s.insertErr != nil {
		err := s.insertErr
		s.insertErr = nil
		return crmapi.Respondent{}, err
	}
	key := respondentKey(r.TenantID, r.ProjectID, r.PhoneHash)
	if _, dup := s.hashIndex[key]; dup {
		return crmapi.Respondent{}, crmapi.ErrDuplicateRespondent
	}
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	}
	if r.Status == "" {
		r.Status = crmapi.RespPending
	}
	if r.Source == "" {
		r.Source = crmapi.SourceImported
	}
	s.rows[r.ID] = r
	s.hashIndex[key] = r.ID
	return r, nil
}

func (s *fakeRespondentStore) GetByID(_ context.Context, _ postgres.Tx, id uuid.UUID) (crmapi.Respondent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getByIDErr != nil {
		err := s.getByIDErr
		s.getByIDErr = nil
		return crmapi.Respondent{}, err
	}
	row, ok := s.rows[id]
	if !ok {
		return crmapi.Respondent{}, crmapi.ErrRespondentNotFound
	}
	return row, nil
}

func (s *fakeRespondentStore) GetByHash(_ context.Context, _ postgres.Tx, tenantID, projectID uuid.UUID, phoneHash []byte) (crmapi.Respondent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getByHashCalls++
	if s.getByHashErr != nil {
		err := s.getByHashErr
		s.getByHashErr = nil
		return crmapi.Respondent{}, err
	}
	id, ok := s.hashIndex[respondentKey(tenantID, projectID, phoneHash)]
	if !ok {
		return crmapi.Respondent{}, crmapi.ErrRespondentNotFound
	}
	return s.rows[id], nil
}

func (s *fakeRespondentStore) IsBlockedDNC(_ context.Context, _ postgres.Tx, tenantID, projectID uuid.UUID, phoneHash []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.isBlockedDNCCalls++
	if s.isBlockedDNCErr != nil {
		err := s.isBlockedDNCErr
		s.isBlockedDNCErr = nil
		return false, err
	}
	return s.dncBlocks[respondentKey(tenantID, projectID, phoneHash)], nil
}

// InsertBatch satisfies api.RespondentStorePort.InsertBatch. The fake
// performs the same dedupe semantics as the real store (a duplicate in
// the supplied slice causes the entire batch to fail), so tests can
// assert the import service correctly pre-dedupes.
func (s *fakeRespondentStore) InsertBatch(_ context.Context, _ postgres.Tx, rows []crmapi.Respondent) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertBatchCalls++
	if s.insertBatchErr != nil {
		err := s.insertBatchErr
		s.insertBatchErr = nil
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	// Pre-validate the entire batch — CopyFrom is all-or-nothing.
	keys := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		k := respondentKey(r.TenantID, r.ProjectID, r.PhoneHash)
		if _, dup := seen[k]; dup {
			return 0, crmapi.ErrDuplicateRespondent
		}
		seen[k] = struct{}{}
		if _, dup := s.hashIndex[k]; dup {
			return 0, crmapi.ErrDuplicateRespondent
		}
		keys = append(keys, k)
	}
	for i, r := range rows {
		row := r
		if row.ID == uuid.Nil {
			row.ID = uuid.New()
		}
		if row.CreatedAt.IsZero() {
			row.CreatedAt = time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
		}
		if row.Status == "" {
			row.Status = crmapi.RespPending
		}
		if row.Source == "" {
			row.Source = crmapi.SourceImported
		}
		s.rows[row.ID] = row
		s.hashIndex[keys[i]] = row.ID
	}
	return len(rows), nil
}

// SoftDelete satisfies api.RespondentStorePort.SoftDelete. The fake
// stamps the supplied timestamp + reason on the row at id; a missing
// or already-deleted row returns ErrRespondentNotFound.
func (s *fakeRespondentStore) SoftDelete(_ context.Context, _ postgres.Tx, id uuid.UUID, reason string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.softDeleteErr != nil {
		err := s.softDeleteErr
		s.softDeleteErr = nil
		return err
	}
	s.softDeleteCalls++
	row, ok := s.rows[id]
	if !ok || row.DeleteAt != nil {
		return crmapi.ErrRespondentNotFound
	}
	t := at
	row.DeleteAt = &t
	if reason != "" {
		// store reason inside attributes for round-trip assertions
		// (production stores it in deletion_reason; the fake mirrors
		// its semantics without adding a column).
		if row.Attributes == nil {
			row.Attributes = map[string]any{}
		}
		row.Attributes["__deletion_reason"] = reason
	}
	s.rows[id] = row
	return nil
}

// PurgeOlderThan satisfies api.RespondentStorePort.PurgeOlderThan. The
// fake walks its in-memory rows, removes ones whose DeleteAt < cutoff
// (up to limit), and returns the affected ids.
func (s *fakeRespondentStore) PurgeOlderThan(_ context.Context, _ postgres.Tx, cutoff time.Time, limit int) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.purgeErr != nil {
		err := s.purgeErr
		s.purgeErr = nil
		return nil, err
	}
	s.purgeCalls++
	if limit <= 0 {
		return nil, nil
	}
	candidates := make([]uuid.UUID, 0)
	for id, row := range s.rows {
		if row.DeleteAt != nil && row.DeleteAt.Before(cutoff) {
			candidates = append(candidates, id)
			if len(candidates) == limit {
				break
			}
		}
	}
	for _, id := range candidates {
		row := s.rows[id]
		key := respondentKey(row.TenantID, row.ProjectID, row.PhoneHash)
		delete(s.hashIndex, key)
		delete(s.rows, id)
	}
	return candidates, nil
}

// Search satisfies api.RespondentStorePort.Search. Returns the
// in-memory rows matching the filter; pagination is honored.
func (s *fakeRespondentStore) Search(_ context.Context, _ postgres.Tx, f crmapi.SearchRespondentsFilter) ([]crmapi.Respondent, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.searchErr != nil {
		err := s.searchErr
		s.searchErr = nil
		return nil, 0, err
	}
	s.searchCalls++
	matches := make([]crmapi.Respondent, 0)
	for _, row := range s.rows {
		if row.TenantID != f.TenantID || row.ProjectID != f.ProjectID {
			continue
		}
		if row.DeleteAt != nil {
			continue
		}
		if f.Status != nil && row.Status != *f.Status {
			continue
		}
		if f.Region != "" && row.RegionCode != f.Region {
			continue
		}
		matches = append(matches, row)
	}
	total := int64(len(matches))
	limit := f.PageSize
	if limit <= 0 {
		limit = 50
	}
	offset := 0
	if f.Page > 1 {
		offset = (f.Page - 1) * limit
	}
	if offset >= len(matches) {
		return []crmapi.Respondent{}, total, nil
	}
	end := offset + limit
	if end > len(matches) {
		end = len(matches)
	}
	return matches[offset:end], total, nil
}

// ExistingHashes satisfies api.RespondentStorePort.ExistingHashes. The
// fake walks its in-memory hash index and returns the subset of hashes
// already present.
func (s *fakeRespondentStore) ExistingHashes(_ context.Context, _ postgres.Tx, tenantID, projectID uuid.UUID, hashes [][]byte) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.existingHashesCalls++
	if s.existingHashesErr != nil {
		err := s.existingHashesErr
		s.existingHashesErr = nil
		return nil, err
	}
	if len(hashes) == 0 {
		return nil, nil
	}
	out := make([][]byte, 0, len(hashes))
	for _, h := range hashes {
		k := respondentKey(tenantID, projectID, h)
		if _, ok := s.hashIndex[k]; ok {
			cp := make([]byte, len(h))
			copy(cp, h)
			out = append(out, cp)
		}
	}
	return out, nil
}

// fakeKMS is a no-op KMS resolver: every Encrypt call returns
// "enc:" + plaintext as a single byte slice. Tests assert on exact
// bytes so a real-world ciphertext envelope isn't necessary.
type fakeKMS struct {
	mu sync.Mutex

	encryptCalls int
	encryptErr   error
}

func (k *fakeKMS) EnsureKEK(_ context.Context, _ uuid.UUID) (string, error) {
	return "kek-test", nil
}

func (k *fakeKMS) GenerateDataKey(_ context.Context, _ uuid.UUID) (tenancyapi.DataKey, error) {
	return tenancyapi.DataKey{}, nil
}

func (k *fakeKMS) Encrypt(_ context.Context, _ uuid.UUID, plaintext []byte) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.encryptCalls++
	if k.encryptErr != nil {
		err := k.encryptErr
		k.encryptErr = nil
		return nil, err
	}
	out := make([]byte, 0, len(plaintext)+4)
	out = append(out, 'e', 'n', 'c', ':')
	out = append(out, plaintext...)
	return out, nil
}

func (k *fakeKMS) Decrypt(_ context.Context, _ uuid.UUID, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < 4 || string(ciphertext[:4]) != "enc:" {
		return nil, errors.New("fakeKMS: malformed ciphertext")
	}
	return ciphertext[4:], nil
}

func (k *fakeKMS) InvalidateCache(_ uuid.UUID) {}

// Compile-time assertion that *fakeKMS satisfies tenancyapi.KMSResolver
// — keeps the fake in sync if the interface ever changes.
var _ tenancyapi.KMSResolver = (*fakeKMS)(nil)

// fakePhoneHasher returns a deterministic per-tenant hash:
// sha256-like marker prefixed with tenant first byte + the phone bytes.
// We don't need real HMAC; the service only treats the result as opaque.
type fakePhoneHasher struct {
	mu sync.Mutex

	hashErr error
}

func (h *fakePhoneHasher) Hash(_ context.Context, tenantID uuid.UUID, phone string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hashErr != nil {
		err := h.hashErr
		h.hashErr = nil
		return nil, err
	}
	out := make([]byte, 0, 1+len(phone))
	out = append(out, tenantID[0])
	out = append(out, []byte(phone)...)
	return out, nil
}

func (h *fakePhoneHasher) Normalise(phone string) (string, error) {
	return phone, nil
}

// Compile-time assertion that *fakePhoneHasher satisfies the
// tenancyapi.PhoneHasher contract.
var _ tenancyapi.PhoneHasher = (*fakePhoneHasher)(nil)

// fakeRespondentTxRunner runs every fn synchronously with a zero
// postgres.Tx. Mirrors fakeTxRunner in project_service_test.go.
// RespondentService uses both WithTenant (write paths and per-tenant
// reads) and BypassRLS (resolve-by-id when the tenant is unknown).
type fakeRespondentTxRunner struct {
	mu                sync.Mutex
	withTenantTenants []uuid.UUID
	bypassCount       int
}

func (f *fakeRespondentTxRunner) WithTenant(_ context.Context, tenantID uuid.UUID, fn func(postgres.Tx) error) error {
	f.mu.Lock()
	f.withTenantTenants = append(f.withTenantTenants, tenantID)
	f.mu.Unlock()
	return fn(postgres.Tx{})
}

func (f *fakeRespondentTxRunner) BypassRLS(_ context.Context, fn func(postgres.Tx) error) error {
	f.mu.Lock()
	f.bypassCount++
	f.mu.Unlock()
	return fn(postgres.Tx{})
}

// fakeAuditLogger captures Write calls. Mirrors fakeAudit in the
// project tests but defined in this file so the test binary stays
// independent (each test file owns its fixtures).
type fakeRespondentAudit struct {
	mu     sync.Mutex
	events []auditapi.Event
}

func (a *fakeRespondentAudit) Write(_ context.Context, ev auditapi.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
	return nil
}

func (a *fakeRespondentAudit) snapshot() []auditapi.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]auditapi.Event, len(a.events))
	copy(out, a.events)
	return out
}

// newRespSvc wires a RespondentService with hand-rolled fakes; the
// returned references are owned by the caller so tests can inspect
// recorded state directly.
func newRespSvc(t *testing.T) (
	*RespondentService,
	*fakeRespondentTxRunner,
	*fakeRespondentStore,
	*fakeKMS,
	*fakePhoneHasher,
	*fakeRespondentAudit,
) {
	t.Helper()
	tx := &fakeRespondentTxRunner{}
	store := newFakeRespondentStore()
	kms := &fakeKMS{}
	hasher := &fakePhoneHasher{}
	audit := &fakeRespondentAudit{}
	clock := func() time.Time { return time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC) }
	svc := NewRespondentService(tx, store, kms, hasher, audit, clock)
	return svc, tx, store, kms, hasher, audit
}

// validRussianPhone is the canonical mobile number used across happy-
// path tests. The tests assert that the masked output and the audit
// payload never include this string.
const validRussianPhone = "+79161234567"

func TestRespondentService_Create_HappyPath(t *testing.T) {
	t.Parallel()

	svc, tx, store, kms, _, audit := newRespSvc(t)
	tenantID := uuid.New()
	projectID := uuid.New()
	actor := uuid.New()
	ctx := WithActorID(context.Background(), actor)

	got, err := svc.Create(ctx, crmapi.CreateRespondentInput{
		TenantID:  tenantID,
		ProjectID: projectID,
		Phone:     "8 (916) 123-45-67",
	})
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotEqual(t, uuid.Nil, got.ID)
	require.Equal(t, tenantID, got.TenantID)
	require.Equal(t, projectID, got.ProjectID)
	require.Equal(t, "RU", got.RegionCode)
	require.Equal(t, crmapi.RespPending, got.Status)
	require.Equal(t, crmapi.SourceImported, got.Source)
	require.NotNil(t, got.PhoneEncrypted)
	require.NotNil(t, got.PhoneHash)
	require.Empty(t, got.Phone, "Create must NOT populate plaintext Phone in the response")

	// WithTenant called with the supplied tenant id.
	require.Len(t, tx.withTenantTenants, 1)
	require.Equal(t, tenantID, tx.withTenantTenants[0])

	// One Encrypt call ran; one row written.
	require.Equal(t, 1, kms.encryptCalls)
	require.Len(t, store.rows, 1)

	// Audit row emitted: action + payload check; phone NEVER in payload.
	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "crm.respondent.created", events[0].Action)
	require.Equal(t, "respondent:"+got.ID.String(), events[0].Target)
	require.Equal(t, tenantID, events[0].TenantID)
	require.Equal(t, projectID, events[0].Payload["project_id"])
	require.Equal(t, "RU", events[0].Payload["region_code"])
	require.Equal(t, crmapi.SourceImported, events[0].Payload["source"])

	// Sanity: serialise the audit payload and confirm the canonical
	// phone never appears anywhere in the audit event (target,
	// payload, etc.).
	raw, err := json.Marshal(events[0])
	require.NoError(t, err)
	require.NotContains(t, string(raw), validRussianPhone, "audit row must not embed phone")
	require.NotContains(t, string(raw), "+79161234567", "audit row must not embed phone in any form")
	require.NotContains(t, string(raw), "9161234567", "audit row must not embed phone-suffix")
}

func TestRespondentService_Create_RejectsInvalidPhone(t *testing.T) {
	t.Parallel()

	svc, _, store, kms, _, audit := newRespSvc(t)
	tenantID := uuid.New()
	projectID := uuid.New()

	_, err := svc.Create(context.Background(), crmapi.CreateRespondentInput{
		TenantID:  tenantID,
		ProjectID: projectID,
		Phone:     "abc",
	})
	require.ErrorIs(t, err, crmapi.ErrInvalidPhone)

	// Nothing written, no audit, no KMS work.
	require.Empty(t, store.rows)
	require.Empty(t, audit.snapshot())
	require.Equal(t, 0, kms.encryptCalls)
	require.Equal(t, 0, store.isBlockedDNCCalls)
}

func TestRespondentService_Create_RejectsEmptyPhone(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	_, err := svc.Create(context.Background(), crmapi.CreateRespondentInput{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Phone:     "",
	})
	require.ErrorIs(t, err, crmapi.ErrInvalidPhone)
}

func TestRespondentService_Create_RejectsNilTenantID(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	_, err := svc.Create(context.Background(), crmapi.CreateRespondentInput{
		TenantID:  uuid.Nil,
		ProjectID: uuid.New(),
		Phone:     validRussianPhone,
	})
	require.ErrorIs(t, err, crmapi.ErrInvalidArgument)
}

func TestRespondentService_Create_RejectsNilProjectID(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	_, err := svc.Create(context.Background(), crmapi.CreateRespondentInput{
		TenantID:  uuid.New(),
		ProjectID: uuid.Nil,
		Phone:     validRussianPhone,
	})
	require.ErrorIs(t, err, crmapi.ErrInvalidArgument)
}

func TestRespondentService_Create_BlockedByDNCEmitsAuditAndShortCircuits(t *testing.T) {
	t.Parallel()

	svc, _, store, kms, hasher, audit := newRespSvc(t)
	tenantID := uuid.New()
	projectID := uuid.New()
	ctx := context.Background()

	// Pre-compute the hash the service will look up (fake hasher is
	// deterministic) and seed the DNC block.
	expectedHash, err := hasher.Hash(ctx, tenantID, validRussianPhone)
	require.NoError(t, err)
	store.dncBlocks[respondentKey(tenantID, projectID, expectedHash)] = true

	_, err = svc.Create(ctx, crmapi.CreateRespondentInput{
		TenantID:  tenantID,
		ProjectID: projectID,
		Phone:     validRussianPhone,
	})
	require.ErrorIs(t, err, crmapi.ErrPhoneInDNC)

	// No row written, no Encrypt call.
	require.Empty(t, store.rows)
	require.Equal(t, 0, kms.encryptCalls)
	require.Equal(t, 0, store.insertCalls)

	// Block-audit row emitted (PII-free).
	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "crm.respondent.create_blocked_dnc", events[0].Action)
	require.Equal(t, projectID, events[0].Payload["project_id"])

	raw, jerr := json.Marshal(events[0])
	require.NoError(t, jerr)
	require.NotContains(t, string(raw), validRussianPhone)
}

func TestRespondentService_Create_DuplicateInProject(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, audit := newRespSvc(t)
	tenantID := uuid.New()
	projectID := uuid.New()
	ctx := context.Background()

	// First Create succeeds.
	_, err := svc.Create(ctx, crmapi.CreateRespondentInput{
		TenantID:  tenantID,
		ProjectID: projectID,
		Phone:     validRussianPhone,
	})
	require.NoError(t, err)

	// Second Create with the same number -> ErrDuplicateRespondent.
	_, err = svc.Create(ctx, crmapi.CreateRespondentInput{
		TenantID:  tenantID,
		ProjectID: projectID,
		Phone:     "8 (916) 123-45-67", // same number, different formatting
	})
	require.ErrorIs(t, err, crmapi.ErrDuplicateRespondent)

	// Audit only has the first success row — no second row (per spec:
	// "no audit on dup"). The block-audit only fires for DNC.
	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "crm.respondent.created", events[0].Action)
}

func TestRespondentService_Create_KMSEncryptFailureWritesNoRow(t *testing.T) {
	t.Parallel()

	svc, _, store, kms, _, audit := newRespSvc(t)
	tenantID := uuid.New()
	projectID := uuid.New()

	kms.encryptErr = errors.New("kms unavailable")

	_, err := svc.Create(context.Background(), crmapi.CreateRespondentInput{
		TenantID:  tenantID,
		ProjectID: projectID,
		Phone:     validRussianPhone,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "kms unavailable")

	// Row NOT written. Audit has no "created" row.
	require.Empty(t, store.rows)
	for _, ev := range audit.snapshot() {
		require.NotEqual(t, "crm.respondent.created", ev.Action,
			"failed Create must not emit a created audit row")
	}
}

func TestRespondentService_Create_HasherFailureWritesNoRow(t *testing.T) {
	t.Parallel()

	svc, _, store, kms, hasher, audit := newRespSvc(t)
	hasher.hashErr = errors.New("hasher down")

	_, err := svc.Create(context.Background(), crmapi.CreateRespondentInput{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Phone:     validRussianPhone,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "hasher down")

	require.Empty(t, store.rows)
	require.Equal(t, 0, kms.encryptCalls)
	require.Empty(t, audit.snapshot())
}

func TestRespondentService_Create_DNCQueryFailurePropagates(t *testing.T) {
	t.Parallel()

	svc, _, store, kms, _, audit := newRespSvc(t)
	store.isBlockedDNCErr = errors.New("dnc query failed")

	_, err := svc.Create(context.Background(), crmapi.CreateRespondentInput{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Phone:     validRussianPhone,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "dnc query failed")

	require.Empty(t, store.rows)
	require.Equal(t, 0, kms.encryptCalls)
	require.Empty(t, audit.snapshot())
}

func TestRespondentService_Create_GetByHashErrorPropagates(t *testing.T) {
	t.Parallel()

	svc, _, store, kms, _, _ := newRespSvc(t)
	store.getByHashErr = errors.New("hash query failed")

	_, err := svc.Create(context.Background(), crmapi.CreateRespondentInput{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Phone:     validRussianPhone,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "hash query failed")

	require.Empty(t, store.rows)
	require.Equal(t, 0, kms.encryptCalls)
}

// TestRespondentService_Create_StoreInsertGenericFailurePropagates
// covers the post-Encrypt store.Insert failure path. A non-sentinel
// error from Insert (e.g. connection reset mid-transaction) must wrap
// up to "crm/service: create respondent: %w" — sentinel detection is
// reserved for the canonical sentinels.
func TestRespondentService_Create_StoreInsertGenericFailurePropagates(t *testing.T) {
	t.Parallel()

	svc, _, store, _, _, audit := newRespSvc(t)
	store.insertErr = errors.New("connection reset")

	_, err := svc.Create(context.Background(), crmapi.CreateRespondentInput{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Phone:     validRussianPhone,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "connection reset")

	require.Empty(t, store.rows)
	for _, ev := range audit.snapshot() {
		require.NotEqual(t, "crm.respondent.created", ev.Action)
	}
}

// TestRespondentService_Create_RejectsInvalidSource ensures the source
// validator fires before any I/O — inputs that aren't "imported" or
// "rdd" return ErrInvalidArgument without spending a hash/encrypt
// round-trip.
func TestRespondentService_Create_RejectsInvalidSource(t *testing.T) {
	t.Parallel()

	svc, _, store, kms, _, _ := newRespSvc(t)
	_, err := svc.Create(context.Background(), crmapi.CreateRespondentInput{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Phone:     validRussianPhone,
		Source:    "wonky",
	})
	require.ErrorIs(t, err, crmapi.ErrInvalidArgument)
	require.Empty(t, store.rows)
	require.Equal(t, 0, kms.encryptCalls)
}

// TestRespondentService_Create_AcceptsExplicitRegionCode covers the
// regionCode override branch — when the caller supplies a non-empty
// RegionCode, the service uses it instead of np.Region.
func TestRespondentService_Create_AcceptsExplicitRegionCode(t *testing.T) {
	t.Parallel()

	svc, _, store, _, _, audit := newRespSvc(t)
	got, err := svc.Create(context.Background(), crmapi.CreateRespondentInput{
		TenantID:   uuid.New(),
		ProjectID:  uuid.New(),
		Phone:      validRussianPhone,
		RegionCode: "ЦФО",
	})
	require.NoError(t, err)
	require.Equal(t, "ЦФО", got.RegionCode)
	require.Equal(t, "ЦФО", store.rows[got.ID].RegionCode)

	events := audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, "ЦФО", events[0].Payload["region_code"])
}

func TestRespondentService_Create_DefaultsSourceToImported(t *testing.T) {
	t.Parallel()

	svc, _, store, _, _, _ := newRespSvc(t)
	got, err := svc.Create(context.Background(), crmapi.CreateRespondentInput{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Phone:     validRussianPhone,
		// Source omitted on purpose.
	})
	require.NoError(t, err)
	require.Equal(t, crmapi.SourceImported, got.Source)
	require.Equal(t, crmapi.SourceImported, store.rows[got.ID].Source)
}

func TestRespondentService_Create_AcceptsExplicitRDDSource(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	got, err := svc.Create(context.Background(), crmapi.CreateRespondentInput{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Phone:     validRussianPhone,
		Source:    crmapi.SourceRDD,
	})
	require.NoError(t, err)
	require.Equal(t, crmapi.SourceRDD, got.Source)
}

// Mask is a sanity check on the masked-phone output: the response
// from Create must NOT carry the canonical E.164 in PhoneMasked.
func TestRespondentService_Create_PhoneMaskedDoesNotLeakPhone(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	got, err := svc.Create(context.Background(), crmapi.CreateRespondentInput{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Phone:     validRussianPhone,
	})
	require.NoError(t, err)
	require.NotEmpty(t, got.PhoneMasked)
	require.NotContains(t, got.PhoneMasked, "1234567",
		"masked phone must obscure subscriber digits")
}

func TestNewRespondentService_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()

	tx := &fakeRespondentTxRunner{}
	store := newFakeRespondentStore()
	kms := &fakeKMS{}
	hasher := &fakePhoneHasher{}
	audit := &fakeRespondentAudit{}

	cases := []struct {
		name string
		fn   func()
	}{
		{"nil tx runner", func() { _ = NewRespondentService(nil, store, kms, hasher, audit, nil) }},
		{"nil store", func() { _ = NewRespondentService(tx, nil, kms, hasher, audit, nil) }},
		{"nil kms", func() { _ = NewRespondentService(tx, store, nil, hasher, audit, nil) }},
		{"nil hasher", func() { _ = NewRespondentService(tx, store, kms, nil, audit, nil) }},
		{"nil audit", func() { _ = NewRespondentService(tx, store, kms, hasher, nil, nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Panics(t, tc.fn, "constructor must panic on nil dep")
		})
	}
}

func TestNewRespondentService_NilClockDefaultsToTimeNow(t *testing.T) {
	t.Parallel()

	tx := &fakeRespondentTxRunner{}
	store := newFakeRespondentStore()
	kms := &fakeKMS{}
	hasher := &fakePhoneHasher{}
	audit := &fakeRespondentAudit{}

	svc := NewRespondentService(tx, store, kms, hasher, audit, nil)
	require.NotNil(t, svc.clock)

	got, err := svc.Create(context.Background(), crmapi.CreateRespondentInput{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Phone:     validRussianPhone,
	})
	require.NoError(t, err)
	require.False(t, got.CreatedAt.IsZero())
}

// TestRespondentService_Get_HappyPath exercises Get on a freshly-
// created respondent: it must populate PhoneMasked from the decrypted
// ciphertext and never expose Phone / PhoneEncrypted / PhoneHash.
func TestRespondentService_Get_HappyPath(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	tenantID := uuid.New()
	projectID := uuid.New()
	ctx := context.Background()

	created, err := svc.Create(ctx, crmapi.CreateRespondentInput{
		TenantID:  tenantID,
		ProjectID: projectID,
		Phone:     validRussianPhone,
	})
	require.NoError(t, err)

	got, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
	require.Empty(t, got.Phone, "Get must NOT populate plaintext phone")
	require.NotEmpty(t, got.PhoneMasked)
	require.NotContains(t, got.PhoneMasked, "1234567")
	require.Nil(t, got.PhoneEncrypted, "Get must strip at-rest ciphertext")
	require.Nil(t, got.PhoneHash, "Get must strip at-rest hash")
}

// TestRespondentService_Get_RejectsNilID exercises the nil-id guard.
func TestRespondentService_Get_RejectsNilID(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	_, err := svc.Get(context.Background(), uuid.Nil)
	require.ErrorIs(t, err, crmapi.ErrInvalidArgument)
}

// TestRespondentService_Get_MissingReturnsErrRespondentNotFound covers
// the lookup-miss branch.
func TestRespondentService_Get_MissingReturnsErrRespondentNotFound(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	_, err := svc.Get(context.Background(), uuid.New())
	require.ErrorIs(t, err, crmapi.ErrRespondentNotFound)
}

// TestRespondentService_GetWithPhone_HappyPath exercises the admin
// PII-reveal path: the response carries the plaintext Phone AND a
// masked variant; one audit row "crm.respondent.read_pii" is emitted.
func TestRespondentService_GetWithPhone_HappyPath(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, audit := newRespSvc(t)
	tenantID := uuid.New()
	projectID := uuid.New()
	ctx := context.Background()

	created, err := svc.Create(ctx, crmapi.CreateRespondentInput{
		TenantID:  tenantID,
		ProjectID: projectID,
		Phone:     validRussianPhone,
	})
	require.NoError(t, err)

	got, err := svc.GetWithPhone(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, validRussianPhone, got.Phone)
	require.NotEmpty(t, got.PhoneMasked)

	events := audit.snapshot()
	// 1 created + 1 read_pii
	require.Len(t, events, 2)
	require.Equal(t, "crm.respondent.read_pii", events[1].Action)
	require.Equal(t, "respondent:"+created.ID.String(), events[1].Target)

	// audit payload must NOT contain the plaintext.
	raw, jerr := json.Marshal(events[1])
	require.NoError(t, jerr)
	require.NotContains(t, string(raw), validRussianPhone, "PII audit row must not embed plaintext")
}

// TestRespondentService_GetWithPhone_DeletedReturnsErrRespondentDeleted
// exercises the soft-deleted branch.
func TestRespondentService_GetWithPhone_DeletedReturnsErrRespondentDeleted(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	ctx := context.Background()

	created, err := svc.Create(ctx, crmapi.CreateRespondentInput{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Phone:     validRussianPhone,
	})
	require.NoError(t, err)

	_, err = svc.Delete(ctx, created.ID)
	require.NoError(t, err)

	_, err = svc.GetWithPhone(ctx, created.ID)
	require.ErrorIs(t, err, crmapi.ErrRespondentDeleted)
}

// TestRespondentService_Search_HappyPath inserts two respondents and
// asserts Search returns them with masked phones, total=2.
func TestRespondentService_Search_HappyPath(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	tenantID := uuid.New()
	projectID := uuid.New()
	ctx := context.Background()

	_, err := svc.Create(ctx, crmapi.CreateRespondentInput{
		TenantID:  tenantID,
		ProjectID: projectID,
		Phone:     "+79161234567",
	})
	require.NoError(t, err)
	_, err = svc.Create(ctx, crmapi.CreateRespondentInput{
		TenantID:  tenantID,
		ProjectID: projectID,
		Phone:     "+79261234567",
	})
	require.NoError(t, err)

	res, err := svc.Search(ctx, crmapi.SearchRespondentsFilter{
		TenantID:  tenantID,
		ProjectID: projectID,
		Page:      1,
		PageSize:  10,
	})
	require.NoError(t, err)
	require.Equal(t, 2, res.TotalCount)
	require.Len(t, res.Items, 2)
	for _, r := range res.Items {
		require.Empty(t, r.Phone, "search items must never carry plaintext")
		require.NotEmpty(t, r.PhoneMasked)
		require.Nil(t, r.PhoneEncrypted)
		require.Nil(t, r.PhoneHash)
	}
}

// TestRespondentService_Search_RejectsNilTenantID covers the validator.
func TestRespondentService_Search_RejectsNilTenantID(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	_, err := svc.Search(context.Background(), crmapi.SearchRespondentsFilter{
		ProjectID: uuid.New(),
	})
	require.ErrorIs(t, err, crmapi.ErrInvalidArgument)
}

// TestRespondentService_Search_RejectsNilProjectID covers the second
// branch of the validator.
func TestRespondentService_Search_RejectsNilProjectID(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	_, err := svc.Search(context.Background(), crmapi.SearchRespondentsFilter{
		TenantID: uuid.New(),
	})
	require.ErrorIs(t, err, crmapi.ErrInvalidArgument)
}

// TestRespondentService_Delete_HappyPath asserts DeletionRequest is
// returned with DeleteAt = now+30d and a "crm.respondent.deleted"
// audit row is emitted.
func TestRespondentService_Delete_HappyPath(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, audit := newRespSvc(t)
	tenantID := uuid.New()
	projectID := uuid.New()
	ctx := context.Background()

	created, err := svc.Create(ctx, crmapi.CreateRespondentInput{
		TenantID:  tenantID,
		ProjectID: projectID,
		Phone:     validRussianPhone,
	})
	require.NoError(t, err)

	dr, err := svc.Delete(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, dr.RespondentID)

	expected := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC).Add(deletionGracePeriod)
	require.True(t, dr.DeleteAt.Equal(expected),
		"DeleteAt must be 30d after the service clock; got %v want %v", dr.DeleteAt, expected)

	events := audit.snapshot()
	// 1 created + 1 deleted
	require.Len(t, events, 2)
	require.Equal(t, "crm.respondent.deleted", events[1].Action)
	require.Equal(t, "user_request", events[1].Payload["reason"])
}

// TestRespondentService_Delete_AlreadyDeletedReturnsErrRespondentDeleted
// asserts a second Delete on the same id is idempotent (returns the
// dedicated sentinel rather than ErrRespondentNotFound).
func TestRespondentService_Delete_AlreadyDeletedReturnsErrRespondentDeleted(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	ctx := context.Background()

	created, err := svc.Create(ctx, crmapi.CreateRespondentInput{
		TenantID:  uuid.New(),
		ProjectID: uuid.New(),
		Phone:     validRussianPhone,
	})
	require.NoError(t, err)

	_, err = svc.Delete(ctx, created.ID)
	require.NoError(t, err)

	_, err = svc.Delete(ctx, created.ID)
	require.ErrorIs(t, err, crmapi.ErrRespondentDeleted)
}

// TestRespondentService_Delete_RejectsNilID covers the nil-id guard.
func TestRespondentService_Delete_RejectsNilID(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	_, err := svc.Delete(context.Background(), uuid.Nil)
	require.ErrorIs(t, err, crmapi.ErrInvalidArgument)
}

// TestRespondentService_Delete_MissingReturnsNotFound covers the
// resolve-by-id missing branch.
func TestRespondentService_Delete_MissingReturnsNotFound(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	_, err := svc.Delete(context.Background(), uuid.New())
	require.ErrorIs(t, err, crmapi.ErrRespondentNotFound)
}

// TestRespondentService_StubbedImportMethodsReturnError keeps
// Import/GetImportStatus signatures honest for callers that don't
// configure the optional asynq deps.
func TestRespondentService_StubbedImportMethodsReturnError(t *testing.T) {
	t.Parallel()

	svc, _, _, _, _, _ := newRespSvc(t)
	ctx := context.Background()

	_, err := svc.Import(ctx, crmapi.ImportRequest{ProjectID: uuid.New()})
	require.Error(t, err)

	_, err = svc.GetImportStatus(ctx, "job-1")
	require.Error(t, err)
}
