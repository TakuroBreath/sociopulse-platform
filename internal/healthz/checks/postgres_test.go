package checks

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(_ context.Context) error { return f.err }

func TestPostgresCheckOK(t *testing.T) {
	t.Parallel()
	c := PostgresCheck{Pool: fakePinger{}}
	require.NoError(t, c.Check(context.Background()))
	assert.Equal(t, "postgres", c.Name())
}

func TestPostgresCheckPropagatesError(t *testing.T) {
	t.Parallel()
	c := PostgresCheck{Pool: fakePinger{err: errors.New("conn refused")}}
	err := c.Check(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conn refused")
}
