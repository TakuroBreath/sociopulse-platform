# 02. Module Contracts

This document is the **specification of every public surface** in
`internal/<module>/api/`. Task 4 of Plan 00a implements these as Go files.
Implementation plans (Plans 04 through 14) consume these contracts and
must not redefine them.

For each module:

- One paragraph of responsibility.
- The list of public interfaces (interface name + method signatures).
- The list of public DTOs (struct name + field set, condensed).
- The list of sentinel errors and the sentence each one means.
- The list of NATS subjects published / consumed (canonical scheme from
  spec §10.2: `tenant.<tenant_id>.<area>.<entity>.<id>.<event>`) and any
  asynq task type strings.

Signatures are quoted verbatim from the source plan files. Where a
signature is long, the field set is condensed but every name is preserved.

Naming conventions: errors are `var ErrXxx = errors.New("module:
description")`. Subject constants are `Subject<Verb>` (e.g.
`SubjectProjectCreated`). Asynq task types are `Task<Verb>` (e.g.
`TaskRespondentImport`). DTOs are CamelCase nouns; request DTOs end in
`Request` or `Input`, response DTOs in `Response`, `Result`, `Output`, or
the noun itself.

A compile-time check for every adapter implementing an `api/` interface
lives next to the adapter:

```go
var _ api.Hub = (*service.Hub)(nil)
```

The 12 sections below cover every module. The order matches the
dependency graph: leaves first (`audit`, `tenancy`), then everything else.

---

## 1. `audit`

**Responsibility.** Append-only audit log. Provides one Write entry point
that other modules call after every state-changing action; persists rows
to the `audit_log` table; runs the weekly archive pass that moves rows
older than one year to S3 cold tier (FR-K1, FR-K4). Has no internal
dependencies — pure leaf module.

### Interfaces

```go
// internal/audit/api/

type Logger interface {
    // Write inserts an audit row. payload may include any JSON-encodable
    // value; the implementation strips redaction patterns before INSERT.
    Write(ctx context.Context, e Event) error
}

type Reader interface {
    // List returns audit rows for a tenant filtered by action and time.
    // Used by the admin "audit log" page and by 152-ФЗ subject-rights
    // handlers (FR-K).
    List(ctx context.Context, f ListFilter) ([]Event, string /* nextCursor */, error)
}

type Archiver interface {
    // ArchivePass moves rows with ts < cutoff to cold-tier S3 and deletes
    // them from Postgres. Idempotent: re-runs after partial failure.
    // Run by cmd/worker on a weekly schedule.
    ArchivePass(ctx context.Context, cutoff time.Time) (movedRows int64, err error)
}
```

### DTOs

```go
type Event struct {
    ID         uuid.UUID
    TenantID   uuid.UUID         // optional: cross-tenant Service-Owner events
    ActorID    *uuid.UUID        // user_id or nil for system actions
    ActorKind  ActorKind         // user | system | service-owner
    Action     string            // e.g. "auth.login", "recording.accessed"
    Target     string            // resource pointer ("call:<id>", "user:<id>")
    Payload    map[string]any    // jsonb, redacted
    IP         netip.Addr
    UserAgent  string
    Timestamp  time.Time
}

type ListFilter struct {
    TenantID  uuid.UUID
    Action    string             // exact match if non-empty
    From, To  time.Time
    ActorID   *uuid.UUID
    Cursor    string             // opaque (timestamp + id encoded)
    Limit     int                // 1..500, default 100
}

type ActorKind string

const (
    ActorUser         ActorKind = "user"
    ActorSystem       ActorKind = "system"
    ActorServiceOwner ActorKind = "service-owner"
)
```

### Sentinel Errors

```go
var (
    ErrInvalidEvent = errors.New("audit: invalid event")  // missing required field
    ErrNotFound     = errors.New("audit: not found")
)
```

### Events

Publishes (durable, JetStream stream `AUDIT`, retention 90 days):

| Subject | Payload | Notes |
|---|---|---|
| `tenant.<t>.audit.event` | `Event` | Mirrored by `analytics-ingestor` for long-window queries; consumed by the cold-tier archiver after 1 y. |

Consumes: none.

asynq tasks: none (`ArchivePass` is invoked from `cmd/worker` via a
scheduled cron, not a task queue).

---

## 2. `tenancy`

**Responsibility.** Owns the multi-tenancy primitive: `tenants` and
`tenant_settings` rows, per-tenant Yandex KMS KEK lifecycle, envelope
encryption (DEK per payload, KEK per tenant), HMAC-SHA256 phone hashing
with per-tenant pepper, settings cache with NATS-driven invalidation, and
per-tenant S3 bucket provisioning. The trunk module: every other module
depends on it for tenant context, encryption, and settings lookup.

### Interfaces

