package api

// Package api — analytics module events.
//
// analytics is a sink: it does not publish events of its own. The
// IngestPipeline consumes the durable JetStream stream ANALYTICS (24 h
// retention, explicit ack, max-ack-pending 20 000) bound to the subjects
// below and inserts each event into the matching ClickHouse table.
const (
	// SubjectCallFinalized is the dialer-published call.finalized event subject.
	// Sink table: events_calls.
	SubjectCallFinalized = "dialer.call.finalized"
	// SubjectOperatorStateChanged is the dialer-published operator.state.changed event subject.
	// Sink table: events_operator_state.
	SubjectOperatorStateChanged = "operator.state.changed"
	// SubjectRecordingUploaded is the recording-published recording.uploaded event subject.
	// Sink table: events_recording_uploaded.
	SubjectRecordingUploaded = "recording.uploaded"
)
