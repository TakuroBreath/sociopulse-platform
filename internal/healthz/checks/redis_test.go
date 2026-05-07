package checks

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedisCheckOK(t *testing.T) {
	t.Parallel()
	c := RedisCheck{Client: fakePinger{}}
	require.NoError(t, c.Check(context.Background()))
	assert.Equal(t, "redis", c.Name())
}

func TestRedisCheckPropagatesError(t *testing.T) {
	t.Parallel()
	c := RedisCheck{Client: fakePinger{err: errors.New("redis: down")}}
	err := c.Check(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redis: down")
}