```go
// internal/tenancy/api/

type TenantService interface {
    Create(ctx context.Context, req CreateTenantRequest) (Tenant, error)
    Get(ctx context.Context, id uuid.UUID) (Tenant, error)
    GetByOrgCode(ctx context.Context, orgCode string) (Tenant, error)
    List(ctx context.Context, filter ListTenantsFilter) ([]Tenant, error)
    Suspend(ctx context.Context, id uuid.UUID, reason string) error
    Resume(ctx context.Context, id uuid.UUID) error
    Archive(ctx context.Context, id uuid.UUID) error
}

type KMSResolver interface {
    EnsureKEK(ctx context.Context, tenantID uuid.UUID) (kekID string, err error)
    GenerateDataKey(ctx context.Context, tenantID uuid.UUID) (DataKey, error)
    Encrypt(ctx context.Context, tenantID uuid.UUID, plaintext []byte) ([]byte, error)
    Decrypt(ctx context.Context, tenantID uuid.UUID, ciphertext []byte) ([]byte, error)
    InvalidateCache(tenantID uuid.UUID)
}

type PhoneHasher interface {
    Hash(ctx context.Context, tenantID uuid.UUID, phone string) ([]byte, error)
    Normalise(phone string) (string, error)
}

type SettingsCache interface {
    // Lookup* methods avoid the Get-name collision with TenantService.Get
    // so the Tenancy aggregate (below) can embed all four sub-interfaces
    // directly. See `internal/tenancy/api/doc.go` for the rationale.
    Lookup(ctx context.Context, tenantID uuid.UUID, key string) (SettingValue, error)
    LookupWithDefault(ctx context.Context, tenantID uuid.UUID, key string, def SettingValue) (SettingValue, error)
    LookupAll(ctx context.Context, tenantID uuid.UUID) (map[string]SettingValue, error)
    Set(ctx context.Context, tenantID uuid.UUID, key string, value SettingValue) error
    Delete(ctx context.Context, tenantID uuid.UUID, key string) error
    InvalidateLocal(tenantID uuid.UUID, key string)
    InvalidateAllLocal(tenantID uuid.UUID)
}

type BucketProvisioner interface {
    EnsureBucket(ctx context.Context, tenantID uuid.UUID) (bucket string, err error)
}

// Aggregate exposed to other modules — saves them taking four deps.
//
// SettingsCache uses Lookup/LookupWithDefault/LookupAll (rather than the
// historical Get/GetWithDefault/GetAll) so the four sub-interfaces have a
// disjoint method set and can be embedded directly. The compile-time test
// fixture `var _ interface{ TenantService; SettingsCache; KMSResolver;
// PhoneHasher } = (Tenancy)(nil)` in `internal/tenancy/api/types_test.go`
// guards this invariant.
type Tenancy interface {
    TenantService
    SettingsCache
    KMSResolver
    PhoneHasher
}

// Module-internal interfaces declared in api/ so that test doubles in
// service-layer tests can satisfy them without importing the internal
// store package. Plan 04 Task 2 wires the concrete PostgresStore.
//
// Mutating Store methods accept a postgres.Tx so the service can
// co-locate the row write with a transactional outbox Append (and an
// eventual audit row) inside a single tenancy_admin transaction.
type Store interface {
    // Mutating: caller owns tx (tenancy_admin BypassRLS path).
    Insert(ctx context.Context, tx postgres.Tx, t Tenant) (Tenant, error)
    UpdateStatus(ctx context.Context, tx postgres.Tx, id uuid.UUID, status TenantStatus) error
    UpsertSetting(ctx context.Context, tx postgres.Tx, tenantID uuid.UUID, key string, value SettingValue) error
    DeleteSetting(ctx context.Context, tx postgres.Tx, tenantID uuid.UUID, key string) error

    // Read-only: store opens a short-lived BypassRLS tx internally.
    Get(ctx context.Context, id uuid.UUID) (Tenant, error)
    GetByOrgCode(ctx context.Context, orgCode string) (Tenant, error)
    List(ctx context.Context, filter ListTenantsFilter) ([]Tenant, error)
    GetPhoneHashPepper(ctx context.Context, tenantID uuid.UUID) ([]byte, error)
    GetSetting(ctx context.Context, tenantID uuid.UUID, key string) (SettingValue, error)
    GetAllSettings(ctx context.Context, tenantID uuid.UUID) (map[string]SettingValue, error)
}

// SettingsPublisher abstracts the message-bus emission of lifecycle and
// cache-invalidation events so the service layer is testable without
// NATS. The durable lifecycle path is the transactional outbox (pkg/outbox);
// SettingsPublisher is best-effort cache invalidation.
type SettingsPublisher interface {
    PublishCreated(ctx context.Context, t Tenant) error
    PublishSuspended(ctx context.Context, tenantID uuid.UUID) error
    PublishArchived(ctx context.Context, tenantID uuid.UUID) error
    PublishSettingUpdated(ctx context.Context, tenantID uuid.UUID, key string) error
    PublishSettingDeleted(ctx context.Context, tenantID uuid.UUID, key string) error
}
```

### DTOs

```go
type TenantStatus string

const (
    TenantStatusActive    TenantStatus = "active"
    TenantStatusSuspended TenantStatus = "suspended"
    TenantStatusArchived  TenantStatus = "archived"
)

type Tenant struct {
    ID              uuid.UUID
    OrgCode         string         // public code, e.g. "CC-MOSKVA-01"
    Name            string
    Status          TenantStatus
    KMSKEKID        string         // Yandex KMS symmetric key ID
    PhoneHashPepper []byte         // 32 bytes; json:"-" — never serialised
    CreatedAt       time.Time
}

type CreateTenantRequest struct {
    OrgCode string
    Name    string
}

type ListTenantsFilter struct {
    Status  *TenantStatus
    OrgCode string         // exact match if non-empty
    Limit   int
    Offset  int
}

type DataKey struct {
    Plaintext  []byte // 32 bytes for AES-256
    Ciphertext []byte // KMS-encrypted blob
    KeyVersion string // KEK version that wrapped this DEK
}

type SettingValue struct{ /* json.RawMessage with typed accessors */ }

func SettingValueFromAny(v any) (SettingValue, error)
func SettingValueFromRaw(b []byte) SettingValue
func (v SettingValue) AsString() (string, error)
func (v SettingValue) AsInt() (int64, error)
func (v SettingValue) AsBool() (bool, error)
func (v SettingValue) AsDuration() (time.Duration, error)
func (v SettingValue) AsJSON(dst any) error
```

### Sentinel Errors

```go
var (
    ErrNotFound         = errors.New("tenancy: not found")
    ErrAlreadyExists    = errors.New("tenancy: already exists")
    ErrInvalidArgument  = errors.New("tenancy: invalid argument")
    ErrSuspended        = errors.New("tenancy: suspended")
    ErrArchived         = errors.New("tenancy: archived")
    ErrKMSUnavailable   = errors.New("tenancy: kms unavailable")
    ErrPermissionDenied = errors.New("tenancy: permission denied")
)
```

### Events

Publishes (best-effort core-NATS, no retention):

| Subject | Payload |
|---|---|
| `tenant.<t>.created` | `Tenant` |
| `tenant.<t>.suspended` | `{tenant_id, reason}` |
| `tenant.<t>.resumed` | `{tenant_id}` |
| `tenant.<t>.archived` | `{tenant_id}` |
| `tenant.<t>.settings.updated` | `{tenant_id, key}` (cache invalidation) |

Consumes: `tenant.<t>.settings.updated` (peers invalidate local cache).

asynq tasks: none.

---

## 3. `auth`

**Responsibility.** Authentication, sessions, and access control.
Argon2id password hashing, JWT issuance/validation (HS256, access 15 min,
refresh 30 days, refresh-rotation reuse detection), TOTP enroll/verify,
RBAC matrix (operator/supervisor/admin), per-IP and per-account rate
limiting + lockout, force-logout-all session revocation. Exposes mTLS-
free endpoints under `/api/auth/*`.

### Interfaces

```go
// internal/auth/api/

type Authenticator interface {
    Login(ctx context.Context, in LoginInput) (AuthResult, error)
    LoginTOTP(ctx context.Context, in LoginTOTPInput) (AuthResult, error)
    Refresh(ctx context.Context, refreshToken string, ip netip.Addr) (AuthResult, error)
    Logout(ctx context.Context, refreshToken string) error
    ValidateAccessToken(ctx context.Context, accessToken string) (Claims, error)
}

type UserService interface {
    Create(ctx context.Context, in CreateUserInput) (User, string /* temp password */, error)
    List(ctx context.Context, in ListUsersInput) ([]User, int64, error)
    Get(ctx context.Context, id uuid.UUID) (User, error)
    UpdateRole(ctx context.Context, id uuid.UUID, roles []Role) (User, error)
    Archive(ctx context.Context, id uuid.UUID) error
    Restore(ctx context.Context, id uuid.UUID) error
    ResetPassword(ctx context.Context, id uuid.UUID) (string /* temp password */, error)
    ChangePassword(ctx context.Context, id uuid.UUID, oldPassword, newPassword string) error
}

type SessionRevoker interface {
    RevokeSession(ctx context.Context, sid string) error
    RevokeAllForUser(ctx context.Context, userID uuid.UUID) error
    IsRevoked(ctx context.Context, sid, jti string) (bool, error)
}

type RBACChecker interface {
    Check(ctx context.Context, claims Claims, action Action, resource Resource) error
}

type JWTIssuer interface {
    IssueAccess(c Claims) (token string, expiresAt time.Time, err error)
    IssueRefresh(c Claims) (token string, expiresAt time.Time, err error)
    Validate(token, expectedType string) (Claims, error)
}

type TOTPService interface {
    Enroll(ctx context.Context, userID uuid.UUID) (TOTPEnrollment, error)
    Confirm(ctx context.Context, userID uuid.UUID, code string) error
    Verify(ctx context.Context, userID uuid.UUID, code string) error
    Disable(ctx context.Context, userID uuid.UUID) error
    Status(ctx context.Context, userID uuid.UUID) (TOTPStatus, error)
}
```

### DTOs

```go
type Role string

const (
    RoleOperator   Role = "operator"
    RoleSupervisor Role = "supervisor"
    RoleAdmin      Role = "admin"
)

type Claims struct {
    UserID    uuid.UUID `json:"sub"`
    TenantID  uuid.UUID `json:"tid"`
    Login     string    `json:"login"`
    Roles     []Role    `json:"roles"`
    SessionID string    `json:"sid"` // stable across access+refresh
    JTI       string    `json:"jti"` // unique per token
    IssuedAt  time.Time `json:"iat"`
    ExpiresAt time.Time `json:"exp"`
    TOTPDone  bool      `json:"totp_done,omitempty"`
}

type AuthResult struct {
    AccessToken      string
    AccessExpiresAt  time.Time
    RefreshToken     string
    RefreshExpiresAt time.Time
    User             User
    TOTPRequired     bool // if true caller must complete /login/totp
}

type User struct {
    ID            uuid.UUID
    TenantID      uuid.UUID
    Login         string
    FullName      string
    Email         string
    Roles         []Role
    TOTPEnabled   bool
    MustChangePwd bool
    CreatedAt, UpdatedAt time.Time
    ArchivedAt    *time.Time
}

type LoginInput struct {
    OrgID    string // public tenant code
    Login    string
    Password string
    IP       netip.Addr
    UserAgent string
}

type LoginTOTPInput struct {
    PartialToken string // short-lived (5 min) returned by Login when TOTP_required
    Code         string
    IP           netip.Addr
    UserAgent    string
}

type CreateUserInput struct {
    TenantID uuid.UUID
    Login    string
    FullName string
    Email    string
    Roles    []Role
    ActorID  uuid.UUID
}

type ListUsersInput struct {
    TenantID        uuid.UUID
    IncludeArchived bool
    Limit           int32
    Offset          int32
}

type Action string  // e.g. "user.create", "recording.access"
type Resource struct {
    Kind string         // "user", "project", "call", ...
    ID   uuid.UUID      // optional; zero means "any"
}

type TOTPEnrollment struct {
    Secret      string   // base32, returned ONCE at enroll
    OTPAuthURL  string   // otpauth://...
    BackupCodes []string // 8-10 single-use codes, returned ONCE
}

