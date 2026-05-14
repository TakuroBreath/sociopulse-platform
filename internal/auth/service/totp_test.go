package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/require"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
	"github.com/sociopulse/platform/pkg/passwords"
	"github.com/sociopulse/platform/pkg/postgres"
)

// ============================================================================
// Hand-rolled fakes
// ============================================================================

// fakeTOTPStore is an in-memory api.TOTPStore. It records every state
// transition so tests can assert on the exact sequence of writes.
type fakeTOTPStore struct {
	mu            sync.Mutex
	rows          map[uuid.UUID]authapi.TOTPState
	calls         []string
	upsErr        error
	getAnyErr     error
	confirmErr    error
	updLastVerErr error
	markBackupErr error
}

func newFakeTOTPStore() *fakeTOTPStore {
	return &fakeTOTPStore{rows: make(map[uuid.UUID]authapi.TOTPState)}
}

func (s *fakeTOTPStore) record(call string) {
	s.calls = append(s.calls, call)
}

func (s *fakeTOTPStore) Upsert(_ context.Context, _ postgres.Tx, userID, tenantID uuid.UUID, encSecret []byte, hashes []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("Upsert")
	if s.upsErr != nil {
		err := s.upsErr
		s.upsErr = nil
		return err
	}
	row := s.rows[userID]
	row.UserID = userID
	row.TenantID = tenantID
	row.SecretEncrypted = append([]byte(nil), encSecret...)
	row.Enrolled = false
	row.EnrolledAt = nil
	row.BackupCodeHashes = append([]string(nil), hashes...)
	row.BackupUsedCount = 0
	s.rows[userID] = row
	return nil
}

func (s *fakeTOTPStore) Get(_ context.Context, _ postgres.Tx, userID uuid.UUID) (authapi.TOTPState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("Get")
	row, ok := s.rows[userID]
	if !ok || !row.Enrolled {
		return authapi.TOTPState{}, authapi.ErrTOTPNotEnrolled
	}
	return row, nil
}

func (s *fakeTOTPStore) GetAny(_ context.Context, _ postgres.Tx, userID uuid.UUID) (authapi.TOTPState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("GetAny")
	if s.getAnyErr != nil {
		err := s.getAnyErr
		s.getAnyErr = nil
		return authapi.TOTPState{}, err
	}
	row, ok := s.rows[userID]
	if !ok {
		return authapi.TOTPState{}, authapi.ErrTOTPNotEnrolled
	}
	return row, nil
}

func (s *fakeTOTPStore) Confirm(_ context.Context, _ postgres.Tx, userID uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("Confirm")
	if s.confirmErr != nil {
		err := s.confirmErr
		s.confirmErr = nil
		return err
	}
	row, ok := s.rows[userID]
	if !ok {
		return authapi.ErrTOTPNotEnrolled
	}
	row.Enrolled = true
	stamped := at
	row.EnrolledAt = &stamped
	s.rows[userID] = row
	return nil
}

func (s *fakeTOTPStore) Delete(_ context.Context, _ postgres.Tx, userID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("Delete")
	delete(s.rows, userID)
	return nil
}

func (s *fakeTOTPStore) UpdateLastVerified(_ context.Context, _ postgres.Tx, userID uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("UpdateLastVerified")
	if s.updLastVerErr != nil {
		err := s.updLastVerErr
		s.updLastVerErr = nil
		return err
	}
	row, ok := s.rows[userID]
	if !ok {
		return authapi.ErrTOTPNotEnrolled
	}
	stamped := at
	row.LastVerifiedAt = &stamped
	s.rows[userID] = row
	return nil
}

func (s *fakeTOTPStore) MarkBackupUsed(_ context.Context, _ postgres.Tx, userID uuid.UUID, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("MarkBackupUsed")
	if s.markBackupErr != nil {
		err := s.markBackupErr
		s.markBackupErr = nil
		return err
	}
	row, ok := s.rows[userID]
	if !ok {
		return authapi.ErrTOTPNotEnrolled
	}
	out := row.BackupCodeHashes[:0]
	hit := false
	for _, h := range row.BackupCodeHashes {
		if !hit && h == hash {
			hit = true
			continue
		}
		out = append(out, h)
	}
	if !hit {
		return authapi.ErrTOTPInvalid
	}
	row.BackupCodeHashes = out
	row.BackupUsedCount++
	s.rows[userID] = row
	return nil
}

func (s *fakeTOTPStore) snapshot(userID uuid.UUID) (authapi.TOTPState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[userID]
	return r, ok
}

func (s *fakeTOTPStore) callsCopy() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	copy(out, s.calls)
	return out
}

