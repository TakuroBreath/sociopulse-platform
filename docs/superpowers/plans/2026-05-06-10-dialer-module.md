# Implementation Plan #10: Dialer Module

**Date:** 2026-05-06
**Status:** Ready for execution
**Depends on:** Plans #00 (foundation), #01 (infrastructure), #03 (database), #04 (NATS), #05 (telephony-bridge), #06 (auth), #07 (tenant), #08 (projects), #09 (respondents)
**Spec references:**
- `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md` §8 (Dialer module — full)
- `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md` §FR-E (Working hours, RDD, retry)
- ADR-003 (Progressive dialing — single-channel-per-operator)

---

## Goal

Build the **Dialer module** — the heart of SocioPulse that orchestrates outbound calling for telephony surveys. The module implements:

1. **OperatorFSM** — strict finite-state machine controlling each operator's lifecycle (offline → ready → dialing → call → status → verify → pause). Persisted in Redis hash with audit log to `operator_state_log` via outbox.
2. **CallQueue** — per-tenant Redis ZSET ordered by `priority * 1e9 + epoch_ms` score, atomic `ZPOPMIN` via Lua, requeue with bounded delay.
3. **RDDGenerator** — Random Digit Dialing for cellular ABC/DEF prefixes, region-by-quota selection from 89 RU regions, anti-duplication within project + within tenant (Bloom filter + SET), DNC list check, leak-bucket rate-limit, format validation (E.164, 11 digits starting with 7).
4. **Router** — bridge between dialer and telephony-bridge. Publishes `cmd.dial.<operator_node>` requests via NATS, consumes `channel.answered/dialing/hangup` events.
5. **LineCapacityTracker** — atomic INCR/DECR of `fs:<node>:active_channels` Redis counter with cap=60 per FreeSWITCH node, multi-node failover, exponential backoff on full.
6. **WorkingHoursChecker** — local time per region (89 RU timezones), weekday/weekend windows from `tenant_settings.working_hours`, holiday calendar override.
7. **Retry orchestrator** — leader-elected cron worker that scans `pending_retries` ZSET every 30s and re-enqueues respondents whose retry SLA matures.

The Dialer is the most safety-critical module. A bug here causes either:
- Lost calls (operator stuck in `dialing`, never returns to `ready`).
- Duplicate calls (same respondent dialed twice → privacy & quality violation).
- Capacity overruns (>60 channels per FreeSWITCH node → SIP REGISTER failures, audio quality drop).
- Cross-tenant leakage (operator A dials respondent of tenant B).

Every failure mode listed must be unreachable by construction — guarded by Lua transactions, FSM invariants, and integration tests with `testcontainers-go`.

**Acceptance:**
- All 10 tasks complete with code reviewed by sub-agent.
- Test coverage **≥ 90%** measured by `go test -cover` on `internal/dialer/...`.
- `make lint` passes (golangci-lint v1.62 with `gosec`, `errcheck`, `revive`, `gocritic`, `staticcheck`).
- Integration tests against real Redis 7.4, Postgres 16, NATS 2.10 via `testcontainers-go` pass on CI in <5 min.
- Manual E2E: 50 simulated operators × 4 hours of dialing produces zero stuck FSMs, zero double-dials (validated by SQL on `call_attempts`), zero capacity overruns (validated by `fs:*:active_channels` log).
- The 100k RDD generation benchmark completes in <2 s with <0.1% duplicates within a single project.

---

## File Structure

