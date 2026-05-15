package dialer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	dialerapi "github.com/sociopulse/platform/internal/dialer/api"
	"github.com/sociopulse/platform/internal/modules"
	telephonyapi "github.com/sociopulse/platform/internal/telephony/api"
	tenancyapi "github.com/sociopulse/platform/internal/tenancy/api"
)

// TestNewPgCallTenantResolver_PanicsOnNilPool surfaces the
// composition-root misconfiguration at construction (mirrors the
// canonical NewPgCallOperatorLookup contract). A live Postgres lookup
// would mask a nil-pool wiring bug behind a delayed panic on first
// request; failing fast at boot is the right shape.
func TestNewPgCallTenantResolver_PanicsOnNilPool(t *testing.T) {
	t.Parallel()
	assert.PanicsWithValue(t,
		"dialer.NewPgCallTenantResolver: pool must be non-nil",
		func() { _ = NewPgCallTenantResolver(nil) },
		"NewPgCallTenantResolver(nil) must panic with the canonical message",
	)
}

// TestPgCallTenantResolver_LookupCallTenant_NilCallID rejects a
// zero-UUID at the boundary so a misformed handler call cannot fall
// through to a real BypassRLS scan that would return a misleading
// "no rows" result.
func TestPgCallTenantResolver_LookupCallTenant_NilCallID(t *testing.T) {
	t.Parallel()
	// We construct against a non-nil but unconnected pool — the nil-id
	// guard runs BEFORE any pool method, so no live conn is required.
	r := &PgCallTenantResolver{}
	_, err := r.LookupCallTenant(t.Context(), uuid.Nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil callID")
	// Crucially NOT ErrCallNotFound — a programmer bug (nil id) must
	// not look like a benign 404 to the upstream middleware.
	assert.NotErrorIs(t, err, dialerapi.ErrCallNotFound,
		"nil callID must be a programmer error, not folded to 404")
}

// fakeSettingsCache is a tiny tenancyapi.SettingsCache fake exposing
// only the Lookup methods used by settingsLookupAdapter. The other
// methods panic if a regression accidentally calls them.
type fakeSettingsCache struct {
	value tenancyapi.SettingValue
	ok    bool
	err   error
}

func (f *fakeSettingsCache) Lookup(_ context.Context, _ uuid.UUID, _ string) (tenancyapi.SettingValue, error) {
	if f.err != nil {
		return tenancyapi.SettingValue{}, f.err
	}
	if !f.ok {
		return tenancyapi.SettingValue{}, tenancyapi.ErrNotFound
	}
	return f.value, nil
}

func (*fakeSettingsCache) LookupWithDefault(_ context.Context, _ uuid.UUID, _ string, _ tenancyapi.SettingValue) (tenancyapi.SettingValue, error) {
	panic("unexpected: dialer adapter does not call LookupWithDefault")
}

func (*fakeSettingsCache) LookupAll(_ context.Context, _ uuid.UUID) (map[string]tenancyapi.SettingValue, error) {
	panic("unexpected: dialer adapter does not call LookupAll")
}

func (*fakeSettingsCache) Set(_ context.Context, _ uuid.UUID, _ string, _ tenancyapi.SettingValue) error {
	panic("unexpected")
}
func (*fakeSettingsCache) Delete(_ context.Context, _ uuid.UUID, _ string) error {
	panic("unexpected")
}
func (*fakeSettingsCache) InvalidateLocal(_ uuid.UUID, _ string) {
	panic("unexpected")
}
func (*fakeSettingsCache) InvalidateAllLocal(_ uuid.UUID) {
	panic("unexpected")
}

// TestSettingsLookupAdapterTranslatesNotFound: tenancy.ErrNotFound
// becomes ok=false (not an error).
func TestSettingsLookupAdapterTranslatesNotFound(t *testing.T) {
	t.Parallel()

	a := &settingsLookupAdapter{cache: &fakeSettingsCache{ok: false}}
	raw, ok, err := a.Lookup(t.Context(), uuid.New(), "working_hours")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, raw)
}

