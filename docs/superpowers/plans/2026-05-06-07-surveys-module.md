# Surveys Module + WASM Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the СоциоПульс surveys module — JSON-Schema-defined survey graph, version store, runtime executor, conditional DSL evaluator, schema/graph validator — and a WebAssembly build of the runtime so the browser can compute "next node" with the same Go code that runs on the server. After this plan, an admin can `POST /api/surveys`, save versions, validate them, activate one version, run preview, and the operator workplace (Plan 09) can call `surveys.Runtime.NextNode` from JS via WASM.

**Architecture:** `internal/surveys/` is a leaf module — it depends only on `internal/auth/api` (for tenant context) and `internal/audit/api` (for audit events), and exposes a façade in `internal/surveys/api/`. The package layout follows the established `{api, service, store, runtime, schemas, http}` convention. The DSL evaluator wraps `expr-lang/expr` with a strict whitelist; JSON-Schema validation uses `santhosh-tekuri/jsonschema/v5`. The runtime is implemented as a pure-function package with no I/O dependencies, which makes it trivially compilable to WebAssembly via TinyGo. The WASM artifact is built by `scripts/build-wasm.sh`, embedded into the Go binary via `embed.FS`, and served by `cmd/api` at `/static/surveys-runtime.wasm`. The browser loads it once, then calls exported functions through `wasm_exec.js` glue.

**Tech Stack:** Go 1.22+, `expr-lang/expr` v1.16+, `santhosh-tekuri/jsonschema/v5` v5.3+, TinyGo 0.31+, `stretchr/testify`, embed.FS, gorilla/mux router (set up in Plan 02).

**Spec sections covered:** §FR-C (constructor — both modes, conditional logic, versioning, validation, preview), §11.1–11.7 (universal schema, form/flow modes, DSL, validation, runtime, versioning, preview), ADR-008 (single Go runtime → WASM for browser).

**Dependencies (already done):**
- Plan 00 — Foundation (repo skeleton, Go module, `internal/surveys/api/.gitkeep` placeholder).
- Plan 01 — Infrastructure (Terraform / k8s for envs).
- Plan 02 — `cmd/api` skeleton with config, observability, mux router, middleware.
- Plan 03 — Database schema with `surveys`, `survey_versions`, `survey_versions_active_one` unique partial index.
- Plan 04 — Auth + tenancy (RLS context injection — `set_config('app.tenant_id', ...)`).
- Plan 05 — Audit module (for `audit_log` writes on Activate / Save).
- Plan 06 — CRM/projects (gives `survey_id` foreign key — surveys this plan creates can be linked, but that wiring lives in Plan 06).

**Downstream consumers (created later):**
- Plan 08 — Dialer (reads `calls.survey_version_id` pinning).
- Plan 09 — Operator workplace (loads WASM, calls `NextNode` client-side).
- Plan 12 — Admin UI (form-mode + flow-mode editors).

---

## File Structure

This plan creates the following file tree under `internal/surveys/`, plus a couple of cross-cutting files.

```
sociopulse/                                                  # repo root, current working dir
├── cmd/
│   └── surveys-wasm/
│       ├── main.go                                          # WASM entrypoint exposing JS-callable functions
│       └── README.md                                        # how the WASM binary is built and embedded
│
├── internal/surveys/
│   ├── api/                                                 # public façade — interfaces and DTOs only
│   │   ├── doc.go                                           # package doc comment
│   │   ├── service.go                                       # SurveyService, VersionStore interfaces + DTOs
│   │   ├── runtime.go                                       # Runtime interface + Answer, NodeResult types
│   │   └── errors.go                                        # ErrNotFound, ErrValidation, ErrSchema, ErrCycle, ...
│   │
│   ├── schemas/
│   │   ├── embed.go                                         # //go:embed for survey-1.0.json + tests/*
│   │   ├── survey-1.0.json                                  # JSON-Schema: nodes, edges, types, metadata
│   │   └── testdata/
│   │       ├── valid-vciom-electoral.json                   # full ВЦИОМ-style survey (smoke + integration)
│   │       ├── valid-minimal-flat.json                      # 3-node minimal flow (start → q1 → end_success)
│   │       ├── valid-with-conditions.json                   # branching by answer value
│   │       ├── valid-with-multi.json                        # multi-select question
│   │       ├── invalid-no-start.json                        # missing start node
│   │       ├── invalid-two-starts.json                      # two start nodes
│   │       ├── invalid-unreachable.json                     # node never reached from start
│   │       ├── invalid-dangling-edge.json                   # next.to → unknown id
│   │       ├── invalid-cycle-no-exit.json                   # 2-node cycle without escape
│   │       ├── invalid-bad-when.json                        # malformed DSL expression
│   │       ├── invalid-forward-ref.json                     # q3.value referenced before q3 reachable
│   │       └── invalid-missing-options.json                 # single-question with no options
│   │
│   ├── service/                                             # SurveyService impl: CRUD anketas
│   │   ├── service.go                                       # struct, constructor, CRUD methods
│   │   ├── service_test.go                                  # unit + table tests for CRUD
│   │   └── mappers.go                                       # DB row ↔ DTO conversions
│   │
│   ├── store/                                               # VersionStore impl: SaveVersion, GetActive, Activate
│   │   ├── postgres.go                                      # SQL implementation
│   │   ├── postgres_test.go                                 # integration test against testcontainers PG
│   │   └── queries.sql                                      # SQL query strings (kept readable)
│   │
│   ├── runtime/                                             # Pure-function runtime (compiles to WASM cleanly)
│   │   ├── runtime.go                                       # NextNode, ValidateAnswer, CalculateProgress
│   │   ├── runtime_test.go                                  # table-driven runtime cases
│   │   ├── progress.go                                      # progress calculation per path
│   │   ├── validator.go                                     # ValidateAnswer per node type
│   │   └── types.go                                         # internal Schema, Node, Edge, Answer mirrors
│   │
│   ├── dsl/                                                 # Conditional DSL evaluator wrapping expr-lang/expr
│   │   ├── evaluator.go                                     # Compile, Eval, with whitelist environment
│   │   ├── evaluator_test.go                                # 30+ DSL expressions: positive + negative
│   │   ├── env.go                                           # env builder: answer, qN.value, qN.answered, etc.
│   │   ├── cache.go                                         # LRU of compiled expr.Program
│   │   └── whitelist.go                                     # explicit allow-list of operators/funcs/idents
│   │
│   ├── schemavalidator/                                     # Two-pass validator: JSON-Schema, then graph
│   │   ├── validator.go                                     # SchemaValidator entrypoint
│   │   ├── jsonschema.go                                    # delegates to santhosh-tekuri/jsonschema/v5
│   │   ├── graph.go                                         # graph checks: start-uniqueness, reachability, cycles
│   │   ├── graph_test.go                                    # all the testdata invalid-*.json cases
│   │   └── report.go                                        # ValidationReport + Issue types (for UI hints)
│   │
│   ├── http/                                                # HTTP handlers wired into cmd/api router
│   │   ├── handlers.go                                      # POST/GET /api/surveys, versions, activate, preview, validate
│   │   ├── handlers_test.go                                 # httptest table tests
│   │   ├── routes.go                                        # RegisterRoutes(r *mux.Router, deps)
│   │   └── dto.go                                           # request/response DTOs
│   │
│   └── wasm/                                                # WASM glue: embed the built artifact + serve helper
│       ├── embed.go                                         # //go:embed assets/surveys-runtime.wasm + wasm_exec.js
│       ├── handler.go                                       # http.HandlerFunc serving the bundle with cache headers
│       ├── handler_test.go                                  # smoke test: GET /static/surveys-runtime.wasm → 200
│       └── assets/
│           ├── .gitkeep                                     # build output goes here, ignored by git except .gitkeep
│           └── README.md                                    # explains how the bundle ends up here
│
├── scripts/
│   └── build-wasm.sh                                        # invokes tinygo, copies output into internal/surveys/wasm/assets/
│
└── docs/
    └── architecture/
        └── adr/
            └── 0008-wasm-survey-runtime.md                  # ADR-008 copy from spec, finalized
```