// fakeUserStore is the narrow projection of api.UserStorePort the
// TOTPService needs.
type fakeUserStore struct {
	mu      sync.Mutex
	users   map[uuid.UUID]authapi.User
	totpSet map[uuid.UUID]bool
	calls   []string
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{
		users:   make(map[uuid.UUID]authapi.User),
		totpSet: make(map[uuid.UUID]bool),
	}
}

func (f *fakeUserStore) seed(u authapi.User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[u.ID] = u
}

func (f *fakeUserStore) GetByID(_ context.Context, _ postgres.Tx, id uuid.UUID) (authapi.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "GetByID")
	u, ok := f.users[id]
	if !ok {
		return authapi.User{}, authapi.ErrUserNotFound
	}
	return u, nil
}

func (f *fakeUserStore) SetTOTPEnabled(_ context.Context, _ postgres.Tx, id uuid.UUID, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("SetTOTPEnabled(%v)", enabled))
	if _, ok := f.users[id]; !ok {
		return authapi.ErrUserNotFound
	}
	f.totpSet[id] = enabled
	u := f.users[id]
	u.TOTPEnabled = enabled
	f.users[id] = u
	return nil
}

func (f *fakeUserStore) totpEnabled(id uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.totpSet[id]
}

// xorKMS is a fake KMSResolver that XORs plaintext with a fixed key. The
// resulting ciphertext is provably ≠ plaintext (assuming non-zero key
// and non-zero plaintext) so the at-rest encryption assertion in the
// test suite is meaningful. Decrypt reverses the operation. Tagged
// with a 1-byte header so we can distinguish "encrypted" bytes from a
// raw secret in test assertions.
type xorKMS struct {
	key byte
}

const xorKMSHeader byte = 0xAA

var _ tenancyapi.KMSResolver = (*xorKMS)(nil)

func (k *xorKMS) EnsureKEK(_ context.Context, _ uuid.UUID) (string, error) {
	return "fake-kek", nil
}

func (k *xorKMS) GenerateDataKey(_ context.Context, _ uuid.UUID) (tenancyapi.DataKey, error) {
	return tenancyapi.DataKey{}, errors.New("xorKMS: GenerateDataKey not used")
}

// Encrypt simulates an AAD-aware KMS: the (tenant, scope, rowID) tuple
// is hashed into the prefix so a Decrypt with mismatching args fails.
// This lets totp_test.go exercise scope-swap and row-swap defences at
// the service layer without dragging in real AES-GCM (Plan 13.2.5 Task 6).
func (k *xorKMS) Encrypt(_ context.Context, tenantID uuid.UUID, scope, rowID string, plaintext []byte) ([]byte, error) {
	tag := xorAADTag(tenantID, scope, rowID)
	out := make([]byte, 0, 1+len(tag)+len(plaintext))
	out = append(out, xorKMSHeader)
	out = append(out, tag...)
	for _, b := range plaintext {
		out = append(out, b^k.key)
	}
	return out, nil
}

func (k *xorKMS) Decrypt(_ context.Context, tenantID uuid.UUID, scope, rowID string, ciphertext []byte) ([]byte, error) {
	// Authentication-class failures wrap tenancyapi.ErrInvalidArgument so
	// callers can errors.Is against the sentinel — mirrors the production
	// KMSResolverImpl.Decrypt contract (returns "%w: aes-gcm open: %w"
	// wrapping tenancyapi.ErrInvalidArgument; see Plan 13.2.5 Task 6).
	if len(ciphertext) == 0 || ciphertext[0] != xorKMSHeader {
		return nil, fmt.Errorf("%w: xorKMS: bad header", tenancyapi.ErrInvalidArgument)
	}
	wantTag := xorAADTag(tenantID, scope, rowID)
	if len(ciphertext) < 1+len(wantTag) {
		return nil, fmt.Errorf("%w: xorKMS: ciphertext too short", tenancyapi.ErrInvalidArgument)
	}
	gotTag := ciphertext[1 : 1+len(wantTag)]
	for i, b := range gotTag {
		if b != wantTag[i] {
			return nil, fmt.Errorf("%w: xorKMS: AAD tag mismatch", tenancyapi.ErrInvalidArgument)
		}
	}
	body := ciphertext[1+len(wantTag):]
	out := make([]byte, len(body))
	for i, b := range body {
		out[i] = b ^ k.key
	}
	return out, nil
}

// xorAADTag returns a deterministic 16-byte digest of the AAD inputs.
// SHA-256 is overkill for tests but keeps the helper short.
func xorAADTag(tenantID uuid.UUID, scope, rowID string) []byte {
	h := sha256.New()
	h.Write(tenantID[:])
	h.Write([]byte{0x00})
	h.Write([]byte(scope))
	h.Write([]byte{0x00})
	h.Write([]byte(rowID))
	sum := h.Sum(nil)
	return sum[:16]
}

