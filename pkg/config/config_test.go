package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigZeroValueIsInvalid(t *testing.T) {
	t.Parallel()
	var c Config
	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "service")
}

func TestConfigDevDefaults(t *testing.T) {
	t.Parallel()
	c := DefaultDev()
	require.NoError(t, c.Validate())
	assert.Equal(t, "development", c.Service.Env)
	assert.Equal(t, "debug", c.Service.LogLevel)
	assert.Equal(t, ":8080", c.HTTP.Bind)
	assert.Equal(t, ":8081", c.WS.Bind)
	assert.Equal(t, ":9090", c.Observability.Metrics.Bind)
	assert.Equal(t, ":9091", c.GRPC.Bind)
	assert.Equal(t, 10*time.Second, c.HTTP.ReadTimeout)
	assert.Equal(t, 30*time.Second, c.HTTP.WriteTimeout)
	assert.True(t, c.GRPC.ReflectionEnabled)
	assert.Equal(t, 15*time.Second, c.Shutdown.GracePeriod)
}

func TestConfigProductionRequiresLockboxSecrets(t *testing.T) {
	t.Parallel()
	c := DefaultDev()
	c.Service.Env = "production"
	c.Database.Postgres.DSN = "postgres://app@pgbouncer:6432/sociopulse?sslmode=require"
	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "production")
}
