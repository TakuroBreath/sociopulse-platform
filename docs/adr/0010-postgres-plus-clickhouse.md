# ADR-0010: Postgres + ClickHouse для OLTP+OLAP-разделения

**Статус:** Accepted
**Дата:** 2026-05-06
**Принимающий:** platform team

## Контекст

На 50k звонков/день и горизонте 1+ год Postgres не вытянет аналитические запросы (group by tenant + project + month) без серьёзных индексов и денормализации.

## Альтернативы

- A: Только Postgres. Нужны materialized views, проблемы при обновлениях.
- B: Postgres + ClickHouse для аналитики. Двойное хранение, но чистая archi.
- C: Postgres + Druid / TimescaleDB. TimescaleDB ближе, но менее выразительный для OLAP.

## Решение

B.

**Trade-off**: дополнительная инфра-стоимость и ETL-сложность; зато аналитика отделена от OLTP, не влияет на latency API.

## Последствия

—

## Связанное

- Спека §22 (ADR-010)
- ADR-0011 (NATS JetStream — шина для ETL Postgres → ClickHouse)
- `docs/architecture/00-overview.md` (схема системы)