func (k *xorKMS) InvalidateCache(_ uuid.UUID) {}

// ============================================================================
// Test scaffolding
// ============================================================================

type totpFixture struct {
	svc   *TOTPService
	store *fakeTOTPStore
	users *fakeUserStore
	audit *fakeAudit
	kms   *xorKMS
	clock func() time.Time
	now   *time.Time
	user  authapi.User
}

// fixedClock returns a clock function and a settable time pointer. Tests
// advance the pointer to simulate skew without coupling to time.Now.
func fixedClock(start time.Time) (func() time.Time, *time.Time) {
	cur := start
	return func() time.Time { return cur }, &cur
}

// cheapBackupHasher uses Argon2id with the smallest valid cost. It is
// not a password-equivalent; backup codes are one-shot tokens.
func cheapBackupHasher() passwords.Hasher {
	return passwords.NewHasher(passwords.Params{
		Memory:      8,
		Iterations:  1,
		Parallelism: 1,
		SaltLength:  8,
		KeyLength:   16,
	})
}

func newTOTPFixture(t *testing.T) *totpFixture {
	t.Helper()
	store := newFakeTOTPStore()
	users := newFakeUserStore()
	audit := &fakeAudit{}
	kms := &xorKMS{key: 0x5A}
	clock, now := fixedClock(time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))

	user := authapi.User{
		ID:       uuid.New(),
		TenantID: uuid.New(),
		Login:    "alice",
	}
	users.seed(user)

	svc, err := NewTOTPService(TOTPDeps{
		Issuer:       "SocioPulse",
		Pool:         &fakeTxRunner{},
		Store:        store,
		Users:        users,
		KMS:          kms,
		BackupHasher: cheapBackupHasher(),
		Audit:        audit,
		Clock:        clock,
	})
	require.NoError(t, err)
	return &totpFixture{
		svc:   svc,
		store: store,
		users: users,
		audit: audit,
		kms:   kms,
		clock: clock,
		now:   now,
		user:  user,
	}
}

// ============================================================================
// Constructor validation
// ============================================================================

func TestNewTOTPService_RejectsMissingDeps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(d *TOTPDeps)
		want   string
	}{
		{"pool", func(d *TOTPDeps) { d.Pool = nil }, "Pool"},
		{"store", func(d *TOTPDeps) { d.Store = nil }, "Store"},
		{"users", func(d *TOTPDeps) { d.Users = nil }, "Users"},
		{"kms", func(d *TOTPDeps) { d.KMS = nil }, "KMS"},
		{"backup hasher", func(d *TOTPDeps) { d.BackupHasher = nil }, "BackupHasher"},
		{"audit", func(d *TOTPDeps) { d.Audit = nil }, "Audit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps := TOTPDeps{
				Pool:         &fakeTxRunner{},
				Store:        newFakeTOTPStore(),
				Users:        newFakeUserStore(),
				KMS:          &xorKMS{key: 1},
				BackupHasher: cheapBackupHasher(),
				Audit:        &fakeAudit{},
			}
			tc.mutate(&deps)
			_, err := NewTOTPService(deps)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// ============================================================================
// Plan-defined cases (Step 5)
// ============================================================================

// Case 1: Enroll returns 32-char base32 secret, otpauth URL, 10 backup codes.
func TestTOTPService_Enroll_ShapesAndPersists(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)

	// Secret: 20-byte random, base32-encoded -> 32 chars (no padding).
	require.Len(t, enroll.Secret, 32)
	// otpauth URL carries our issuer + login.
	require.True(t, strings.HasPrefix(enroll.OTPAuthURL, "otpauth://totp/"))
	require.Contains(t, enroll.OTPAuthURL, "SocioPulse")
	require.Contains(t, enroll.OTPAuthURL, "alice")
	// 10 backup codes, 10 hex chars each.
	require.Len(t, enroll.BackupCodes, totpBackupCodesCount)
	for _, code := range enroll.BackupCodes {
		require.Len(t, code, totpBackupCodeBytes*2)
	}

	// Persistence: row exists, enrolled=false, hashes count == 10,
	// secret_enc starts with the xorKMS header (encrypted at rest).
	row, ok := fx.store.snapshot(fx.user.ID)
	require.True(t, ok)
	require.False(t, row.Enrolled)
	require.Len(t, row.BackupCodeHashes, totpBackupCodesCount)
	require.Equal(t, xorKMSHeader, row.SecretEncrypted[0])
}

