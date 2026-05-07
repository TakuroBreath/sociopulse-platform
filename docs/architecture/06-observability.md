# 06. Observability

This document is the project's observability contract. It tells you the
exact field set every log line must carry, how Prometheus metric names
are formed, what OpenTelemetry span names look like, and how PII is
redacted at the encoder. Spec §15 is the rationale; this document is
the rule.

The three-pillar baseline: **logs (zap)** + **metrics (Prometheus)** +
**traces (OpenTelemetry)**. The shared correlation key across all three
is `trace_id`, which propagates via W3C TraceContext on every cross-
service hop. Every log line that has a `trace_id` field is greppable
back to the trace that produced it; every metric series can be sliced
by trace exemplars (Mimir + Tempo integration).

## Logging — zap

The logger is `go.uber.org/zap` (ADR-0012). zap's zero-allocation
encoder is non-negotiable on hot paths (thousands of WS frames/sec on a
busy `realtime` replica).

### Field set

Every log line carries a fixed prefix of fields. Add them to the child
logger as soon as you have the value, and let zap thread them through:

| Field | Source | When required |
|---|---|---|
| `service` | `service.name` from config | always |
| `service.environment` | `service.env` from config | always |
| `module` | hard-coded per package (`auth`, `dialer`, ...) | always |
| `request_id` | gin middleware (`X-Request-Id` header or new UUIDv7) | every HTTP / gRPC request |
| `trace_id` | OTel context | every span-bearing operation |
| `span_id` | OTel context | every span-bearing operation |
| `tenant_id` | resolved by auth middleware (`Claims.TenantID`) | every tenant-scoped operation |
| `user_id` | `Claims.UserID` | when an authenticated user is the actor |
| `op_id` | OperatorFSM operator id | dialer / realtime per-operator events |
| `call_id` | dialer / telephony `call_id` | per-call events |
| `recording_id` | recording metadata id | recording-only events |
| `command_id` | telephony bridge UUIDv7 | telephony cmd subscriber events |

Constructor pattern, used at every layer:

```go
log := s.log.With(
    zap.String("module", "auth"),
    zap.String("request_id", reqID),
    zap.String("tenant_id", tenantID.String()),
    zap.String("user_id", userID.String()),
)

log.Info("login.success",
    zap.String("login", login),
    zap.Duration("argon_ms", elapsed),
)
```

Two strict rules:

1. **Use `zap.String`, `zap.Int64`, `zap.Duration`, ... — typed
   constructors.** Never `zap.Any` for request-scoped fields, never a
   raw `interface{}` slice. The `loggercheck` linter (with
   `require-string-key: true`) blocks the slice form; we go further
   in review and reject `zap.Any` for anything that has a typed
   constructor.
2. **Fields are stable across modules.** `tenant_id` is always
   `zap.String("tenant_id", tenantID.String())` — never `tenantID`,
   never `tenant`. The grep-ability of Loki depends on this.

### Levels and sampling

- `debug` — only in development / staging; 5% sampling in production
  (so a misconfigured `debug` log does not flood storage).
- `info` — production default; 100% retention.
- `warn` — recoverable issues, retryable failures, slow-consumer drops.
- `error` — every unhandled error from the outermost handler (single
  handling rule, see `03-error-handling.md`).
- `fatal` / `panic` — process-level invariant violations only.

The sampler is configured at logger construction
(`zap.SamplerConfig{Initial: 100, Thereafter: 100}` for `info`+; a
separate `WithDebugSamplingRatio(0.05)` wrap for `debug`).

### Redaction

The logger's encoder includes a redaction filter that masks values
matching configurable regexes (`config.observability.logging.redact_patterns`):

```yaml
observability:
  logging:
    redact_patterns:
      - "phone:\\+?7\\d{10}"          # E.164 Russian phone
      - "token:[A-Za-z0-9_\\-\\.]+"   # JWT
      - "password:\\S+"
      - "totp_secret:[A-Z2-7]+"       # base32 TOTP secret
```

On match, the value is replaced with `phone:+7***1234` (last 4 digits
preserved for triage), `token:eyJh***`, `password:<redacted>`. The
filter operates on the JSON-encoded line **after** zap renders it, so
even a careless `zap.Any` of a struct containing a phone field is
masked.

A unit test in `pkg/obs/encoder_test.go` runs the redactor over a fixed
fixture set and asserts each pattern matches; CI enforces this. New
patterns added in PRs must extend the fixture.

PII handling for *intentionally logged* phone numbers (e.g. a single
admin-side debug trace): use the `phone_masked` formatter
`pkg/obs.MaskPhone(phone) -> "+7-9** ***-**-12"` and pass the masked
string. The redactor is a safety net, not an alternative to writing
masked values explicitly.

### Log destinations

- Production: zap writes JSON to stdout; **promtail** sidecar in k8s
  pushes to **Grafana Loki**. Retention: 30 d hot, 1 y cold (S3).
- Local dev: zap writes a console encoder to stdout (
  `service.env=development` in config selects this).
- Tests: `zaptest.NewLogger(t)` — captures into the test log; output
  is silent on success.