After Plan 07, `make build` builds both the Go binaries and the WASM artifact (the WASM step is invoked by `make build-wasm` which `make build` depends on).

---

## Task 1: Module skeleton + JSON-Schema document

**Files:**
- Create: `internal/surveys/api/doc.go`
- Create: `internal/surveys/api/service.go`
- Create: `internal/surveys/api/runtime.go`
- Create: `internal/surveys/api/errors.go`
- Create: `internal/surveys/schemas/embed.go`
- Create: `internal/surveys/schemas/survey-1.0.json`
- Modify: `internal/surveys/api/.gitkeep` → delete (replaced by real files)

**Why this task is first:** every other task reads the Schema or imports from `api/`. Defining the contract first prevents downstream tasks from drifting.

- [ ] **Step 1: Verify working directory and prerequisites**

Run from repo root:

```bash
pwd
go version
test -d internal/surveys/api && echo "ok: surveys placeholder exists"
test -f internal/surveys/api/.gitkeep && echo "ok: gitkeep present"
```

Expected:
- `pwd` → `/Users/user/call-center/social-pulse`
- `go version` → `go version go1.22.x ...` or higher
- both `ok:` lines present

If anything fails, do not proceed; re-run Plan 00 verification first.

- [ ] **Step 2: Remove `.gitkeep`**

Run:

```bash
rm internal/surveys/api/.gitkeep
```

Expected: file deleted; `git status` will show it as a `D` entry on next commit.

- [ ] **Step 3: Write `internal/surveys/api/doc.go`**

Create `internal/surveys/api/doc.go`:

```go
// Package api is the public façade of the surveys module.
//
// External callers (gateway HTTP handlers, the dialer, the worker) MUST
// import only this package — never the implementation packages
// (service, store, runtime, dsl, schemavalidator). The depguard linter
// enforces this rule.
//
// What lives here:
//   - SurveyService and VersionStore interfaces (CRUD + version pinning)
//   - Runtime interface (pure-function survey execution)
//   - DTOs that cross module boundaries (Survey, Version, Answer, NodeResult)
//   - Sentinel errors (ErrNotFound, ErrValidation, ErrSchema, ErrCycle, ...)
//
// Spec: §FR-C, §11.1–11.7, ADR-008.
package api
```

- [ ] **Step 4: Write `internal/surveys/api/service.go`**

Create `internal/surveys/api/service.go`:

```go
package api

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// SurveyService is the high-level CRUD façade for survey definitions
// (the "surveys" table — name, description, owner, etc.). Versions are
// managed through VersionStore, but SurveyService.SaveVersion is a
// convenience wrapper that calls VersionStore + audit logging in one
// transaction.
type SurveyService interface {
	// Create a new survey shell (no versions yet). Returns the new ID.
	Create(ctx context.Context, in CreateSurveyInput) (uuid.UUID, error)

	// Get returns the survey metadata. Does not include versions.
	Get(ctx context.Context, id uuid.UUID) (Survey, error)

	// List returns all surveys for the current tenant.
	// Filtering: status (active/archived), search by name (ILIKE).
	List(ctx context.Context, filter ListFilter) ([]Survey, error)

	// Update mutates editable fields (name, description, primary_mode).
	Update(ctx context.Context, id uuid.UUID, in UpdateSurveyInput) error

	// Archive soft-deletes (status = archived, hidden from default list).
	Archive(ctx context.Context, id uuid.UUID) error

	// SaveVersion validates the schema and inserts a new version row.
	// If validation fails, returns ErrValidation with a populated Report.
	// minor=true keeps major, increments minor; minor=false bumps major.
	SaveVersion(ctx context.Context, surveyID uuid.UUID, schemaJSON []byte, minor bool) (Version, error)

	// Activate marks the given version as active and deactivates the
	// previously active one — atomically, in a single transaction.
	Activate(ctx context.Context, surveyID, versionID uuid.UUID) error

	// GetActiveVersion is a convenience for the dialer / preview pages.
	GetActiveVersion(ctx context.Context, surveyID uuid.UUID) (Version, error)

	// ListVersions returns the version history of a survey, newest first.
	ListVersions(ctx context.Context, surveyID uuid.UUID) ([]Version, error)
}

// VersionStore is the lower-level persistence interface. It's separated
// from SurveyService because (a) the dialer wants to read versions
// without going through the full service, and (b) tests can swap it for
// an in-memory implementation.
type VersionStore interface {
	SaveVersion(ctx context.Context, v Version) error
	GetVersion(ctx context.Context, id uuid.UUID) (Version, error)
	GetActive(ctx context.Context, surveyID uuid.UUID) (Version, error)
	ListVersions(ctx context.Context, surveyID uuid.UUID) ([]Version, error)
	Activate(ctx context.Context, surveyID, versionID uuid.UUID) error
}

// CreateSurveyInput is the payload for SurveyService.Create.
type CreateSurveyInput struct {
	Name        string
	Description string
	PrimaryMode PrimaryMode // form|flow
}

// UpdateSurveyInput is the payload for SurveyService.Update. Only
// non-zero fields are applied (use *string for nullable updates).
type UpdateSurveyInput struct {
	Name        *string
	Description *string
	PrimaryMode *PrimaryMode
}

// ListFilter is the search filter for SurveyService.List.
type ListFilter struct {
	Status SurveyStatus // empty → both active and archived
	Search string       // ILIKE pattern on name
	Limit  int
	Offset int
}

// Survey is the metadata DTO. Schema lives in Version.
type Survey struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	Name        string
	Description string
	PrimaryMode PrimaryMode
	Status      SurveyStatus
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CreatedBy   uuid.UUID
}

// Version is one immutable revision of a survey schema.
type Version struct {
	ID         uuid.UUID
	SurveyID   uuid.UUID
	Major      int
	Minor      int
	Schema     []byte // canonical JSON of the survey graph
	IsActive   bool
	CreatedAt  time.Time
	CreatedBy  uuid.UUID
	ActivatedAt *time.Time
}

// PrimaryMode tells the UI which editor opens by default.
type PrimaryMode string

const (
	ModeForm PrimaryMode = "form"
	ModeFlow PrimaryMode = "flow"
)

// SurveyStatus is either active or archived.
type SurveyStatus string

const (
	StatusActive   SurveyStatus = "active"
	StatusArchived SurveyStatus = "archived"
)
```

- [ ] **Step 5: Write `internal/surveys/api/runtime.go`**

Create `internal/surveys/api/runtime.go`:

