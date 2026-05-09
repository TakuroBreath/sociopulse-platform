package api_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	authapi "github.com/sociopulse/platform/internal/auth/api"
)

// TestSubjectUserDeleted_Constant verifies the canonical subject literal
// matches the plan-11-realtime convention tenant.<t>.<area>.<entity>.<event>.
func TestSubjectUserDeleted_Constant(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "tenant.<t>.auth.user.deleted", authapi.SubjectUserDeleted)
}

// TestSubjectUserDeletedFor_RendersConcreteSubject verifies the
// for-tenant helper produces the runtime subject string the outbox
// relay publishes on.
func TestSubjectUserDeletedFor_RendersConcreteSubject(t *testing.T) {
	t.Parallel()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000abc")
	got := authapi.SubjectUserDeletedFor(tid)
	assert.Equal(t,
		"tenant.00000000-0000-0000-0000-000000000abc.auth.user.deleted",
		got,
	)
}
