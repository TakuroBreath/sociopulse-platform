# 03. Error Handling

This document is the project's error policy. It folds the
`samber/cc-skills-golang@golang-error-handling` skill into rules specific
to our gin / gRPC / NATS surface, and it is the document `errorlint`,
`errname`, and `nilerr` linters back. When they disagree, the linter
wins — but the linter is configured to enforce these rules, so
disagreements should be rare.

## Sentinel Errors per Module

Every module exposes its sentinel errors from a single file:

```
internal/<module>/api/errors.go
```

Each entry follows the pattern:

```go
var ErrXxx = errors.New("module: short description")
```

Three rules:

1. **Module-prefixed message.** `"auth: invalid credentials"`,
   `"crm: project not found"`, `"recording: integrity check failed"`.
   The prefix lets greps over Loki turn a log line back into a module
   instantly.
2. **Lowercase, no trailing punctuation.** `errors.New` strings should
   read like `fmt.Errorf` arguments, because they are concatenated with
   `%w` upstream.
3. **One sentinel per *cause class*, not per call site.** Twenty
   `ErrInvalidCredentialsFooBarBaz` clutter the surface; one
   `ErrInvalidCredentials` plus a wrapped reason serves the same
   purpose with less noise.

The full per-module list of sentinels lives in
`02-module-contracts.md`. Plans 04-14 implement them verbatim. New
sentinels added in later work must update both files.

A sentinel may be paired with a typed error when the caller needs
structured data:

```go
var ErrValidation = errors.New("surveys: validation failed")

type ValidationError struct {
    Report Report
}

func (v *ValidationError) Error() string  { return ErrValidation.Error() }
func (v *ValidationError) Unwrap() error  { return ErrValidation }
```

`errors.Is(err, ErrValidation)` still matches; `var ve *ValidationError;
errors.As(err, &ve)` lets the HTTP layer surface the structured report.

## Wrapping

Every cross-layer error return wraps with context:

```go
if err := s.store.GetByCode(ctx, tenantID, code); err != nil {
    if errors.Is(err, store.ErrNotFound) {
        return nil, fmt.Errorf("get project by code %q: %w", code, api.ErrProjectNotFound)
    }
    return nil, fmt.Errorf("get project by code %q: %w", code, err)
}
```

Three rules:

1. **Always `%w`** for the wrapped error, **never `%v`**. The
   `errorlint:errorf` rule fails the build on `%v` for an `error` arg.
2. **Add caller-side context, not callee-side.** `"get project by code"`
   is the caller's intent; the callee's `"sql: no rows"` chain is
   already inside `err`. Avoid duplicating the callee.
3. **Low-cardinality strings only.** Variable data (`tenantID`,
   `respondentID`, `phone`) goes into structured logger fields or
   `oops.With(...)` attached at the boundary. Interpolating
   high-cardinality data into the message string blows up Loki indexes
   and makes log queries useless. The `golang-error-handling` skill is
   explicit about this.

For aggregating multiple errors (parallel fan-out, multi-phase save):

```go
errs := errors.Join(err1, err2, err3)
return fmt.Errorf("save project: %w", errs)
```

## Single Handling Rule

An error is **either logged or returned, never both**. The convention:

- The function that **first creates or returns** an error never logs
  it. It returns wrapped.