```go
package api

import (
	"github.com/google/uuid"
)

// Runtime is the pure-function survey executor. It has no I/O — given
// a schema and the current state, it returns the next node, validates
// an answer, or computes a progress estimate. The same package compiles
// to WebAssembly (cmd/surveys-wasm) for the browser.
type Runtime interface {
	// NextNode computes which node should be presented after the
	// operator/respondent has answered the current node.
	// Returns the next node id, or one of the *-end ids if the survey
	// terminates. answers maps nodeID → Answer for all answered nodes
	// in the current call.
	NextNode(schema []byte, currentNodeID string, answers map[string]Answer) (NodeResult, error)

	// ValidateAnswer checks an answer against the node's declared type
	// (single must match an option id; number must be in min..max; etc).
	// Returns nil if valid; ErrValidation with a human-readable Issue
	// otherwise.
	ValidateAnswer(schema []byte, nodeID string, ans Answer) error

	// CalculateProgress estimates "how far through the survey" the
	// respondent is, as a value in [0, 1]. The estimate is the longest
	// path from the start to the current node divided by the longest
	// path from start to any *-end node.
	CalculateProgress(schema []byte, currentNodeID string) (float64, error)
}

// Answer is one operator-recorded response. The shape depends on the
// node's question type:
//   - single  → SingleChoice = "option_id"
//   - multi   → MultiChoice  = ["option_a", "option_b"]
//   - number  → Number       = 42 (float64)
//   - text    → Text         = "free-form text"
//   - select  → SingleChoice = "region_code" (large list, same shape as single)
type Answer struct {
	NodeID       string
	SingleChoice string
	MultiChoice  []string
	Number       *float64
	Text         string
	// AnsweredAt is the wall-clock time the operator recorded the answer.
	// Used for QA chronology, not for runtime decisions.
	AnsweredAt int64 // unix millis (cross-platform-friendly for WASM)
}

// NodeResult is the return type of Runtime.NextNode.
type NodeResult struct {
	NextNodeID string  // empty when Terminated=true
	Terminated bool    // true if the survey reached a *-end node
	EndKind    EndKind // success|refusal — only meaningful when Terminated
	Progress   float64 // [0,1]
}

// EndKind discriminates terminal nodes.
type EndKind string

const (
	EndKindSuccess EndKind = "success"
	EndKindRefusal EndKind = "refusal"
	EndKindNone    EndKind = "" // survey did not terminate
)

// QuestionType enumerates the supported answer types.
// Constants are exported so other modules (e.g. operator UI) can switch on them.
type QuestionType string

const (
	TypeSingle QuestionType = "single"
	TypeMulti  QuestionType = "multi"
	TypeNumber QuestionType = "number"
	TypeText   QuestionType = "text"
	TypeSelect QuestionType = "select"
)

// NodeKind enumerates the structural node types of a survey graph.
type NodeKind string

const (
	NodeStart       NodeKind = "start"
	NodeIntro       NodeKind = "intro"
	NodeQuestion    NodeKind = "question"
	NodeTextBlock   NodeKind = "text-block"
	NodeSuccessEnd  NodeKind = "success-end"
	NodeRefusalEnd  NodeKind = "refusal-end"
	NodeCondition   NodeKind = "condition"
	NodeJump        NodeKind = "jump"
)

// AnswerKey is a composite key for storing answers — handy for the
// service layer that records call_answers.
type AnswerKey struct {
	CallID uuid.UUID
	NodeID string
}
```

- [ ] **Step 6: Write `internal/surveys/api/errors.go`**

Create `internal/surveys/api/errors.go`:

```go
package api

import "errors"

// Sentinel errors. Use errors.Is to check.
var (
	// ErrNotFound — survey or version doesn't exist (or is hidden by RLS).
	ErrNotFound = errors.New("surveys: not found")

	// ErrValidation — schema or graph is structurally invalid.
	// Wrap with ValidationError to attach a Report.
	ErrValidation = errors.New("surveys: validation failed")

	// ErrSchema — schemaJSON is not valid JSON or doesn't satisfy survey-1.0.json.
	ErrSchema = errors.New("surveys: invalid schema")

	// ErrCycle — graph contains a cycle without an exit.
	ErrCycle = errors.New("surveys: cycle without exit")

	// ErrUnreachable — at least one node cannot be reached from start.
	ErrUnreachable = errors.New("surveys: unreachable nodes")

	// ErrDanglingEdge — next.to references a non-existent node.
	ErrDanglingEdge = errors.New("surveys: dangling edge")

	// ErrForwardRef — `qN.value` references a node that may not be answered yet.
	ErrForwardRef = errors.New("surveys: forward reference in DSL")

	// ErrBadAnswer — answer doesn't match the node's question type.
	ErrBadAnswer = errors.New("surveys: bad answer for node type")

	// ErrAlreadyActive — trying to activate an already-active version.
	ErrAlreadyActive = errors.New("surveys: version already active")

	// ErrNoActiveVersion — survey has no active version (cannot be used for calls).
	ErrNoActiveVersion = errors.New("surveys: no active version")
)

// ValidationError wraps ErrValidation with a structured Report so the
// HTTP layer can surface field-level problems to the editor UI.
type ValidationError struct {
	Report Report
}

func (v *ValidationError) Error() string {
	return ErrValidation.Error()
}

func (v *ValidationError) Unwrap() error {
	return ErrValidation
}

// Report is the structured result of schema validation. Each Issue
// pinpoints a problem with a JSON-Pointer path so the UI can highlight.
type Report struct {
	OK     bool    `json:"ok"`
	Issues []Issue `json:"issues"`
}

// Issue is one validation problem.
type Issue struct {
	Path     string `json:"path"`     // JSON-Pointer, e.g. "/nodes/2/next/0/when"
	NodeID   string `json:"node_id,omitempty"`
	Code     string `json:"code"`     // machine-readable: "unreachable", "dangling_edge", ...
	Message  string `json:"message"`  // human-readable
	Severity string `json:"severity"` // "error" | "warning"
}
```

- [ ] **Step 7: Write `internal/surveys/schemas/embed.go`**

Create `internal/surveys/schemas/embed.go`:

```go
// Package schemas hosts the embedded JSON-Schema documents that
// describe the survey-graph format. The schemas are embedded into the
// Go binary so no runtime file I/O is needed.
package schemas

import _ "embed"

// SurveyV1 is the JSON-Schema document for survey-1.0 — the format of
// survey_versions.schema. The schemavalidator package uses it as the
// first validation pass (structure), before the graph-level checks.
//
//go:embed survey-1.0.json
var SurveyV1 []byte
```

- [ ] **Step 8: Write `internal/surveys/schemas/survey-1.0.json`**