// TestSettingsLookupAdapterReturnsRawJSON: a real value translates to
// the underlying JSON bytes.
func TestSettingsLookupAdapterReturnsRawJSON(t *testing.T) {
	t.Parallel()

	v, err := tenancyapi.SettingValueFromAny(map[string]any{"foo": "bar"})
	require.NoError(t, err)
	a := &settingsLookupAdapter{cache: &fakeSettingsCache{value: v, ok: true}}

	raw, ok, err := a.Lookup(t.Context(), uuid.New(), "working_hours")
	require.NoError(t, err)
	assert.True(t, ok)
	// The raw JSON should include the "foo":"bar" pair.
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "bar", got["foo"])
}

// TestSettingsLookupAdapterPropagatesOtherErrors: any error other than
// tenancy.ErrNotFound surfaces verbatim.
func TestSettingsLookupAdapterPropagatesOtherErrors(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	a := &settingsLookupAdapter{cache: &fakeSettingsCache{err: wantErr}}
	_, _, err := a.Lookup(t.Context(), uuid.New(), "working_hours")
	assert.ErrorIs(t, err, wantErr)
}

// TestSettingsLookupAdapterNilCacheNoop: a nil cache returns ok=false.
// Belt-and-suspenders for the worker boot path that constructs the
// adapter without a Tenancy aggregate.
func TestSettingsLookupAdapterNilCacheNoop(t *testing.T) {
	t.Parallel()

	var a *settingsLookupAdapter
	raw, ok, err := a.Lookup(t.Context(), uuid.New(), "working_hours")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, raw)
}

// TestNoopSettingsLookup: the always-default fallback returns ok=false
// for every key.
func TestNoopSettingsLookup(t *testing.T) {
	t.Parallel()
	a := noopSettingsLookup{}
	raw, ok, err := a.Lookup(t.Context(), uuid.New(), "working_hours")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, raw)
}

// fakeKMSResolver implements tenancyapi.KMSResolver for the adapter
// tests. We only exercise Decrypt; the other methods panic.
type fakeKMSResolver struct {
	out []byte
	err error
}

func (*fakeKMSResolver) EnsureKEK(context.Context, uuid.UUID) (string, error) {
	panic("unexpected")
}
func (*fakeKMSResolver) GenerateDataKey(context.Context, uuid.UUID) (tenancyapi.DataKey, error) {
	panic("unexpected")
}
func (*fakeKMSResolver) Encrypt(context.Context, uuid.UUID, string, string, []byte) ([]byte, error) {
	panic("unexpected")
}
func (f *fakeKMSResolver) Decrypt(_ context.Context, _ uuid.UUID, _, _ string, _ []byte) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}
func (*fakeKMSResolver) InvalidateCache(uuid.UUID) {
	panic("unexpected")
}

// TestKMSDecryptorAdapterForwardsToKMS: happy path.
func TestKMSDecryptorAdapterForwardsToKMS(t *testing.T) {
	t.Parallel()

	want := []byte("+79991234567")
	a := &kmsDecryptorAdapter{kms: &fakeKMSResolver{out: want}}
	got, err := a.Decrypt(t.Context(), uuid.New(), uuid.New(), []byte("ciphertext"))
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestKMSDecryptorAdapterPropagatesError.
func TestKMSDecryptorAdapterPropagatesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("kms boom")
	a := &kmsDecryptorAdapter{kms: &fakeKMSResolver{err: wantErr}}
	_, err := a.Decrypt(t.Context(), uuid.New(), uuid.New(), []byte("ciphertext"))
	assert.ErrorIs(t, err, wantErr)
}

// TestKMSDecryptorAdapterNilKMSReturnsError: nil KMS surfaces a clean
// error rather than panicking.
func TestKMSDecryptorAdapterNilKMSReturnsError(t *testing.T) {
	t.Parallel()

	var a *kmsDecryptorAdapter
	_, err := a.Decrypt(t.Context(), uuid.New(), uuid.New(), []byte("ciphertext"))
	assert.Error(t, err)
}