// Case 9: Encrypted-at-rest sanity. The stored bytes are NOT equal to
// the plaintext base32 secret.
func TestTOTPService_Enroll_SecretNotPlaintextAtRest(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)

	row, ok := fx.store.snapshot(fx.user.ID)
	require.True(t, ok)
	// Encrypted bytes must not match the plaintext base32 secret bytes.
	require.NotEqual(t, []byte(enroll.Secret), row.SecretEncrypted)
	// And specifically, no run of plaintext bytes appears verbatim.
	require.False(t, bytes.Contains(row.SecretEncrypted, []byte(enroll.Secret)))
}

// Case 2: Calling Enroll twice (before Confirm) overwrites the row.
func TestTOTPService_Enroll_OverwritesPartialRow(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	first, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	rowAfterFirst, _ := fx.store.snapshot(fx.user.ID)

	second, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	rowAfterSecond, _ := fx.store.snapshot(fx.user.ID)

	// New secret + new ciphertext + new backup hashes.
	require.NotEqual(t, first.Secret, second.Secret)
	require.NotEqual(t, rowAfterFirst.SecretEncrypted, rowAfterSecond.SecretEncrypted)
	require.NotEqual(t, rowAfterFirst.BackupCodeHashes, rowAfterSecond.BackupCodeHashes)
	require.False(t, rowAfterSecond.Enrolled)
}

// Case 8: Enroll on an already-enrolled user returns ErrTOTPAlreadyEnabled.
func TestTOTPService_Enroll_RejectsAlreadyEnabled(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)

	// Drive a Confirm so the row is enrolled.
	code, err := totp.GenerateCode(enroll.Secret, *fx.now)
	require.NoError(t, err)
	require.NoError(t, fx.svc.Confirm(context.Background(), fx.user.ID, code))

	// Re-enroll must fail.
	_, err = fx.svc.Enroll(context.Background(), fx.user.ID)
	require.ErrorIs(t, err, authapi.ErrTOTPAlreadyEnabled)
}

// Case 3: Confirm rejects an invalid code; row stays partial.
func TestTOTPService_Confirm_RejectsInvalidCode(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	_, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)

	err = fx.svc.Confirm(context.Background(), fx.user.ID, "000000")
	require.ErrorIs(t, err, authapi.ErrTOTPInvalid)

	row, _ := fx.store.snapshot(fx.user.ID)
	require.False(t, row.Enrolled)
	require.False(t, fx.users.totpEnabled(fx.user.ID))
}

// Case 4: Confirm with valid code flips enrolled, sets users.totp_enabled,
// audits auth.totp.enrolled.
func TestTOTPService_Confirm_AcceptsValidCodeAndAudits(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)

	code, err := totp.GenerateCode(enroll.Secret, *fx.now)
	require.NoError(t, err)
	require.NoError(t, fx.svc.Confirm(context.Background(), fx.user.ID, code))

	row, _ := fx.store.snapshot(fx.user.ID)
	require.True(t, row.Enrolled)
	require.NotNil(t, row.EnrolledAt)
	require.True(t, fx.users.totpEnabled(fx.user.ID))

	// Audit row emitted with the canonical action label.
	events := fx.audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, authapi.AuditActionTOTPEnrolled, events[0].Action)
	require.Equal(t, "user:"+fx.user.ID.String(), events[0].Target)
	require.NotNil(t, events[0].ActorID)
}

// TestTOTPService_SecretCiphertextSwap_AcrossUsers_Rejected is the
// service-layer demonstration that Plan 13.2.5 Task 6 closes the
// swap-attack defect. Two users enrol; the attacker swaps user B's
// SecretEncrypted bytes into user A's row. The next Confirm against
// user A invokes KMSResolver.Decrypt with (tenant, scope, A.ID) — the
// AAD does not match the ciphertext encrypted under (tenant, scope,
// B.ID), so decryption fails at the AEAD layer.
//
// The fake xorKMS in this test file reproduces the AAD bind contract
// (xorAADTag is part of the prefix); a swap thus surfaces as the
// "xorKMS: AAD tag mismatch" error.
func TestTOTPService_SecretCiphertextSwap_AcrossUsers_Rejected(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)

	// Seed user B under the same tenant so the tenant scope alone
	// cannot account for any rejection.
	userB := authapi.User{
		ID:       uuid.New(),
		TenantID: fx.user.TenantID,
		Login:    "bob",
	}
	fx.users.seed(userB)

	// Enrol user A (the fixture default).
	_, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	// Enrol user B.
	_, err = fx.svc.Enroll(context.Background(), userB.ID)
	require.NoError(t, err)

	// Attacker: copy user B's ciphertext into user A's row.
	rowA, _ := fx.store.snapshot(fx.user.ID)
	rowB, _ := fx.store.snapshot(userB.ID)
	require.NotEqual(t, rowA.SecretEncrypted, rowB.SecretEncrypted,
		"prerequisite: each user must have distinct ciphertext bytes")
	fx.store.mu.Lock()
	rowA.SecretEncrypted = append([]byte(nil), rowB.SecretEncrypted...)
	fx.store.rows[fx.user.ID] = rowA
	fx.store.mu.Unlock()

	// Confirm against user A: the decrypt call passes (tenant, scope,
	// userA.ID) as AAD, but the ciphertext was Encrypt'd with userB.ID
	// in its AAD. Decrypt MUST fail at the AEAD layer and surface as
	// tenancyapi.ErrInvalidArgument — the sentinel the production
	// KMSResolverImpl.Decrypt wraps for auth-tag failures. errors.Is
	// (not Contains on the error string) so a future refactor of either
	// fake or production wrapper text cannot silently mask the sentinel.
	err = fx.svc.Confirm(context.Background(), fx.user.ID, "000000")
	require.Error(t, err, "swap attack must surface as an error, not a successful confirm")
	require.ErrorIs(t, err, tenancyapi.ErrInvalidArgument,
		"AAD mismatch MUST surface as tenancyapi.ErrInvalidArgument; got %v", err)
}

