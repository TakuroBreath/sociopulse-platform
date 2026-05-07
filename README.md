# СоциоПульс

SaaS-платформа для проведения телефонных социологических опросов: автодозвон,
анкетирование, контроль операторов, контроль качества записей.

## Documentation

- **System design:** [docs/superpowers/specs/2026-05-06-sociopulse-system-design.md](docs/superpowers/specs/2026-05-06-sociopulse-system-design.md)
- **Architecture overview:** [ARCHITECTURE.md](ARCHITECTURE.md)
- **Contributing:** [CONTRIBUTING.md](CONTRIBUTING.md)
- **Implementation plans:** [docs/superpowers/plans/](docs/superpowers/plans/)

## Quickstart

Requirements: Go 1.22+, Docker, Make.

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