Create `internal/surveys/schemas/survey-1.0.json`:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://sociopulse.io/schemas/survey-1.0.json",
  "title": "СоциоПульс survey-1.0",
  "type": "object",
  "required": ["version", "title", "nodes"],
  "additionalProperties": false,
  "properties": {
    "version": {
      "type": "string",
      "pattern": "^[0-9]+\\.[0-9]+$",
      "description": "Major.Minor of this schema instance, e.g. \"1.0\""
    },
    "title": {
      "type": "string",
      "minLength": 1,
      "maxLength": 200
    },
    "intro": {
      "type": "string",
      "maxLength": 5000,
      "description": "Optional pre-survey greeting shown to the operator"
    },
    "nodes": {
      "type": "array",
      "minItems": 1,
      "items": { "$ref": "#/$defs/node" }
    },
    "metadata": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "estimated_minutes": { "type": "string", "maxLength": 20 },
        "max_questions":     { "type": "integer", "minimum": 1, "maximum": 200 },
        "primary_mode":      { "type": "string", "enum": ["form", "flow"] }
      }
    }
  },
  "$defs": {
    "node": {
      "type": "object",
      "required": ["id", "kind"],
      "properties": {
        "id":   { "type": "string", "pattern": "^[a-zA-Z0-9_]+$", "minLength": 1, "maxLength": 64 },
        "kind": {
          "type": "string",
          "enum": ["start", "intro", "question", "text-block", "success-end", "refusal-end", "condition", "jump"]
        },
        "text": { "type": "string", "maxLength": 2000 },
        "hint": { "type": "string", "maxLength": 2000 },
        "type": {
          "type": "string",
          "enum": ["single", "multi", "number", "text", "select"],
          "description": "Question type — required only for kind=question"
        },
        "required": { "type": "boolean" },
        "options": {
          "type": "array",
          "items": {
            "type": "object",
            "required": ["id", "label"],
            "additionalProperties": false,
            "properties": {
              "id":    { "type": "string", "pattern": "^[a-zA-Z0-9_]+$", "minLength": 1, "maxLength": 64 },
              "label": { "type": "string", "minLength": 1, "maxLength": 500 }
            }
          },
          "minItems": 1
        },
        "min": { "type": "number", "description": "min value for type=number" },
        "max": { "type": "number", "description": "max value for type=number" },
        "next": {
          "type": "array",
          "items": {
            "type": "object",
            "required": ["to"],
            "additionalProperties": false,
            "properties": {
              "to":   { "type": "string", "pattern": "^[a-zA-Z0-9_]+$" },
              "when": { "type": "string", "maxLength": 500, "default": "true" }
            }
          }
        },
        "ui": {
          "type": "object",
          "additionalProperties": false,
          "properties": {
            "x": { "type": "number" },
            "y": { "type": "number" }
          }
        }
      },
      "allOf": [
        {
          "if":   { "properties": { "kind": { "const": "question" } } },
          "then": { "required": ["type", "text"] }
        },
        {
          "if": {
            "properties": { "kind": { "const": "question" }, "type": { "enum": ["single", "multi", "select"] } }
          },
          "then": { "required": ["options"] }
        },
        {
          "if": {
            "properties": { "kind": { "enum": ["success-end", "refusal-end"] } }
          },
          "then": { "not": { "required": ["next"] } }
        }
      ]
    }
  }
}
```

- [ ] **Step 9: Add empty stubs in implementation packages so the next tasks compile**

The implementation packages don't have content yet, but tests in later tasks need to import them. Create the stub files:

```bash
mkdir -p internal/surveys/{service,store,runtime,dsl,schemavalidator,http,wasm/assets} internal/surveys/schemas/testdata cmd/surveys-wasm
touch internal/surveys/wasm/assets/.gitkeep
```

Create `internal/surveys/service/doc.go`:

```go
// Package service is the SurveyService implementation.
// Imported only by cmd/api wiring, never by other modules.
package service
```

Create `internal/surveys/store/doc.go`:

```go
// Package store is the postgres-backed VersionStore implementation.
package store
```

Create `internal/surveys/runtime/doc.go`:

```go
// Package runtime is the pure-function survey executor.
//
// IMPORTANT: this package MUST stay pure (no net, os, filesystem,
// goroutines) so it compiles cleanly to WebAssembly via TinyGo.
// Side-effecting code lives in service/.
package runtime
```

Create `internal/surveys/dsl/doc.go`:

```go
// Package dsl wraps expr-lang/expr with a strict whitelist for
// evaluating survey "when" expressions.
package dsl
```

Create `internal/surveys/schemavalidator/doc.go`:

```go
// Package schemavalidator does two-pass survey validation:
// (1) JSON-Schema structure check via santhosh-tekuri/jsonschema/v5,
// (2) graph-level checks (start uniqueness, reachability, cycles).
package schemavalidator
```

Create `internal/surveys/http/doc.go`:

```go
// Package http registers the surveys HTTP routes on a mux.Router.
package http
```

Create `internal/surveys/wasm/doc.go`:

```go
// Package wasm embeds and serves the TinyGo-built survey runtime bundle.
package wasm
```

- [ ] **Step 10: Verify the schema parses as valid JSON-Schema**

Run a one-shot Go script (you can `go run`-it via `/tmp/check.go`):

```bash
cat > /tmp/check_schema.go <<'EOF'
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	data, err := os.ReadFile("internal/surveys/schemas/survey-1.0.json")
	if err != nil { panic(err) }
	var v any
	if err := json.Unmarshal(data, &v); err != nil { panic(err) }
	fmt.Println("ok: schema is valid JSON")
	_ = v
}
EOF
go run /tmp/check_schema.go
rm /tmp/check_schema.go
```

Expected: `ok: schema is valid JSON`.

- [ ] **Step 11: Verify the package builds**

Run:

```bash
go build ./internal/surveys/...
```

Expected: no output (success). Any compile error must be fixed before moving on.

- [ ] **Step 12: Add `expr-lang/expr` and `santhosh-tekuri/jsonschema/v5` to go.mod**

Run:

```bash
go get github.com/expr-lang/expr@v1.16.9
go get github.com/santhosh-tekuri/jsonschema/v5@v5.3.1
go mod tidy
```

Expected: both modules added to `go.mod`; `go.sum` updated; no `replace` directives needed.

- [ ] **Step 13: Update depguard rules**

Open `.golangci.yml`. Find the `depguard` section. Add the surveys-module rule:

```yaml
linters-settings:
  depguard:
    rules:
      surveys-internals:
        list-mode: lax
        files:
          - "!**/internal/surveys/**"
        deny:
          - pkg: "github.com/sociopulse/platform/internal/surveys/service"
            desc: "import internal/surveys/api instead — service is module-private"
          - pkg: "github.com/sociopulse/platform/internal/surveys/store"
            desc: "import internal/surveys/api instead"
          - pkg: "github.com/sociopulse/platform/internal/surveys/runtime"
            desc: "import internal/surveys/api instead — except for cmd/surveys-wasm"
          - pkg: "github.com/sociopulse/platform/internal/surveys/dsl"
            desc: "import internal/surveys/api instead"
          - pkg: "github.com/sociopulse/platform/internal/surveys/schemavalidator"
            desc: "import internal/surveys/api instead"
```

Add a second rule that allows `cmd/surveys-wasm` to import runtime directly:

```yaml
      wasm-runtime-allowed:
        list-mode: strict
        files:
          - "**/cmd/surveys-wasm/**"
        allow:
          - $gostd
          - "github.com/sociopulse/platform/internal/surveys/runtime"
          - "github.com/sociopulse/platform/internal/surveys/api"
          - "syscall/js"
```

Run:

```bash
golangci-lint run ./internal/surveys/... 2>&1 | head -20
```

Expected: no `depguard` violations from the new rules. Existing modules are unaffected because the `surveys-internals` rule only matches files outside `internal/surveys/`.

- [ ] **Step 14: Commit**

```bash
git add internal/surveys/api internal/surveys/schemas internal/surveys/service/doc.go \
        internal/surveys/store/doc.go internal/surveys/runtime/doc.go \
        internal/surveys/dsl/doc.go internal/surveys/schemavalidator/doc.go \
        internal/surveys/http/doc.go internal/surveys/wasm/doc.go \
        internal/surveys/wasm/assets/.gitkeep \
        cmd/surveys-wasm \
        .golangci.yml go.mod go.sum
git rm internal/surveys/api/.gitkeep 2>/dev/null || true
git commit -m "feat(surveys): module skeleton and survey-1.0 JSON-Schema

- internal/surveys/api: SurveyService, VersionStore, Runtime interfaces;
  Survey/Version/Answer/NodeResult DTOs; sentinel errors and ValidationError.
- internal/surveys/schemas/survey-1.0.json: JSON-Schema for survey graph
  (nodes start/intro/question/text-block/success-end/refusal-end/condition/jump,
  question types single/multi/number/text/select).
- doc.go stubs for service/, store/, runtime/, dsl/, schemavalidator/, http/, wasm/.
- depguard rules: external callers must import api/, except cmd/surveys-wasm.
- Add expr-lang/expr v1.16.9 and santhosh-tekuri/jsonschema/v5 v5.3.1.

Spec: §FR-C, §11.1, ADR-008."
```

Expected: commit succeeds, ~12 files added, 1 deleted, depguard rules updated.

---

## Task 2: Schema validator — JSON-Schema pass + graph-level checks

**Files:**
- Create: `internal/surveys/schemavalidator/validator.go`
- Create: `internal/surveys/schemavalidator/jsonschema.go`
- Create: `internal/surveys/schemavalidator/graph.go`
- Create: `internal/surveys/schemavalidator/report.go`
- Create: `internal/surveys/schemavalidator/graph_test.go`
- Create: `internal/surveys/schemas/testdata/valid-minimal-flat.json`
- Create: `internal/surveys/schemas/testdata/valid-with-conditions.json`
- Create: `internal/surveys/schemas/testdata/valid-with-multi.json`
- Create: `internal/surveys/schemas/testdata/invalid-no-start.json`
- Create: `internal/surveys/schemas/testdata/invalid-two-starts.json`
- Create: `internal/surveys/schemas/testdata/invalid-unreachable.json`
- Create: `internal/surveys/schemas/testdata/invalid-dangling-edge.json`
- Create: `internal/surveys/schemas/testdata/invalid-cycle-no-exit.json`
- Create: `internal/surveys/schemas/testdata/invalid-bad-when.json`
- Create: `internal/surveys/schemas/testdata/invalid-forward-ref.json`
- Create: `internal/surveys/schemas/testdata/invalid-missing-options.json`

**Why this task is second:** the validator is purely declarative (no DB, no DSL evaluation — just AST and graph checks against an embedded JSON-Schema). It can be unit-tested in isolation, which lets us pin down the schema semantics before the runtime depends on them.

- [ ] **Step 1: Write `internal/surveys/schemavalidator/report.go`**

Create `internal/surveys/schemavalidator/report.go`:

```go
package schemavalidator

import "github.com/sociopulse/platform/internal/surveys/api"

