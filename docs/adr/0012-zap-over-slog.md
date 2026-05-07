# ADR-0012: Go logger — zap (вместо slog)

**Статус:** Accepted
**Дата:** 2026-05-06
**Принимающий:** platform team

## Контекст

Пользователь явно выбрал zap.

## Альтернативы

—

## Решение

`go.uber.org/zap` с production-config + redaction-middleware.

**Trade-off**: zap менее каноничен после появления `slog` в std-lib (Go 1.21+), но он быстрее (zero-alloc) и поддерживает sampling из коробки. Для high-throughput сервиса (тысячи WS-фреймов/сек) zap предпочтительнее.

## Последствия

—

## Связанное

- Спека §22 (ADR-012)
- ADR-0014 (gin интегрируется через `gin-contrib/zap`)
- `docs/architecture/06-observability.md`