// TestPassthroughDecryptor: returns the ciphertext bytes verbatim.
func TestPassthroughDecryptor(t *testing.T) {
	t.Parallel()

	in := []byte("+79991234567")
	out, err := passthroughDecryptor{}.Decrypt(t.Context(), uuid.New(), uuid.New(), in)
	require.NoError(t, err)
	assert.Equal(t, in, out)
	// Modifying the returned slice does not corrupt the caller's
	// input.
	out[0] = 'X'
	assert.NotEqual(t, in, out)
}

// TestPassthroughDecryptorEmptyCiphertext.
func TestPassthroughDecryptorEmptyCiphertext(t *testing.T) {
	t.Parallel()
	_, err := passthroughDecryptor{}.Decrypt(t.Context(), uuid.New(), uuid.New(), nil)
	assert.Error(t, err)
}

// TestStubCapacityPool returns an empty healthy-node list.
func TestStubCapacityPool(t *testing.T) {
	t.Parallel()
	assert.Empty(t, stubCapacityPool{}.HealthyNodes())
}

// TestStubBackpressure exercises every method.
func TestStubBackpressure(t *testing.T) {
	t.Parallel()
	bp := stubBackpressure{}
	ok, err := bp.TryAcquire(t.Context(), "node1")
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, bp.Release(t.Context(), "node1"))

	v, err := bp.Get(t.Context(), "node1")
	require.NoError(t, err)
	assert.Equal(t, 0, v)

	assert.Equal(t, 0, bp.Cap())
}

// TestStubEventConsumerSubscribeIsNoop.
func TestStubEventConsumerSubscribeIsNoop(t *testing.T) {
	t.Parallel()
	c := &stubEventConsumer{logger: zaptest.NewLogger(t)}
	called := false
	unsub, err := c.Subscribe(t.Context(), uuid.New(), func(context.Context, telephonyapi.ChannelEvent) error {
		called = true
		return nil
	})
	require.NoError(t, err)
	require.NotNil(t, unsub)
	unsub() // idempotent no-op
	assert.False(t, called, "stub consumer must never invoke the handler")
}

// TestLookupTenancyMissingReturnsNil verifies that a missing locator
// entry surfaces nil (rather than panicking).
func TestLookupTenancyMissingReturnsNil(t *testing.T) {
	t.Parallel()

	loc := modules.NewMapLocator()
	got := lookupTenancy(loc, zaptest.NewLogger(t))
	assert.Nil(t, got)
}

// TestLookupTenancyWrongTypeLogsAndReturnsNil.
func TestLookupTenancyWrongTypeLogsAndReturnsNil(t *testing.T) {
	t.Parallel()

	loc := modules.NewMapLocator()
	loc.Register(locatorTenancy, "not a tenancy")
	got := lookupTenancy(loc, zaptest.NewLogger(t))
	assert.Nil(t, got)
}

// TestLookupKMSResolverMissingReturnsNil.
func TestLookupKMSResolverMissingReturnsNil(t *testing.T) {
	t.Parallel()
	loc := modules.NewMapLocator()
	got := lookupKMSResolver(loc, zaptest.NewLogger(t))
	assert.Nil(t, got)
}

// TestLookupKMSResolverNilLocatorReturnsNil.
func TestLookupKMSResolverNilLocatorReturnsNil(t *testing.T) {
	t.Parallel()
	got := lookupKMSResolver(nil, zaptest.NewLogger(t))
	assert.Nil(t, got)
}

// TestLookupKMSResolverWrongTypeLogsAndReturnsNil verifies the
// type-assert error path.
func TestLookupKMSResolverWrongTypeLogsAndReturnsNil(t *testing.T) {
	t.Parallel()
	loc := modules.NewMapLocator()
	loc.Register(locatorKMSResolver, "string is not a KMSResolver")
	got := lookupKMSResolver(loc, zaptest.NewLogger(t))
	assert.Nil(t, got)
}