### Loggercheck

The `loggercheck` linter (in `.golangci.yml`) catches three classes of
bug:

```go
// caught — zap requires string keys.
log.Info("msg", 42, "value")

// caught — odd number of fields.
log.Info("msg", zap.String("a", "1"), zap.String("b"))

// caught — duplicate field name.
log.Info("msg", zap.String("a", "1"), zap.String("a", "2"))
```

Configuration enables both `slog` and `zap` modes; once we (eventually)
migrate to slog (ADR-0016 candidate) we will flip
`loggercheck.zap = false` and remove the zap entry.

## Metrics — Prometheus

Each binary exposes `/metrics` on port 9090. The namespace is fixed:
`sociopulse`. All custom metrics begin with that prefix.

### Naming

Format: **`sociopulse_<module>_<entity>_<unit>`**. Snake_case
throughout. Examples:

| Metric | Type | Labels |
|---|---|---|
| `sociopulse_calls_total` | counter | `tenant_id, project_id, status, region` |
| `sociopulse_call_duration_seconds` | histogram | `tenant_id, status` |
| `sociopulse_dialer_queue_depth` | gauge | `tenant_id, project_id` |
| `sociopulse_dialer_active_channels` | gauge | `fs_node, trunk_id` |
| `sociopulse_operator_state_seconds` | counter | `tenant_id, operator_id, state` |
| `sociopulse_recording_upload_lag_seconds` | gauge | `fs_node` |
| `sociopulse_recording_upload_total` | counter | `tenant_id, status` |
| `sociopulse_quota_progress_ratio` | gauge | `tenant_id, project_id, dimension, value` |
| `sociopulse_rdd_generated_total` | counter | `tenant_id, project_id, region` |
| `sociopulse_http_request_duration_seconds` | histogram | `method, path, status` |
| `sociopulse_http_inflight_requests` | gauge | (none) |
| `sociopulse_ws_connections_active` | gauge | `tenant_id` |
| `sociopulse_nats_messages_total` | counter | `subject_root, direction (in/out)` |
| `sociopulse_db_query_duration_seconds` | histogram | `module, query` |

Plus the standard `go_*` collectors from `prometheus/client_golang`.

### Cardinality discipline

The label set above is the **maximum**. Do not add per-respondent,
per-call, or per-recording labels — they are unbounded. The rule:

- `tenant_id` is allowed only on per-tenant counters where the tenant
  is the natural slice. 30 tenants × ~10 statuses × ~20 regions = 6 k
  series, manageable.
- `operator_id` is allowed only where we slice by operator
  (`operator_state_seconds`). 500 operators × 7 states = 3.5 k series.
- `respondent_id`, `call_id`, `recording_id`, `phone_*` — **never**.
  These fields belong in logs and traces, never in metrics.

The `cardinality-budget` doc (`docs/observability/cardinality.md`,
created in Plan 20) enumerates each allowed label and the upper bound
on its value set. PRs that add a new label must update that doc.

### Histogram buckets

Default bucket sets are project-wide:

- HTTP duration: `[0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5]`
  seconds.
- Call duration: `[5, 15, 30, 60, 120, 300, 600, 1200]` seconds.
- DB query duration: `[0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.5, 1]`
  seconds.
- WS frame ack latency: `[0.01, 0.05, 0.1, 0.25, 0.5, 1, 2]` seconds.

These live as constants in `pkg/obs/buckets.go`. Modules use them by
constant name (`obs.BucketsHTTP`) so a global change updates everyone.

### Registration

Modules register their collectors in `service.NewModule(deps)`:

```go
func newMetrics(reg prometheus.Registerer) *metrics {
    m := &metrics{
        callsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
            Namespace: "sociopulse",
            Subsystem: "dialer",
            Name:      "calls_total",
            Help:      "Total number of finalized calls.",
        }, []string{"tenant_id", "project_id", "status", "region"}),
        // ...
    }
    reg.MustRegister(m.callsTotal /*, ... */)
    return m
}
```

The registry is the **module-scoped** `prometheus.Registerer` passed
in via `deps`. `cmd/api/main.go` constructs `prometheus.NewRegistry()`
once and `MustRegister`s the default `go_*` collectors plus a
namespace prefix.

`cmd/worker`, `cmd/telephony-bridge`, `cmd/recording-uploader` each
have their own registry; metrics from different binaries do not share
series (they have different `service` labels at scrape time).

## Tracing — OpenTelemetry

Tracer SDK: `go.opentelemetry.io/otel`. Propagation:
W3C TraceContext + Baggage. Exporter: OTLP gRPC →
**OpenTelemetry Collector** → **Grafana Tempo** (S3-backed).

Sampling: head-based, **10% in production**, **100% in staging**,
**100% locally**. The `otel_sampling_ratio` config key tunes
production.

### Span naming

Format: **`<module>.<Service>.<Method>`**. Examples:

```
auth.AuthService.Login
auth.JWTIssuer.Validate
crm.ProjectService.Create
crm.QuotaTracker.Increment
dialer.OperatorFSM.GoReady
dialer.CallQueue.PickNext
recording.RecordingService.Commit
recording.RecordingService.OpenAudioStream
surveys.Runtime.NextNode
analytics.IngestPipeline.flushCalls
realtime.Hub.Broadcast
```

