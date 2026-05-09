package nats_bridge //nolint:revive // package name mirrors the module's filesystem path

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	telapi "github.com/sociopulse/platform/internal/telephony/api"
)

// TestBuildCallURL_FormatsSofiaGateway documents the FS-side originate URL
// shape: sofia/gateway/<trunk>/<number>. Pulled into its own test so a
// future regression to the format surfaces a clear diff.
func TestBuildCallURL_FormatsSofiaGateway(t *testing.T) {
	t.Parallel()
	got := buildCallURL("primary", "+15551234567")
	assert.Equal(t, "sofia/gateway/primary/+15551234567", got)
}

// TestBuildOriginateRequest_PopulatesIdentityVariables asserts the
// identity-mapping behaviour: the cross-module DTO's tenant / call /
// command UUIDs land in FS channel variables under the sociopulse_*
// namespace, and optional fields (recording, operator_ext) propagate
// when set, omit when empty.
func TestBuildOriginateRequest_PopulatesIdentityVariables(t *testing.T) {
	t.Parallel()

	cmdID := uuid.New()
	tenantID := uuid.New()
	callID := uuid.New()

	cmd := telapi.OriginateCommand{
		CommandID:      cmdID,
		TenantID:       tenantID,
		CallID:         callID,
		OperatorExt:    "1001",
		Number:         "+15558880000",
		TrunkID:        "tr-1",
		RecordingPath:  "/var/rec/x.wav",
		CallerID:       "+15551110000",
		DialingTimeout: 30 * time.Second,
	}

	got := buildOriginateRequest(cmd)
	require.Equal(t, "sofia/gateway/tr-1/+15558880000", got.CallURL)
	assert.Equal(t, "+15551110000", got.Caller)
	assert.Equal(t, 30*time.Second, got.Timeout)

	require.NotNil(t, got.Variables)
	assert.Equal(t, cmdID.String(), got.Variables["sociopulse_command"])
	assert.Equal(t, tenantID.String(), got.Variables["sociopulse_tenant_id"])
	assert.Equal(t, callID.String(), got.Variables["sociopulse_call_id"])
	assert.Equal(t, "/var/rec/x.wav", got.Variables["recording_path"])
	assert.Equal(t, "1001", got.Variables["operator_ext"])
}

// TestBuildOriginateRequest_OmitsEmptyOptionalFields proves the optional
// channel variables (recording_path, operator_ext) are NOT set when the
// DTO leaves them empty — avoids pushing noisy "" values onto the FS
// originate variable string.
func TestBuildOriginateRequest_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	cmd := telapi.OriginateCommand{
		CommandID: uuid.New(),
		TenantID:  uuid.New(),
		CallID:    uuid.New(),
		Number:    "+15558880000",
		TrunkID:   "tr-1",
	}
	got := buildOriginateRequest(cmd)
	require.NotNil(t, got.Variables)
	_, hasRec := got.Variables["recording_path"]
	_, hasExt := got.Variables["operator_ext"]
	assert.False(t, hasRec, "recording_path must be omitted when empty")
	assert.False(t, hasExt, "operator_ext must be omitted when empty")
}