- The **outermost handler** (HTTP middleware, gRPC interceptor, asynq
  task wrapper, NATS subscriber's flush callback) logs once with the
  full chain.

This matters because Loki dedupes within a window but cannot dedupe
across; double-logging makes a single failure look like five different
failures and hides the real volume.

The skill also frames this as "log OR return"; we say it the same way
in code review.

```go
// WRONG — double-logging.
func (s *Service) Foo(ctx context.Context) error {
    if err := s.store.Bar(ctx); err != nil {
        s.log.Error("store.Bar failed", zap.Error(err))   // ← log here…
        return fmt.Errorf("foo: %w", err)                  // …and return.
    }
    return nil
}

// RIGHT — return wrapped, let the outermost handler log.
func (s *Service) Foo(ctx context.Context) error {
    if err := s.store.Bar(ctx); err != nil {
        return fmt.Errorf("foo: %w", err)
    }
    return nil
}
```

Caveat: it is fine — even encouraged — to log at `debug` level on the
way up, especially for long-distance request flows (telephony bridge,
recording-uploader, dialer FSM transitions). What "single handling"
forbids is **a second `error`-level log** for the same incident.

## Errors-as-Values, Not Panics

A panic is for **unrecoverable programmer errors** (nil dereferences,
out-of-bounds index, broken invariant detected at runtime). Never
panic on:

- A failed network call.
- A missing record.
- An invalid input.
- A timeout.

These are expected error conditions and must be returned. The
`gateway` middleware installs a `gin.Recovery()` that turns any escaped
panic into HTTP 500 + structured log + metric increment, but that is a
last-resort safety net, not a control-flow tool.

When you genuinely need to assert a programmer-level invariant:

```go
if currentNode == nil {
    panic("surveys: NextNode invoked with nil currentNode — caller bug")
}
```

These should be exceedingly rare in production code and always have a
`// caller bug` rationale.

## gRPC Errors

gRPC servers translate sentinel errors at the boundary using
`status.Errorf(codes.X, ...)` and pass structured details when useful
(`errdetails.PreconditionFailure`, `errdetails.QuotaFailure`).

The mapping for our two gRPC services:

| Sentinel error | gRPC code | When |
|---|---|---|
| `recording.ErrNotFound` | `codes.NotFound` | call_id absent or RLS hides it |
| `recording.ErrAlreadyDeleted` | `codes.FailedPrecondition` | retention deleted the row |
| `recording.ErrTenantMismatch` | `codes.PermissionDenied` | mTLS SAN does not match payload tenant_id |
| `recording.ErrInvalidInput` | `codes.InvalidArgument` | bad sha256 / negative bytes / etc. |
| `recording.ErrIntegrityFailed` | `codes.DataLoss` | sha256 redo did not match |
| `tenancy.ErrPermissionDenied` | `codes.PermissionDenied` | service-owner mTLS check failed |
| `telephony.ErrCommandFailed` | `codes.Internal` | ESL refused the action |
| `telephony.ErrIdempotentReplay` | `codes.AlreadyExists` | duplicate command_id |
| `context.Canceled` / `context.DeadlineExceeded` | `codes.Canceled` / `codes.DeadlineExceeded` | propagated as-is |

The mapping lives in `pkg/grpc/errors.go` and is shared across all
gRPC servers. New mappings live there, not in handlers.

A gRPC interceptor (`pkg/grpc/middleware/error.go`) calls the mapper
on every method return:

```go
func ErrorMapper() grpc.UnaryServerInterceptor {
    return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
        resp, err := h(ctx, req)
        if err == nil {
            return resp, nil
        }
        return nil, errorsToStatus(err)
    }
}
```

Server methods therefore return the **domain** sentinel; the
interceptor turns it into a `*status.Status`. Tests assert on the
sentinel, not on the status code, except for the interceptor's own
test which exists precisely to validate the mapping.

## HTTP Errors (gin)

The HTTP API surface (gin handlers per ADR-0014) renders a single
error envelope across every endpoint:

```jsonc
{
  "error": {
    "code": "auth.invalid_credentials",
    "message": "Invalid email or password.",
    "details": {
      // optional structured payload, e.g. validation issues for surveys
    }
  }
}
```

A handler never builds this envelope inline. It returns from a
sentinel (or wraps), and the gin middleware in `pkg/httputil` does the
mapping:

```go
// pkg/httputil/error_handler.go
func ErrorHandler() gin.HandlerFunc {
    return func(c *gin.Context) {
        c.Next()
        if len(c.Errors) == 0 {
            return
        }
        err := c.Errors.Last().Err
        status, payload := mapError(err)
        c.AbortWithStatusJSON(status, gin.H{"error": payload})
    }
}
```

Handlers register an error and return:

```go
func (h *Handler) Login(c *gin.Context) {
    var req LoginRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        _ = c.Error(fmt.Errorf("decode: %w", api.ErrInvalidArgument))
        return
    }
    res, err := h.auth.Login(c.Request.Context(), api.LoginInput{
        OrgID: req.OrgID, Login: req.Login, Password: req.Password,
        IP: c.ClientIP(), UserAgent: c.Request.UserAgent(),
    })
    if err != nil {
        _ = c.Error(err)
        return
    }
    c.JSON(http.StatusOK, toLoginResponse(res))
}
```

The mapping lives in `pkg/httputil/error_map.go`. Today's table (each
new sentinel must be added):