// Confirm on an already-enrolled row is idempotent (no-op).
func TestTOTPService_Confirm_IdempotentOnEnrolled(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	code, err := totp.GenerateCode(enroll.Secret, *fx.now)
	require.NoError(t, err)
	require.NoError(t, fx.svc.Confirm(context.Background(), fx.user.ID, code))

	// Second Confirm is a no-op; even a wrong code MUST NOT undo the
	// enrolment because the early-return short-circuits before validation.
	require.NoError(t, fx.svc.Confirm(context.Background(), fx.user.ID, "000000"))
	row, _ := fx.store.snapshot(fx.user.ID)
	require.True(t, row.Enrolled)
}

// Case 5: Verify with valid code -> true; UpdateLastVerified called; audit.
func TestTOTPService_Verify_ValidCodeStampsLastVerified(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	first, err := totp.GenerateCode(enroll.Secret, *fx.now)
	require.NoError(t, err)
	require.NoError(t, fx.svc.Confirm(context.Background(), fx.user.ID, first))

	// Drop calls accumulated by the enrol path so we can assert cleanly.
	fx.store.mu.Lock()
	fx.store.calls = nil
	fx.store.mu.Unlock()

	// Generate a fresh code at the same instant. The time bucket is
	// identical so this is a different TOTP than the enrol code only
	// when the period rolled — to keep the assertion stable, use the
	// same secret + clock.
	ok, err := fx.svc.Verify(context.Background(), fx.user.ID, first)
	require.NoError(t, err)
	require.True(t, ok)

	row, _ := fx.store.snapshot(fx.user.ID)
	require.NotNil(t, row.LastVerifiedAt)
	require.Contains(t, fx.store.callsCopy(), "UpdateLastVerified")
}

// Case 6: Verify with stale code (>1 period off) returns false.
func TestTOTPService_Verify_StaleCodeReturnsFalse(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	enrolCode, err := totp.GenerateCode(enroll.Secret, *fx.now)
	require.NoError(t, err)
	require.NoError(t, fx.svc.Confirm(context.Background(), fx.user.ID, enrolCode))

	// Move clock forward by 5 minutes — well outside the ±1 period (60 s)
	// validation window. The previously-issued code must now fail.
	*fx.now = fx.now.Add(5 * time.Minute)

	ok, err := fx.svc.Verify(context.Background(), fx.user.ID, enrolCode)
	require.NoError(t, err)
	require.False(t, ok)
}

// Verify on a user without enrolment surfaces ErrTOTPNotEnrolled.
func TestTOTPService_Verify_NotEnrolled(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	ok, err := fx.svc.Verify(context.Background(), fx.user.ID, "123456")
	require.False(t, ok)
	require.ErrorIs(t, err, authapi.ErrTOTPNotEnrolled)
}

// Verify on a partial-enrollment row also surfaces ErrTOTPNotEnrolled —
// the row exists but enrolled=false so we never let the secret answer.
func TestTOTPService_Verify_PartialEnrollment(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	_, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)

	ok, err := fx.svc.Verify(context.Background(), fx.user.ID, "123456")
	require.False(t, ok)
	require.ErrorIs(t, err, authapi.ErrTOTPNotEnrolled)
}

