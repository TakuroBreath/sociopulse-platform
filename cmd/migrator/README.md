# cmd/migrator

`migrator` is the СоциоПульс schema migration runner. It is a wrapper around
[golang-migrate](https://github.com/golang-migrate/migrate) using the
`pgx/v5` driver.

## Usage

```bash
DATABASE_URL=postgres://app:pwd@pgbouncer:6432/sociopulse?sslmode=verify-full \
MIGRATIONS_PATH=file:///etc/sociopulse/migrations \
migrator up
```

Subcommands:

| Cmd               | Description                                            |
|-------------------|--------------------------------------------------------|
| `up`              | Apply every pending up migration                       |
| `down`            | Revert every applied migration (use only in dev/test)  |
| `down --steps=N`  | Revert N steps                                         |
| `status`          | Print the current version and dirty flag               |
| `force <version>` | Set version + clear dirty flag (manual recovery only)  |

## Environment

| Env                | Default                              | Notes                            |
|--------------------|--------------------------------------|----------------------------------|
| `DATABASE_URL`     | _(required)_                         | Postgres DSN                     |
| `MIGRATIONS_PATH`  | `file:///etc/sociopulse/migrations`  | `file://` URL of the migrations  |

## Exit codes

| Code | Meaning                                                     |
|------|-------------------------------------------------------------|
| 0    | Success                                                     |
| 1    | Usage error (bad sub-command, missing argv, empty DSN)      |
| 2    | Migration or connection error                               |

## Production

Runs as a Kubernetes `Job` with the ArgoCD `PreSync` hook before each
`cmd/api` rollout (see `deployments/helm/api/templates/migrator-job.yaml`).
The Job blocks the rollout if migrations fail.

## Development

```bash
make dev-up                   # boot Postgres in Docker
DATABASE_URL=postgres://app:devpass@localhost:5432/sociopulse?sslmode=disable \
MIGRATIONS_PATH=file://$PWD/migrations \
go run ./cmd/migrator status
```

Or via Make targets (see top-level `Makefile`):

```bash
make migrate-up      # apply all migrations
make migrate-status  # print version + dirty
make migrate-down    # revert all (DEV ONLY)
```

## Tests

Unit tests cover argv validation and exit-code routing:

```bash
go test -race ./cmd/migrator/...
```

Integration tests boot Postgres 16 in `testcontainers-go` and exercise
`up`, `down`, `force`, and `status` end-to-end:

```bash
go test -tags=integration -timeout 5m ./cmd/migrator/...
```