// TestLookupKMSResolverHappyPath: a real KMSResolver in the locator
// flows back unchanged.
func TestLookupKMSResolverHappyPath(t *testing.T) {
	t.Parallel()
	loc := modules.NewMapLocator()
	want := &fakeKMSResolver{out: []byte("phone")}
	loc.Register(locatorKMSResolver, tenancyapi.KMSResolver(want))
	got := lookupKMSResolver(loc, zaptest.NewLogger(t))
	require.NotNil(t, got)
}

// TestLookupTenancyHappyPath: a real Tenancy in the locator flows
// back unchanged.
func TestLookupTenancyHappyPath(t *testing.T) {
	t.Parallel()
	loc := modules.NewMapLocator()
	loc.Register(locatorTenancy, tenancyapi.Tenancy(&fakeTenancy{}))
	got := lookupTenancy(loc, zaptest.NewLogger(t))
	require.NotNil(t, got)
}

// TestLookupTenancyNilLocatorReturnsNil.
func TestLookupTenancyNilLocatorReturnsNil(t *testing.T) {
	t.Parallel()
	got := lookupTenancy(nil, zaptest.NewLogger(t))
	assert.Nil(t, got)
}

// TestLookupCommandPublisherNilLocator covers the nil-locator branch.
func TestLookupCommandPublisherNilLocator(t *testing.T) {
	t.Parallel()
	_, ok := lookupCommandPublisher(nil, zaptest.NewLogger(t))
	assert.False(t, ok)
}

// fakeTenancy is a minimal tenancyapi.Tenancy double — used to test
// the locator round-trip in lookupTenancy. Every method of the four
// embedded interfaces panics; the test only verifies the type
// assertion succeeds.
type fakeTenancy struct {
	fakeSettingsCache
	fakeKMSResolver
}

// TenantService methods.
func (*fakeTenancy) Create(context.Context, tenancyapi.CreateTenantRequest) (tenancyapi.Tenant, error) {
	panic("unexpected: Create")
}
func (*fakeTenancy) Get(context.Context, uuid.UUID) (tenancyapi.Tenant, error) {
	panic("unexpected: Get")
}
func (*fakeTenancy) GetByOrgCode(context.Context, string) (tenancyapi.Tenant, error) {
	panic("unexpected: GetByOrgCode")
}
func (*fakeTenancy) List(context.Context, tenancyapi.ListTenantsFilter) ([]tenancyapi.Tenant, error) {
	panic("unexpected: List")
}
func (*fakeTenancy) Suspend(context.Context, uuid.UUID, string) error {
	panic("unexpected: Suspend")
}
func (*fakeTenancy) Resume(context.Context, uuid.UUID) error {
	panic("unexpected: Resume")
}
func (*fakeTenancy) Archive(context.Context, uuid.UUID) error {
	panic("unexpected: Archive")
}

// PhoneHasher methods.
func (*fakeTenancy) Hash(context.Context, uuid.UUID, string) ([]byte, error) {
	panic("unexpected: Hash")
}
func (*fakeTenancy) Normalise(string) (string, error) {
	panic("unexpected: Normalise")
}

// TestLookupCommandPublisherMissingReturnsFalse.
func TestLookupCommandPublisherMissingReturnsFalse(t *testing.T) {
	t.Parallel()
	loc := modules.NewMapLocator()
	_, ok := lookupCommandPublisher(loc, zaptest.NewLogger(t))
	assert.False(t, ok)
}

// TestLookupCommandPublisherWrongType: registered under the right key
// but with a non-CommandPublisher value.
func TestLookupCommandPublisherWrongType(t *testing.T) {
	t.Parallel()
	loc := modules.NewMapLocator()
	loc.Register(locatorCommandPublisher, "string is not a publisher")
	_, ok := lookupCommandPublisher(loc, zaptest.NewLogger(t))
	assert.False(t, ok)
}