// Case 7: Disable removes the row, sets users.totp_enabled=false, audits.
func TestTOTPService_Disable_RemovesRowAndAudits(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	code, err := totp.GenerateCode(enroll.Secret, *fx.now)
	require.NoError(t, err)
	require.NoError(t, fx.svc.Confirm(context.Background(), fx.user.ID, code))

	// Drop pre-existing audits so the assertion is on the Disable event only.
	fx.audit.mu.Lock()
	fx.audit.events = nil
	fx.audit.mu.Unlock()

	require.NoError(t, fx.svc.Disable(context.Background(), fx.user.ID))

	_, ok := fx.store.snapshot(fx.user.ID)
	require.False(t, ok, "row removed after Disable")
	require.False(t, fx.users.totpEnabled(fx.user.ID))

	events := fx.audit.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, authapi.AuditActionTOTPDisabled, events[0].Action)
}

// Disable on a user who never enrolled is idempotent.
func TestTOTPService_Disable_IdempotentOnAbsent(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	require.NoError(t, fx.svc.Disable(context.Background(), fx.user.ID))
}

// Case 10: backup-code path — Verify with a backup code matches and the
// store is told to consume it.
func TestTOTPService_Verify_BackupCodeIsConsumed(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	enrolCode, err := totp.GenerateCode(enroll.Secret, *fx.now)
	require.NoError(t, err)
	require.NoError(t, fx.svc.Confirm(context.Background(), fx.user.ID, enrolCode))

	// Reset call log so the assertion focuses on the verify path.
	fx.store.mu.Lock()
	fx.store.calls = nil
	fx.store.mu.Unlock()

	// Use one of the issued backup codes — these are NOT TOTP-shaped so
	// the TOTP check must fail and the backup walk picks them up.
	backup := enroll.BackupCodes[3]
	ok, err := fx.svc.Verify(context.Background(), fx.user.ID, backup)
	require.NoError(t, err)
	require.True(t, ok)

	require.Contains(t, fx.store.callsCopy(), "MarkBackupUsed")

	row, _ := fx.store.snapshot(fx.user.ID)
	require.Len(t, row.BackupCodeHashes, totpBackupCodesCount-1)
	require.Equal(t, 1, row.BackupUsedCount)

	// Re-using the same code now fails.
	ok, err = fx.svc.Verify(context.Background(), fx.user.ID, backup)
	require.NoError(t, err)
	require.False(t, ok)
}

// Status reports zero values for a user without enrolment.
func TestTOTPService_Status_NoEnrollment(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	st, err := fx.svc.Status(context.Background(), fx.user.ID)
	require.NoError(t, err)
	require.False(t, st.Enabled)
	require.Equal(t, 0, st.BackupRemaining)
	require.Nil(t, st.EnrolledAt)
	require.Nil(t, st.LastVerifiedAt)
}

// Status on an enrolled user reports the live counts and timestamps.
func TestTOTPService_Status_AfterEnroll(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	code, err := totp.GenerateCode(enroll.Secret, *fx.now)
	require.NoError(t, err)
	require.NoError(t, fx.svc.Confirm(context.Background(), fx.user.ID, code))

	st, err := fx.svc.Status(context.Background(), fx.user.ID)
	require.NoError(t, err)
	require.True(t, st.Enabled)
	require.NotNil(t, st.EnrolledAt)
	require.Equal(t, totpBackupCodesCount, st.BackupRemaining)
}

// Confirm on an unknown user surfaces ErrUserNotFound.
func TestTOTPService_Confirm_UnknownUser(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	err := fx.svc.Confirm(context.Background(), uuid.New(), "123456")
	require.ErrorIs(t, err, authapi.ErrUserNotFound)
}

// Enroll with a custom skew/period exercises the deps-default fallbacks
// without changing observable behaviour for the standard config.
func TestTOTPService_Defaults_AppliedWhenZero(t *testing.T) {
	t.Parallel()

	store := newFakeTOTPStore()
	users := newFakeUserStore()
	user := authapi.User{ID: uuid.New(), TenantID: uuid.New(), Login: "bob"}
	users.seed(user)

	svc, err := NewTOTPService(TOTPDeps{
		// Issuer/Period/Skew/Clock all zero -> defaults apply.
		Pool:         &fakeTxRunner{},
		Store:        store,
		Users:        users,
		KMS:          &xorKMS{key: 0x01},
		BackupHasher: cheapBackupHasher(),
		Audit:        &fakeAudit{},
	})
	require.NoError(t, err)

	enroll, err := svc.Enroll(context.Background(), user.ID)
	require.NoError(t, err)
	require.Contains(t, enroll.OTPAuthURL, totpIssuerDefault)
}

// Compile-time conformance is in totp.go (var _ = blocks). The runtime
// test below ensures the ServiceLayer satisfies the consumer interface
// the Authenticator depends on.
func TestTOTPService_SatisfiesAuthenticatorVerifier(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	var _ TOTPVerifier = fx.svc
	var _ authapi.TOTPVerifier = fx.svc
	var _ authapi.TOTPService = fx.svc
}

