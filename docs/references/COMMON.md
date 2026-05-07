# Common references — cross-cutting

> Applied across multiple plans. Read this once; per-plan files extend it with topic-specific links.

---

## Tooling for agents (use these, don't guess)

Before reading any spec or library doc below from training data, **verify with current sources**:

### `context7` MCP — for library API
When you need current API of any library (`golang-jwt/jwt/v5`, `pgx/v5`, `gin`, `pquerna/otp`, `aws-sdk-go-v2`, etc.):
1. `mcp__plugin_context7_context7__resolve-library-id` — find the lib's ID by name.
2. `mcp__plugin_context7_context7__query-docs` — query the actual current docs.

Don't guess function signatures, option struct fields, or error types from training data — they're frequently wrong (libs evolve fast). Wrong guesses → subagent dispatch loops.

### `WebSearch` — for problem-solving
For specific errors, recent CVEs, or "how do I do X with Y" questions, search the web. Stack Overflow / Habr / GitHub issues / library blogs almost always have better answers than my (possibly stale) training data.

Especially valuable for:
- Yandex Cloud quirks (training data is thin here)
- FreeSWITCH error messages
- Russian-language SIP-trunk specifics
- Recent library version migration issues

### `WebFetch` — for specific URLs
The links below in this file are **starting points**. To read them, `WebFetch` at use-time so content is current.

---

## Compliance posture (152-ФЗ)

**Pragmatic, not theatrical.** Project does NOT pursue formal certification (УЗ-3 dossier, ФСТЭК reports, Roskomnadzor audit prep). Build with good security hygiene, ship.

