# cmd/telephony-bridge

The СоциоПульс telephony bridge sidecar — a separate binary from `cmd/api`
that owns the only ESL connections to the FreeSWITCH fleet, dispatches
NATS-published commands to FS, and re-publishes FS channel events back on
NATS so downstream services (dialer, realtime, recording-uploader) can react.

`cmd/api` itself does **not** dial FreeSWITCH. It only mounts the
`/internal/freeswitch/directory` HTTP route (Plan 09 Task 8 will add it) for
the FS directory XML callback. Everything else — originate/hangup/mixmonitor
commands, channel-event consumption, per-call backpressure, line-capacity
tracking — runs in this binary.

## Status

Plan 09 Task 1 (this commit) ships the composition root with **stub**
telephony subsystems: `internal/telephony/pool`, `router`, `nats_bridge` are
skeletons large enough to compile and answer health probes. Tasks 2-6
progressively replace each stub:

- Task 2 — ESL client (`internal/telephony/esl`)
- Task 3 — idempotency + command dispatch (`nats_bridge` body)
- Task 4 — pool + healthcheck loop + reconciler (`pool` body)
- Task 5 — router + trunk catalog refresh (`router` body)
- Task 6 — line capacity tracker

## Running locally

```bash
# 1. Boot infra (Postgres + Redis + NATS) via the project's docker-compose:
make dev-up

# 2. Run the bridge against ./configs/development/config.yaml:
go run ./cmd/telephony-bridge

# 3. Probe it:
curl localhost:8080/healthz   # -> "ok"
curl localhost:8080/readyz    # -> JSON status; 503 until NATS+Redis+pool healthy
curl localhost:9090/metrics   # -> Prometheus exposition
```

Override the config directory:

```bash
go run ./cmd/telephony-bridge --config-dir=./configs/development
# or via env:
SOCIOPULSE_CONFIG_DIR=./configs/development go run ./cmd/telephony-bridge
```

## Endpoints

| Endpoint            | Port  | Purpose                                             |
|---------------------|-------|-----------------------------------------------------|
| `/healthz`          | 8080  | Liveness — always 200 once the listener is up.      |
| `/readyz`           | 8080  | Readiness — checks NATS connected, Redis ping, pool.|
| `/metrics`          | 9090  | Prometheus exposition (default OpenMetrics format). |

The metrics port is configured by `cfg.Observability.Metrics.Bind` so
production can pin it to an internal interface.

## Configuration

The bridge reads the same `pkg/config.Snapshot` (Viper + atomic Pointer) that
`cmd/api` uses. The configuration sub-trees this binary actually consumes:

- `service` — env / log_level / region / name (used for log + span fields).
- `database.redis` — Redis client (idempotency keys, line-capacity counters).
- `nats` — bridge subscribes to `tenant.*.telephony.cmd.>` and publishes to
  `tenant.<t>.telephony.event.<call_id>.<verb>`.
- `telephony.bridge.fs_nodes` — required, must be non-empty. Each entry is a
  `FSNode` (`id`, `esl_endpoint`, `esl_cert`, `esl_key`).
- `telephony.bridge.max_concurrent_per_node` — backpressure cap (default 60).
- `telephony.trunks` — trunk catalog (Plan 09 Task 5 — Router).
- `observability.{otel,metrics,logging}` — logger/tracer/Prometheus.
- `shutdown.grace_period` — graceful-shutdown deadline applied per stage.

See `pkg/config/telephony.go` for the canonical struct definitions.

## Graceful shutdown

On SIGINT or SIGTERM the bridge drains in this order:

1. Stop accepting new HTTP probes / scrapes (close health + metrics listeners).
2. Drain the NATS bridge (in-flight commands run to completion or deadline).
3. Stop the router refresh loop.
4. Close the ESL pool.
5. Drain the NATS connection (inflight publishes finish).
6. Close the Redis client.

Each stage uses a detached `context.Background()` with `cfg.Shutdown.GracePeriod`
budget — inheriting the cancelled parent context would abort drain immediately.

## Tests

Unit tests live next to the code:

```bash
go test -race -count=1 ./cmd/telephony-bridge/... ./internal/telephony/...
```

The composition-root tests use `miniredis.RunT(t)` for Redis. NATS is exercised
via the disconnected path (`RetryOnFailedConnect(true)` against an unreachable
URL); the connected path will be covered by the integration tests Plan 09
Task 7 introduces.

Integration tests (Plan 09 Task 7+):

```bash
make test-integration-telephony   # placeholder; spins up FS via Testcontainers
```
