# 05. Configuration

This document describes how a `cmd/<binary>` process resolves its
configuration at startup and at runtime. The full registry of keys is
spec §14.2; this document explains the **layering and lifecycle** of
those keys, not their semantic content.

The library is `spf13/viper` (ADR-0013) — chosen over `koanf` for
feature breadth (file watch, env-binding, sub-config slicing). The
trade-off is a heavier dependency; we accept it because it solves all
six concerns below in one library.

## Layering Order

A key's effective value comes from the first layer that defines it,
walking top to bottom:

```
┌──────────────────────────────────────────────────────────────┐
│ 1. tenant_settings (per-tenant, runtime)                     │  ◄─ in-process cache (TTL 30s)
├──────────────────────────────────────────────────────────────┤
│ 2. Yandex Lockbox secret (production)                        │  ◄─ External Secrets Operator
├──────────────────────────────────────────────────────────────┤
│ 3. Environment variables                                     │  ◄─ k8s env or developer's shell
├──────────────────────────────────────────────────────────────┤
│ 4. configs/<env>/config.yaml (deploy-time defaults)          │  ◄─ committed to repo
├──────────────────────────────────────────────────────────────┤
│ 5. internal hard-coded defaults (Config{} zero values)       │  ◄─ last-resort fallbacks
└──────────────────────────────────────────────────────────────┘
```

The two boundaries deserve emphasis:

- **`tenant_settings` is per-tenant**, the other layers are
  process-global. A key like `dialer.attempt_max` may live in *both*
  (`config.yaml: dialer.defaults.attempt_max` is the process default;
  `tenant_settings(tenant_id, 'dialer.attempt_max')` overrides per
  tenant). The lookup pattern is fixed: every consumer of a per-tenant
  setting calls `tenancy.SettingsCache.GetWithDefault(ctx, tenantID,
  key, def)` and the YAML default is the `def` argument.
- **Lockbox holds secrets only**: TLS key passphrases, JWT signing
  secret, KMS service-account key, Postgres passwords, Docker Hub
  token. Configuration values that are safe to commit (timeouts,
  pool sizes, service names) live in YAML.

## Layer 4 — `configs/<env>/config.yaml`

Three subdirectories ship with the repo: `configs/development/`,
`configs/staging/`, `configs/production/`. Each contains:

```
configs/<env>/
├── config.yaml         # main config, mounted into k8s ConfigMap
├── auth.yaml           # auth block (per-module fragments, merged at startup)
├── dialer.yaml
├── telephony.yaml
├── recording.yaml
├── ...
└── README.md           # which keys this env overrides vs. parent
```

The fragments are loaded in alphabetical order; later files override
earlier ones for repeated keys. `cmd/api/main.go` is responsible for
the load:

```go
v := viper.New()
v.AddConfigPath("/etc/sociopulse")
v.AddConfigPath("./configs/" + env)
v.SetConfigName("config")
v.SetConfigType("yaml")
v.SetEnvPrefix("SOCIOPULSE")
v.AutomaticEnv()
v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
if err := v.ReadInConfig(); err != nil { ... }
for _, frag := range []string{"auth", "dialer", "telephony", ...} {
    sub := viper.New()
    sub.SetConfigFile(filepath.Join(path, frag + ".yaml"))
    if err := sub.ReadInConfig(); err == nil {
        _ = v.MergeConfigMap(sub.AllSettings())
    }
}
```

The full layout of the YAML is in spec §14.2. Headlines:

