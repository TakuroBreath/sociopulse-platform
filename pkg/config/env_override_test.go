package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvOverridesYAML(t *testing.T) {
	// No t.Parallel(): t.Setenv is incompatible with parallel tests in Go.
	dir := t.TempDir()
	writeYAML(t, dir, fullDevYAML)
	t.Setenv("SOCIOPULSE_DATABASE_POSTGRES_DSN", "postgres://app:envpass@db:5432/sociopulse?sslmode=require")
	t.Setenv("SOCIOPULSE_HTTP_BIND", ":18080")

	snap, err := Load(LoadOptions{Dir: dir})
	require.NoError(t, err)
	c := snap.Get()
	assert.Equal(t, "postgres://app:envpass@db:5432/sociopulse?sslmode=require", c.Database.Postgres.DSN)
	assert.Equal(t, ":18080", c.HTTP.Bind)
}