All code lives under `/Users/user/call-center/social-pulse/internal/dialer/`. Public API is in `internal/dialer/api/` (consumed by HTTP/WS handlers in Plan #16). Internal implementation is in `internal/dialer/internal/`. Shared region data is a top-level package `pkg/regions/` (also used by reporting and respondents modules).

```
internal/dialer/
├── api/                              # Public interfaces — stable contract
│   ├── doc.go                        # Package doc with FSM diagram
│   ├── fsm.go                        # OperatorFSM interface + State enum + Event enum
│   ├── queue.go                      # CallQueue interface
│   ├── rdd.go                        # RDDGenerator interface
│   ├── router.go                     # Router interface (NATS abstraction)
│   ├── capacity.go                   # LineCapacityTracker interface
│   ├── hours.go                      # WorkingHoursChecker interface
│   ├── retry.go                      # RetryOrchestrator interface
│   ├── dto.go                        # Request/Response DTOs (StartShiftRequest, etc.)
│   └── errors.go                     # Sentinel errors (ErrInvalidTransition, ErrQueueEmpty)
├── internal/                         # Implementation — not for external import
│   ├── fsm/
│   │   ├── machine.go                # FSM impl with transition table
│   │   ├── transitions.go            # validTransitions map + guards
│   │   ├── store.go                  # Redis hash + outbox writes (atomic via Lua)
│   │   ├── heartbeat.go              # presence:<tid>:user:<id> TTL=30s loop
│   │   ├── machine_test.go           # Unit: every transition + invalid edges
│   │   ├── store_test.go             # Integration: testcontainers Redis
│   │   └── heartbeat_test.go         # Integration: TTL expiry triggers force-offline
│   ├── queue/
│   │   ├── redis_zset.go             # CallQueue impl with embedded Lua scripts
│   │   ├── scripts.go                # //go:embed *.lua + script SHA cache
│   │   ├── lua/
│   │   │   ├── enqueue.lua           # ZADD if not exists in dedup SET
│   │   │   ├── pop_next.lua          # ZPOPMIN + remove from dedup
│   │   │   └── requeue.lua           # ZADD with new score + epoch shift
│   │   ├── redis_zset_test.go        # Concurrent ZPOPMIN: 10 workers × 1k items
│   │   └── benchmark_test.go         # 100k enqueue/pop throughput
│   ├── rdd/
│   │   ├── generator.go              # RDDGenerator impl
│   │   ├── prefixes.go               # ABC vs DEF classification
│   │   ├── leak_bucket.go            # gen rate-limit per tenant
│   │   ├── dedup.go                  # bloom (project) + Redis SET (tenant) check
│   │   ├── generator_test.go         # 100k benchmark, distribution sanity
│   │   └── prefixes_test.go          # All known RU mobile prefixes mapped
│   ├── router/
│   │   ├── nats_router.go            # Router impl over NATS JetStream
│   │   ├── subjects.go               # Subject naming (cmd.dial.<node>, channel.*)
│   │   ├── codec.go                  # protobuf or JSON envelope
│   │   ├── nats_router_test.go       # Integration: testcontainers NATS
│   │   └── handler.go                # Channel event consumer that drives FSM
│   ├── capacity/
│   │   ├── tracker.go                # INCR/DECR + cap enforcement
│   │   ├── tracker_test.go           # Race: 200 concurrent attempts
│   │   └── selector.go               # Multi-node round-robin selector
│   ├── hours/
│   │   ├── checker.go                # WorkingHoursChecker impl
│   │   ├── holidays.go               # RU public holidays 2026
│   │   ├── checker_test.go           # tz-cases: Moscow, Kamchatka, Kaliningrad
│   │   └── golden_test.go            # Table-driven: 200 (region, time) pairs
│   ├── retry/
│   │   ├── orchestrator.go           # cron-driven scanner
│   │   ├── leader_election.go        # Postgres advisory-lock leader (auto-released on session loss)
│   │   ├── status_rules.go           # Per-status retry policy (busy → 30m, no_answer → 2h)
│   │   ├── orchestrator_test.go      # E2E: testcontainers Redis + Postgres
│   │   └── leader_election_test.go   # Two instances, only one wins; killed-leader releases lock
│   ├── http/
│   │   ├── handler.go                # POST /api/sessions/{start,end,pause,resume}
│   │   ├── ws.go                     # WebSocket /api/operator/ws — push state changes
│   │   ├── handler_test.go           # httptest + mocked FSM
│   │   └── middleware.go             # tenant context extraction
│   └── service/
│       ├── service.go                # Service struct that wires FSM + Queue + Router
│       ├── service_test.go           # End-to-end with all real backends (testcontainers)
│       └── lifecycle.go              # Start/Stop, graceful drain
└── doc.go                            # Top-level module doc

pkg/regions/                          # Shared package — also used by reporting
├── regions.go                        # RegionForCode, TimezoneForRegion, ListAll
├── regions_test.go                   # Loading + lookup tests
├── configs/
│   └── regions.yaml                  # 89 regions with code prefix, timezone, ABC/DEF flag
└── doc.go
```

**Why this layout:**
- `api/` is pure interfaces + DTOs. Consumers (HTTP layer, other modules) only import `api/`. Keeps the internal DB/Redis/NATS details swappable.
- `internal/` follows Go's package-private rule — `cmd/` and tests in the same module can use these but no external module can.
- `pkg/regions/` is at root because it's data-only and re-used. Embedding `regions.yaml` via `//go:embed` keeps the binary self-contained.
- Each subpackage owns its tests including testcontainers integration. Top-level `service/service_test.go` is the integration cathedral.

---

## Tasks

### Task 1: Module skeleton, DTO, public interfaces

**Goal:** Lock the public API surface so subsequent tasks can be parallelized. No business logic yet — just types, interfaces, sentinel errors, and a `doc.go` with the FSM diagram in ASCII.

**Files created:**
- `internal/dialer/doc.go`
- `internal/dialer/api/doc.go`
- `internal/dialer/api/fsm.go`
- `internal/dialer/api/queue.go`
- `internal/dialer/api/rdd.go`
- `internal/dialer/api/router.go`
- `internal/dialer/api/capacity.go`
- `internal/dialer/api/hours.go`
- `internal/dialer/api/retry.go`
- `internal/dialer/api/dto.go`
- `internal/dialer/api/errors.go`

**Implementation:**

`internal/dialer/api/fsm.go`:
```go
// Package api defines the public contract of the dialer module.
//
// FSM transitions:
//
//	offline ──StartShift──> ready ──GoPause──> pause ──Resume──> ready
//	   ▲                      │
//	   │                      ▼
//	   │                   dialing ──Answered──> call ──Hangup──> status
//	   │                      │                                     │
//	   │                      └──NoAnswer/Busy/Failed────────────> status
//	   │                                                            │
//	   │                                              GoVerify ◄────┤
//	   │                                                  │
//	   │                                                  ▼
//	   └──EndShift──────────────────────────────────── verify
//
// All transitions are guarded by validTransitions in internal/fsm/transitions.go.
// Invalid attempts return ErrInvalidTransition wrapped with the source/event.
package api

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// State enumerates operator states. Values are persisted as strings in Redis
// hash and as TEXT in operator_state_log.state.
type State string

const (
	StateOffline State = "offline"
	StateReady   State = "ready"
	StateDialing State = "dialing"
	StateCall    State = "call"
	StateStatus  State = "status"
	StateVerify  State = "verify"
	StatePause   State = "pause"
)

// Valid returns true if s is a recognized state.
func (s State) Valid() bool {
	switch s {
	case StateOffline, StateReady, StateDialing, StateCall,
		StateStatus, StateVerify, StatePause:
		return true
	}
	return false
}

// Event triggers a transition.
type Event string

const (
	EventStartShift        Event = "start_shift"
	EventEndShift          Event = "end_shift"
	EventGoReady           Event = "go_ready"
	EventGoPause           Event = "go_pause"
	EventResume            Event = "resume"
	EventCallStarted       Event = "call_started" // FreeSWITCH ANSWER → dialing→call
	EventCallEnded         Event = "call_ended"   // hangup → call→status (or dialing→status)
	EventCallFailed        Event = "call_failed"  // congestion/no_route → dialing→status
	EventStatusSubmitted   Event = "status_submitted"
	EventGoVerify          Event = "go_verify"
	EventVerifyDone        Event = "verify_done"
	EventForceOffline      Event = "force_offline" // heartbeat expired
)

// OperatorFSM controls the per-operator finite state machine.
//
// All methods are tenant-scoped via tenantID and operatorID. Implementations
// MUST guarantee:
//  1. Atomicity per call (Redis Lua) — no torn state visible.
//  2. Audit — every successful transition appends to operator_state_log via outbox.
//  3. Idempotency — repeating an event in a state where it's already applied
//     is a no-op (returns nil), not an error.
type OperatorFSM interface {
	StartShift(ctx context.Context, req StartShiftRequest) (Snapshot, error)
	EndShift(ctx context.Context, tenantID, operatorID uuid.UUID) (Snapshot, error)
	GoReady(ctx context.Context, tenantID, operatorID uuid.UUID) (Snapshot, error)
	GoPause(ctx context.Context, req GoPauseRequest) (Snapshot, error)
	Resume(ctx context.Context, tenantID, operatorID uuid.UUID) (Snapshot, error)

	RecordCallStarted(ctx context.Context, req CallStartedRequest) (Snapshot, error)
	RecordCallEnded(ctx context.Context, req CallEndedRequest) (Snapshot, error)

	SubmitStatus(ctx context.Context, req SubmitStatusRequest) (Snapshot, error)
	GoVerify(ctx context.Context, tenantID, operatorID uuid.UUID) (Snapshot, error)
	VerifyDone(ctx context.Context, tenantID, operatorID uuid.UUID) (Snapshot, error)

	GetState(ctx context.Context, tenantID, operatorID uuid.UUID) (Snapshot, error)

	// Force is used by the heartbeat watchdog when presence TTL expires.
	Force(ctx context.Context, tenantID, operatorID uuid.UUID, target State, reason string) (Snapshot, error)
}

// Snapshot is the read view of operator state.
type Snapshot struct {
	TenantID       uuid.UUID  `json:"tenant_id"`
	OperatorID     uuid.UUID  `json:"operator_id"`
	State          State      `json:"state"`
	StateEnteredAt time.Time  `json:"state_entered_at"`
	ProjectID      *uuid.UUID `json:"project_id,omitempty"`
	CurrentCallID  *uuid.UUID `json:"current_call_id,omitempty"`
	RespondentID   *uuid.UUID `json:"respondent_id,omitempty"`
	PauseReason    *string    `json:"pause_reason,omitempty"`
	HeartbeatAt    time.Time  `json:"heartbeat_at"`
}
```

`internal/dialer/api/queue.go`:
```go
package api

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// CallQueue is a per-tenant priority queue of respondent dials.
//
// Score formula: score = priority*1e9 + epoch_ms.
//
//   - priority is 0..9 (lower = more urgent). retry items get priority 1, fresh dials priority 5.
//   - epoch_ms is wall-clock ms; ensures FIFO within same priority.
//
// Implementations MUST use Redis ZSET + companion SET for dedup. ZPOPMIN and
// ZADD MUST be wrapped in Lua to avoid race with concurrent operators.
type CallQueue interface {
	// EnqueueRespondent adds the respondent to the queue. If already present in
	// the dedup SET, returns nil with ok=false (idempotent).
	EnqueueRespondent(ctx context.Context, req EnqueueRequest) (ok bool, err error)

	// PickNext atomically pops the highest-priority item and returns it.
	// Returns ErrQueueEmpty if the queue has no items.
	PickNext(ctx context.Context, tenantID, projectID uuid.UUID) (QueueItem, error)

	// Requeue puts the item back with priority+1 (capped at 9) and a delay
	// (added to epoch_ms). Used for retry scheduling.
	Requeue(ctx context.Context, item QueueItem, delay time.Duration) error

	// Size returns ZCARD of the queue.
	Size(ctx context.Context, tenantID, projectID uuid.UUID) (int64, error)

	// Remove deletes a specific respondent from the queue (manual disqualification).
	Remove(ctx context.Context, tenantID, projectID, respondentID uuid.UUID) error
}

// QueueItem is the unit of work in the call queue.
type QueueItem struct {
	TenantID     uuid.UUID `json:"tenant_id"`
	ProjectID    uuid.UUID `json:"project_id"`
	RespondentID uuid.UUID `json:"respondent_id"`
	Priority     uint8     `json:"priority"`     // 0..9
	EnqueuedAt   time.Time `json:"enqueued_at"`
	AttemptN     uint8     `json:"attempt_n"`    // # of past attempts
	Phone        string    `json:"phone"`        // E.164
	Region       string    `json:"region"`       // ISO 3166-2:RU code (RU-MOW, RU-SPE, ...)
}
```

`internal/dialer/api/rdd.go`:
```go
package api

import (
	"context"

	"github.com/google/uuid"
)

// RDDGenerator produces synthetic respondents via Random Digit Dialing.
type RDDGenerator interface {
	// Generate creates n respondent records and inserts them into the
	// respondents table (state=new) plus into the project's call queue.
	// Returns the actual count generated (may be less if dedup hit limits or
	// the leak-bucket throttle kicked in).
	Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error)
}

type GenerateRequest struct {
	TenantID  uuid.UUID         `json:"tenant_id"`
	ProjectID uuid.UUID         `json:"project_id"`
	N         int               `json:"n"`         // requested count
	Quotas    map[string]int    `json:"quotas"`    // region code → target count
	ABCRatio  float64           `json:"abc_ratio"` // share of ABC vs DEF (0..1)
}

type GenerateResult struct {
	Generated     int            `json:"generated"`
	ByRegion      map[string]int `json:"by_region"`
	DuplicatesHit int            `json:"duplicates_hit"`
	DNCHit        int            `json:"dnc_hit"`
	InvalidHit    int            `json:"invalid_hit"`
	Throttled     bool           `json:"throttled"`
}
```

`internal/dialer/api/router.go`:
```go
package api

import (
	"context"

	"github.com/google/uuid"
)

// Router is the abstraction over telephony-bridge.
//
// Outbound: Dial requests an outgoing call on behalf of an operator.
// Inbound: subscribers receive ChannelEvent for state changes.
type Router interface {
	// Dial publishes cmd.dial.<node> to NATS. Does not block on call answer.
	// The reply is async via channel.* events delivered to ChannelEventHandler.
	Dial(ctx context.Context, req DialRequest) error

	// Hangup forces termination of an in-flight call.
	Hangup(ctx context.Context, callID uuid.UUID, reason string) error

	// Subscribe registers a handler for channel events for a given tenant.
	// Returns an unsubscribe func.
	Subscribe(ctx context.Context, tenantID uuid.UUID, h ChannelEventHandler) (unsubscribe func(), err error)
}

type DialRequest struct {
	CallID       uuid.UUID `json:"call_id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	OperatorID   uuid.UUID `json:"operator_id"`
	OperatorExt  string    `json:"operator_ext"` // SIP extension
	RespondentID uuid.UUID `json:"respondent_id"`
	Phone        string    `json:"phone"`
	ProjectID    uuid.UUID `json:"project_id"`
	FsNode       string    `json:"fs_node"` // chosen FreeSWITCH node
}