type TOTPStatus struct {
    Enabled       bool
    EnrolledAt    *time.Time
    LastVerifiedAt *time.Time
    BackupRemaining int
}
```

### Sentinel Errors

```go
var (
    ErrInvalidCredentials = errors.New("auth: invalid credentials")
    ErrAccountLocked      = errors.New("auth: account locked")
    ErrAccountArchived    = errors.New("auth: account archived")
    ErrTOTPRequired       = errors.New("auth: TOTP required")
    ErrTOTPInvalid        = errors.New("auth: TOTP code invalid")
    ErrPasswordExpired    = errors.New("auth: password must be changed")
    ErrTokenInvalid       = errors.New("auth: token invalid or expired")
    ErrTokenRevoked       = errors.New("auth: token revoked")
    ErrRateLimitExceeded  = errors.New("auth: rate limit exceeded")
    ErrInsufficientRole   = errors.New("auth: insufficient role")
    ErrRefreshReplay      = errors.New("auth: refresh-token replay detected")
)
```

### Events

Publishes (consumed by `audit` aggregator):

| Subject | When |
|---|---|
| `tenant.<t>.audit.event` (action=`auth.login`) | successful login |
| `tenant.<t>.audit.event` (action=`auth.logout`) | logout |
| `tenant.<t>.audit.event` (action=`auth.totp_enrolled`) | TOTP enroll confirmed |
| `tenant.<t>.audit.event` (action=`auth.session_revoked`) | force-logout-all |
| `tenant.<t>.audit.event` (action=`auth.refresh_replay`) | refresh-rotation reuse |

Consumes: none.

asynq tasks: none.

---

## 4. `crm`

**Responsibility.** Project lifecycle, respondents, quotas, DNC, async
CSV/XLSX import, deletion right (152-ФЗ §13.3 — 30-day soft-delete +
worker purge). Russian phone validation/normalization (E.164 + АВС/DEF
prefix mapping). Real-time quota tracker with Redis cache and Postgres
reconciliation worker.

### Interfaces

```go
// internal/crm/api/

type ProjectService interface {
    Create(ctx context.Context, in CreateProjectInput) (*Project, error)
    Get(ctx context.Context, id uuid.UUID) (*Project, error)
    List(ctx context.Context, f ListProjectsFilter) (*ListProjectsResult, error)
    Update(ctx context.Context, id uuid.UUID, in UpdateProjectInput) (*Project, error)
    Pause(ctx context.Context, id uuid.UUID) error
    Resume(ctx context.Context, id uuid.UUID) error
    Archive(ctx context.Context, id uuid.UUID) error
    GetProgress(ctx context.Context, id uuid.UUID) (*ProjectProgress, error)
    Assign(ctx context.Context, id uuid.UUID, operatorIDs []uuid.UUID) error
    Unassign(ctx context.Context, id uuid.UUID, operatorID uuid.UUID) error
    ListMembers(ctx context.Context, id uuid.UUID) ([]ProjectMember, error)
}

type RespondentService interface {
    Create(ctx context.Context, in CreateRespondentInput) (*Respondent, error)
    Get(ctx context.Context, id uuid.UUID) (*Respondent, error)
    GetWithPhone(ctx context.Context, id uuid.UUID) (*Respondent, error) // admin-only
    Search(ctx context.Context, f SearchRespondentsFilter) (*SearchRespondentsResult, error)
    Delete(ctx context.Context, id uuid.UUID) (*DeletionRequest, error)
    Import(ctx context.Context, req ImportRequest) (*ImportTicket, error)
    GetImportStatus(ctx context.Context, jobID string) (*ImportStatus, error)
}

type QuotaTracker interface {
    IsFull(ctx context.Context, projectID uuid.UUID, dims map[string]string) (bool, error)
    Increment(ctx context.Context, projectID uuid.UUID, dims map[string]string) error
    GetProgress(ctx context.Context, projectID uuid.UUID) ([]QuotaSnapshot, error)
}

type DNCManager interface {
    IsBlocked(ctx context.Context, projectID uuid.UUID, phone string) (bool, error)
    Add(ctx context.Context, projectID *uuid.UUID, phone, source string) error
    Remove(ctx context.Context, projectID *uuid.UUID, phone string) error
    Import(ctx context.Context, projectID *uuid.UUID, csv []byte) (added int, err error)
    List(ctx context.Context, projectID *uuid.UUID, page, pageSize int) ([]DNCEntry, int, error)
}
```

### DTOs

```go
type ProjectStatus string

const (
    StatusActive   ProjectStatus = "active"
    StatusPaused   ProjectStatus = "paused"
    StatusArchived ProjectStatus = "archived"
)

type Project struct {
    ID, TenantID                      uuid.UUID
    Code, Name, Customer              string
    Status                            ProjectStatus
    TargetCount                       int
    PeriodFrom, PeriodTo              *time.Time
    SurveyID, DefaultSurveyVersionID  *uuid.UUID
    IsAdvertising                     bool
    CreatedAt                         time.Time
    Quotas                            []Quota
    Assignments                       []ProjectMember
}

type CreateProjectInput struct {
    Code, Name, Customer string
    TargetCount          int
    PeriodFrom, PeriodTo *time.Time
    SurveyID             *uuid.UUID
    IsAdvertising        bool
    InitialQuotas        []Quota
    InitialMembers       []uuid.UUID
}

type UpdateProjectInput struct {
    Name, Customer       *string
    TargetCount          *int
    PeriodFrom, PeriodTo *time.Time
    SurveyID             *uuid.UUID
}

type ProjectMember struct {
    OperatorID uuid.UUID
    AssignedAt time.Time
}

type ListProjectsFilter struct {
    Status   *ProjectStatus
    Search   string
    Page     int
    PageSize int
}

type ListProjectsResult struct {
    Items      []Project
    TotalCount int
}

type ProjectProgress struct {
    ProjectID                                  uuid.UUID
    TargetCount, CompletedCount, InProgressCount, PendingCount, DNCCount, ExhaustedCount, WrongCount int
    PercentDone                                float64
    PaceLast24h                                int
    ETACompletion                              *time.Time
    QuotaProgress                              []QuotaSnapshot
}

type Quota struct {
    DimensionKind  string  // "region" | "gender" | "age_bucket" | "custom"
    DimensionValue string
    Target         int
}

type QuotaSnapshot struct {
    DimensionKind, DimensionValue string
    Target, Done                  int
    PercentDone                   float64
    IsFull                        bool
}

type RespondentStatus string

const (
    RespPending           RespondentStatus = "pending"
    RespDialing           RespondentStatus = "dialing"
    RespCompleted         RespondentStatus = "completed"
    RespDNC               RespondentStatus = "dnc"
    RespExhausted         RespondentStatus = "exhausted"
    RespWrong             RespondentStatus = "wrong"
    RespDeletionRequested RespondentStatus = "deletion-requested"
)

type Respondent struct {
    ID, TenantID, ProjectID  uuid.UUID
    PhoneMasked              string         // "+7-9** ***-**-12" — display-safe
    Phone                    string         // populated only by GetWithPhone
    RegionCode               string
    Attributes               map[string]any
    Status                   RespondentStatus
    Attempts                 int
    LastAttemptAt, NextAttemptAt *time.Time
    Source                   string         // "imported" | "rdd"
    CreatedAt                time.Time
    DeleteAt                 *time.Time
}

type CreateRespondentInput struct {
    ProjectID  uuid.UUID
    Phone      string
    RegionCode string
    Attributes map[string]any
}

type SearchRespondentsFilter struct {
    ProjectID    uuid.UUID
    Status       *RespondentStatus
    PhoneSearch  string
    Region       string
    Page, PageSize int
}

type SearchRespondentsResult struct {
    Items      []Respondent
    TotalCount int
}

type ImportRequest struct {
    ProjectID    uuid.UUID
    Filename     string
    ContentType  string
    Body         []byte
    ColumnMap    map[string]string
    DefaultAttrs map[string]any
}

type ImportTicket struct {
    JobID     string
    ProjectID uuid.UUID
    Total     int
    StartedAt time.Time
}

type ImportStatus struct {
    JobID                    string
    State                    string  // "queued" | "running" | "succeeded" | "failed"
    Total, Processed, Inserted, Skipped int
    Errors                   []ImportError
    StartedAt                time.Time
    FinishedAt               *time.Time
}

type ImportError struct {
    Row     int
    Phone   string
    Message string
}

type DeletionRequest struct {
    RespondentID uuid.UUID
    DeleteAt     time.Time
}