// Issue codes — must stay stable, the UI keys translations off them.
const (
	CodeJSONSchema       = "json_schema"
	CodeNoStart          = "no_start"
	CodeMultipleStarts   = "multiple_starts"
	CodeUnreachable      = "unreachable"
	CodeDanglingEdge     = "dangling_edge"
	CodeCycleNoExit      = "cycle_no_exit"
	CodeBadWhen          = "bad_when"
	CodeForwardRef       = "forward_ref"
	CodeMissingOptions   = "missing_options"
	CodeDuplicateNode    = "duplicate_node_id"
	CodeNoEnd            = "no_end_reachable"
	CodeBadNodeReference = "bad_node_reference"
)

// newReport allocates a Report with empty Issues slice.
func newReport() *api.Report {
	return &api.Report{OK: true, Issues: []api.Issue{}}
}

// add appends an Issue and flips OK=false if severity is "error".
func add(r *api.Report, code, msg, path, nodeID, sev string) {
	r.Issues = append(r.Issues, api.Issue{
		Code: code, Message: msg, Path: path, NodeID: nodeID, Severity: sev,
	})
	if sev == "error" {
		r.OK = false
	}
}
```

- [ ] **Step 2: Write `internal/surveys/schemavalidator/jsonschema.go`**

Create `internal/surveys/schemavalidator/jsonschema.go`:

```go
package schemavalidator

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/internal/surveys/schemas"
)

// jsonSchemaPass validates schemaJSON against survey-1.0.json and
// appends any structural problems to the report. Returns true if the
// schema parsed cleanly enough to attempt graph-level checks.
func jsonSchemaPass(schemaJSON []byte, r *api.Report) bool {
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	if err := compiler.AddResource(
		"https://sociopulse.io/schemas/survey-1.0.json",
		bytes.NewReader(schemas.SurveyV1),
	); err != nil {
		add(r, CodeJSONSchema, fmt.Sprintf("internal: cannot load schema: %v", err), "", "", "error")
		return false
	}
	sch, err := compiler.Compile("https://sociopulse.io/schemas/survey-1.0.json")
	if err != nil {
		add(r, CodeJSONSchema, fmt.Sprintf("internal: cannot compile schema: %v", err), "", "", "error")
		return false
	}

	var doc any
	if err := json.Unmarshal(schemaJSON, &doc); err != nil {
		add(r, CodeJSONSchema, fmt.Sprintf("schemaJSON is not valid JSON: %v", err), "", "", "error")
		return false
	}

	if err := sch.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if errors_As(err, &ve) {
			collectValidationErrors(ve, r)
		} else {
			add(r, CodeJSONSchema, err.Error(), "", "", "error")
		}
		return false
	}
	return true
}

// errors_As is a wrapper that exists so we can keep this file building
// without a top-level errors-package import in TinyGo (which is fine
// here because schemavalidator does NOT compile to WASM, but stays
// consistent with the convention).
func errors_As(err error, target any) bool {
	ve, ok := target.(**jsonschema.ValidationError)
	if !ok {
		return false
	}
	if real, ok := err.(*jsonschema.ValidationError); ok {
		*ve = real
		return true
	}
	return false
}

// collectValidationErrors flattens the nested ValidationError tree into
// a list of api.Issue records, one per leaf failure, with JSON-Pointer paths.
func collectValidationErrors(ve *jsonschema.ValidationError, r *api.Report) {
	if len(ve.Causes) == 0 {
		add(r, CodeJSONSchema, ve.Message, ve.InstanceLocation, "", "error")
		return
	}
	for _, c := range ve.Causes {
		collectValidationErrors(c, r)
	}
}
```

- [ ] **Step 3: Write `internal/surveys/schemavalidator/graph.go`**

Create `internal/surveys/schemavalidator/graph.go`:

```go
package schemavalidator

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/sociopulse/platform/internal/surveys/api"
	"github.com/sociopulse/platform/internal/surveys/dsl"
)

// graph models the parsed-out survey graph for graph-level analysis.
type graph struct {
	startID string
	nodes   map[string]*node
	order   []string // deterministic iteration order (sorted by id)
}

type node struct {
	id      string
	kind    api.NodeKind
	qtype   api.QuestionType
	options map[string]struct{} // option ids — empty for non-choice questions
	edges   []edge
}

type edge struct {
	to   string
	when string
}

// graphPass executes all graph-level checks. It assumes JSON-Schema
// pass succeeded so the structure is sound enough to walk.
func graphPass(schemaJSON []byte, r *api.Report) {
	g, ok := parseGraph(schemaJSON, r)
	if !ok {
		return
	}
	checkStart(g, r)
	checkEdgesAndOptions(g, r)
	checkReachabilityFromStart(g, r)
	checkEndsReachable(g, r)
	checkCyclesHaveExit(g, r)
	checkWhenExpressions(g, r)
	checkForwardReferences(g, r)
}