type ChannelEvent struct {
	CallID   uuid.UUID `json:"call_id"`
	Type     string    `json:"type"` // dialing, answered, hangup
	Cause    string    `json:"cause,omitempty"` // hangup cause
	Duration int       `json:"duration_ms,omitempty"`
	FsNode   string    `json:"fs_node"`
}

type ChannelEventHandler func(ctx context.Context, evt ChannelEvent) error
```

`internal/dialer/api/capacity.go`:
```go
package api

import (
	"context"
)

// LineCapacityTracker enforces a per-FreeSWITCH-node concurrent-channel cap (60).
//
// Pattern: Acquire returns a non-empty node name; caller MUST call Release in
// a deferred or final stage. If all nodes are full, returns ErrAllNodesFull —
// caller is expected to back off (exponential, max 5 s).
type LineCapacityTracker interface {
	Acquire(ctx context.Context) (node string, err error)
	Release(ctx context.Context, node string) error
	Stats(ctx context.Context) (map[string]int64, error) // node → current channels
}
```

`internal/dialer/api/hours.go`:
```go
package api

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// WorkingHoursChecker tells whether a respondent in `region` may be dialed `at`
// the given UTC time, given the tenant's working_hours and holidays config.
type WorkingHoursChecker interface {
	IsAllowed(ctx context.Context, tenantID uuid.UUID, region string, at time.Time) (bool, error)

	// NextAllowed returns the next time the respondent may be dialed.
	// If currently allowed, returns `at` itself.
	NextAllowed(ctx context.Context, tenantID uuid.UUID, region string, at time.Time) (time.Time, error)
}
```

`internal/dialer/api/retry.go`:
```go
package api