type DNCEntry struct {
    PhoneMasked string
    Source      string
    AddedAt     int64
}
```

### Sentinel Errors

```go
var (
    ErrProjectNotFound      = errors.New("crm: project not found")
    ErrProjectCodeTaken     = errors.New("crm: project code already exists in tenant")
    ErrProjectArchived      = errors.New("crm: project is archived")
    ErrInvalidStatus        = errors.New("crm: invalid status transition")
    ErrRespondentNotFound   = errors.New("crm: respondent not found")
    ErrInvalidPhone         = errors.New("crm: invalid phone")
    ErrPhoneInDNC           = errors.New("crm: phone in DNC")
    ErrDuplicateRespondent  = errors.New("crm: duplicate respondent (phone_hash)")
    ErrInvalidQuotaKind     = errors.New("crm: unknown quota dimension")
    ErrImportInProgress     = errors.New("crm: another import already running")
    ErrImportPayloadTooBig  = errors.New("crm: import payload exceeds limit")
    ErrAdvertisingRejected  = errors.New("crm: is_advertising=true is not allowed in v1")
)
```

### Events

Publishes (NATS subjects):

| Subject constant | Subject string |
|---|---|
| `SubjectProjectCreated`   | `crm.project.created` |
| `SubjectProjectUpdated`   | `crm.project.updated` |
| `SubjectProjectStatus`    | `crm.project.status_changed` |
| `SubjectImportStarted`    | `crm.respondents.import.started` |
| `SubjectImportProgress`   | `crm.respondents.import.progress` |
| `SubjectImportFinished`   | `crm.respondents.import.finished` |
| `SubjectImportFailed`     | `crm.respondents.import.failed` |
| `SubjectRespondentDelete` | `crm.respondent.deletion_requested` |
| `SubjectQuotaIncrement`   | `crm.quota.incremented` |
| `SubjectDNCAdded`         | `crm.dnc.added` |

Consumes: none directly.

asynq task types:

| Constant | Task type |
|---|---|
| `TaskRespondentImport` | `crm:respondent.import` |
| `TaskRespondentsPurge` | `crm:respondents.purge` |
| `TaskQuotasRecompute`  | `crm:quotas.recompute` |
| `TaskDNCImport`        | `crm:dnc.import` |

---

## 5. `surveys`

**Responsibility.** Survey definitions and immutable versions, JSON
schema validation (`docs/api/schemas/survey-1.0.json`), graph validation
(unreachability, cycle-without-exit, dangling edges, forward refs in
DSL), DSL evaluator (`expr-lang/expr` subset), runtime (next-node + answer
validation + progress estimate), version activation atomicity. The
runtime is also compiled to WebAssembly for browser preview (ADR-0008).

### Interfaces

```go
// internal/surveys/api/

type SurveyService interface {
    Create(ctx context.Context, in CreateSurveyInput) (uuid.UUID, error)
    Get(ctx context.Context, id uuid.UUID) (Survey, error)
    List(ctx context.Context, filter ListFilter) ([]Survey, error)
    Update(ctx context.Context, id uuid.UUID, in UpdateSurveyInput) error
    Archive(ctx context.Context, id uuid.UUID) error
    SaveVersion(ctx context.Context, surveyID uuid.UUID, schemaJSON []byte, minor bool) (Version, error)
    Activate(ctx context.Context, surveyID, versionID uuid.UUID) error
    GetActiveVersion(ctx context.Context, surveyID uuid.UUID) (Version, error)
    ListVersions(ctx context.Context, surveyID uuid.UUID) ([]Version, error)
}

type VersionStore interface {
    SaveVersion(ctx context.Context, v Version) error
    GetVersion(ctx context.Context, id uuid.UUID) (Version, error)
    GetActive(ctx context.Context, surveyID uuid.UUID) (Version, error)
    ListVersions(ctx context.Context, surveyID uuid.UUID) ([]Version, error)
    Activate(ctx context.Context, surveyID, versionID uuid.UUID) error
}

type Runtime interface {
    NextNode(schema []byte, currentNodeID string, answers map[string]Answer) (NodeResult, error)
    ValidateAnswer(schema []byte, nodeID string, ans Answer) error
    CalculateProgress(schema []byte, currentNodeID string) (float64, error)
}
```

### DTOs

```go
type Survey struct {
    ID, TenantID  uuid.UUID
    Name, Description string
    PrimaryMode   PrimaryMode
    Status        SurveyStatus
    CreatedAt, UpdatedAt time.Time
    CreatedBy     uuid.UUID
}

type Version struct {
    ID, SurveyID  uuid.UUID
    Major, Minor  int
    Schema        []byte // canonical JSON of the survey graph
    IsActive      bool
    CreatedAt     time.Time
    CreatedBy     uuid.UUID
    ActivatedAt   *time.Time
}

type CreateSurveyInput struct {
    Name, Description string
    PrimaryMode       PrimaryMode
}

type UpdateSurveyInput struct {
    Name, Description *string
    PrimaryMode       *PrimaryMode
}

type ListFilter struct {
    Status SurveyStatus
    Search string
    Limit, Offset int
}

type Answer struct {
    NodeID       string
    SingleChoice string
    MultiChoice  []string
    Number       *float64
    Text         string
    AnsweredAt   int64 // unix millis
}

type NodeResult struct {
    NextNodeID string
    Terminated bool
    EndKind    EndKind  // success | refusal | "" if not terminated
    Progress   float64  // [0,1]
}

type AnswerKey struct {
    CallID uuid.UUID
    NodeID string
}

type PrimaryMode string
const (
    ModeForm PrimaryMode = "form"
    ModeFlow PrimaryMode = "flow"
)

type SurveyStatus string
const (
    StatusActive   SurveyStatus = "active"
    StatusArchived SurveyStatus = "archived"
)

type EndKind string
const (
    EndKindSuccess EndKind = "success"
    EndKindRefusal EndKind = "refusal"
    EndKindNone    EndKind = ""
)

type QuestionType string
const (
    TypeSingle QuestionType = "single"
    TypeMulti  QuestionType = "multi"
    TypeNumber QuestionType = "number"
    TypeText   QuestionType = "text"
    TypeSelect QuestionType = "select"
)

type NodeKind string
const (
    NodeStart      NodeKind = "start"
    NodeIntro      NodeKind = "intro"
    NodeQuestion   NodeKind = "question"
    NodeTextBlock  NodeKind = "text-block"
    NodeSuccessEnd NodeKind = "success-end"
    NodeRefusalEnd NodeKind = "refusal-end"
    NodeCondition  NodeKind = "condition"
    NodeJump       NodeKind = "jump"
)

// Validation report wraps the structured errors returned by SaveVersion.
type ValidationError struct {
    Report Report
}

type Report struct {
    Issues []Issue
}

type Issue struct {
    Code    string  // "cycle", "unreachable", "dangling-edge", ...
    NodeID  string
    Message string
}
```

### Sentinel Errors

```go
var (
    ErrNotFound        = errors.New("surveys: not found")
    ErrValidation      = errors.New("surveys: validation failed") // wrap with *ValidationError
    ErrSchema          = errors.New("surveys: invalid schema")
    ErrCycle           = errors.New("surveys: cycle without exit")
    ErrUnreachable     = errors.New("surveys: unreachable nodes")
    ErrDanglingEdge    = errors.New("surveys: dangling edge")
    ErrForwardRef      = errors.New("surveys: forward reference in DSL")
    ErrBadAnswer       = errors.New("surveys: bad answer for node type")
    ErrAlreadyActive   = errors.New("surveys: version already active")
    ErrNoActiveVersion = errors.New("surveys: no active version")
)
```

### Events

Publishes:

| Subject | Payload |
|---|---|
| `tenant.<t>.surveys.version.saved` | `{survey_id, version_id, major, minor}` |
| `tenant.<t>.surveys.version.activated` | `{survey_id, version_id}` |

Consumes: none.

asynq tasks: none.

---

## 6. `telephony`

**Responsibility.** ESL ↔ NATS bridge. Owns the only ESL connections to
FreeSWITCH nodes, exposes a NATS subject pair (cmd in / event out) that
the dialer talks to. Idempotent command processing via Redis
`SETNX op:idempotency:<command_id>`. Health-checks FS nodes every 5 s,
publishes `bridge.health` heartbeats. Provides FreeSWITCH directory XML
endpoint for SIP-WSS user provisioning. The package surface here is the
contract dialer / realtime use; the wire details (eslgo client,
connection pool) live in `internal/telephony/{esl,pool}` private
packages.

### Interfaces

```go
// internal/telephony/api/

// CommandPublisher is what the dialer uses to ask the bridge to do
// something on FS.
type CommandPublisher interface {
    Originate(ctx context.Context, cmd OriginateCommand) error
    Hangup(ctx context.Context, cmd HangupCommand) error
    Mixmonitor(ctx context.Context, cmd MixmonitorCommand) error
    Play(ctx context.Context, cmd PlayCommand) error
    CreateUser(ctx context.Context, cmd CreateUserCommand) error
    DeleteUser(ctx context.Context, cmd DeleteUserCommand) error
}

// EventConsumer registers a handler for bridge events. Returned
// unsubscribe() must be called at shutdown.
type EventConsumer interface {
    Subscribe(ctx context.Context, tenantID uuid.UUID, h EventHandler) (unsubscribe func(), err error)
}

type EventHandler func(ctx context.Context, evt ChannelEvent) error

// Router selects {fs_node, trunk} for a given operator+phone+strategy.
// Used by dialer just before issuing Originate.
type Router interface {
    Select(ctx context.Context, req SelectRequest) (SelectionResult, error)
}

// LineCapacityTracker enforces `max_concurrent_per_node` (default 60).
// Acquire returns ErrAllNodesFull when every node is at cap; caller backs off.
type LineCapacityTracker interface {
    Acquire(ctx context.Context) (node string, err error)
    Release(ctx context.Context, node string) error
    Stats(ctx context.Context) (map[string]int64, error)
}
```

### DTOs

```go
type OriginateCommand struct {
    CommandID    uuid.UUID  // UUIDv7, idempotency key
    TenantID     uuid.UUID
    CallID       uuid.UUID
    OperatorExt  string     // SIP user
    Number       string     // E.164
    TrunkID      string     // gateway name in mod_sofia
    FSNode       string
    PromptURL    string     // optional consent prompt
    RecordingPath string
    CallerID     string
    DialingTimeout time.Duration
}

