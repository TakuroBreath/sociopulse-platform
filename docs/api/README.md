# docs/api/

API contracts and generated reference docs.

- OpenAPI specs for HTTP endpoints (per-module)
- gRPC `.proto` files (recording, telephony)
- Generated TypeScript types (Plan 15)

## Subdirectories

- [`billing/v1/openapi.yaml`](billing/v1/openapi.yaml) — finance / tariff
  endpoints (Plan 14).
- [`recording/`](recording/) — recording stream + search endpoints
  (Plan 12 / Plan 13).
- [`reports/`](reports/) — reports + async-job endpoints (Plan 13).
- [`collections/sociopulse/`](collections/sociopulse/) — Bruno REST
  collection covering the public HTTP surface of `cmd/api` organised by
  module, with login flow + JWT auto-capture + happy path + at least
  one negative case per endpoint. For manual developer exploration and
  a QA pre-release sweep; complements the automated regression net in
  `tests/smoke/`. See the collection's own
  [README](collections/sociopulse/README.md) for install / usage.