| Sentinel | HTTP status | Code in envelope |
|---|---|---|
| `ErrInvalidCredentials` | 401 | `auth.invalid_credentials` |
| `ErrAccountLocked` | 423 | `auth.account_locked` |
| `ErrAccountArchived` | 403 | `auth.account_archived` |
| `ErrTOTPRequired` | 401 | `auth.totp_required` |
| `ErrTOTPInvalid` | 401 | `auth.totp_invalid` |
| `ErrPasswordExpired` | 403 | `auth.password_expired` |
| `ErrTokenInvalid` | 401 | `auth.token_invalid` |
| `ErrTokenRevoked` | 401 | `auth.token_revoked` |
| `ErrRateLimitExceeded` | 429 | `auth.rate_limited` |
| `ErrInsufficientRole` | 403 | `auth.forbidden` |
| `ErrRefreshReplay` | 401 | `auth.refresh_replay` |
| `tenancy.ErrNotFound` | 404 | `tenancy.not_found` |
| `tenancy.ErrAlreadyExists` | 409 | `tenancy.conflict` |
| `tenancy.ErrInvalidArgument` | 400 | `tenancy.invalid_argument` |
| `crm.ErrProjectNotFound` | 404 | `crm.project_not_found` |
| `crm.ErrProjectCodeTaken` | 409 | `crm.project_code_taken` |
| `crm.ErrInvalidStatus` | 409 | `crm.invalid_status` |
| `crm.ErrInvalidPhone` | 400 | `crm.invalid_phone` |
| `crm.ErrPhoneInDNC` | 422 | `crm.phone_in_dnc` |
| `crm.ErrAdvertisingRejected` | 422 | `crm.advertising_rejected` |
| `surveys.ErrNotFound` | 404 | `surveys.not_found` |
| `surveys.ErrValidation` | 400 | `surveys.validation_failed` (with `details.issues`) |
| `surveys.ErrSchema` | 400 | `surveys.invalid_schema` |
| `dialer.ErrInvalidTransition` | 409 | `dialer.invalid_transition` |
| `dialer.ErrAllNodesFull` | 503 | `dialer.capacity_exhausted` |
| `dialer.ErrOutsideWorkingHours` | 422 | `dialer.outside_hours` |
| `recording.ErrNotFound` | 404 | `recording.not_found` |
| `recording.ErrIntegrityFailed` | 502 | `recording.integrity_failed` |
| `reports.ErrUnknownKind` | 400 | `reports.unknown_kind` |
| `reports.ErrTooLarge` | 422 | `reports.too_large` |
| `billing.ErrNoTariffs` | 412 | `billing.no_tariffs` |
| `realtime.ErrTopicForbidden` | 403 (HTTP listen-in start) | `realtime.topic_forbidden` |
| (default fallback) | 500 | `internal_error` |

The fallback path also emits a metric `sociopulse_http_unmapped_error_total{module}` — a non-zero
value here is a code-review backlog item, not a runtime alarm.