**What we DO** (because it's good engineering anyway):
- AES-256-GCM envelope encryption for PII (phones, names) — per-tenant KEK via Yandex KMS.
- PostgreSQL RLS + `SET LOCAL app.tenant_id` for tenant isolation.
- Audit log (when fully wired in a later plan) — who-accessed-what-when in `audit_log` table.
- IVR consent prompt before recording (Plan 12).
- Data residency on Yandex Cloud RU-Central-1.

**What we DON'T do for v1**:
- Formal compliance dossiers / pen-test reports for regulators.
- Encrypted-at-rest phone-hash pepper (deferred future hardening).
- Beyond-baseline access-control audit trails.

If real audit ever surfaces — these are tractable add-ons. Don't burn cycles on ceremony when there's no auditor to satisfy.

For background reference (read if needed, don't internalize):
- [**152-ФЗ on pravo.gov.ru**](http://pravo.gov.ru/proxy/ips/?docbody=&firstDoc=1&lastDoc=1&nd=102108261)
- [**Yandex Cloud compliance page**](https://yandex.cloud/ru/security/compliance) — Yandex has УЗ-1..УЗ-4 certs, so infra-level compliance is "covered" if it ever matters.
- **38-ФЗ vs 152-ФЗ**: соц. опросы — 152-ФЗ (ПДн); реклама — 38-ФЗ. Мы только 152-ФЗ. См. ADR-0001/0003.

---

## Yandex Cloud

### Canonical
- [**Yandex Cloud documentation root**](https://yandex.cloud/ru/docs) — официальная.
- [**Managed PostgreSQL**](https://yandex.cloud/ru/docs/managed-postgresql/) — то, что мы используем как target. Версии, лимиты, расширения.
- [**Object Storage (S3-совместимый)**](https://yandex.cloud/ru/docs/storage/) — наш recordings/reports/backups. SSE-KMS интеграция описана отдельно.
- [**Key Management Service (KMS)**](https://yandex.cloud/ru/docs/kms/) — наш per-tenant KEK. Лимиты на TPS важны (см. raw doc).
- [**Lockbox**](https://yandex.cloud/ru/docs/lockbox/) — secret manager. Используем для production secrets.
- [**Managed Kubernetes (MKS)**](https://yandex.cloud/ru/docs/managed-kubernetes/) — production-deployment target.

### Go SDK
- [**yandex-cloud/go-sdk on GitHub**](https://github.com/yandex-cloud/go-sdk) — high-level SDK.
- [**yandex-cloud/go-genproto**](https://github.com/yandex-cloud/go-genproto) — gRPC stubs. Иногда удобнее напрямую.

### Practical
- [**Yandex Cloud blog на Хабре**](https://habr.com/ru/companies/yandex/) — официальные посты.
- [**Yandex Cloud Community**](https://yandex.cloud/ru/community) — Telegram + Discord.

### Gotchas
- **KMS rate limits**: 200 RPS по умолчанию. Для нашего сценария (50k звонков/день, 1 DEK на запись) — на грани. **Cache DEK обязательно** (что мы и делаем в `KMSResolver`).
- **Object Storage eventual consistency** — листинг bucket'а после записи может на секунды показывать устаревшее. Не полагаться на `ListObjects` сразу после `PutObject`.
- **Managed PostgreSQL** — нет суперюзера. Создание расширений ограничено. `pgcrypto`, `uuid-ossp`, `pg_trgm` — есть. Custom extensions — нельзя.
- **Network egress** между зонами AZ не бесплатен. Кросс-AZ replica = $$.

---

## Go best practices (project-wide)

### Canonical
- [**Effective Go**](https://go.dev/doc/effective_go) — старая, но всё ещё база.
- [**Go Style Guide (Google)**](https://google.github.io/styleguide/go/) — самый строгий публичный гайд. Мы не следуем буквально, но дух — да.
- [**Go Code Review Comments**](https://github.com/golang/go/wiki/CodeReviewComments) — короткий список "best practices что должны проверять на ревью".

### Project-specific
- [`docs/architecture/07-go-coding-standards.md`](../architecture/07-go-coding-standards.md) — наш distilled standard.
- [`docs/architecture/08-tdd-discipline.md`](../architecture/08-tdd-discipline.md) — TDD методология.
- [`samber/cc-skills-golang`](https://github.com/samber/cc-skills) — community skill pack, 12 skills installed at `~/.agents/skills/golang-*/`.

### Tools
- [**golangci-lint**](https://golangci-lint.run/) — наш линтер. 35 включённых линтеров.
- [**testcontainers-go**](https://golang.testcontainers.org/) — integration tests с реальной PG/Redis/NATS.
- [**goleak**](https://github.com/uber-go/goleak) — обнаружение утечек горутин.

### Gotchas
- **`time.After` в for-loop** — leak'ит таймер на каждой итерации. Use `time.NewTimer` + `Reset`. Enforced by `make grep-time-after`.
- **`pgxpool.Pool` напрямую** — bypasses RLS. Use `pkg/postgres.Pool.WithTenant`. Enforced by depguard `pgxpool-isolation`.
- **`math/rand` (v1)** — depguard banned. Use `math/rand/v2` (non-security) or `crypto/rand` (security).
- **`interface{}`** — use `any`. Idiomatic since Go 1.18.

---

## Multi-tenant SaaS Postgres + RLS

### Canonical
- [**PostgreSQL — Row Security Policies**](https://www.postgresql.org/docs/16/ddl-rowsecurity.html) — официальная док.
- [**PostgreSQL — Server Configuration**](https://www.postgresql.org/docs/16/runtime-config.html) — `app.tenant_id` мы плагаем сюда через `set_config()`.

### Practical
- [**AWS — Multi-tenant data isolation with PostgreSQL Row Level Security**](https://aws.amazon.com/blogs/database/multi-tenant-data-isolation-with-postgresql-row-level-security/) — ровно наш паттерн (set local app.tenant_id), хотя на AWS-стеке.
- [**Citus blog — Multi-tenant patterns**](https://www.citusdata.com/blog/) — поиск "multi-tenant"; ~20 статей разного качества.
- [**Crunchy Data — Postgres Row Security for Multi-Tenant Applications**](https://www.crunchydata.com/blog/) — поиск "row security multi-tenant".

### Gotchas
- **PgBouncer transaction-mode + prepared statements** — конфликтуют. Отключить prepared statement caching в pgx (`statement_cache_capacity: 0`). Уже сделано в `pkg/postgres.Open`.
- **`set_config('app.tenant_id', $1, true)`** — `true` = `LOCAL` (откатывается после tx). Без него секретное значение остаётся в connection pool и течёт между tenant'ами. Разница в один аргумент = security catastrophe.
- **`BYPASSRLS` role** — `tenancy_admin` в нашей схеме. Только для cross-tenant Service-Owner ops. Никогда не выдавайте `app` user'у.

---

## Outbox / Event-driven patterns

### Canonical
- [**Microservices.io — Transactional Outbox**](https://microservices.io/patterns/data/transactional-outbox.html) (Chris Richardson) — каноническое описание.
- [**Confluent — Application modernization patterns: Transactional Outbox**](https://www.confluent.io/blog/transactional-outbox-pattern/) — глубокий разбор.
- [**Debezium — Outbox Event Router**](https://debezium.io/documentation/reference/stable/transformations/outbox-event-router.html) — embedded реализация. Их SQL — наш SQL.

### Practical
- [**"Designing Data-Intensive Applications"**](https://dataintensive.net/) (Martin Kleppmann), главы 10-11 — теория event-driven.

### Gotchas
- **`FOR UPDATE SKIP LOCKED`** — наш relay-демон может крутиться на нескольких репликах одновременно без deduplication. Спасибо `SKIP LOCKED`.
- **`published_at IS NULL`** + индекс — обязателен. Без него full-table scan каждые `Tick`.
- **idempotency** — consumer должен быть idempotent. Outbox даёт at-least-once, не exactly-once.

---

## NATS JetStream

### Canonical
- [**NATS docs root**](https://docs.nats.io/)
- [**JetStream**](https://docs.nats.io/nats-concepts/jetstream) — durable messaging.
- [**Subjects**](https://docs.nats.io/nats-concepts/subjects) — naming conventions, wildcards.

### Project-specific
- Naming convention: `tenant.<t>.<area>.<entity>.<id>.<event>`. См. spec §10.2 + `02-module-contracts.md`.

### Practical
- [**Synadia blog**](https://www.synadia.com/blog) — авторы NATS, реальные prod-кейсы.

### Gotchas
- **Account isolation** — для нашей multi-tenancy используем NATS accounts. Один account на всю платформу + subject-level фильтрация — НЕ безопасно (subject есть string, можно подобрать). Лучше — per-environment NATS accounts.
- **Stream retention** — JetStream хранит сообщения вечно по умолчанию. Set `MaxAge` или `MaxBytes` явно.

---

## FreeSWITCH (для Plan 08+)

См. [`plan-08-freeswitch.md`](plan-08-freeswitch.md) когда будет создан. Краткие отсылки:

- [**FreeSWITCH Confluence**](https://developer.signalwire.com/freeswitch/) — official wiki.
- [**ClueCon talks**](https://www.youtube.com/c/ClueCon) — конференция FreeSWITCH.
- **FreeSWITCH 1.8 Cookbook** (Anthony Minessale) — must-read book.

---

## Recording integrity (для Plan 12)

См. [`plan-12-recording.md`](plan-12-recording.md) когда будет создан. Краткие отсылки:

- [**S3 — Multipart upload**](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpuoverview.html) — overview.
- **VICIDial recording sources** — единственный open-source референс.

---

## Что НЕ положено в COMMON

Topics, которые нужны только в одном плане, идут в свой `plan-NN-*.md`:
- Argon2id, JWT, TOTP, RBAC → Plan 05
- Phone normalization, libphonenumber → Plan 06
- TinyGo, expr-lang, WASM → Plan 07
- ESL protocol, mod_sofia, mod_verto → Plan 08-09
- OperatorFSM, queueing theory → Plan 10
- WebSocket Hub, listen-in conferencing → Plan 11
- ffmpeg pipeline, mod_record → Plan 12
- ClickHouse → Plan 13
- Tariff math, money types → Plan 14
- OTel/Prometheus/Grafana/Loki → Plan 20