type HangupCommand struct {
    CommandID uuid.UUID
    CallID    uuid.UUID
    Cause     string  // "NORMAL_CLEARING", "USER_BUSY", ...
}

type MixmonitorCommand struct {
    CommandID        uuid.UUID
    CallID           uuid.UUID
    Mode             MixmonitorMode  // silent | read | write | both
    ListenerEndpoint string          // e.g. "user/lst_xxx"
}

type MixmonitorMode string
const (
    MMSilent MixmonitorMode = "silent"
    MMRead   MixmonitorMode = "read"
    MMWrite  MixmonitorMode = "write"
    MMBoth   MixmonitorMode = "both"
)

type PlayCommand struct {
    CommandID uuid.UUID
    CallID    uuid.UUID
    URL       string
}

type CreateUserCommand struct {
    CommandID  uuid.UUID
    TenantID   uuid.UUID
    SIPUser    string
    SIPPasswd  string  // pre-hashed at boundary
    ContextHint string
}

type DeleteUserCommand struct {
    CommandID uuid.UUID
    TenantID  uuid.UUID
    SIPUser   string
}

type ChannelEvent struct {
    EventID    uuid.UUID
    TenantID   uuid.UUID
    CallID     uuid.UUID
    FSNode     string
    Type       ChannelEventType  // dialing | answer | bridge | unbridge | hangup | dtmf | record_stop
    HangupCause string           // populated when Type=hangup
    SIPResponse int              // populated when Type=hangup
    DurationMS  int64
    Timestamp   time.Time
    Headers     map[string]string
}

type ChannelEventType string
const (
    EventDialing    ChannelEventType = "dialing"
    EventAnswer     ChannelEventType = "answer"
    EventBridge     ChannelEventType = "bridge"
    EventUnbridge   ChannelEventType = "unbridge"
    EventHangup     ChannelEventType = "hangup"
    EventDTMF       ChannelEventType = "dtmf"
    EventRecordStop ChannelEventType = "record_stop"
)

type SelectRequest struct {
    TenantID    uuid.UUID
    OperatorID  uuid.UUID
    Region      string
    Strategy    RoutingStrategy
}

type RoutingStrategy string
const (
    RouteRoundRobin            RoutingStrategy = "round_robin"
    RouteWeighted              RoutingStrategy = "weighted"
    RouteLeastCost             RoutingStrategy = "least_cost"
    RouteLeastCostWithFallback RoutingStrategy = "least_cost_with_fallback"
)

type SelectionResult struct {
    FSNode  string
    TrunkID string
    Reason  string  // "primary" | "fallback:<trunk>" | "least-cost"
}
```

### Sentinel Errors

```go
var (
    ErrAuthFailed     = errors.New("telephony: esl auth failed")
    ErrNotConnected   = errors.New("telephony: not connected")
    ErrCommandFailed  = errors.New("telephony: command failed")
    ErrTimeout        = errors.New("telephony: timeout")
    ErrAllNodesFull   = errors.New("telephony: no healthy node with capacity")
    ErrNoTrunkAvailable = errors.New("telephony: no available trunk")
    ErrIdempotentReplay = errors.New("telephony: command_id already executed")
)
```

### Events

Publishes (durable JetStream stream `TELEPHONY`, retention 7 days,
explicit ack):

| Subject | Source FS event |
|---|---|
| `tenant.<t>.telephony.event.<call_id>.create` | `CHANNEL_CREATE` |
| `tenant.<t>.telephony.event.<call_id>.answer` | `CHANNEL_ANSWER` |
| `tenant.<t>.telephony.event.<call_id>.hangup_complete` | `CHANNEL_HANGUP_COMPLETE` |
| `tenant.<t>.telephony.event.<call_id>.bridge` | `CHANNEL_BRIDGE` |
| `tenant.<t>.telephony.event.<call_id>.unbridge` | `CHANNEL_UNBRIDGE` |
| `tenant.<t>.telephony.event.<call_id>.dtmf` | `DTMF` |
| `tenant.<t>.telephony.event.<call_id>.record_stop` | `RECORD_STOP` |
| `tenant.<t>.telephony.bridge.health` | internal heartbeat |

Consumes (best-effort, core-NATS):

| Subject | Verb |
|---|---|
| `tenant.<t>.telephony.cmd.<call_id>` | originate / hangup / mixmonitor / play / create_user / delete_user (discriminated by command type field) |

asynq tasks: none.

---

## 7. `dialer`

**Responsibility.** OperatorFSM (`offline → ready → dialing → call →
status → verify → ready`, plus `pause` from any), CallQueue (Redis
ZSET with priority+epoch score), RDDGenerator (Random Digit Dialing for
DEF/АВС-codes against undeposited quotas), Router (NATS abstraction for
telephony commands), LineCapacityTracker (per-FS-node 60-channel cap),
WorkingHoursChecker (per-tenant + per-region timezone), RetryOrchestrator
(scheduled re-enqueue of mature pending retries). Heart of the
auto-dialler.

### Interfaces

```go
// internal/dialer/api/

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
    Force(ctx context.Context, tenantID, operatorID uuid.UUID, target State, reason string) (Snapshot, error)
}

type CallQueue interface {
    EnqueueRespondent(ctx context.Context, req EnqueueRequest) (ok bool, err error)
    PickNext(ctx context.Context, tenantID, projectID uuid.UUID) (QueueItem, error)
    Requeue(ctx context.Context, item QueueItem, delay time.Duration) error
    Size(ctx context.Context, tenantID, projectID uuid.UUID) (int64, error)
    Remove(ctx context.Context, tenantID, projectID, respondentID uuid.UUID) error
}

type RDDGenerator interface {
    Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error)
}

type Router interface {
    Dial(ctx context.Context, req DialRequest) error
    Hangup(ctx context.Context, callID uuid.UUID, reason string) error
    Subscribe(ctx context.Context, tenantID uuid.UUID, h ChannelEventHandler) (unsubscribe func(), err error)
}

type LineCapacityTracker interface {
    Acquire(ctx context.Context) (node string, err error)
    Release(ctx context.Context, node string) error
    Stats(ctx context.Context) (map[string]int64, error)
}

type WorkingHoursChecker interface {
    IsAllowed(ctx context.Context, tenantID uuid.UUID, region string, at time.Time) (bool, error)
    NextAllowed(ctx context.Context, tenantID uuid.UUID, region string, at time.Time) (time.Time, error)
}

type RetryOrchestrator interface {
    Run(ctx context.Context) error  // blocks until ctx cancels
}

type ChannelEventHandler func(ctx context.Context, evt ChannelEvent) error
```

### DTOs

```go
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

type Event string
const (
    EventStartShift      Event = "start_shift"
    EventEndShift        Event = "end_shift"
    EventGoReady         Event = "go_ready"
    EventGoPause         Event = "go_pause"
    EventResume          Event = "resume"
    EventCallStarted     Event = "call_started"
    EventCallEnded       Event = "call_ended"
    EventCallFailed      Event = "call_failed"
    EventStatusSubmitted Event = "status_submitted"
    EventGoVerify        Event = "go_verify"
    EventVerifyDone      Event = "verify_done"
    EventForceOffline    Event = "force_offline"
)

type Snapshot struct {
    TenantID, OperatorID  uuid.UUID
    State                 State
    StateEnteredAt        time.Time
    ProjectID             *uuid.UUID
    CurrentCallID         *uuid.UUID
    RespondentID          *uuid.UUID
    PauseReason           *string
    HeartbeatAt           time.Time
}

type QueueItem struct {
    TenantID, ProjectID, RespondentID uuid.UUID
    Priority    uint8     // 0..9
    EnqueuedAt  time.Time
    AttemptN    uint8
    Phone       string    // E.164
    Region      string    // ISO 3166-2:RU code
}

type EnqueueRequest struct {
    TenantID, ProjectID, RespondentID uuid.UUID
    Phone, Region                     string
    Priority, AttemptN                uint8
}

type StartShiftRequest struct {
    TenantID, OperatorID, ProjectID uuid.UUID
    ClientIP                        string
}

type GoPauseRequest struct {
    TenantID, OperatorID uuid.UUID
    Reason               string  // bio_break, technical, training, ...
}

type CallStartedRequest struct {
    TenantID, OperatorID, CallID, RespondentID uuid.UUID
    StartedAt                                  time.Time
}

type CallEndedRequest struct {
    TenantID, OperatorID, CallID uuid.UUID
    EndedAt                      time.Time
    Cause                        string
    DurationMS                   int
}

type SubmitStatusRequest struct {
    TenantID, OperatorID, CallID, RespondentID uuid.UUID
    Status, Comment                            string
}