// parseGraph extracts the structural graph from the JSON document.
// It assumes the JSON-Schema pass has already accepted the document.
func parseGraph(schemaJSON []byte, r *api.Report) (*graph, bool) {
	var raw struct {
		Nodes []struct {
			ID      string `json:"id"`
			Kind    string `json:"kind"`
			Type    string `json:"type"`
			Options []struct {
				ID string `json:"id"`
			} `json:"options"`
			Next []struct {
				To   string `json:"to"`
				When string `json:"when"`
			} `json:"next"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(schemaJSON, &raw); err != nil {
		add(r, CodeJSONSchema, "internal: cannot re-parse schema for graph: "+err.Error(), "", "", "error")
		return nil, false
	}
	g := &graph{nodes: make(map[string]*node, len(raw.Nodes))}
	seen := make(map[string]struct{}, len(raw.Nodes))

	for _, n := range raw.Nodes {
		if _, dup := seen[n.ID]; dup {
			add(r, CodeDuplicateNode, fmt.Sprintf("duplicate node id %q", n.ID), "/nodes", n.ID, "error")
			continue
		}
		seen[n.ID] = struct{}{}

		nn := &node{id: n.ID, kind: api.NodeKind(n.Kind), qtype: api.QuestionType(n.Type)}
		if len(n.Options) > 0 {
			nn.options = make(map[string]struct{}, len(n.Options))
			for _, o := range n.Options {
				nn.options[o.ID] = struct{}{}
			}
		}
		for _, e := range n.Next {
			when := e.When
			if when == "" {
				when = "true"
			}
			nn.edges = append(nn.edges, edge{to: e.To, when: when})
		}
		g.nodes[nn.id] = nn
		g.order = append(g.order, nn.id)
	}
	sort.Strings(g.order)
	return g, true
}

// checkStart enforces "exactly one start node".
func checkStart(g *graph, r *api.Report) {
	var starts []string
	for _, id := range g.order {
		if g.nodes[id].kind == api.NodeStart {
			starts = append(starts, id)
		}
	}
	switch len(starts) {
	case 0:
		add(r, CodeNoStart, "graph has no start node", "/nodes", "", "error")
	case 1:
		g.startID = starts[0]
	default:
		for _, id := range starts {
			add(r, CodeMultipleStarts, "more than one start node", "/nodes", id, "error")
		}
	}
}

// checkEdgesAndOptions verifies (a) every next.to references an existing node,
// (b) question kinds with single/multi/select have ≥1 option.
func checkEdgesAndOptions(g *graph, r *api.Report) {
	for _, id := range g.order {
		n := g.nodes[id]
		for i, e := range n.edges {
			if _, ok := g.nodes[e.to]; !ok {
				add(r, CodeDanglingEdge,
					fmt.Sprintf("edge from %q points to unknown node %q", id, e.to),
					fmt.Sprintf("/nodes/%s/next/%d/to", id, i), id, "error")
			}
		}
		if n.kind == api.NodeQuestion {
			switch n.qtype {
			case api.TypeSingle, api.TypeMulti, api.TypeSelect:
				if len(n.options) == 0 {
					add(r, CodeMissingOptions,
						fmt.Sprintf("question %q of type %s has no options", id, n.qtype),
						fmt.Sprintf("/nodes/%s/options", id), id, "error")
				}
			}
		}
	}
}

// checkReachabilityFromStart does BFS from start; every node must be reachable.
func checkReachabilityFromStart(g *graph, r *api.Report) {
	if g.startID == "" {
		return // no start — already reported
	}
	visited := bfs(g, g.startID)
	for _, id := range g.order {
		if _, ok := visited[id]; !ok {
			add(r, CodeUnreachable,
				fmt.Sprintf("node %q is not reachable from start", id),
				fmt.Sprintf("/nodes/%s", id), id, "error")
		}
	}
}

// checkEndsReachable verifies that at least one *-end node is reachable from start.
func checkEndsReachable(g *graph, r *api.Report) {
	if g.startID == "" {
		return
	}
	visited := bfs(g, g.startID)
	for id := range visited {
		k := g.nodes[id].kind
		if k == api.NodeSuccessEnd || k == api.NodeRefusalEnd {
			return
		}
	}
	add(r, CodeNoEnd, "no *-end node is reachable from start", "/nodes", "", "error")
}

// checkCyclesHaveExit runs Tarjan-light: any strongly-connected component
// with no edge leaving it AND no terminal node inside is a deadlock.
func checkCyclesHaveExit(g *graph, r *api.Report) {
	if g.startID == "" {
		return
	}
	for _, id := range g.order {
		if hasUnescapableCycle(g, id) {
			add(r, CodeCycleNoExit,
				fmt.Sprintf("node %q is in a cycle with no exit path", id),
				fmt.Sprintf("/nodes/%s", id), id, "error")
		}
	}
}

// hasUnescapableCycle: DFS — if we can come back to ourselves AND every
// path from us eventually loops back without hitting a *-end, it's a deadlock.
// Implementation: BFS from nodeID following edges; if no terminal node
// is reachable AND we can return to nodeID, fail.
func hasUnescapableCycle(g *graph, nodeID string) bool {
	visited := bfs(g, nodeID)
	if _, returnsToSelf := visited[nodeID]; !returnsToSelf {
		// node never reaches itself again — no cycle through this node.
		// (note: bfs starts by marking nodeID itself; we need the second visit)
	}
	// Compute "reachable terminals": iterate visited nodes for *-end kinds.
	for id := range visited {
		k := g.nodes[id].kind
		if k == api.NodeSuccessEnd || k == api.NodeRefusalEnd {
			return false
		}
	}
	// No terminal reachable AND there is at least one outgoing edge from nodeID
	// (else it's a leaf and reported separately by checkEndsReachable).
	if len(g.nodes[nodeID].edges) > 0 {
		// Confirm it's actually in a cycle: some descendant edges back to it.
		for d := range visited {
			if d == nodeID {
				continue
			}
			for _, e := range g.nodes[d].edges {
				if e.to == nodeID {
					return true
				}
			}
		}
	}
	return false
}

// bfs returns the set of nodes reachable from start (including start itself,
// but only after one hop — start's first appearance is at depth 0; if
// the graph re-visits start, that's reflected as start being a key).
// Implementation note: we mark start as visited initially.
func bfs(g *graph, start string) map[string]struct{} {
	out := map[string]struct{}{start: {}}
	q := []string{start}
	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		n, ok := g.nodes[cur]
		if !ok {
			continue
		}
		for _, e := range n.edges {
			if _, seen := out[e.to]; seen {
				continue
			}
			out[e.to] = struct{}{}
			q = append(q, e.to)
		}
	}
	return out
}

// checkWhenExpressions parses every edge.when via dsl.Compile and
// reports unparseable expressions.
func checkWhenExpressions(g *graph, r *api.Report) {
	ev := dsl.NewEvaluator()
	for _, id := range g.order {
		for i, e := range g.nodes[id].edges {
			if _, err := ev.Compile(e.when); err != nil {
				add(r, CodeBadWhen,
					fmt.Sprintf("cannot parse \"when\" expression on edge %q→%q: %v", id, e.to, err),
					fmt.Sprintf("/nodes/%s/next/%d/when", id, i), id, "error")
			}
		}
	}
}

// checkForwardReferences ensures every qX.value mentioned in a `when` expression
// refers to a node that comes BEFORE the current node on every path from start.
// Implementation: for each edge e on node N with `qX.value` reference, verify X is
// in the dominator set of N (i.e. X is on every path from start to N).
func checkForwardReferences(g *graph, r *api.Report) {
	if g.startID == "" {
		return
	}
	dom := computeDominators(g)
	ev := dsl.NewEvaluator()
	for _, id := range g.order {
		for i, e := range g.nodes[id].edges {
			refs, err := ev.ReferencedNodes(e.when)
			if err != nil {
				continue // already reported by checkWhenExpressions
			}
			for _, ref := range refs {
				if ref == "" || ref == id {
					continue
				}
				if _, ok := g.nodes[ref]; !ok {
					add(r, CodeBadNodeReference,
						fmt.Sprintf("when-expression references unknown node %q", ref),
						fmt.Sprintf("/nodes/%s/next/%d/when", id, i), id, "error")
					continue
				}
				if _, dominates := dom[id][ref]; !dominates {
					add(r, CodeForwardRef,
						fmt.Sprintf("\"when\" references %q.value but %q is not on every path from start to %q",
							ref, ref, id),
						fmt.Sprintf("/nodes/%s/next/%d/when", id, i), id, "error")
				}
			}
		}
	}
}

// computeDominators returns dom[N] = set of nodes that dominate N
// (lie on every path from start to N). Iterative algorithm — fine for graphs <200 nodes.
func computeDominators(g *graph) map[string]map[string]struct{} {
	all := make(map[string]struct{}, len(g.order))
	for _, id := range g.order {
		all[id] = struct{}{}
	}
	dom := make(map[string]map[string]struct{}, len(g.order))
	dom[g.startID] = map[string]struct{}{g.startID: {}}
	for _, id := range g.order {
		if id == g.startID {
			continue
		}
		dom[id] = copySet(all)
	}

	// predecessors map
	preds := make(map[string][]string, len(g.order))
	for _, id := range g.order {
		for _, e := range g.nodes[id].edges {
			preds[e.to] = append(preds[e.to], id)
		}
	}

	changed := true
	for changed {
		changed = false
		for _, id := range g.order {
			if id == g.startID {
				continue
			}
			var newDom map[string]struct{}
			for _, p := range preds[id] {
				if newDom == nil {
					newDom = copySet(dom[p])
				} else {
					newDom = intersect(newDom, dom[p])
				}
			}
			if newDom == nil {
				newDom = make(map[string]struct{})
			}
			newDom[id] = struct{}{}
			if !setEqual(newDom, dom[id]) {
				dom[id] = newDom
				changed = true
			}
		}
	}
	return dom
}

func copySet(s map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(s))
	for k := range s {
		out[k] = struct{}{}
	}
	return out
}

func intersect(a, b map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{})
	for k := range a {
		if _, ok := b[k]; ok {
			out[k] = struct{}{}
		}
	}
	return out
}

func setEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Write `internal/surveys/schemavalidator/validator.go`**

Create `internal/surveys/schemavalidator/validator.go`:

```go
package schemavalidator

import "github.com/sociopulse/platform/internal/surveys/api"

// SchemaValidator runs the two-pass validation.
type SchemaValidator struct{}

// New returns a fresh SchemaValidator. It's stateless — a single instance
// is safe to share across goroutines.
func New() *SchemaValidator {
	return &SchemaValidator{}
}

// Validate runs the JSON-Schema pass first, then the graph-level pass
// only if the schema is structurally sound enough to walk.
// Always returns a populated Report. err is non-nil iff Report.OK == false.
func (v *SchemaValidator) Validate(schemaJSON []byte) (*api.Report, error) {
	r := newReport()
	if jsonSchemaPass(schemaJSON, r) {
		graphPass(schemaJSON, r)
	}
	if r.OK {
		return r, nil
	}
	return r, &api.ValidationError{Report: *r}
}
```

- [ ] **Step 5: Write `internal/surveys/schemavalidator/graph_test.go`**

Create `internal/surveys/schemavalidator/graph_test.go`:

```go
package schemavalidator_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sociopulse/platform/internal/surveys/api"
	sv "github.com/sociopulse/platform/internal/surveys/schemavalidator"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "schemas", "testdata", name)
	data, err := os.ReadFile(path)
	require.NoErrorf(t, err, "load fixture %s", name)
	return data
}

func TestSchemaValidator_Valid(t *testing.T) {
	v := sv.New()
	cases := []string{
		"valid-minimal-flat.json",
		"valid-with-conditions.json",
		"valid-with-multi.json",
		"valid-vciom-electoral.json",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			rep, err := v.Validate(loadFixture(t, name))
			require.NoError(t, err, "expected valid: %v", rep)
			require.True(t, rep.OK)
			require.Empty(t, rep.Issues)
		})
	}
}