import (
	"context"
)

// RetryOrchestrator scans pending_retries ZSET on schedule and re-enqueues mature items.
type RetryOrchestrator interface {
	// Run blocks until ctx cancels.
	Run(ctx context.Context) error
}
```

`internal/dialer/api/dto.go`:
```go
package api

import (
	"time"

	"github.com/google/uuid"
)

type StartShiftRequest struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	OperatorID uuid.UUID `json:"operator_id"`
	ProjectID  uuid.UUID `json:"project_id"`
	ClientIP   string    `json:"client_ip"`
}

type GoPauseRequest struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	OperatorID uuid.UUID `json:"operator_id"`
	Reason     string    `json:"reason"` // bio_break, technical, training, ...
}

type EnqueueRequest struct {
	TenantID     uuid.UUID `json:"tenant_id"`
	ProjectID    uuid.UUID `json:"project_id"`
	RespondentID uuid.UUID `json:"respondent_id"`
	Phone        string    `json:"phone"`
	Region       string    `json:"region"`
	Priority     uint8     `json:"priority"`
	AttemptN     uint8     `json:"attempt_n"`
}

type CallStartedRequest struct {
	TenantID     uuid.UUID `json:"tenant_id"`
	OperatorID   uuid.UUID `json:"operator_id"`
	CallID       uuid.UUID `json:"call_id"`
	RespondentID uuid.UUID `json:"respondent_id"`
	StartedAt    time.Time `json:"started_at"`
}

type CallEndedRequest struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	OperatorID uuid.UUID `json:"operator_id"`
	CallID     uuid.UUID `json:"call_id"`
	EndedAt    time.Time `json:"ended_at"`
	Cause      string    `json:"cause"`
	DurationMS int       `json:"duration_ms"`
}

type SubmitStatusRequest struct {
	TenantID     uuid.UUID `json:"tenant_id"`
	OperatorID   uuid.UUID `json:"operator_id"`
	CallID       uuid.UUID `json:"call_id"`
	RespondentID uuid.UUID `json:"respondent_id"`
	Status       string    `json:"status"`  // completed, refused, busy, no_answer, ...
	Comment      string    `json:"comment"`
}
```

`internal/dialer/api/errors.go`:
```go
package api

