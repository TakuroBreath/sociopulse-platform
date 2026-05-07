# СоциоПульс

SaaS-платформа для проведения телефонных социологических опросов: автодозвон,
анкетирование, контроль операторов, контроль качества записей.

## Documentation

- **System design:** [docs/superpowers/specs/2026-05-06-sociopulse-system-design.md](docs/superpowers/specs/2026-05-06-sociopulse-system-design.md)
- **Architecture overview:** [ARCHITECTURE.md](ARCHITECTURE.md)
- **Contributing:** [CONTRIBUTING.md](CONTRIBUTING.md)
- **Implementation plans:** [docs/superpowers/plans/](docs/superpowers/plans/)

## Quickstart

Requirements: Go 1.26+, Docker, Make.

```bash
# Run linters
make lint

# Run tests
make test

# Build all binaries
make build

# Run cmd/api locally on :8080
make run
```

Visit http://localhost:8080/healthz to verify.

## Local development

Backend services (`cmd/api`, `cmd/worker`, `cmd/telephony-bridge`, etc.) run
natively on your machine via `go run`. External dependencies (Postgres, Redis,
NATS, optionally ClickHouse and MinIO) run as Docker containers managed by
`docker-compose.dev.yml`.

All ports bind to `127.0.0.1` only — nothing in the dev stack is reachable
from your network.

### Quick start

```bash
# Boot core dependencies (Postgres + Redis + NATS):
make dev-up

# Run cmd/api locally against the containers:
go run ./cmd/api --config configs/development/config.yaml

# In another terminal — run a worker:
go run ./cmd/worker --config configs/development/config.yaml
```

### Profiles

- `make dev-up` — core only (PG + Redis + NATS).
- `make dev-up PROFILE=analytics` — adds ClickHouse for analytics module work.
- `make dev-up PROFILE=storage` — adds MinIO (S3 emulator) for recording-module work.
- `make dev-up PROFILE=full` — everything above.

### Useful commands

- `make dev-logs` — tail all container logs.
- `make dev-psql` — open a `psql` shell against the dev Postgres.
- `make dev-redis-cli` — open `redis-cli`.
- `make dev-nats` — show NATS monitoring info.
- `make dev-down` — stop all containers (data preserved in volumes).
- `make dev-reset` — stop and **delete all data** (destructive). Use when
  migrations get tangled.

### Tests

Integration tests use `testcontainers-go` and start their own ephemeral
containers per test, separate from the dev stack. You don't need
`make dev-up` to run `go test`.

### Production != Dev

This Compose stack is for **local development only**. Production runs on
Yandex Managed Kubernetes (MKS) with Yandex Managed PostgreSQL / Redis /
ClickHouse / Object Storage — see `sociopulse-infra` and Plan 01.

## Docker images

CI publishes multi-tagged images to Docker Hub on every push to `main` and on
version tags (`v*.*.*`). Public registry — no login required to pull:

- `docker.io/maxtakuro/sociopulse-api:latest` — last build on `main`
- `docker.io/maxtakuro/sociopulse-api:main` — same as above (alias)
- `docker.io/maxtakuro/sociopulse-api:sha-<short-sha>` — pinned to commit
- `docker.io/maxtakuro/sociopulse-api:v0.0.1-foundation` — version tag

```bash
docker pull maxtakuro/sociopulse-api:latest
docker run --rm -p 8080:8080 maxtakuro/sociopulse-api:latest
curl http://localhost:8080/healthz   # → ok
```

Build locally:

```bash
make docker-build               # → sociopulse-api:dev
```

## Repository layout

- `cmd/` — executable entry points (api, worker, migrator, telephony-bridge, recording-uploader)
- `internal/` — private domain modules (auth, crm, surveys, dialer, realtime, recording, ...)
- `pkg/` — shareable utility packages
- `migrations/` — SQL migrations
- `configs/` — YAML configs (per environment)
- `deployments/` — IaC + Kubernetes manifests
- `web/` — React frontend
- `docs/` — documentation tree

## License

Proprietary. See [LICENSE](LICENSE).