// Make sure the audit row carries a sane Timestamp so downstream
// archivers can partition on it.
func TestTOTPService_AuditTimestampNonZero(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	code, err := totp.GenerateCode(enroll.Secret, *fx.now)
	require.NoError(t, err)
	require.NoError(t, fx.svc.Confirm(context.Background(), fx.user.ID, code))

	for _, ev := range fx.audit.snapshot() {
		require.False(t, ev.Timestamp.IsZero(), "audit event timestamp must be set: %s", ev.Action)
	}
}

// ============================================================================
// Error-path coverage — exercises the unhappy branches the
// happy-path tests above don't reach (KMS / store / users failures).
// ============================================================================

// Enroll surfaces ErrUserNotFound when the user lookup fails.
func TestTOTPService_Enroll_UnknownUser(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	_, err := fx.svc.Enroll(context.Background(), uuid.New())
	require.ErrorIs(t, err, authapi.ErrUserNotFound)
}

// Enroll wraps a store error from the upsert path.
func TestTOTPService_Enroll_StoreUpsertError(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	fx.store.upsErr = errors.New("boom")
	_, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "totp upsert")
}

// errKMS is a KMSResolver that fails on Encrypt or Decrypt depending on
// configuration — used to drive the failure paths that xorKMS short-circuits.
type errKMS struct {
	encErr error
	decErr error
}

var _ tenancyapi.KMSResolver = (*errKMS)(nil)

func (k *errKMS) EnsureKEK(_ context.Context, _ uuid.UUID) (string, error) {
	return "", nil
}
func (k *errKMS) GenerateDataKey(_ context.Context, _ uuid.UUID) (tenancyapi.DataKey, error) {
	return tenancyapi.DataKey{}, errors.New("not used")
}
func (k *errKMS) Encrypt(_ context.Context, _ uuid.UUID, _, _ string, plaintext []byte) ([]byte, error) {
	if k.encErr != nil {
		return nil, k.encErr
	}
	return append([]byte{xorKMSHeader}, plaintext...), nil
}
func (k *errKMS) Decrypt(_ context.Context, _ uuid.UUID, _, _ string, ciphertext []byte) ([]byte, error) {
	if k.decErr != nil {
		return nil, k.decErr
	}
	return ciphertext[1:], nil
}
func (k *errKMS) InvalidateCache(_ uuid.UUID) {}

func newFixtureWithKMS(t *testing.T, kms tenancyapi.KMSResolver) *totpFixture {
	t.Helper()
	store := newFakeTOTPStore()
	users := newFakeUserStore()
	audit := &fakeAudit{}
	clock, now := fixedClock(time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))
	user := authapi.User{ID: uuid.New(), TenantID: uuid.New(), Login: "alice"}
	users.seed(user)
	svc, err := NewTOTPService(TOTPDeps{
		Pool:         &fakeTxRunner{},
		Store:        store,
		Users:        users,
		KMS:          kms,
		BackupHasher: cheapBackupHasher(),
		Audit:        audit,
		Clock:        clock,
	})
	require.NoError(t, err)
	return &totpFixture{svc: svc, store: store, users: users, audit: audit, clock: clock, now: now, user: user}
}

// Enroll surfaces a KMS encrypt failure.
func TestTOTPService_Enroll_KMSEncryptError(t *testing.T) {
	t.Parallel()

	fx := newFixtureWithKMS(t, &errKMS{encErr: errors.New("kms-down")})
	_, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "kms encrypt")
}

// Confirm surfaces a KMS decrypt failure.
func TestTOTPService_Confirm_KMSDecryptError(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	_, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	// Swap the KMS for one that fails on Decrypt to drive the unhappy
	// branch in Confirm.
	fx.svc.kms = &errKMS{decErr: errors.New("kms-decrypt-down")}
	err = fx.svc.Confirm(context.Background(), fx.user.ID, "123456")
	require.Error(t, err)
	require.Contains(t, err.Error(), "kms decrypt")
}

// Verify surfaces a KMS decrypt failure.
func TestTOTPService_Verify_KMSDecryptError(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	code, err := totp.GenerateCode(enroll.Secret, *fx.now)
	require.NoError(t, err)
	require.NoError(t, fx.svc.Confirm(context.Background(), fx.user.ID, code))

	fx.svc.kms = &errKMS{decErr: errors.New("kms-decrypt-down")}
	ok, err := fx.svc.Verify(context.Background(), fx.user.ID, code)
	require.False(t, ok)
	require.Error(t, err)
	require.Contains(t, err.Error(), "kms decrypt")
}

