package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/tenancy/api"
	// blank import is what cmd/api will do — it triggers the init() that
	// installs api.Register.
	_ "github.com/sociopulse/platform/internal/tenancy/service"
)

func TestRegisterSeam_IsInstalledByInit(t *testing.T) {
	t.Parallel()

	require.NotNil(t, api.Register, "service.init must install api.Register")
}

func TestRegisterSeam_RejectsMissingLogger(t *testing.T) {
	t.Parallel()

	_, err := api.Register(context.Background(), api.Deps{})
	require.Error(t, err)
}

func TestRegisterSeam_RejectsMissingPool(t *testing.T) {
	t.Parallel()

	_, err := api.Register(context.Background(), api.Deps{
		Logger: zaptest.NewLogger(t),
	})
	require.Error(t, err)
}
