# ADR-0013: viper для конфигурации (yaml + env override + hot-reload)

**Статус:** Accepted
**Дата:** 2026-05-06
**Принимающий:** platform team

## Контекст

Нужен механизм чтения конфигурации с поддержкой yaml, env-override и hot-reload отдельных параметров (например, `log_level`).

## Альтернативы

- viper — feature-полный, тяжеловат.
- koanf — легче, тот же API.

## Решение

`spf13/viper` для чтения `config.yaml` с env-override и `fsnotify`-watch (для hot-reload `log_level` и т.п.).

**Trade-off**: viper тяжеловат, но даёт нужный feature-set. Альтернатива — `koanf` (легче, тот же API).

## Последствия

—

## Связанное

- Спека §22 (ADR-013)
- `docs/architecture/05-configuration.md`