```yaml
service:
  env: production           # development|staging|production
  log_level: info
  region: yc-ru-central-1

http:
  bind: ":8080"
  read_timeout: 10s
  write_timeout: 30s
  max_body_size: 10MB

database:
  postgres:
    dsn: postgres://app:${PG_PASSWORD}@pgbouncer:6432/sociopulse?sslmode=require
    max_conns: 50
    max_idle_time: 5m
  clickhouse:
    dsn: clickhouse://app:${CH_PASSWORD}@ch-cluster:9000/sociopulse
    batch_size: 10000
    flush_interval: 5s
  redis:
    addr: redis-master:6379
    pool_size: 50

nats:
  urls: ["nats://nats-1:4222","nats://nats-2:4222","nats://nats-3:4222"]
  account: cmd-api

dialer:
  defaults:
    attempt_max: 3
    retry_no_answer_delay: 4h
    retry_busy_delay: 30m
    retry_dropped_delay: 2h
    retry_tech_failure_delay: 5m
    dialing_timeout: 25s
    pause_max: 15m
    rdd:
      enabled: true
      max_rate_per_sec: 10

# ... see spec §14.2 for full registry
```

YAML is the **defaults file**. It is committed; it is reviewed; it is
the source of truth for what a fresh install looks like. Anything an
attacker would benefit from (DSN with password, Lockbox key path, KMS
folder ID) lives behind `${ENV_VAR}` placeholders that viper expands at
load time.

## Layer 3 — Environment Variables

Every YAML key has a deterministic env-var name:

```
service.log_level         →  SOCIOPULSE_SERVICE_LOG_LEVEL
http.bind                 →  SOCIOPULSE_HTTP_BIND
database.postgres.dsn     →  SOCIOPULSE_DATABASE_POSTGRES_DSN
dialer.defaults.attempt_max → SOCIOPULSE_DIALER_DEFAULTS_ATTEMPT_MAX
```

The mapping is `SOCIOPULSE_` prefix, dot-to-underscore, uppercase.
Set in viper via `v.SetEnvPrefix("SOCIOPULSE")` +
`v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))` +
`v.AutomaticEnv()`.

In Kubernetes, env vars are populated from:

- A `Deployment.spec.template.spec.containers[].env` block for
  non-secret values mirrored from the ConfigMap.
- `valueFrom: secretKeyRef` for Lockbox-backed secrets (see Layer 2).

In local development, env vars come from the shell or a `.env.local`
file (gitignored) loaded by `direnv`. The repo ships
`configs/development/.env.example` documenting which env vars a
developer must set.

## Layer 2 — Yandex Lockbox

Secrets live in **Yandex Lockbox**, a managed secret store. They are
mounted into pods via the **External Secrets Operator** (ESO), which
syncs Lockbox payloads into Kubernetes Secret resources every 5
minutes. From the pod's perspective they are env vars or files —
viper does not know they came from Lockbox.

Inventory of secrets per binary:

| Binary | Secret | Env var / mount path |
|---|---|---|
| `cmd/api`, `cmd/worker` | Postgres password | `SOCIOPULSE_DATABASE_POSTGRES_DSN` (full DSN — Lockbox stores the whole string) |
| `cmd/api`, `cmd/worker` | ClickHouse password | `SOCIOPULSE_DATABASE_CLICKHOUSE_DSN` |
| `cmd/api`, `cmd/worker` | Redis password | `SOCIOPULSE_DATABASE_REDIS_PASSWORD` |
| `cmd/api` | JWT signing secret | `SOCIOPULSE_AUTH_JWT_SECRET` |
| `cmd/api` | KMS service-account key | `/etc/sociopulse/kms/sa-key.json` (file mount) |
| `cmd/telephony-bridge` | ESL password | `SOCIOPULSE_TELEPHONY_BRIDGE_ESL_PASSWORD` |
| `cmd/telephony-bridge` | mTLS client cert + key | `/etc/sociopulse/certs/esl-client*.pem` |
| `cmd/recording-uploader` | mTLS client cert + key for gRPC | `/etc/sociopulse/certs/uploader-client*.pem` |
| `cmd/recording-uploader` | S3 access keys (IAM SA token) | injected by IAM-SDK from VM metadata |

Secrets do NOT appear in the YAML. The YAML may reference them via
`${...}` interpolation (`dsn: postgres://app:${PG_PASSWORD}@...`); the
interpolation is resolved by viper at startup using the env layer.

## Layer 1 — `tenant_settings`