For non-method spans (helpers, asynq tasks, NATS handlers), use a
`<module>.<verb>` pattern:

```
analytics.dlq_publish
recording.s3_put
recording.kms_decrypt
dialer.outbox_relay
worker.task.crm_respondent_import
nats.subscribe.handle
```

For database / external calls:

```
db.query           → span attribute db.statement (parameterised, no PII)
nats.publish
nats.subscribe.handle
esl.command
s3.put / s3.get
kms.encrypt / kms.decrypt
```

### Span attributes

Every span carries:

- `tenant.id`         (tenant the span is acting on, when known)
- `actor.user_id`     (the principal driving the request, when known)
- `business.op`       (a stable opname tag — e.g. `"call.dial"`,
  `"recording.commit"`)
- `request.id`        (mirrors the zap field)
- `module`            (the owning module name)

Plus span-kind-specific attributes:

- HTTP server spans: `http.method`, `http.route`, `http.status_code`.
- Database spans: `db.system`, `db.statement` (parameterised).
- Messaging spans: `messaging.system`, `messaging.destination`,
  `messaging.message_id`.
- gRPC spans: `rpc.system`, `rpc.service`, `rpc.method`,
  `grpc.status_code`.

The `pkg/obs/tracing.go` constructor adds a tracer-level processor
that attaches `service.environment`, `service.name`, `service.version`
to every span automatically; we don't restate them.

### Errors on spans

When an operation fails, the outermost handler records on the active
span:

```go
span.RecordError(err)
span.SetStatus(codes.Error, err.Error())
```

The error message is also redacted by the tracer's processor using the
same regex set as the zap encoder. This is the only place the
redaction surface is duplicated; the regexes themselves come from one
config.

## SLI / SLO / Alerts

Spec §15.5 lists the project's SLIs and SLO targets. Headlines and
the alerting policy that backs them:

| SLI | SLO target | Alert policy |
|---|---|---|
| HTTP API availability | 99.5% / 30 d | error budget < 50% → warning; < 10% → critical |
| HTTP API p95 latency | < 300 ms | > 500 ms 5 min → warning |
| Real-time event latency p95 | < 500 ms | > 1 s 5 min → warning |
| Recording upload lag p95 | < 5 min | > 1 h 5 min → critical |
| Recording upload success rate | > 99.95% | < 99.9% over 1 h → critical |
| Trunk health up | > 95% | trunk down > 5 min → critical, on-call |
| Postgres replication lag | < 5 s | > 30 s → warning |
| Quota recompute job last run | < 90 min ago | > 90 min → warning |

Alert routing:

- **Critical** → Yandex OnCall / PagerDuty → on-call duty.
- **Warning** → Slack `#sociopulse-alerts`.

Alert rules live in
`deployments/helm/sociopulse/templates/prometheusrule.yaml` (Plan 20
Task 1 builds the initial set).

## Dashboards

Pre-built Grafana dashboards (committed JSON in
`deployments/grafana/dashboards/`):

- **System overview** — live calls, ready operators, queue depth,
  error rate, latencies.
- **Per-tenant overview** — calls by status, recording lag, quota
  progress.
- **Telephony** — trunk health, FS-node load, ESL command latency.
- **Recording pipeline** — upload throughput, encrypt/decrypt timings,
  S3 errors.
- **Operators** — state distribution, KPI by operator.
- **DB** — query latencies, pool, replication.
- **Cost** — storage growth, S3 requests, KMS calls.

## Linter Mapping

| Rule | Linter |
|---|---|
| zap/slog key-value pairs correct | `loggercheck` |
| Context propagation through chain | `contextcheck` |
| HTTP request without context | `noctx` |
| `bodyclose` for `*http.Response` | `bodyclose` |
| `sqlclosecheck` / `rowserrcheck` | `sqlclosecheck`, `rowserrcheck` |
| Cardinality budget (cardinality-budget.md) | (review) |
| Span name `<module>.<Service>.<Method>` | (review) |
| Field name stability (`tenant_id` not `tenantID`) | (review) |

Three rules above are review-only because static analysis cannot
robustly check them. A new metric label or span name lands with a PR
checklist item ("dashboard reviewer initialled the cardinality
budget"); the alternative — a bespoke linter — is more cost than
benefit at our size.

## Cross-references

- Spec §15 — full observability narrative (this document is the
  enforceable subset).
- Spec §15.4 — span list.
- `02-module-contracts.md` — module names that prefix metrics + spans.
- `03-error-handling.md` § Single-handling rule — defines where the
  one-and-only-one error log lives.
- `05-configuration.md` § Redaction — where the regex list lives in
  YAML.
- `07-go-coding-standards.md` § Linter mapping — `loggercheck`,
  `contextcheck`, `noctx`.
- ADR-0012 — zap chosen over slog (for now).
- `samber/cc-skills-golang@golang-error-handling` — structured logging
  guidance the redaction policy is built on.
