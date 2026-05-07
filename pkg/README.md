# pkg/

Reusable utility packages potentially shareable across services. The
inhabitants are documented in
`docs/architecture/01-package-layout.md` § `pkg/`. Concrete
implementations are filled in by later plans; this scaffolding (Plan
00a Task 5) defines the public surface and keeps `go build` green so
modules can wire the contracts.

| Package | Real wiring lands in |
|---|---|
| `pkg/postgres` | Plan 03 Task 4 |
| `pkg/outbox` | Plan 03 Task 6 |
| `pkg/encryption` | Plan 03 Task 5 |
| `pkg/observability` | Plan 02 Task 2 |
| `pkg/config` | Plan 02 Task 1 |
| `pkg/eventbus` | Plan 03 Task 7 |
| `pkg/grpc` | Plan 02 Task 4 |
| `pkg/httputil` | Plan 02 Task 3 |
| `pkg/middleware/auth` | Plan 04 Task 4 |

`pkg/` MUST NOT import from `internal/` (per § Rules in
01-package-layout). The single documented exception lives at
`pkg/middleware/auth`, which consumes the
`internal/auth/api.ClaimsValidator` interface.