`details` is a free-form `map[string]any` populated only by the small
set of sentinels that benefit from it (`surveys.ErrValidation` carries
`issues: [{code, node_id, message}, ...]`; `crm.ErrInvalidPhone` carries
`row_number` for batch imports). Most sentinels render with an empty
`details`.

## `samber/oops` at the Boundary

`samber/oops` (used by the cc-skills `golang-error-handling` skill as
the production-grade wrapper) is enabled at the **outermost handler
layer only**:

- `pkg/httputil/error_handler.go` — the gin error middleware.
- `pkg/grpc/middleware/error.go` — the gRPC unary/stream interceptor.
- `internal/<module>/events/subscriber.go` — NATS message handler
  outer wrapper.
- `cmd/worker/...` — asynq task outer wrapper.

At those points we wrap with `oops.With(...)` to attach structured
context that flows directly into the zap logger:

```go
import "github.com/samber/oops"

func (s *Subscriber) handleCallFinalized(ctx context.Context, msg *nats.Msg) error {
    var env api.EventEnvelope
    if err := json.Unmarshal(msg.Data, &env); err != nil {
        return oops.
            In("analytics.ingest").
            With("subject", msg.Subject).
            With("size_bytes", len(msg.Data)).
            Wrapf(err, "unmarshal envelope")
    }
    // ... business logic
}
```

The middleware layer reads back the `oops` context and emits structured
zap fields automatically. We do NOT use `oops` deep in services or
stores — `fmt.Errorf("ctx: %w", err)` is enough for in-process flow,
and adding `oops.With(...)` at every layer creates noise without value.

The split is: **fmt.Errorf throughout the call chain; oops.With at the
boundary**. Linter does not enforce this; reviewers do.

## Per-Surface Cheatsheet

```
caller (HTTP / gRPC / NATS / asynq)
  │
  ▼
outermost handler
  │   logs once with full chain (zap.Error(err))
  │   wraps with oops.With() if attaching structured context
  │
  ▼
service method
  │   returns fmt.Errorf("verb noun: %w", api.ErrXxx)
  │   never logs
  │
  ▼
store / external client
  │   returns the underlying driver error wrapped with verb
  │   never logs
  │
  ▼
external system (Postgres, Redis, NATS, S3, KMS, ESL)
```

## Linter Mapping

| Rule | Linter | What it catches |
|---|---|---|
| `%w` in `fmt.Errorf` | `errorlint:errorf` | `%v` for an `error` arg |
| `errors.Is` over `==` | `errorlint:comparison` | `if err == api.ErrFoo` |
| `errors.As` over type assertion | `errorlint:asserts` | `if e, ok := err.(*api.ValidationError); ok` |
| `ErrXxx` naming | `errname` | `var Foo = errors.New(...)` |
| `nil != nil` interface confusion | `nilerr` | `if err != nil { return nil }` patterns |
| Single-handling rule | (review) | not mechanically detectable |
| Low-cardinality strings | (review) | `errors.New(fmt.Sprintf(...))` is a hint |
| `*http.Response.Body.Close()` | `bodyclose` | leaked HTTP bodies |
| `rows.Close()` + `rows.Err()` | `sqlclosecheck`, `rowserrcheck` | leaked SQL rows / unread errors |
| Context propagation through chain | `contextcheck` | `s.store.X(context.Background(), ...)` inside a handler |
| HTTP request without context | `noctx` | `http.NewRequest(...)` over `http.NewRequestWithContext` |

## Cross-references

- `02-module-contracts.md` — full sentinel list per module.
- `06-observability.md` — zap field set, OTel error span attributes.
- `07-go-coding-standards.md` § Errors — the cc-skills heritage.
- `samber/cc-skills-golang@golang-error-handling` —
  `~/.agents/skills/golang-error-handling/SKILL.md`.
- ADR-0012 — zap as logger.
- ADR-0014 — gin as router.