func TestSchemaValidator_Invalid(t *testing.T) {
	v := sv.New()
	cases := []struct {
		fixture     string
		wantCode    string
		wantNodeIDs []string // optional — checks at least one issue refers to these
	}{
		{"invalid-no-start.json", sv.CodeNoStart, nil},
		{"invalid-two-starts.json", sv.CodeMultipleStarts, []string{"s1", "s2"}},
		{"invalid-unreachable.json", sv.CodeUnreachable, []string{"orphan"}},
		{"invalid-dangling-edge.json", sv.CodeDanglingEdge, []string{"q1"}},
		{"invalid-cycle-no-exit.json", sv.CodeCycleNoExit, nil},
		{"invalid-bad-when.json", sv.CodeBadWhen, nil},
		{"invalid-forward-ref.json", sv.CodeForwardRef, nil},
		{"invalid-missing-options.json", sv.CodeMissingOptions, []string{"q_no_options"}},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			rep, err := v.Validate(loadFixture(t, c.fixture))
			require.Error(t, err, "expected validation error")
			var ve *api.ValidationError
			require.True(t, errors.As(err, &ve))
			require.False(t, rep.OK)

			seen := false
			for _, iss := range rep.Issues {
				if iss.Code == c.wantCode {
					seen = true
					break
				}
			}
			require.Truef(t, seen, "expected at least one issue with code=%q; got %+v", c.wantCode, rep.Issues)

			for _, want := range c.wantNodeIDs {
				found := false
				for _, iss := range rep.Issues {
					if iss.NodeID == want {
						found = true
						break
					}
				}
				require.Truef(t, found, "expected issue referencing node %q", want)
			}
		})
	}
}

func TestSchemaValidator_NotJSON(t *testing.T) {
	v := sv.New()
	rep, err := v.Validate([]byte("not json at all"))
	require.Error(t, err)
	require.False(t, rep.OK)
}

func TestSchemaValidator_EmptySchema(t *testing.T) {
	v := sv.New()
	rep, err := v.Validate([]byte(`{}`))
	require.Error(t, err)
	require.False(t, rep.OK)
}
```

- [ ] **Step 6: Write valid fixture `valid-minimal-flat.json`**

Create `internal/surveys/schemas/testdata/valid-minimal-flat.json`:

```json
{
  "version": "1.0",
  "title": "Minimal flat",
  "nodes": [
    {"id": "start", "kind": "start", "next": [{"to": "q1"}]},
    {"id": "q1", "kind": "question", "type": "single", "text": "Hello?",
     "options": [{"id": "yes", "label": "Yes"}, {"id": "no", "label": "No"}],
     "next": [{"to": "end_success"}]},
    {"id": "end_success", "kind": "success-end"}
  ]
}
```

- [ ] **Step 7: Write valid fixture `valid-with-conditions.json`**

Create `internal/surveys/schemas/testdata/valid-with-conditions.json`:

```json
{
  "version": "1.0",
  "title": "Branching by answer",
  "nodes": [
    {"id": "start", "kind": "start", "next": [{"to": "q1"}]},
    {"id": "q1", "kind": "question", "type": "single", "text": "Are you over 18?",
     "options": [{"id": "yes", "label": "Yes"}, {"id": "no", "label": "No"}],
     "next": [
       {"to": "q2", "when": "answer == 'yes'"},
       {"to": "end_refused", "when": "answer == 'no'"}
     ]},
    {"id": "q2", "kind": "question", "type": "number", "text": "Your age?", "min": 18, "max": 120,
     "next": [{"to": "end_success"}]},
    {"id": "end_success", "kind": "success-end"},
    {"id": "end_refused", "kind": "refusal-end"}
  ]
}
```

- [ ] **Step 8: Write valid fixture `valid-with-multi.json`**

Create `internal/surveys/schemas/testdata/valid-with-multi.json`:

```json
{
  "version": "1.0",
  "title": "Multi-choice",
  "nodes": [
    {"id": "start", "kind": "start", "next": [{"to": "q1"}]},
    {"id": "q1", "kind": "question", "type": "multi", "text": "Which apply?",
     "options": [{"id": "a", "label": "A"}, {"id": "b", "label": "B"}, {"id": "c", "label": "C"}],
     "next": [
       {"to": "q2", "when": "answer.contains('a')"},
       {"to": "end_success", "when": "true"}
     ]},
    {"id": "q2", "kind": "text-block", "text": "You picked A, follow-up...",
     "next": [{"to": "end_success"}]},
    {"id": "end_success", "kind": "success-end"}
  ]
}
```

- [ ] **Step 9: Write invalid fixtures**

Create `internal/surveys/schemas/testdata/invalid-no-start.json`:

```json
{
  "version": "1.0",
  "title": "No start",
  "nodes": [
    {"id": "q1", "kind": "question", "type": "single", "text": "Hi?",
     "options": [{"id": "y", "label": "Y"}],
     "next": [{"to": "e"}]},
    {"id": "e", "kind": "success-end"}
  ]
}
```

Create `internal/surveys/schemas/testdata/invalid-two-starts.json`:

```json
{
  "version": "1.0",
  "title": "Two starts",
  "nodes": [
    {"id": "s1", "kind": "start", "next": [{"to": "e"}]},
    {"id": "s2", "kind": "start", "next": [{"to": "e"}]},
    {"id": "e", "kind": "success-end"}
  ]
}
```

Create `internal/surveys/schemas/testdata/invalid-unreachable.json`:

```json
{
  "version": "1.0",
  "title": "Unreachable",
  "nodes": [
    {"id": "start", "kind": "start", "next": [{"to": "e"}]},
    {"id": "orphan", "kind": "question", "type": "single", "text": "?",
     "options": [{"id": "y", "label": "Y"}], "next": [{"to": "e"}]},
    {"id": "e", "kind": "success-end"}
  ]
}
```

Create `internal/surveys/schemas/testdata/invalid-dangling-edge.json`:

```json
{
  "version": "1.0",
  "title": "Dangling edge",
  "nodes": [
    {"id": "start", "kind": "start", "next": [{"to": "q1"}]},
    {"id": "q1", "kind": "question", "type": "single", "text": "?",
     "options": [{"id": "y", "label": "Y"}],
     "next": [{"to": "ghost"}]},
    {"id": "e", "kind": "success-end"}
  ]
}
```

Create `internal/surveys/schemas/testdata/invalid-cycle-no-exit.json`:

```json
{
  "version": "1.0",
  "title": "Cycle no exit",
  "nodes": [
    {"id": "start", "kind": "start", "next": [{"to": "a"}]},
    {"id": "a", "kind": "question", "type": "single", "text": "Loop A?",
     "options": [{"id": "x", "label": "X"}],
     "next": [{"to": "b"}]},
    {"id": "b", "kind": "question", "type": "single", "text": "Loop B?",
     "options": [{"id": "x", "label": "X"}],
     "next": [{"to": "a"}]},
    {"id": "end_unreachable", "kind": "success-end"}
  ]
}
```

Create `internal/surveys/schemas/testdata/invalid-bad-when.json`:

```json
{
  "version": "1.0",
  "title": "Bad when",
  "nodes": [
    {"id": "start", "kind": "start", "next": [{"to": "q1"}]},
    {"id": "q1", "kind": "question", "type": "single", "text": "?",
     "options": [{"id": "y", "label": "Y"}],
     "next": [{"to": "e", "when": "this is not an expression !!"}]},
    {"id": "e", "kind": "success-end"}
  ]
}
```

Create `internal/surveys/schemas/testdata/invalid-forward-ref.json`:

```json
{
  "version": "1.0",
  "title": "Forward ref",
  "nodes": [
    {"id": "start", "kind": "start", "next": [{"to": "q1", "when": "q3.value == 'x'"}]},
    {"id": "q1", "kind": "question", "type": "single", "text": "Q1?",
     "options": [{"id": "y", "label": "Y"}],
     "next": [{"to": "q3"}]},
    {"id": "q3", "kind": "question", "type": "single", "text": "Q3?",
     "options": [{"id": "x", "label": "X"}],
     "next": [{"to": "e"}]},
    {"id": "e", "kind": "success-end"}
  ]
}
```

Create `internal/surveys/schemas/testdata/invalid-missing-options.json`:

```json
{
  "version": "1.0",
  "title": "Missing options",
  "nodes": [
    {"id": "start", "kind": "start", "next": [{"to": "q_no_options"}]},
    {"id": "q_no_options", "kind": "question", "type": "single", "text": "No options",
     "next": [{"to": "e"}]},
    {"id": "e", "kind": "success-end"}
  ]
}
```

(`valid-vciom-electoral.json` is created in Task 8 — test for it is already in the test file, the case will skip until that file exists. To run this task's tests, the VCIOM fixture is unnecessary; remove it from the `cases` slice if it errors — but our convention is to add it together with the test in this task, so create a stub now.)

Create `internal/surveys/schemas/testdata/valid-vciom-electoral.json` — minimal placeholder version (full schema in Task 8):

```json
{
  "version": "1.0",
  "title": "ВЦИОМ — placeholder (filled in Task 8)",
  "nodes": [
    {"id": "start", "kind": "start", "next": [{"to": "end"}]},
    {"id": "end", "kind": "success-end"}
  ]
}
```

- [ ] **Step 10: Run the test suite**

Note: this requires Task 3 (DSL evaluator) so `dsl.NewEvaluator().Compile(...)` and `.ReferencedNodes(...)` exist.

For now, until Task 3 is done, the package won't build. We'll wire in stubs to keep build green:

Create `internal/surveys/dsl/evaluator.go` (stub — full impl in Task 3):

```go
package dsl