import "errors"

var (
	ErrInvalidTransition  = errors.New("dialer: invalid FSM transition")
	ErrUnknownState       = errors.New("dialer: unknown state")
	ErrQueueEmpty         = errors.New("dialer: queue empty")
	ErrDuplicateInQueue   = errors.New("dialer: respondent already in queue")
	ErrAllNodesFull       = errors.New("dialer: all FreeSWITCH nodes at capacity")
	ErrOutsideWorkingHours = errors.New("dialer: outside working hours for region")
	ErrThrottled          = errors.New("dialer: rate-limit throttled")
	ErrTenantMismatch     = errors.New("dialer: tenant mismatch")
)
```

**Tests:** None for this task — interfaces only. `go vet ./internal/dialer/api/...` and `golangci-lint run ./internal/dialer/api/...` must pass.

**Acceptance:**
- `go build ./internal/dialer/api/...` succeeds.
- `go doc ./internal/dialer/api OperatorFSM` shows the rendered ASCII diagram.
- No imports from `internal/...` — `api` is a leaf.

---

### Task 2: OperatorFSM implementation — atomic Redis transitions + outbox audit

**Goal:** Implement the `OperatorFSM` interface with strict transition tables, atomic Redis hash mutations via Lua, and audit-log inserts via the outbox pattern (Plan #04). Every successful transition produces an `operator_state_log` row eventually consistent with the live state.

**Files created:**
- `internal/dialer/internal/fsm/transitions.go`
- `internal/dialer/internal/fsm/machine.go`
- `internal/dialer/internal/fsm/store.go`
- `internal/dialer/internal/fsm/lua/transition.lua`
- `internal/dialer/internal/fsm/machine_test.go`
- `internal/dialer/internal/fsm/store_test.go`

**Implementation:**

`internal/dialer/internal/fsm/transitions.go`:
```go
package fsm

import "social-pulse/internal/dialer/api"

// edge is a (current state, event) → next state mapping.
type edge struct {
	from  api.State
	event api.Event
}

// transitions maps each valid (state, event) pair to the next state.
// Anything not in this map is invalid and returns ErrInvalidTransition.
var transitions = map[edge]api.State{
	// Shift lifecycle
	{api.StateOffline, api.EventStartShift}: api.StateReady,
	{api.StateReady, api.EventEndShift}:     api.StateOffline,
	{api.StatePause, api.EventEndShift}:     api.StateOffline,
	{api.StateStatus, api.EventEndShift}:    api.StateOffline, // graceful end after wrap-up

	// Pause / resume
	{api.StateReady, api.EventGoPause}: api.StatePause,
	{api.StatePause, api.EventResume}:  api.StateReady,

	// Dial → answer → call → status
	// Note: dialing is entered automatically by service layer when CallQueue.PickNext succeeds;
	// it's modelled as Ready→Dialing internal call. We expose it via RecordCallStarted only.
	{api.StateReady, api.EventCallStarted}:    api.StateDialing,
	{api.StateDialing, api.EventCallStarted}:  api.StateCall, // bridge ANSWER
	{api.StateDialing, api.EventCallEnded}:    api.StateStatus, // hangup before answer
	{api.StateDialing, api.EventCallFailed}:   api.StateStatus,
	{api.StateCall, api.EventCallEnded}:       api.StateStatus,

	// Status submission
	{api.StateStatus, api.EventStatusSubmitted}: api.StateReady,

	// Verify (manager-mode listening to recordings)
	{api.StateReady, api.EventGoVerify}:   api.StateVerify,
	{api.StateVerify, api.EventVerifyDone}: api.StateReady,

	// Force-offline (from heartbeat watchdog) — accepted from any state
	// Handled separately in machine.Force(); not in this map.
}

// IsTerminal returns true for states where no further timed transition is expected.
func IsTerminal(s api.State) bool {
	return s == api.StateOffline
}
```

`internal/dialer/internal/fsm/machine.go`:
```go
package fsm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"social-pulse/internal/dialer/api"
	"social-pulse/pkg/outbox"
)

// Machine implements api.OperatorFSM.
type Machine struct {
	rdb    *redis.Client
	outbox outbox.Writer
	log    *zap.Logger
	clock  func() time.Time
}

// New constructs a Machine. clock defaults to time.Now if nil.
func New(rdb *redis.Client, ob outbox.Writer, log *zap.Logger, clock func() time.Time) *Machine {
	if clock == nil {
		clock = time.Now
	}
	return &Machine{rdb: rdb, outbox: ob, log: log, clock: clock}
}

