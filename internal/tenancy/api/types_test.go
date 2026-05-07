package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/tenancy/api"
)

func TestTenant_StatusTransitions_AreEnumerated(t *testing.T) {
	t.Parallel()
	require.Equal(t, api.TenantStatusActive, api.TenantStatus("active"))
	require.Equal(t, api.TenantStatusSuspended, api.TenantStatus("suspended"))
	require.Equal(t, api.TenantStatusArchived, api.TenantStatus("archived"))
}

func TestTenantStatus_Valid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		s    api.TenantStatus
		ok   bool
	}{
		{"active", api.TenantStatusActive, true},
		{"suspended", api.TenantStatusSuspended, true},
		{"archived", api.TenantStatusArchived, true},
		{"empty", api.TenantStatus(""), false},
		{"unknown", api.TenantStatus("ghost"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.ok, tc.s.Valid())
		})
	}
}

func TestTenant_Validate_RejectsEmptyOrgCode(t *testing.T) {
	t.Parallel()
	tn := api.Tenant{
		ID:      uuid.New(),
		OrgCode: "",
		Name:    "x",
		Status:  api.TenantStatusActive,
	}
	err := tn.Validate()
	require.ErrorIs(t, err, api.ErrInvalidArgument)
}

func TestTenant_Validate_RejectsTooLongOrgCode(t *testing.T) {
	t.Parallel()
	tn := api.Tenant{
		ID:      uuid.New(),
		OrgCode: string(make([]byte, 65)),
		Name:    "x",
		Status:  api.TenantStatusActive,
	}
	err := tn.Validate()
	require.ErrorIs(t, err, api.ErrInvalidArgument)
}

func TestTenant_Validate_RejectsEmptyName(t *testing.T) {
	t.Parallel()
	tn := api.Tenant{
		ID:      uuid.New(),
		OrgCode: "CC-MOSKVA-01",
		Name:    "",
		Status:  api.TenantStatusActive,
	}
	err := tn.Validate()
	require.ErrorIs(t, err, api.ErrInvalidArgument)
}

func TestTenant_Validate_RejectsBadStatus(t *testing.T) {
	t.Parallel()
	tn := api.Tenant{
		ID:      uuid.New(),
		OrgCode: "CC-MOSKVA-01",
		Name:    "ВЦИОМ-Москва",
		Status:  api.TenantStatus("ghost"),
	}
	err := tn.Validate()
	require.ErrorIs(t, err, api.ErrInvalidArgument)
}

func TestTenant_Validate_AcceptsValid(t *testing.T) {
	t.Parallel()
	tn := api.Tenant{
		ID:      uuid.New(),
		OrgCode: "CC-MOSKVA-01",
		Name:    "ВЦИОМ-Москва",
		Status:  api.TenantStatusActive,
	}
	require.NoError(t, tn.Validate())
}

func TestSettingValue_TypedAccessors(t *testing.T) {
	t.Parallel()

	v, err := api.SettingValueFromAny("4h")
	require.NoError(t, err)
	d, err := v.AsDuration()
	require.NoError(t, err)
	require.Equal(t, 4*time.Hour, d)

	vint, err := api.SettingValueFromAny(int64(3))
	require.NoError(t, err)
	i, err := vint.AsInt()
	require.NoError(t, err)
	require.Equal(t, int64(3), i)

	vstr, err := api.SettingValueFromAny("hello")
	require.NoError(t, err)
	s, err := vstr.AsString()
	require.NoError(t, err)
	require.Equal(t, "hello", s)

	vbool, err := api.SettingValueFromAny(true)
	require.NoError(t, err)
	b, err := vbool.AsBool()
	require.NoError(t, err)
	require.True(t, b)
}

func TestSettingValue_FromRawCopiesBytes(t *testing.T) {
	t.Parallel()
	src := []byte(`"hello"`)
	v := api.SettingValueFromRaw(src)
	// Mutate the source slice; the SettingValue must remain unaffected.
	src[1] = 'X'
	s, err := v.AsString()
	require.NoError(t, err)
	require.Equal(t, "hello", s)
}

func TestSettingValue_TypeMismatch_WrapsErrInvalidArgument(t *testing.T) {
	t.Parallel()
	v, err := api.SettingValueFromAny("not-a-number")
	require.NoError(t, err)

	_, err = v.AsInt()
	require.ErrorIs(t, err, api.ErrInvalidArgument)

	_, err = v.AsBool()
	require.ErrorIs(t, err, api.ErrInvalidArgument)

	_, err = v.AsDuration()
	require.ErrorIs(t, err, api.ErrInvalidArgument)
}

func TestSentinelErrors_AreDistinct(t *testing.T) {
	t.Parallel()
	// Each sentinel must be its own value (not aliased to another).
	all := []error{
		api.ErrNotFound,
		api.ErrAlreadyExists,
		api.ErrInvalidArgument,
		api.ErrSuspended,
		api.ErrArchived,
		api.ErrKMSUnavailable,
		api.ErrPermissionDenied,
	}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			require.NotErrorIs(t, a, b, "sentinel %d collides with %d", i, j)
		}
	}
}

// Compile-time fixture: Tenancy interface must include all four sub-interfaces
// directly (so consumers can take a single dependency rather than four). This
// requires renaming SettingsCache.Get/GetWithDefault/GetAll to Lookup variants
// to avoid the Get-method collision with TenantService.Get.
func TestTenancyAggregate_EmbedsAllFourSubInterfaces(t *testing.T) {
	t.Parallel()
	var _ context.Context
	var _ interface {
		api.TenantService
		api.SettingsCache
		api.KMSResolver
		api.PhoneHasher
	} = (api.Tenancy)(nil)
}

// Compile-time fixture: Module exists with the constructor seam. The Register
// var is set by service/register.go (Plan 04 Task 2); api/ stays free of
// service/ imports.
func TestModule_RegisterSeam_Exists(t *testing.T) {
	t.Parallel()
	// Register is a package-level variable function; nil check just proves the
	// symbol exists with the expected signature.
	var fn func(ctx context.Context, deps api.Deps) (*api.Module, error) = api.Register
	_ = fn // not invoked here — service-layer test exercises wiring
}
