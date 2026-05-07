# ADR-0006: PostgreSQL RLS + transaction-mode PgBouncer + SET LOCAL

**Статус:** Accepted
**Дата:** 2026-05-06
**Принимающий:** platform team

## Контекст

Multi-tenancy через RLS требует, чтобы `app.tenant_id` был правильно установлен в каждой транзакции. PgBouncer в session mode не масштабируется (1 backend connection ≈ 1 client).

## Альтернативы

- A: `PgBouncer session mode` + `set_config` persist=true. Не масштабируется.
- B: `PgBouncer transaction mode` + `SET LOCAL app.tenant_id`. Масштабируется, требует дисциплины.
- C: Без PgBouncer (прямые connection'ы). Не масштабируется.

## Решение

B.

## Последствия

Каждая API-операция = одна транзакция; запрещены долгие транзакции в hot-path; готовый код-pattern в `gateway` middleware.

## Связанное

- Спека §22 (ADR-006)
- `docs/architecture/05-configuration.md` (DSN/PgBouncer-настройки)
- ADR-0014 (gin — middleware-цепочка для tenant context)
