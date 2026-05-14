package api

// Package api — analytics module events.
//
// analytics is a sink: it does not publish events of its own. The
// IngestPipeline consumes the durable JetStream stream ANALYTICS (24 h
// retention, explicit ack, max-ack-pending 20 000) bound to the subjects
// below and inserts each event into the matching ClickHouse table.
//
// Subjects (Plan 13.2 § Q4 + Q7):
//   - SubjectCallsAnalytics + SubjectOperatorStateAnalytics are
//     CROSS-TENANT (no tenant token in the subject). The dialer publishes
//     them with the tenant_id encoded in the payload; the ingester reads
//     it from there.
//   - SubjectRecordingUploadedWildcard is the wildcard binding the ingester
//     uses to receive per-tenant recording.uploaded publishes (the
//     concrete subject is tenant.<t>.recording.uploaded, materialised by
//     internal/recording/api.SubjectRecordingUploadedFor). The ingester
//     extracts tenant_id from the subject token, not the payload.
const (
	// SubjectCallsAnalytics is the cross-tenant denormalised call event
	// for ClickHouse. Sink table: events_calls.
	SubjectCallsAnalytics = "analytics.event.calls"
	// SubjectOperatorStateAnalytics is the cross-tenant denormalised
	// operator state row for ClickHouse. Sink table: events_operator_state.
	SubjectOperatorStateAnalytics = "analytics.event.operator_state"
	// SubjectRecordingUploadedWildcard is the per-tenant wildcard the
	// ingester subscribes to. Concrete subject:
	// tenant.<t>.recording.uploaded. Sink table: events_recording_uploaded.
	SubjectRecordingUploadedWildcard = "tenant.*.recording.uploaded"
)