Per-tenant runtime configuration lives in the Postgres
`tenant_settings` table (spec §6.3, §14.3):

```sql
create table tenant_settings (
  tenant_id   uuid not null references tenants(id),
  key         text not null,
  value       jsonb not null,
  updated_at  timestamptz not null default now(),
  primary key (tenant_id, key)
);
```

The full registry of allowed keys is in spec §14.3. Examples:

| Key | Type | Default | Spec §14.3 row |
|---|---|---|---|
| `dialer.attempt_max` | int | 3 | max attempts per respondent |
| `dialer.retry_no_answer_delay` | duration | `4h` | retry delay for no-answer |
| `dialer.dialing_timeout` | duration | `25s` | how long we wait for ANSWER |
| `dialer.pause_max` | duration | `15m` | pause overrun threshold |
| `dialer.rdd.enabled` | bool | `true` | feature flag |
| `dialer.routing_strategy` | enum | `least_cost_with_fallback` | trunk routing |
| `dialer.caller_id` | string | (from YAML) | outgoing caller-ID |
| `recording.consent_prompt_url` | string | standard | IVR consent URL |
| `recording.hot_retention_days` | int | 365 | recording hot tier |
| `recording.cold_retention_days` | int | 730 | cold tier |
| `surveys.max_questions` | int | 25 | per-survey question cap |
| `surveys.cost_per_completed_rub` | int | 120 | operator per-survey wage |
| `auth.password_min_length` | int | 8 | minimum password length |
| `auth.totp_required` | bool | `false` | per-tenant TOTP enforcement |
| `quality.violation_categories` | json[] | standard | QA violation taxonomy |
| `notifications.pause_overrun_threshold` | duration | `15m` | when to alert |
| `ui.theme_default` | enum | `light` | tenant default theme |
| `ui.font_size_default` | enum | `md` | default font size |
| `quotas.dimensions` | json | `[region]` | which dimensions to track |

`tenancy.SettingsCache` (defined in `02-module-contracts.md` § Tenancy)
is the **only** allowed access path. Direct Postgres reads of
`tenant_settings` from outside the tenancy module are a depguard
violation. The cache pattern:

```go
def, err := api.SettingValueFromAny(3)
if err != nil {
    return fmt.Errorf("default for dialer.attempt_max: %w", err)
}
v, err := s.tenancy.GetWithDefault(ctx, tenantID, "dialer.attempt_max", def)
if err != nil {
    return fmt.Errorf("get dialer.attempt_max: %w", err)
}
maxAttempts, err := v.AsInt()
if err != nil {
    return fmt.Errorf("parse dialer.attempt_max: %w", err)
}
```

The cache:

- Loads on first miss from Postgres.
- TTL = 30 s (process-local).
- Invalidates on `tenant.<t>.settings.updated` NATS event published
  by `SettingsCache.Set`.
- `InvalidateAllLocal(tenantID)` on `tenant.<t>.archived`.

The 30 s TTL is the **maximum staleness** any tenant change tolerates;
the NATS invalidation makes the typical case immediate.

## Hot-Reload of Static Config

`viper.WatchConfig` is enabled for non-secret keys. A change to
`configs/<env>/config.yaml` (or any fragment) triggers
`viper.OnConfigChange`, which:

1. Re-reads the merged config.
2. Applies field-by-field: `service.log_level` rebuilds the zap logger;
   `http.read_timeout` is captured by the next request; pool sizes are
   noted but not applied (resizing pgxpool live is risky — defer to a
   restart).
3. Logs every applied change at `info` level: `"config reloaded:
   service.log_level=debug"`.

Secrets are NOT watched. A SIGHUP causes the process to exit cleanly
and Kubernetes restarts it — that is how we pick up rotated secrets.
Reasoning: keeping a stale TLS cert behind a long-running goroutine
is more dangerous than a 2-second restart.

The `cmd/<binary>` author documents which keys are hot-reloadable in
`cmd/<binary>/main.go`'s package doc.

## Tenant Overrides Are Application-Level, Not Config-Level