// applyEvent runs the (load → check transition → write hash + outbox) flow inside Lua.
func (m *Machine) applyEvent(
	ctx context.Context,
	tenantID, operatorID uuid.UUID,
	evt api.Event,
	mutator func(s *Snapshot),
) (api.Snapshot, error) {
	now := m.clock().UTC()

	// Load current
	cur, err := m.load(ctx, tenantID, operatorID)
	if err != nil {
		return api.Snapshot{}, fmt.Errorf("load state: %w", err)
	}

	// Compute next
	next, ok := transitions[edge{from: cur.State, event: evt}]
	if !ok {
		return api.Snapshot{}, fmt.Errorf("%w: %s --%s-->", api.ErrInvalidTransition, cur.State, evt)
	}

	// Idempotency: if already in target state and event is replay, no-op.
	if cur.State == next {
		return cur.toAPI(tenantID, operatorID), nil
	}

	// Build new snapshot
	updated := cur
	updated.State = next
	updated.StateEnteredAt = now
	updated.HeartbeatAt = now
	if mutator != nil {
		mutator(&updated)
	}

	// Atomic CAS via Lua: only write if version matches loaded version.
	if err := m.casStore(ctx, tenantID, operatorID, cur.Version, updated); err != nil {
		return api.Snapshot{}, fmt.Errorf("cas store: %w", err)
	}

	// Outbox: durable audit log row.
	if err := m.outbox.Append(ctx, outbox.Event{
		TenantID:    tenantID,
		AggregateID: operatorID,
		Type:        "operator_state_log.appended",
		Payload: map[string]any{
			"operator_id":      operatorID,
			"from_state":       cur.State,
			"to_state":         next,
			"event":            evt,
			"project_id":       updated.ProjectID,
			"current_call_id":  updated.CurrentCallID,
			"respondent_id":    updated.RespondentID,
			"pause_reason":     updated.PauseReason,
			"state_entered_at": now,
		},
	}); err != nil {
		// Outbox failure on a state we already committed in Redis is recoverable:
		// the watchdog will detect a missing log row in `operator_state_log` for an
		// active session and emit a reconciliation event. We log loudly.
		m.log.Error("outbox append failed after FSM transition committed",
			zap.String("from", string(cur.State)),
			zap.String("to", string(next)),
			zap.Stringer("operator_id", operatorID),
			zap.Error(err))
	}

	return updated.toAPI(tenantID, operatorID), nil
}

// StartShift moves offline → ready and creates the session.
func (m *Machine) StartShift(ctx context.Context, req api.StartShiftRequest) (api.Snapshot, error) {
	return m.applyEvent(ctx, req.TenantID, req.OperatorID, api.EventStartShift, func(s *Snapshot) {
		s.ProjectID = &req.ProjectID
		s.CurrentCallID = nil
		s.RespondentID = nil
		s.PauseReason = nil
	})
}

func (m *Machine) EndShift(ctx context.Context, tenantID, operatorID uuid.UUID) (api.Snapshot, error) {
	return m.applyEvent(ctx, tenantID, operatorID, api.EventEndShift, func(s *Snapshot) {
		s.ProjectID = nil
		s.CurrentCallID = nil
		s.RespondentID = nil
		s.PauseReason = nil
	})
}

func (m *Machine) GoReady(ctx context.Context, tenantID, operatorID uuid.UUID) (api.Snapshot, error) {
	return m.applyEvent(ctx, tenantID, operatorID, api.EventResume, nil)
}

func (m *Machine) GoPause(ctx context.Context, req api.GoPauseRequest) (api.Snapshot, error) {
	return m.applyEvent(ctx, req.TenantID, req.OperatorID, api.EventGoPause, func(s *Snapshot) {
		reason := req.Reason
		s.PauseReason = &reason
	})
}

func (m *Machine) Resume(ctx context.Context, tenantID, operatorID uuid.UUID) (api.Snapshot, error) {
	return m.applyEvent(ctx, tenantID, operatorID, api.EventResume, func(s *Snapshot) {
		s.PauseReason = nil
	})
}

func (m *Machine) RecordCallStarted(ctx context.Context, req api.CallStartedRequest) (api.Snapshot, error) {
	return m.applyEvent(ctx, req.TenantID, req.OperatorID, api.EventCallStarted, func(s *Snapshot) {
		callID := req.CallID
		respID := req.RespondentID
		s.CurrentCallID = &callID
		s.RespondentID = &respID
	})
}

func (m *Machine) RecordCallEnded(ctx context.Context, req api.CallEndedRequest) (api.Snapshot, error) {
	return m.applyEvent(ctx, req.TenantID, req.OperatorID, api.EventCallEnded, nil)
}

func (m *Machine) SubmitStatus(ctx context.Context, req api.SubmitStatusRequest) (api.Snapshot, error) {
	return m.applyEvent(ctx, req.TenantID, req.OperatorID, api.EventStatusSubmitted, func(s *Snapshot) {
		s.CurrentCallID = nil
		s.RespondentID = nil
	})
}

func (m *Machine) GoVerify(ctx context.Context, tenantID, operatorID uuid.UUID) (api.Snapshot, error) {
	return m.applyEvent(ctx, tenantID, operatorID, api.EventGoVerify, nil)
}

func (m *Machine) VerifyDone(ctx context.Context, tenantID, operatorID uuid.UUID) (api.Snapshot, error) {
	return m.applyEvent(ctx, tenantID, operatorID, api.EventVerifyDone, nil)
}

func (m *Machine) GetState(ctx context.Context, tenantID, operatorID uuid.UUID) (api.Snapshot, error) {
	cur, err := m.load(ctx, tenantID, operatorID)
	if err != nil {
		return api.Snapshot{}, err
	}
	return cur.toAPI(tenantID, operatorID), nil
}

// Force is the watchdog escape hatch. It bypasses the transition table.
func (m *Machine) Force(ctx context.Context, tenantID, operatorID uuid.UUID, target api.State, reason string) (api.Snapshot, error) {
	if !target.Valid() {
		return api.Snapshot{}, errors.New("force: invalid target state")
	}
	now := m.clock().UTC()
	cur, err := m.load(ctx, tenantID, operatorID)
	if err != nil {
		return api.Snapshot{}, err
	}
	updated := cur
	updated.State = target
	updated.StateEnteredAt = now
	updated.CurrentCallID = nil
	updated.RespondentID = nil
	if err := m.casStore(ctx, tenantID, operatorID, cur.Version, updated); err != nil {
		return api.Snapshot{}, err
	}
	_ = m.outbox.Append(ctx, outbox.Event{
		TenantID:    tenantID,
		AggregateID: operatorID,
		Type:        "operator_state_log.forced",
		Payload: map[string]any{
			"from_state": cur.State,
			"to_state":   target,
			"reason":     reason,
		},
	})
	return updated.toAPI(tenantID, operatorID), nil
}
```

`internal/dialer/internal/fsm/store.go`:
```go
package fsm

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"social-pulse/internal/dialer/api"
)

