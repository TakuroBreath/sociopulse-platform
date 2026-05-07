# Architecture Decision Records

Single source of truth: ADRs follow the [Nygard format](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions). Each ADR documents one significant architectural decision (context, alternatives, choice, consequences).

## Statuses

- **Proposed** — under discussion.
- **Accepted** — decided; in force.
- **Conditional Accept** — accepted contingent on a stated proof-of-concept or measurement.
- **Deprecated** — no longer recommended; existing code may comply, new code should not.
- **Superseded by ADR-NNNN** — a later ADR replaces this one.

## ADR template

```markdown
# ADR-NNNN: <English Title>

**Статус:** Accepted | Proposed | Deprecated | Superseded by ADR-NNNN
**Дата:** YYYY-MM-DD
**Принимающий:** <name or "platform team">

## Контекст
<copied verbatim from spec>

## Решение
<copied verbatim from spec>

## Альтернативы
<copied verbatim from spec; if absent in spec, write "—">

## Последствия
<copied verbatim from spec; positive/negative/neutral trade-offs>

## Связанное
- Спека §22 (ADR-NNNN)
- ADR-NNNN (cross-link if mentioned in body)
- Plan NN Task N (if implementation plan exists)
```

## Index

| # | Title | Status | Date |
|---|---|---|---|
| [0001](0001-mod-verto-sip-wss.md) | Аудио-путь оператора через WebRTC (mod_verto/SIP-WSS) | Accepted | 2026-05-06 |
| [0002](0002-freeswitch-self-hosted-multi-trunk.md) | FreeSWITCH self-hosted, multi-trunk routing | Accepted | 2026-05-06 |
| [0003](0003-progressive-dialer.md) | Progressive-dialer для v1, predictive — на v2 | Accepted | 2026-05-06 |
| [0004](0004-modular-monolith.md) | Модульный монолит на Go + 2 sidecar'а | Accepted | 2026-05-06 |
| [0005](0005-recording-integrity-99-5.md) | Целостность записей — 99.5% | Accepted | 2026-05-06 |
| [0006](0006-pgbouncer-transaction-mode.md) | PgBouncer transaction-mode + RLS | Accepted | 2026-05-06 |
| [0007](0007-freeswitch-outside-k8s.md) | FreeSWITCH вне Kubernetes | Accepted | 2026-05-06 |
| [0008](0008-survey-runtime-wasm.md) | Survey runtime: TinyGo→WASM с TS-port fallback | Conditional Accept | 2026-05-06 |
| [0009](0009-handwritten-css.md) | CSS — handwritten | Accepted | 2026-05-06 |
| [0010](0010-postgres-plus-clickhouse.md) | Postgres + ClickHouse | Accepted | 2026-05-06 |
| [0011](0011-nats-over-kafka.md) | NATS JetStream вместо Kafka | Accepted | 2026-05-06 |
| [0012](0012-zap-over-slog.md) | zap вместо slog | Accepted | 2026-05-06 |
| [0013](0013-viper-config.md) | viper для конфигурации | Accepted | 2026-05-06 |
| [0014](0014-gin-http-router.md) | HTTP-роутер: gin-gonic/gin | Accepted | 2026-05-07 |
| [0015](0015-tdd-mandatory.md) | TDD as mandatory discipline | Accepted | 2026-05-07 |

## Relationship to spec

The spec at `docs/superpowers/specs/2026-05-06-sociopulse-system-design.md` § 22 contains the original ADR records. The standalone files in this directory are **the canonical version going forward** — when an ADR needs to be updated or superseded, edit the file here, not the spec. The spec is a snapshot; this directory is living history.

## Adding a new ADR

1. Pick the next available number (zero-padded, 4 digits).
2. Copy the template into `docs/adr/NNNN-<slug>.md`.
3. Add a row to the Index above.
4. If the new ADR supersedes an existing one, update the existing one's Status to `Superseded by ADR-NNNN`.
5. Reference the ADR in commit messages using `ADR-NNNN` and in code via comments.