// Confirm on a user with no enrolment row -> ErrTOTPNotEnrolled.
func TestTOTPService_Confirm_NoEnrollmentRow(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	err := fx.svc.Confirm(context.Background(), fx.user.ID, "123456")
	require.ErrorIs(t, err, authapi.ErrTOTPNotEnrolled)
}

// Disable on an unknown user surfaces ErrUserNotFound (resolveUser path).
func TestTOTPService_Disable_UnknownUser(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	err := fx.svc.Disable(context.Background(), uuid.New())
	require.ErrorIs(t, err, authapi.ErrUserNotFound)
}

// Status on an unknown user surfaces ErrUserNotFound.
func TestTOTPService_Status_UnknownUser(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	_, err := fx.svc.Status(context.Background(), uuid.New())
	require.ErrorIs(t, err, authapi.ErrUserNotFound)
}

// Verify on an unknown user surfaces ErrUserNotFound.
func TestTOTPService_Verify_UnknownUser(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	ok, err := fx.svc.Verify(context.Background(), uuid.New(), "123456")
	require.False(t, ok)
	require.ErrorIs(t, err, authapi.ErrUserNotFound)
}

// Enroll preflight surfaces a non-NotEnrolled store error verbatim
// (wrapped). Drives the "auth/service: totp enroll preflight" branch.
func TestTOTPService_Enroll_PreflightStoreError(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	fx.store.getAnyErr = errors.New("scan boom")
	_, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "enroll preflight")
}

// Confirm wraps a store-side Confirm error inside its outer
// "auth/service: totp confirm" wrapper.
func TestTOTPService_Confirm_StoreErrorWrapped(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	fx.store.confirmErr = errors.New("rls denied")
	code, err := totp.GenerateCode(enroll.Secret, *fx.now)
	require.NoError(t, err)
	err = fx.svc.Confirm(context.Background(), fx.user.ID, code)
	require.Error(t, err)
	require.Contains(t, err.Error(), "totp confirm")
}

// Verify wraps an UpdateLastVerified failure rather than swallowing it
// silently — that branch ensures audit/observability remains honest.
func TestTOTPService_Verify_UpdateLastVerifiedErrorBubbles(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	code, err := totp.GenerateCode(enroll.Secret, *fx.now)
	require.NoError(t, err)
	require.NoError(t, fx.svc.Confirm(context.Background(), fx.user.ID, code))

	fx.store.updLastVerErr = errors.New("boom")
	ok, err := fx.svc.Verify(context.Background(), fx.user.ID, code)
	require.False(t, ok)
	require.Error(t, err)
	require.Contains(t, err.Error(), "update last verified")
}

// Verify treats a concurrent backup-code race (MarkBackupUsed returns
// ErrTOTPInvalid) as a wrong code rather than a service failure.
func TestTOTPService_Verify_BackupRaceTreatedAsWrong(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	code, err := totp.GenerateCode(enroll.Secret, *fx.now)
	require.NoError(t, err)
	require.NoError(t, fx.svc.Confirm(context.Background(), fx.user.ID, code))

	fx.store.markBackupErr = authapi.ErrTOTPInvalid
	ok, err := fx.svc.Verify(context.Background(), fx.user.ID, enroll.BackupCodes[0])
	require.NoError(t, err)
	require.False(t, ok)
}

// Verify wraps a non-ErrTOTPInvalid MarkBackupUsed failure.
func TestTOTPService_Verify_BackupMarkUsedHardError(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)
	code, err := totp.GenerateCode(enroll.Secret, *fx.now)
	require.NoError(t, err)
	require.NoError(t, fx.svc.Confirm(context.Background(), fx.user.ID, code))

	fx.store.markBackupErr = errors.New("rls denied")
	ok, err := fx.svc.Verify(context.Background(), fx.user.ID, enroll.BackupCodes[0])
	require.False(t, ok)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mark backup used")
}

// Smoke: ensure the otpauth URL parses back via pquerna/otp and yields
// the same secret + period + digits we configured. Round-tripping
// through the URL path is the contract a real authenticator app
// follows when scanning the QR code.
func TestTOTPService_OTPAuthURL_RoundTrips(t *testing.T) {
	t.Parallel()

	fx := newTOTPFixture(t)
	enroll, err := fx.svc.Enroll(context.Background(), fx.user.ID)
	require.NoError(t, err)

	parsed, err := otp.NewKeyFromURL(enroll.OTPAuthURL)
	require.NoError(t, err)
	require.Equal(t, enroll.Secret, parsed.Secret())
	require.Equal(t, otp.DigitsSix.String(), parsed.Digits().String())
	require.EqualValues(t, totpPeriodDefault, parsed.Period())
}
