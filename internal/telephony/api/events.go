package api

import (
	"fmt"

	"github.com/google/uuid"
)

// NATS subject placeholders for the durable JetStream stream TELEPHONY
// (7-day retention, explicit ack). The bridge publishes these and the
// dialer (and realtime) subscribe.
//
// The literal-string constants below contain "<t>" and "<call_id>"
// placeholders; the runtime materialises concrete subjects via the
// Subject<X>For helpers.
const (
	// SubjectChannelCreate is published on FreeSWITCH CHANNEL_CREATE.
	SubjectChannelCreate = "tenant.<t>.telephony.event.<call_id>.create"
	// SubjectChannelAnswer is published on CHANNEL_ANSWER.
	SubjectChannelAnswer = "tenant.<t>.telephony.event.<call_id>.answer"
	// SubjectChannelHangupComplete is published on CHANNEL_HANGUP_COMPLETE.
	SubjectChannelHangupComplete = "tenant.<t>.telephony.event.<call_id>.hangup_complete"
	// SubjectChannelBridge is published on CHANNEL_BRIDGE.
	SubjectChannelBridge = "tenant.<t>.telephony.event.<call_id>.bridge"
	// SubjectChannelUnbridge is published on CHANNEL_UNBRIDGE.
	SubjectChannelUnbridge = "tenant.<t>.telephony.event.<call_id>.unbridge"
	// SubjectChannelDTMF is published on DTMF.
	SubjectChannelDTMF = "tenant.<t>.telephony.event.<call_id>.dtmf"
	// SubjectChannelRecordStop is published on RECORD_STOP.
	SubjectChannelRecordStop = "tenant.<t>.telephony.event.<call_id>.record_stop"
	// SubjectBridgeHealth is the internal heartbeat subject.
	SubjectBridgeHealth = "tenant.<t>.telephony.bridge.health"
	// SubjectChannelEventWildcard matches every per-call event for a tenant.
	// Used by the dialer's subscriber.
	SubjectChannelEventWildcard = "tenant.<t>.telephony.event.>"

	// SubjectCommand is the inbound command subject (best-effort core-NATS).
	// The dialer publishes; the bridge consumes; the command type is in a
	// discriminator field on the message body.
	SubjectCommand = "tenant.<t>.telephony.cmd.<call_id>"
)

// SubjectChannelEventFor returns the concrete NATS subject for the given
// per-call event. eventType is one of the ChannelEventType constants
// (or its lowercase string equivalent for FS-internal events such as
// "create", "hangup_complete").
func SubjectChannelEventFor(tenantID, callID uuid.UUID, eventType string) string {
	return fmt.Sprintf("tenant.%s.telephony.event.%s.%s", tenantID, callID, eventType)
}

// SubjectBridgeHealthFor returns the concrete heartbeat subject for the tenant.
func SubjectBridgeHealthFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.telephony.bridge.health", tenantID)
}

// SubjectChannelEventWildcardFor returns the wildcard subject the dialer
// subscribes to for all per-call events from a tenant.
func SubjectChannelEventWildcardFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.telephony.event.>", tenantID)
}

// SubjectCommandFor returns the concrete inbound-command subject for the
// given tenant + callID.
func SubjectCommandFor(tenantID, callID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.telephony.cmd.%s", tenantID, callID)
}
