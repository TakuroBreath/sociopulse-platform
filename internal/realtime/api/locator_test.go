package api_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	rtapi "github.com/sociopulse/platform/internal/realtime/api"
)

// TestLocatorCallResolver_Constant pins the locator key string to its
// canonical form. Any drift between this constant and the value cmd/api
// uses to look up the resolver is a wiring bug; the constant is the
// single source of truth.
func TestLocatorCallResolver_Constant(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "realtime.CallResolver", rtapi.LocatorCallResolver)
}
