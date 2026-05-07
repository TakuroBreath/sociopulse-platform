# ADR-0011: NATS JetStream вместо Kafka

**Статус:** Accepted
**Дата:** 2026-05-06
**Принимающий:** platform team

## Контекст

Нужна шина событий между модулями монолита и sidecar'ами, с durability для critical-flows и pub-sub.

## Альтернативы

- A: Kafka. Industry standard, но операционно тяжелее.
- B: NATS JetStream. Лёгкий, нативная Go-интеграция, достаточно durability.
- C: RabbitMQ. Менее подходит для streaming.

## Решение

B.

**Trade-off**: меньшая ёмкость retention vs Kafka, но на нашем масштабе (50k events/day на критичные subjects) NATS достаточен. При росте — миграция возможна.

## Последствия

—

## Связанное

- Спека §22 (ADR-011)
- ADR-0010 (Postgres + ClickHouse — NATS используется в ETL)
- `docs/architecture/00-overview.md`, `docs/architecture/02-module-contracts.md`