A common confusion: "if every tenant has its own `dialer.attempt_max`,
why not put it in the YAML for completeness?" The answer is that
runtime tenant configuration **must not be readable by code that has
not bound a tenant context**. Putting `tenant_attempt_max[<tenant>]`
in YAML would let any module read the value without going through
`tenancy.SettingsCache`, breaking the L4 isolation layer (KMS, RLS,
S3, NATS — all depend on the assumption that tenant-scoped data is
loaded only on demand).

So: process-global defaults in YAML, per-tenant overrides in
`tenant_settings`, accessed via `tenancy.SettingsCache`. Always.

## Reading Config in Code

The recommended pattern: **decode the YAML into a typed struct once at
startup, pass the struct (or a slice) into module constructors, never
poke at viper from a service.**

```go
type Config struct {
    Service  ServiceConfig
    HTTP     HTTPConfig
    Database DatabaseConfig
    NATS     NATSConfig
    Auth     auth.Config
    Dialer   dialer.Config
    // ...
}

type ServiceConfig struct {
    Env      string `mapstructure:"env"      validate:"oneof=development staging production"`
    LogLevel string `mapstructure:"log_level" validate:"oneof=debug info warn error"`
    Region   string `mapstructure:"region"`
}

func Load(env string) (*Config, error) {
    v := viper.New()
    // ... viper setup as above
    var cfg Config
    if err := v.Unmarshal(&cfg); err != nil { return nil, err }
    if err := validator.New().Struct(&cfg); err != nil { return nil, err }
    return &cfg, nil
}
```

Each module exposes its own typed `Config` in the `api/` package.
`cmd/<binary>/main.go` slices the global config and passes the slice
to the module's constructor:

```go
authMod := auth.New()
deps.AuthConfig = cfg.Auth        // typed struct, NOT a viper instance
authMod.Register(deps)
```

Modules that legitimately need to react to runtime config changes
register an `OnConfigChange` callback at startup; modules that don't
just hold the value they were constructed with.

## Validation

Every typed config struct carries struct-tag validations
(`go-playground/validator/v10`):

- `validate:"required"` on identifiers.
- `validate:"oneof=..."` on enums.
- `validate:"min=...,max=..."` on numbers.
- `validate:"hostname_port"` on bind addresses.
- `validate:"url"` on URLs.

`Load` calls `validator.New().Struct(&cfg)` and returns the validation
error verbatim — startup fails loudly with a line number. This catches
typos early (`environment: produciton` → `oneof tag failed`).

## Local Development Quickstart

To run `cmd/api` locally:

```bash
cp configs/development/.env.example .env.local
direnv allow                                       # loads .env.local
make dev-up                                        # docker-compose: pg, redis, nats, minio, kms-fake
make api                                           # builds and runs cmd/api
```

The dev environment uses:

- A locally-mounted `configs/development/config.yaml` instead of a
  ConfigMap.
- A `kms-fake` HTTP service stand-in for Yandex KMS (encrypt-as-passthrough
  with a fixed test key — never used in CI integration tests, which
  spawn a real Yandex KMS client against a sandbox folder).
- MinIO as S3.
- An empty `tenant_settings` table — every per-tenant lookup falls to
  the YAML default.

CI runs `make test` (unit), `make test-integration` (testcontainers),
`make lint`, and `make smoke` (boots `cmd/api`, hits `/healthz`,
SIGTERMs).

## Cross-references

- Spec §14.1–§14.4 — full registry, YAML structure, tenant_settings
  schema, what is *not* configurable.
- `02-module-contracts.md` § Tenancy — the `SettingsCache` interface.
- `03-error-handling.md` — `ErrInvalidArgument` semantics for bad
  setting values.
- `06-observability.md` — the `service.env` value populates the
  `service.environment` zap field on every log line.
- ADR-0013 — viper as the configuration library.
- ADR-0006 — `app.tenant_id` SET LOCAL is part of every TX, but is
  *not* a config concern; it lives in `pkg/postgres`.