type DialRequest struct {
    CallID, TenantID, OperatorID, RespondentID, ProjectID uuid.UUID
    OperatorExt, Phone, FsNode                            string
}

type ChannelEvent struct {
    CallID    uuid.UUID
    Type      string  // dialing | answered | hangup
    Cause     string
    Duration  int     // ms
    FsNode    string
}

type GenerateRequest struct {
    TenantID, ProjectID uuid.UUID
    N                   int
    Quotas              map[string]int  // region code → target count
    ABCRatio            float64         // share of АВС vs DEF in [0,1]
}

type GenerateResult struct {
    Generated     int
    ByRegion      map[string]int
    DuplicatesHit int
    DNCHit        int
    InvalidHit    int
    Throttled     bool
}
```

### Sentinel Errors

```go
var (
    ErrInvalidTransition   = errors.New("dialer: invalid FSM transition")
    ErrUnknownState        = errors.New("dialer: unknown state")
    ErrQueueEmpty          = errors.New("dialer: queue empty")
    ErrDuplicateInQueue    = errors.New("dialer: respondent already in queue")
    ErrAllNodesFull        = errors.New("dialer: all FreeSWITCH nodes at capacity")
    ErrOutsideWorkingHours = errors.New("dialer: outside working hours for region")
    ErrThrottled           = errors.New("dialer: rate-limit throttled")
    ErrTenantMismatch      = errors.New("dialer: tenant mismatch")
)
```

### Events

Publishes (durable JetStream stream `DIALER`, retention 24 h, via outbox):

| Subject | Payload |
|---|---|
| `tenant.<t>.dialer.op.<op_id>.state` | FSM transition (operator state log) |
| `tenant.<t>.dialer.call.<call_id>.lifecycle` | start / answer / hangup at dialer level |
| `tenant.<t>.dialer.call.finalized` | terminal state set, includes cost-bearing fields |
| `analytics.event.calls` | denormalised call row for ClickHouse |
| `analytics.event.operator_state` | denormalised operator-state row |

Consumes (durable, explicit ack):

| Subject | Why |
|---|---|
| `tenant.<t>.telephony.event.<call_id>.*` | drives FSM `dialing→call` and `call→status` transitions |

asynq tasks: scheduled `dialer.retry_due` (every 30 s) — see `cmd/worker`.

---

## 8. `realtime`

**Responsibility.** WebSocket Hub (one Hub per `cmd/api` replica),
fan-out from NATS to local connections, presence tracker (Redis-backed
so cross-replica), subscription RBAC matrix (which roles may subscribe to
which topics), listen-in service (silent for v1; whisper / barge stubbed
for v2), per-connection writer goroutine + backpressure (slow-consumer
drop), force-commands push channel.

### Interfaces

```go
// internal/realtime/api/

type Hub interface {
    Connect(ctx context.Context, conn WSConn, claims Claims) (Connection, error)
    Broadcast(ctx context.Context, topic Topic, payload json.RawMessage, filter BroadcastFilter) int
    DisconnectByUser(ctx context.Context, tenantID, userID string)
    Stats() HubStats
}

type Connection interface {
    ID() string
    Claims() Claims
    Subscribe(topic Topic, filter SubscriptionFilter) (subID string, err error)
    Unsubscribe(subID string)
    Close(reason CloseReason)
}

type WSConn interface {
    ReadFrame(ctx context.Context) (data []byte, err error)
    WriteFrame(ctx context.Context, data []byte) error
    Close(code CloseReason, reason string) error
    RemoteAddr() string
}

type PresenceTracker interface {
    OnConnect(ctx context.Context, tenantID, userID, replicaID string) error
    OnDisconnect(ctx context.Context, tenantID, userID string) error
    Touch(ctx context.Context, tenantID, userID string) error
    IsOnline(ctx context.Context, tenantID, userID string) (bool, error)
    OnlineUsers(ctx context.Context, tenantID string) ([]string, error)
}

type ListenInService interface {
    Start(ctx context.Context, in StartListenRequest) (*ListenSession, error)
    Stop(ctx context.Context, sessionID string) error
    List(ctx context.Context, tenantID string) ([]*ListenSession, error)
}
```

### DTOs

```go
type Claims struct {
    UserID, TenantID string
    Roles            []string
}

type HubStats struct {
    Connections    int
    BySubscription map[Topic]int
}

type BroadcastFilter struct {
    TenantID  string
    UserID    string
    ProjectID string
    CallID    string
}

type Topic string
const (
    TopicOperatorsState Topic = "operators.state"
    TopicDialerQueue    Topic = "dialer.queue"
    TopicTrunksHealth   Topic = "trunks.health"
    TopicCallEvents     Topic = "call.events"        // requires CallID filter
    TopicNotifications  Topic = "notifications.user" // self-only
    TopicForceCommands  Topic = "op.commands"        // self-only, server→client
)

type SubscriptionFilter struct {
    ProjectID, OperatorID, CallID string
}

type Subscription struct {
    ID, ConnID, UserID string
    Topic              Topic
    Filter             SubscriptionFilter
}

type CloseReason int
const (
    CloseNormal       CloseReason = 1000
    CloseGoingAway    CloseReason = 1001
    CloseProtocolErr  CloseReason = 1002
    CloseInvalidData  CloseReason = 1007
    ClosePolicyViol   CloseReason = 1008
    CloseUnauthorized CloseReason = 4401
    CloseRateLimited  CloseReason = 4429
)

type FrameKind string
const (
    FrameAuth         FrameKind = "auth"
    FrameAuthOK       FrameKind = "auth.ok"
    FrameAuthError    FrameKind = "auth.error"
    FrameRefresh      FrameKind = "refresh"
    FrameRefreshOK    FrameKind = "refresh.ok"
    FrameSubscribe    FrameKind = "subscribe"
    FrameSubscribeOK  FrameKind = "subscribe.ok"
    FrameSubscribeErr FrameKind = "subscribe.error"
    FrameUnsubscribe  FrameKind = "unsubscribe"
    FrameEvent        FrameKind = "event"
    FramePing         FrameKind = "ping"
    FramePong         FrameKind = "pong"
    FrameForce        FrameKind = "force.event"
)

type Frame struct {
    Type    FrameKind
    SubID   string
    Topic   Topic
    Filter  *SubscriptionFilter
    Token   string
    Payload json.RawMessage
    Reason  string
}

type ListenMode string
const (
    ListenSilent  ListenMode = "silent"
    ListenWhisper ListenMode = "whisper"  // v2
    ListenBarge   ListenMode = "barge"    // v2
)

type StartListenRequest struct {
    Tenant, ListenerID, CallID string
    Mode                       ListenMode
}

type ListenSession struct {
    ID, TenantID, ListenerID, CallID string
    Mode                             ListenMode
    SIPUser, SIPPassword             string  // password returned ONCE
    VertoWSSURL                      string
    StartedAt                        time.Time
    StoppedAt                        *time.Time
    FreeSwitchNode                   string
}
```

### Sentinel Errors

```go
var (
    ErrAuthFailed       = errors.New("realtime: auth failed")
    ErrAuthRequired     = errors.New("realtime: auth frame required")
    ErrTopicForbidden   = errors.New("realtime: topic not allowed for role")
    ErrUnknownTopic     = errors.New("realtime: unknown topic")
    ErrFilterRequired   = errors.New("realtime: subscription filter is required for this topic")
    ErrCallNotActive    = errors.New("realtime: call not active")
    ErrListenerLimit    = errors.New("realtime: listener limit reached for call")
)
```

### Events

Publishes (best-effort, core-NATS):

| Subject | Payload |
|---|---|
| `tenant.<t>.notify.user.<user_id>` | in-app push (this replica receives, others fan out) |
| `tenant.<t>.audit.event` (action=`realtime.listen_started`) | listener Start |
| `tenant.<t>.audit.event` (action=`realtime.listen_stopped`) | listener Stop |

Consumes (durable, explicit ack):

| Subject | Why |
|---|---|
| `tenant.<t>.dialer.op.<op_id>.state` | re-publish to `operators.state` topic |
| `tenant.<t>.dialer.call.<call_id>.lifecycle` | re-publish to `call.events` |
| `tenant.<t>.telephony.event.<call_id>.*` | re-publish to `call.events` |
| `tenant.<t>.notify.user.<user_id>` | route to a single user's connection |

asynq tasks: none.

---

## 9. `recording`

**Responsibility.** Recording metadata CRUD (`call_recordings` table),
gRPC `RecordingService.Commit` called by `cmd/recording-uploader`,
streamed S3 reads with envelope decryption (DEK from `tenancy`),
integrity verification (sha256 redo), search with cursor pagination,
retention transitions (hot → cold after 365 d, delete after +730 d
total). Also runs the per-tenant retention worker in `cmd/worker`.

### Interfaces

```go
// internal/recording/api/

