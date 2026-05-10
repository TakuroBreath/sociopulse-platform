package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rapi "github.com/sociopulse/platform/internal/recording/api"
)

// fakeCallTenantLookup captures lookup calls.
type fakeCallTenantLookup struct {
	want   uuid.UUID
	err    error
	called bool
}

func (f *fakeCallTenantLookup) LookupTenant(_ context.Context, _ uuid.UUID) (uuid.UUID, error) {
	f.called = true
	if f.err != nil {
		return uuid.Nil, f.err
	}
	return f.want, nil
}

var _ rapi.CallTenantLookup = (*fakeCallTenantLookup)(nil)

// TestCallResolverAdapter_Get_HappyPath — well-formed UUID returns
// (tenant, nil).
func TestCallResolverAdapter_Get_HappyPath(t *testing.T) {
	t.Parallel()

	wantTenant := uuid.New()
	lookup := &fakeCallTenantLookup{want: wantTenant}
	a := newCallResolverAdapter(lookup)

	got, err := a.Get(t.Context(), uuid.New().String())
	require.NoError(t, err)
	assert.Equal(t, wantTenant.String(), got.TenantID)
	assert.True(t, lookup.called)
}

// TestCallResolverAdapter_Get_MalformedUUID — non-UUID string surfaces
// as a wrapped error (TopicRBAC folds into ErrCrossTenantSubscribe).
func TestCallResolverAdapter_Get_MalformedUUID(t *testing.T) {
	t.Parallel()

	lookup := &fakeCallTenantLookup{}
	a := newCallResolverAdapter(lookup)

	_, err := a.Get(t.Context(), "not-a-uuid")
	require.Error(t, err)
	assert.False(t, lookup.called, "lookup must not be called on malformed UUID")
}

// TestCallResolverAdapter_Get_LookupError — propagate the lookup error
// (TopicRBAC will fold into ErrCrossTenantSubscribe).
func TestCallResolverAdapter_Get_LookupError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("call not found")
	lookup := &fakeCallTenantLookup{err: wantErr}
	a := newCallResolverAdapter(lookup)

	_, err := a.Get(t.Context(), uuid.New().String())
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

// TestNewCallResolverAdapter_NilLookupPanics is the wiring guard.
func TestNewCallResolverAdapter_NilLookupPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { _ = newCallResolverAdapter(nil) })
}
