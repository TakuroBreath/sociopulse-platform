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