type RecordingService interface {
    Commit(ctx context.Context, in CommitInput) (CommitOutput, error)
    Get(ctx context.Context, tenantID, callID uuid.UUID) (RecordingMetadata, error)
    Search(ctx context.Context, tenantID uuid.UUID, q SearchQuery) (SearchResult, error)
    OpenAudioStream(ctx context.Context, tenantID, callID uuid.UUID, byteRange *ByteRange) (AudioStream, error)
    VerifyChecksum(ctx context.Context, tenantID, callID uuid.UUID) (VerifyResult, error)
}

type URLSigner interface {
    Sign(ctx context.Context, tenantID, callID uuid.UUID, ttl time.Duration) (signedURL string, err error)
}

type RetentionPlanner interface {
    // RunPass scans recordings whose status transitions are due and
    // applies them. Idempotent — safe to re-run after partial failure.
    RunPass(ctx context.Context, now time.Time) (RetentionStats, error)
}
```

### DTOs

```go
type CommitInput struct {
    TenantID, CallID uuid.UUID
    S3Bucket, AudioObjectKey, DEKObjectKey, KMSKeyID string
    EncryptedDEK     []byte
    BytesSize        int64
    Duration         time.Duration
    SHA256Hex        string  // 64 hex chars
    Codec            string  // "opus"
    SampleRate       int32
    DeleteAt, ColdAt time.Time
    IngestAgentID    string
    RecordedAt       time.Time
}

type CommitOutput struct {
    RecordingID, CallID uuid.UUID
    CommittedAt         time.Time
    IdempotentReplay    bool
}

type RecordingMetadata struct {
    RecordingID, CallID, TenantID uuid.UUID
    S3Bucket, AudioObjectKey      string
    BytesSize                     int64
    Duration                      time.Duration
    SHA256Hex                     string
    Status                        string  // "stored" | "cold" | "deleted"
    CommittedAt, DeleteAt, ColdAt time.Time
    VerifiedAt                    *time.Time
}

type SearchQuery struct {
    ProjectID, OperatorID *uuid.UUID
    Status                []string
    From, To              *time.Time
    Cursor                string  // opaque, encoded committed_at + recording_id
    Limit                 int     // 1..200, default 50
}

type SearchResult struct {
    Items      []RecordingMetadata
    NextCursor string
    HasMore    bool
}

type ByteRange struct {
    Start int64
    End   int64  // inclusive; -1 means open-ended
}

type AudioStream struct {
    Reader        io.ReadCloser
    ContentType   string
    ContentLength int64  // total decrypted length, regardless of Range
    StartOffset   int64
    EndOffset     int64
}

type VerifyResult struct {
    OK            bool
    ExpectedSHA   string
    ActualSHA     string
    BytesScanned  int64
    DurationMS    int64
}

type RetentionStats struct {
    ScannedRows  int64
    MovedToCold  int64
    Deleted      int64
    Errors       int64
}
```

### Sentinel Errors

```go
var (
    ErrNotFound        = errors.New("recording: not found")
    ErrAlreadyDeleted  = errors.New("recording: already deleted")
    ErrTenantMismatch  = errors.New("recording: tenant mismatch")
    ErrCallNotFound    = errors.New("recording: call not found")
    ErrInvalidInput    = errors.New("recording: invalid input")
    ErrIntegrityFailed = errors.New("recording: integrity check failed")
)
```

### Events

Publishes (durable JetStream stream `RECORDING`, retention 30 days, via
outbox):

| Subject | Payload |
|---|---|
| `tenant.<t>.recording.uploaded` | `RecordingMetadata` (without S3 paths) |
| `tenant.<t>.audit.event` (action=`recording.committed`) | sha256, sizes, kms_key_id |
| `tenant.<t>.audit.event` (action=`recording.accessed`) | who, when, byte range |

Consumes: none in the service (the gRPC server is the only entry point).

asynq tasks: `recording:retention.pass` (scheduled daily at 03:00 МСК).

---

## 10. `analytics`

**Responsibility.** NATS → ClickHouse ingest pipeline with explicit ack,
dedup LRU on `event_id`, batched inserts (10 000 rows or 5 s window per
stream), exponential backoff with jitter, dead-letter on poison
payloads. MetricsQuery surface for dashboards: calls, operator state,
region progress, hourly buckets, operator comparisons. Redis-backed
result cache with 30-second TTL.

### Interfaces

```go
// internal/analytics/api/

type IngestPipeline interface {
    // Run blocks until ctx is cancelled. Idempotent on restart.
    Run(ctx context.Context) error
    // Stats returns runtime counters for /metrics.
    Stats() IngestStats
}

type MetricsQuery interface {
    Calls(ctx context.Context, q CallsQuery) (CallsResult, error)
    OperatorState(ctx context.Context, q OperatorStateQuery) (OperatorStateBreakdown, error)
    RegionProgress(ctx context.Context, q RegionProgressQuery) ([]RegionProgressRow, error)
    Hourly(ctx context.Context, q HourlyQuery) ([]HourlyBucket, error)
    OperatorComparisons(ctx context.Context, q OperatorComparisonsQuery) ([]OperatorComparisonRow, error)
}

// ServiceRO is the read-only aggregate used by the HTTP layer.
type ServiceRO interface {
    MetricsQuery
    Overview(ctx context.Context, q OverviewQuery) (OverviewResult, error)
}
```

### DTOs

```go
type EventKind string
const (
    EventKindCallFinalized     EventKind = "dialer.call.finalized"
    EventKindOperatorState     EventKind = "operator.state.changed"
    EventKindRecordingUploaded EventKind = "recording.uploaded"
)

type EventEnvelope struct {
    EventID   uuid.UUID
    Kind      EventKind
    TenantID  uuid.UUID
    Timestamp time.Time
    Payload   json.RawMessage
}

type IngestStats struct {
    PerSubject map[string]SubjectStats
}

type SubjectStats struct {
    Received, Inserted, Failed, DeadLetter uint64
    LagSeconds                              float64
    LastError                               string
}

type Window struct{ From, To time.Time }

func (w Window) Validate() error // ErrInvalidWindow if From≥To or span > 1y

type CallsQuery struct {
    TenantID  uuid.UUID
    ProjectID *uuid.UUID
    Window    Window
}

type CallsResult struct {
    Total, Successful, Failed, Refusals uint64
    AvgDurSec                            float64
    TotalDurSec                          uint64
    ByStatus                             []StatusBucket
}

type StatusBucket struct {
    Status string
    Count  uint64
}

type OperatorStateQuery struct {
    TenantID   uuid.UUID
    OperatorID *uuid.UUID
    ProjectID  *uuid.UUID
    Window     Window
}

type OperatorStateBreakdown struct {
    TalkSec, PauseSec, ReadySec, WrapSec uint64
}

type RegionProgressQuery struct {
    TenantID, ProjectID uuid.UUID
    Window              Window
}

type RegionProgressRow struct {
    RegionCode string
    Done, Plan uint64
    Progress   float64
}

type HourlyQuery struct {
    TenantID  uuid.UUID
    ProjectID *uuid.UUID
    Window    Window
}

type HourlyBucket struct {
    Hour      time.Time
    Count     uint64
    AvgDurSec float64
}

type OperatorComparisonsQuery struct {
    TenantID, ProjectID uuid.UUID
    Window              Window
}

type OperatorComparisonRow struct {
    OperatorID                                uuid.UUID
    DisplayName                               string
    CallsTotal                                uint64
    SuccessRate, AvgTalkSec, PauseShare       float64
    AboveTeamAvg                              bool
}

type OverviewQuery struct {
    TenantID  uuid.UUID
    ProjectID *uuid.UUID
    Window    Window
}

type OverviewResult struct {
    Calls          CallsResult
    OperatorState  OperatorStateBreakdown
    RegionProgress []RegionProgressRow
    Hourly         []HourlyBucket
}
```

### Sentinel Errors

```go
var (
    ErrTenantRequired = errors.New("analytics: tenant required")
    ErrInvalidWindow  = errors.New("analytics: invalid window")
    ErrTransient      = errors.New("analytics: transient ingest error")
    ErrInvalidPayload = errors.New("analytics: invalid payload")
)
```

### Events

Publishes: none (analytics is read-side / sink).

Consumes (durable JetStream stream `ANALYTICS`, retention 24 h, explicit
ack, max-ack-pending 20 000):

| Subject | Stream | Sink table |
|---|---|---|
| `dialer.call.finalized` | ANALYTICS | `events_calls` |
| `operator.state.changed` | ANALYTICS | `events_operator_state` |
| `recording.uploaded` | ANALYTICS | `events_recording_uploaded` |

asynq tasks: none.

---

## 11. `reports`

**Responsibility.** Six preset report templates (operator efficiency,
project summary, calls by status, finance, quality control, hourly
activity), custom reports via period+project+format, async generation
via asynq for large windows (> 30 d or > 100 k rows), XLSX/CSV/PDF
renderers, presigned download URLs (24 h TTL), audit on export. Reports
that the report uses analytics MetricsQuery + recording metadata.

### Interfaces

```go
// internal/reports/api/

