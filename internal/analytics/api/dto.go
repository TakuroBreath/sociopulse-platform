// Package api defines public contracts for the analytics module.
// Other modules import only from this package — never from analytics/service or analytics/store.
//
// analytics is the read-side / sink module. It runs the NATS → ClickHouse
// ingest pipeline (explicit ack, dedup LRU on event_id, batched inserts at
// 10 000 rows or 5 s, exponential backoff with jitter, dead-letter on
// poison payloads), and exposes MetricsQuery for dashboards: calls,
// operator state, region progress, hourly buckets, operator comparisons.
// Results are cached in Redis with a 30 s TTL.
package api

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// EventKind enumerates the NATS event kinds the ingester accepts.
// Values mirror the subject constants in events.go so the kind tag is
// human-readable in logs / traces. EventEnvelope.Kind is currently
// informational only — the ingester routes on the bound subject, not
// the payload field — but the explicit enum is kept so unmarshalled
// payloads can be sanity-checked against the subject they arrived on.
type EventKind string

const (
	// EventKindCalls corresponds to SubjectCallsAnalytics.
	EventKindCalls EventKind = "analytics.event.calls"
	// EventKindOperatorState corresponds to SubjectOperatorStateAnalytics.
	EventKindOperatorState EventKind = "analytics.event.operator_state"
	// EventKindRecordingUploaded corresponds to the per-tenant
	// tenant.<t>.recording.uploaded subject (see
	// SubjectRecordingUploadedWildcard for the ingester binding).
	EventKindRecordingUploaded EventKind = "recording.uploaded"
)

// EventEnvelope is the canonical wrapper used to deliver any analytics-bound
// event over NATS. Payload is the kind-specific JSON body.
type EventEnvelope struct {
	EventID   uuid.UUID       `json:"event_id"`
	Kind      EventKind       `json:"kind"`
	TenantID  uuid.UUID       `json:"tenant_id"`
	Timestamp time.Time       `json:"ts"`
	Payload   json.RawMessage `json:"payload"`
}

// IngestStats is the runtime counters surface for /metrics.
type IngestStats struct {
	PerSubject map[string]SubjectStats
}

// SubjectStats is per-subject ingest counters.
type SubjectStats struct {
	Received   uint64
	Inserted   uint64
	Failed     uint64
	DeadLetter uint64
	LagSeconds float64
	LastError  string
}

// Window is the time range every MetricsQuery method takes.
// It is half-open [From, To). Validate enforces sane bounds.
type Window struct {
	From time.Time
	To   time.Time
}

// Validate returns ErrInvalidWindow when From≥To or the span exceeds 1 year.
func (w Window) Validate() error {
	if !w.From.Before(w.To) {
		return ErrInvalidWindow
	}
	if w.To.Sub(w.From) > 365*24*time.Hour {
		return ErrInvalidWindow
	}
	return nil
}

// CallsQuery narrows MetricsQuery.Calls.
type CallsQuery struct {
	TenantID  uuid.UUID
	ProjectID *uuid.UUID
	Window    Window
}

// CallsResult is the return of MetricsQuery.Calls.
type CallsResult struct {
	Total       uint64
	Successful  uint64
	Failed      uint64
	Refusals    uint64
	AvgDurSec   float64
	TotalDurSec uint64
	ByStatus    []StatusBucket
}

// StatusBucket is one row of the calls-by-status breakdown.
type StatusBucket struct {
	Status string
	Count  uint64
}

// OperatorStateQuery narrows MetricsQuery.OperatorState.
type OperatorStateQuery struct {
	TenantID   uuid.UUID
	OperatorID *uuid.UUID
	ProjectID  *uuid.UUID
	Window     Window
}

// OperatorStateBreakdown is the aggregated time spent in each FSM state.
type OperatorStateBreakdown struct {
	TalkSec  uint64
	PauseSec uint64
	ReadySec uint64
	WrapSec  uint64
}

// RegionProgressQuery narrows MetricsQuery.RegionProgress.
type RegionProgressQuery struct {
	TenantID  uuid.UUID
	ProjectID uuid.UUID
	Window    Window
}

// RegionProgressRow is one row of the per-region progress dashboard.
type RegionProgressRow struct {
	RegionCode string
	Done       uint64
	Plan       uint64
	Progress   float64
}

// HourlyQuery narrows MetricsQuery.Hourly.
type HourlyQuery struct {
	TenantID  uuid.UUID
	ProjectID *uuid.UUID
	Window    Window
}

// HourlyBucket is one row of the per-hour activity histogram.
type HourlyBucket struct {
	Hour      time.Time
	Count     uint64
	AvgDurSec float64
}

// OperatorComparisonsQuery narrows MetricsQuery.OperatorComparisons.
type OperatorComparisonsQuery struct {
	TenantID  uuid.UUID
	ProjectID uuid.UUID
	Window    Window
}

// OperatorComparisonRow is one row of the per-operator comparison report.
type OperatorComparisonRow struct {
	OperatorID   uuid.UUID
	DisplayName  string
	CallsTotal   uint64
	SuccessRate  float64
	AvgTalkSec   float64
	PauseShare   float64
	AboveTeamAvg bool
}

// OverviewQuery narrows ServiceRO.Overview.
type OverviewQuery struct {
	TenantID  uuid.UUID
	ProjectID *uuid.UUID
	Window    Window
}

// OverviewResult is the return of ServiceRO.Overview, aggregating four sub-queries.
type OverviewResult struct {
	Calls          CallsResult
	OperatorState  OperatorStateBreakdown
	RegionProgress []RegionProgressRow
	Hourly         []HourlyBucket
}
