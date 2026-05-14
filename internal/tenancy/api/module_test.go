package api_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/tenancy/api"
)

// stubKMSResolver is a no-op api.KMSResolver. Method bodies are deliberately
// minimal — the test only checks Module.KMSResolver returns the same pointer
// the caller registered, never that it functions.
type stubKMSResolver struct{}

func (stubKMSResolver) EnsureKEK(context.Context, uuid.UUID) (string, error) {
	return "", nil
}

func (stubKMSResolver) GenerateDataKey(context.Context, uuid.UUID) (api.DataKey, error) {
	return api.DataKey{}, nil
}

func (stubKMSResolver) Encrypt(context.Context, uuid.UUID, string, string, []byte) ([]byte, error) {
	return nil, nil
}

func (stubKMSResolver) Decrypt(context.Context, uuid.UUID, string, string, []byte) ([]byte, error) {
	return nil, nil
}

func (stubKMSResolver) InvalidateCache(uuid.UUID) {}

func TestModule_KMSResolver_RoundtripsTheValueSet(t *testing.T) {
	t.Parallel()

	resolver := stubKMSResolver{}
	mod := api.NewModule(api.Deps{}, nil, nil, nil)
	mod.SetKMSResolver(resolver)

	require.NotNil(t, mod.KMSResolver())
	require.Equal(t, resolver, mod.KMSResolver())
}

func TestModule_KMSResolver_DefaultsToNil(t *testing.T) {
	t.Parallel()

	mod := api.NewModule(api.Deps{}, nil, nil, nil)
	require.Nil(t, mod.KMSResolver(),
		"a fresh Module without SetKMSResolver must report nil — caller is expected to gate")
}