type ReportRenderer interface {
    Render(ctx context.Context, in RenderInput) (RenderResult, error)
}

type ReportRunner interface {
    // Run a synchronous report (small window). For larger windows
    // the HTTP handler enqueues a Job instead.
    Run(ctx context.Context, in RunInput) (RunResult, error)
}

type JobQueue interface {
    Enqueue(ctx context.Context, in JobInput) (JobTicket, error)
    Get(ctx context.Context, jobID string) (Job, error)
    List(ctx context.Context, f ListJobsFilter) ([]Job, string /* nextCursor */, error)
    Cancel(ctx context.Context, jobID string) error
}

type JobConsumer interface {
    Run(ctx context.Context) error  // blocks until ctx cancels
}
```

### DTOs

```go
type ReportKind string
const (
    KindOperatorEfficiency ReportKind = "operator_efficiency"
    KindProjectSummary     ReportKind = "project_summary"
    KindCallsByStatus      ReportKind = "calls_by_status"
    KindFinance            ReportKind = "finance"
    KindQualityControl     ReportKind = "quality_control"
    KindHourlyActivity     ReportKind = "hourly_activity"
    KindCustom             ReportKind = "custom"
)

type ExportFormat string
const (
    FormatXLSX ExportFormat = "xlsx"
    FormatCSV  ExportFormat = "csv"
    FormatPDF  ExportFormat = "pdf"
)

type RenderInput struct {
    Kind     ReportKind
    Format   ExportFormat
    Params   map[string]any  // kind-specific (project_id, operator_id, ...)
    Window   Window
    TenantID uuid.UUID
    ActorID  uuid.UUID
}

type RenderResult struct {
    Bytes    []byte
    Filename string
    MIME     string
    SHA256   string
}

type RunInput  = RenderInput
type RunResult = RenderResult

type JobInput struct {
    RenderInput
    NotifyUserID uuid.UUID
}

type JobTicket struct {
    JobID    string
    QueuedAt time.Time
}

type JobState string
const (
    JobQueued    JobState = "queued"
    JobRunning   JobState = "running"
    JobSucceeded JobState = "succeeded"
    JobFailed    JobState = "failed"
    JobCanceled  JobState = "canceled"
)

type Job struct {
    ID         string
    TenantID   uuid.UUID
    Kind       ReportKind
    Format     ExportFormat
    Params     map[string]any
    Window     Window
    State      JobState
    StartedAt, FinishedAt *time.Time
    BytesSize  int64
    Filename   string
    DownloadURL string  // populated when State=succeeded; presigned, 24h TTL
    Error      string
    CreatedBy  uuid.UUID
    CreatedAt  time.Time
}

type ListJobsFilter struct {
    State     *JobState
    Kind      *ReportKind
    From, To  *time.Time
    Cursor    string
    Limit     int
}

type Window = analytics.Window  // alias for clarity in this package
```

### Sentinel Errors

```go
var (
    ErrUnknownKind     = errors.New("reports: unknown kind")
    ErrUnsupportedFmt  = errors.New("reports: unsupported format for kind")
    ErrJobNotFound     = errors.New("reports: job not found")
    ErrInvalidParams   = errors.New("reports: invalid params")
    ErrTooLarge        = errors.New("reports: result exceeds size cap")
    ErrCanceled        = errors.New("reports: job canceled")
)
```

### Events

Publishes:

| Subject | When |
|---|---|
| `tenant.<t>.reports.report.ready` | async job finished, includes `download_url` |
| `tenant.<t>.notify.user.<user_id>` | in-app notification on completion |
| `tenant.<t>.audit.event` (action=`reports.export`) | every render or download |

Consumes: none directly (analytics queries read-only).

asynq tasks: `reports:job.run` — single task type; payload carries
`JobInput`. Worker is `internal/reports/service.JobConsumer`.

---

## 12. `billing`

**Responsibility.** Per-tenant tariff store (telecom rates per trunk,
operator wages per completed survey, storage rate, respondent-base
purchase rate, fixed monthly fees), CostCalculator (pure function:
call → cost in int64 minor units), per-month spend breakdowns, per-
project margin reports, finance dashboard. Subscribes to
`dialer.call.finalized` and writes a `call_costs` row exactly-once
(ON CONFLICT DO NOTHING on `call_id`). Respondent bases are purchased
datasets — the platform charges tenants per-record pulled from a
base, alongside telecom and wages.

### Interfaces

```go
// internal/billing/api/

type CostCalculator interface {
    CallCost(ctx context.Context, in CallCostInput, t Tariffs) (CallCostOutput, error)
}

type TariffStore interface {
    Get(ctx context.Context, tenantID uuid.UUID) (Tariffs, error)
    Update(ctx context.Context, tenantID uuid.UUID, t Tariffs) (Tariffs, error)
}

type RevenueCalculator interface {
    MonthRevenue(ctx context.Context, tenantID, projectID uuid.UUID, p Period) (int64, error)
}

type MarginReport interface {
    Margin(ctx context.Context, tenantID uuid.UUID, p Period) ([]ProjectMargin, error)
}

type SpendReport interface {
    MonthSpend(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, p Period) (MonthBreakdown, error)
    SpendByMonth(ctx context.Context, tenantID uuid.UUID, count int) ([]MonthBreakdown, error)
}

type CallFinalizedHook interface {
    OnCallFinalized(ctx context.Context, in CallCostInput) error
}
```

### DTOs

```go
type Tariffs struct {
    TenantID             uuid.UUID
    Version              int
    UpdatedAt            time.Time
    TrunkCostsMinor      map[string]int64  // trunk_id → cost per minute (RUB minor)
    WagePerSurveyMinor   int64             // operator pay per completed survey
    StorageMinorPerGBMo  int64             // S3 storage rate
    RespondentBasesMinor int64             // purchased respondent-base records, per-record
    FixedFeesMinor       int64             // monthly fixed fees
}

type Period struct {
    From, To time.Time   // half-open [From, To)
}

func Month(year int, month time.Month) Period

type CallCostInput struct {
    CallID, TenantID, ProjectID uuid.UUID
    TrunkUsed                   string
    DurationSec                 int32
    Status                      string
    StorageBytes                int64
    FinalizedAt                 time.Time
}

type CallCostOutput struct {
    TelecomMinor, WagesMinor, StorageMinor, TotalMinor int64
}

type MonthBreakdown struct {
    TenantID                                       uuid.UUID
    Period                                         Period
    TelecomMin, WagesMin, RespondentBasesMin       int64
    StorageMin, FixedFeeMin, TotalMin              int64
    CompletedSurveys                               int64
    TotalCallSeconds                               int64
}

func (b MonthBreakdown) CostPerSurveyMinor() int64
func (b MonthBreakdown) AvgCostPerMinuteMinor() int64

type ProjectMargin struct {
    ProjectID                  uuid.UUID
    ProjectCode, ProjectName   string
    Surveys                    int64
    TelecomMin, WagesMin, RespondentBasesMin, StorageMin, TotalMin int64
    RevenueMin, MarginMin      int64
    CostPerSrvMn               int64
}

type DashboardResponse struct {
    TenantID    uuid.UUID
    Period      Period
    Breakdown   MonthBreakdown
    Projects    []ProjectMargin
    History     []MonthBreakdown  // last 12 months
}

type TariffsResponse struct {
    Tariffs Tariffs
}

type TariffsPatchRequest struct {
    TrunkCostsMinor      map[string]int64  // null entries delete the key
    WagePerSurveyMinor   *int64
    StorageMinorPerGBMo  *int64
    RespondentBasesMinor *int64
    FixedFeesMinor       *int64
}
```

### Sentinel Errors

```go
var (
    ErrNoTariffs     = errors.New("billing: no tariffs configured for tenant")
    ErrInvalidTariff = errors.New("billing: invalid tariff")
    ErrInvalidPeriod = errors.New("billing: invalid period")
)
```

### Events

Publishes:

| Subject | When |
|---|---|
| `tenant.<t>.audit.event` (action=`billing.tariff_updated`) | TariffStore.Update |

Consumes (durable, explicit ack):

| Subject | Why |
|---|---|
| `tenant.<t>.dialer.call.finalized` | drives `CallFinalizedHook.OnCallFinalized` (idempotent INSERT into `call_costs`) |

asynq tasks: none. Spend / margin queries hit Postgres + ClickHouse
directly.

---

## Cross-references

- Spec §5.2 — module roster; this document expands every cell.
- Spec §10.2 — canonical NATS subjects; this document is the
  authoritative module-by-module attribution.
- Spec §17 — coverage targets per layer.
- `00-overview.md` — dependency graph; ensures the import edges in this
  document are acyclic.
- `01-package-layout.md` — file layout for these contracts.
- `03-error-handling.md` — wrapping, mapping to gRPC / HTTP codes.
- ADRs §17 — the §22 "candidate" ADRs (0014, 0015) are out of scope for
  this document; they govern HTTP routing and TDD discipline, not module
  contracts.