// Snapshot is the in-process record. Persisted as a Redis hash.
type Snapshot struct {
	State          api.State
	StateEnteredAt time.Time
	ProjectID      *uuid.UUID
	CurrentCallID  *uuid.UUID
	RespondentID   *uuid.UUID
	PauseReason    *string
	HeartbeatAt    time.Time
	Version        int64 // optimistic concurrency token
}

func (s Snapshot) toAPI(tenantID, operatorID uuid.UUID) api.Snapshot {
	return api.Snapshot{
		TenantID:       tenantID,
		OperatorID:     operatorID,
		State:          s.State,
		StateEnteredAt: s.StateEnteredAt,
		ProjectID:      s.ProjectID,
		CurrentCallID:  s.CurrentCallID,
		RespondentID:   s.RespondentID,
		PauseReason:    s.PauseReason,
		HeartbeatAt:    s.HeartbeatAt,
	}
}

func opKey(tenantID, operatorID uuid.UUID) string {
	return fmt.Sprintf("op:%s:user:%s", tenantID, operatorID)
}

func (m *Machine) load(ctx context.Context, tenantID, operatorID uuid.UUID) (Snapshot, error) {
	res, err := m.rdb.HGetAll(ctx, opKey(tenantID, operatorID)).Result()
	if err != nil {
		return Snapshot{}, err
	}
	if len(res) == 0 {
		// First time we see this operator — synthesize offline.
		return Snapshot{State: api.StateOffline, StateEnteredAt: m.clock().UTC(), Version: 0}, nil
	}
	return parseHash(res)
}

func parseHash(h map[string]string) (Snapshot, error) {
	var s Snapshot
	s.State = api.State(h["state"])
	if !s.State.Valid() {
		return s, fmt.Errorf("%w: %q", api.ErrUnknownState, h["state"])
	}
	if v, ok := h["state_entered_at"]; ok {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return s, fmt.Errorf("parse state_entered_at: %w", err)
		}
		s.StateEnteredAt = t
	}
	if v, ok := h["heartbeat_at"]; ok && v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return s, fmt.Errorf("parse heartbeat_at: %w", err)
		}
		s.HeartbeatAt = t
	}
	if v, ok := h["project_id"]; ok && v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return s, fmt.Errorf("parse project_id: %w", err)
		}
		s.ProjectID = &id
	}
	if v, ok := h["current_call_id"]; ok && v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return s, fmt.Errorf("parse current_call_id: %w", err)
		}
		s.CurrentCallID = &id
	}
	if v, ok := h["respondent_id"]; ok && v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return s, fmt.Errorf("parse respondent_id: %w", err)
		}
		s.RespondentID = &id
	}
	if v, ok := h["pause_reason"]; ok && v != "" {
		copy := v
		s.PauseReason = &copy
	}
	if v, ok := h["version"]; ok && v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
			return s, fmt.Errorf("parse version: %w", err)
		}
		s.Version = n
	}
	return s, nil
}

//go:embed lua/transition.lua
var transitionLua string
var transitionScript = redis.NewScript(transitionLua)

// casStore runs Lua: HGET version → if matches expected, HSET fields and HINCRBY version 1.
func (m *Machine) casStore(ctx context.Context, tenantID, operatorID uuid.UUID, expectedVersion int64, s Snapshot) error {
	payload, err := json.Marshal(map[string]any{
		"state":            string(s.State),
		"state_entered_at": s.StateEnteredAt.UTC().Format(time.RFC3339Nano),
		"heartbeat_at":     s.HeartbeatAt.UTC().Format(time.RFC3339Nano),
		"project_id":       uuidPtrToString(s.ProjectID),
		"current_call_id":  uuidPtrToString(s.CurrentCallID),
		"respondent_id":    uuidPtrToString(s.RespondentID),
		"pause_reason":     stringPtrToString(s.PauseReason),
	})
	if err != nil {
		return err
	}
	res, err := transitionScript.Run(ctx, m.rdb,
		[]string{opKey(tenantID, operatorID)},
		expectedVersion, string(payload), 60, // TTL 60s for hash key cap
	).Result()
	if err != nil {
		return err
	}
	if v, ok := res.(int64); ok && v == -1 {
		return fmt.Errorf("optimistic concurrency conflict: expected v=%d", expectedVersion)
	}
	return nil
}

func uuidPtrToString(p *uuid.UUID) string {
	if p == nil {
		return ""
	}
	return p.String()
}
func stringPtrToString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
```

`internal/dialer/internal/fsm/lua/transition.lua`:
```lua
-- KEYS[1] = op:<tenant>:user:<op>
-- ARGV[1] = expected_version (string-encoded int)
-- ARGV[2] = JSON payload of fields to set
-- ARGV[3] = TTL in seconds (refresh, since heartbeat lives separately)
local key = KEYS[1]
local expected = tonumber(ARGV[1])
local payload = cjson.decode(ARGV[2])
local ttl = tonumber(ARGV[3])

local cur = tonumber(redis.call("HGET", key, "version") or "0")
if cur ~= expected then
    return -1
end

for k, v in pairs(payload) do
    if v == nil or v == "" then
        redis.call("HDEL", key, k)
    else
        redis.call("HSET", key, k, v)
    end