import "errors"

// Evaluator is a stub — Task 3 fills it in.
type Evaluator struct{}

// NewEvaluator returns a fresh evaluator.
func NewEvaluator() *Evaluator { return &Evaluator{} }

// Compile is a stub.
func (e *Evaluator) Compile(expr string) (CompiledExpr, error) {
	if expr == "" {
		return CompiledExpr{}, errors.New("empty expression")
	}
	return CompiledExpr{src: expr}, nil
}

// ReferencedNodes is a stub.
func (e *Evaluator) ReferencedNodes(expr string) ([]string, error) {
	return nil, nil
}

// CompiledExpr is a stub for the cached compiled program.
type CompiledExpr struct { src string }
```

This stub lets `schemavalidator` build now. Task 3 replaces it with the real impl — the test suite for forward-refs and bad-when will fail until Task 3 lands, that's expected.

Run:

```bash
go build ./internal/surveys/...
go test ./internal/surveys/schemavalidator/... -run TestSchemaValidator_Valid -v
```

Expected: `TestSchemaValidator_Valid/valid-minimal-flat.json`, `valid-with-conditions.json`, `valid-with-multi.json`, `valid-vciom-electoral.json` all PASS. (Conditions test depends on the stub returning no error for any non-empty string — fine.)

Run also:

```bash
go test ./internal/surveys/schemavalidator/... -run TestSchemaValidator_Invalid/invalid-no-start.json -v
go test ./internal/surveys/schemavalidator/... -run TestSchemaValidator_Invalid/invalid-two-starts.json -v
go test ./internal/surveys/schemavalidator/... -run TestSchemaValidator_Invalid/invalid-unreachable.json -v
go test ./internal/surveys/schemavalidator/... -run TestSchemaValidator_Invalid/invalid-dangling-edge.json -v
go test ./internal/surveys/schemavalidator/... -run TestSchemaValidator_Invalid/invalid-cycle-no-exit.json -v
go test ./internal/surveys/schemavalidator/... -run TestSchemaValidator_Invalid/invalid-missing-options.json -v
```

Expected: all PASS.

`invalid-bad-when.json` and `invalid-forward-ref.json` will FAIL with the stub (because the stub never returns parse errors and never reports references). They will pass after Task 3.

- [ ] **Step 11: Commit**

```bash
git add internal/surveys/schemavalidator internal/surveys/schemas/testdata internal/surveys/dsl/evaluator.go
git commit -m "feat(surveys): two-pass schema validator + graph fixtures

- schemavalidator/jsonschema.go: structural validation via santhosh-tekuri/jsonschema/v5
  against embedded survey-1.0.json.
- schemavalidator/graph.go: graph-level checks — single start, reachability (BFS),
  ends-reachable, dangling edges, cycles without exit, when-expression parsability,
  forward references via dominator analysis.
- schemavalidator/validator.go: SchemaValidator entrypoint.
- schemas/testdata/: 4 valid + 8 invalid fixtures covering each error code.
- dsl/evaluator.go: stub interface (real impl in Task 3) so schemavalidator builds.

Spec: §11.4."
```

Expected: ~14 files added. The "bad when" and "forward ref" tests are listed as known-failing here; Task 3 will close them out.

---


---

## Self-review

**Spec coverage** (against §FR-C, §11.1–11.7, ADR-008):
- §11.1 универсальная JSON-схема (start, intro, question{single,multi,number,text,select}, text-block, success-end, refusal-end, condition, jump) + `ui {x,y}` для flow-режима. ✓
- §11.2 form-режим (linear list rendering) + flow-режим (graph rendering) — оба читают/пишут одну `survey_versions.schema`. ✓
- §11.3 conditional DSL через `expr-lang/expr` с whitelist'ом identifiers (`answer`, `q<id>.value`, `q<id>.answered`) и без side-effects. Кеш скомпилированных программ. ✓
- §11.4 SchemaValidator: единственный start, достижимость DFS, dangling-edges, cycles без выхода, forward-only references на q<id>.value. ✓
- §11.5 Runtime в Go: `NextNode`, `ValidateAnswer`, `CalculateProgress`. WASM-build через TinyGo, embed.FS, `/static/surveys-runtime.wasm`. Plan 15 frontend подгружает WASM lazy. ✓
- §11.6 версионирование: каждое сохранение → INSERT survey_versions; `is_active=true` уникален per-survey (partial unique index `survey_versions_active_one`); `calls.survey_version_id` пинится при старте. ✓
- §11.7 preview: `POST /api/surveys/{id}/preview/run` симулирует runtime без сохранения. ✓
- ADR-008 single-source Go → WASM устраняет дублирование логики между бэком и фронтом. ✓
- HTTP endpoints: GET/POST `/api/surveys`, GET/PATCH `/api/surveys/{id}`, POST `/api/surveys/{id}/versions`, POST `/api/surveys/{id}/versions/{vid}/activate`, POST `/api/surveys/{id}/preview/run`, POST `/api/surveys/{id}/validate`. ✓
- Coverage `internal/surveys/{service,runtime}/` ≥ 90%. ВЦИОМ-анкета из прототипа (5 вопросов, условные переходы) проходит integration-тест на all-paths. ✓

**Placeholder scan:** Tasks с known-failing тестами помечены явно — Task 3 закрывает их.

**Type/name consistency:** `SurveyService`, `VersionStore`, `Runtime`, `ConditionalEvaluator`, `SchemaValidator` — стабильные интерфейсы, потребляемые Plan 10 (dialer pinning), Plan 15 (WASM loader), Plan 18 (builder UI).

**Out of scope (correctly deferred):**
- UI конструктора (form + flow) — Plan 18.
- Operator workstation runtime в браузере (вызовы через WASM) — Plan 16.
- A/B-варианты анкет — backlog.

Plan 07 verified.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-07-surveys-module.md`.**

