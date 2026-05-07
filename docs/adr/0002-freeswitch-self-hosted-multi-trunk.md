# ADR-0002: FreeSWITCH self-hosted, multi-trunk routing

**Статус:** Accepted
**Дата:** 2026-05-06
**Принимающий:** platform team

## Контекст

На 50k звонков/день нужна управляемая, масштабируемая телефонная плоскость с собственной dialer-логикой и хранением записей у нас.

## Альтернативы

| Дим. | A: Облачный API (Voximplant и т.п.) | B: Self-hosted FS, 1 trunk | C: Self-hosted FS, multi-trunk |
|---|---|---|---|
| Complexity | Low (готовое API) | Medium (свой кластер) | Medium-High (+routing) |
| Cost (50k звонков/мес) | ₽700-900k | ₽525k + инфра | ₽525k + инфра + multi-routing |
| Flexibility | Lockin | High | High |
| Resilience | Vendor SLA | SPOF на trunk'е | High (multi-trunk failover) |

## Решение

C.

## Последствия

- Нужны компетенции по FreeSWITCH (агенты-исполнители имеют их).
- Контракты с ≥ 2 операторами связи (бизнес-процесс, не код).
- Гибкость в дизайне dialer-алгоритмов.

## Связанное

- Спека §22 (ADR-002)
- ADR-0001 (WebRTC-путь от браузера до FS)
- ADR-0007 (FreeSWITCH вне Kubernetes)