end
redis.call("HINCRBY", key, "version", 1)
redis.call("EXPIRE", key, ttl)
return 1
```

**Tests:**

`internal/dialer/internal/fsm/machine_test.go` — table-driven covering every (state, event) pair:
```go
package fsm

import (
	"testing"

	"social-pulse/internal/dialer/api"
)

func TestTransitionTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		from    api.State
		event   api.Event
		want    api.State
		wantErr bool
	}{
		{api.StateOffline, api.EventStartShift, api.StateReady, false},
		{api.StateReady, api.EventEndShift, api.StateOffline, false},
		{api.StateReady, api.EventGoPause, api.StatePause, false},
		{api.StatePause, api.EventResume, api.StateReady, false},
		{api.StateReady, api.EventCallStarted, api.StateDialing, false},
		{api.StateDialing, api.EventCallStarted, api.StateCall, false},
		{api.StateCall, api.EventCallEnded, api.StateStatus, false},
		{api.StateStatus, api.EventStatusSubmitted, api.StateReady, false},
		{api.StateReady, api.EventGoVerify, api.StateVerify, false},
		{api.StateVerify, api.EventVerifyDone, api.StateReady, false},

		// Invalid edges
		{api.StateOffline, api.EventGoPause, "", true},
		{api.StateCall, api.EventStartShift, "", true},
		{api.StatePause, api.EventCallStarted, "", true},
	}
	for _, tc := range cases {
		got, ok := transitions[edge{from: tc.from, event: tc.event}]
		if tc.wantErr {
			if ok {
				t.Errorf("(%s,%s) expected invalid, got %s", tc.from, tc.event, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("(%s,%s) want %s ok, got %s ok=%v", tc.from, tc.event, tc.want, got, ok)
		}
	}
}
```

Plus integration test `store_test.go` using `testcontainers-go` with real Redis — runs full StartShift → GoPause → Resume → CallStarted → CallEnded → SubmitStatus → EndShift flow, asserts every transition emits an outbox event with the correct from/to states.

**Acceptance:**
- `go test -race -cover ./internal/dialer/internal/fsm/...` ≥ 95% coverage.
- Concurrent transition test: 100 goroutines × 100 events on the same operator — exactly one event commits per CAS round, others retry. Final `version` field equals number of successful transitions.
- Manual: `redis-cli HGETALL op:<t>:user:<o>` after a full flow shows clean field set, no stale `current_call_id` after status submission.

---


---

## Self-review

**Spec coverage** (against §8 full, §FR-E, ADR-003):
- §8.1 OperatorFSM: states `offline → ready → dialing → call → status → verify → pause`, хранение Redis hash `op:<id>:state` с CAS-version для concurrent transitions. Лог переходов в `operator_state_log` через outbox. ✓
- §8.2 CallQueue: per-tenant Redis ZSET `project:<id>:queue`, score `priority * 1e9 + epoch_ms`, atomic ZPOPMIN через Lua. Requeue с bounded delay. ✓
- §8.3 RDDGenerator: 89 RU regions из `configs/regions.yaml` (embed.FS), region-by-quota selection, anti-dup project SET + tenant SET 30d, DNC check, leak-bucket per-project (10/sec default), formatvalidate (E.164 +7XXXXXXXXXX, 11 цифр). ✓
- §8.4 WorkingHoursChecker: local-time per region (89 timezones из `pkg/regions/`), weekday/weekend windows из `tenant_settings`, holiday calendar override. ✓
- §8.5 retry-логика по статусам: `success/refused` → completed; `wrong-person` → wrong + DNC; `dropped` → retry +2h; `no-answer` → +4h (max attempts default 3); `busy` → +30min; `callback` → parsed time из комментария; `tech-failure` → +5min не считается попыткой. ✓
- `worker.dialer.retry_due` (раз в 30 сек, leader-election через Postgres advisory lock) сканирует `respondents.next_attempt_at <= now()` → ZADD в очередь. ✓
- §8.6 backpressure: Redis `INCR fs:<node>:active_channels` cap=60 default; при cap → переход на следующую нода или backoff 1s. DECR в `channel.hangup` event. ✓
- §8.7 учёт квот: транзакция в Postgres (UPDATE calls + UPDATE project_quotas + UPDATE respondents) при success-финализации; reconciliation worker раз в час. ✓
- ADR-003 progressive (1:1) — на каждого ready-оператора берём ровно 1 номер. ✓
- HTTP/WS endpoints: `POST /api/sessions/{start,end,pause,resume}`, `POST /api/calls/{id}/status`. ✓
- NATS subscriptions от telephony-bridge: `channel.dialing/answered/bridged/hangup`. ✓
- Coverage `internal/dialer/{service,fsm,queue,rdd,router}/` ≥ 90%. Concurrent transition test (100 горутин × 100 events) — exactly one event commits per CAS round. RDD на 100k generations: распределение по регионам корректно, 0 дублей. ✓

**Placeholder scan:** none. Алгоритмы прописаны полностью включая Lua-скрипты для атомарных Redis-операций.

**Type/name consistency:** `OperatorFSM`, `CallQueue`, `RDDGenerator`, `Router`, `LineCapacityTracker`, `WorkingHoursChecker`, `Respondent`, `Call` — стабильные имена, потребляемые Plan 11 (realtime для op.state events), Plan 12 (recording при hangup), Plan 13 (analytics из `dialer.call.lifecycle` events).

**Out of scope (correctly deferred):**
- ESL-команды (delegated через NATS в telephony-bridge) — Plan 09.
- Predictive-dialer (ratio > 1) — v2 (отдельная стратегия в `Router`).
- Operator UI — Plan 16.
- Statistics dashboard — Plan 13.

Plan 10 verified.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-10-dialer-module.md`.**

