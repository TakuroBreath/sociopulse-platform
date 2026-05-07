# Architecture

This is a short pointer file. The authoritative system design lives in:

**[docs/superpowers/specs/2026-05-06-sociopulse-system-design.md](docs/superpowers/specs/2026-05-06-sociopulse-system-design.md)**

## TL;DR

- **Style:** Modular monolith on Go + 2 sidecars (`telephony-bridge`, `recording-uploader`) + FreeSWITCH cluster.
- **Cloud:** Yandex Cloud (152-ФЗ residency).
- **Telephony:** Self-hosted FreeSWITCH with multi-trunk routing.
- **Operator audio:** WebRTC via mod_verto.
- **Auto-dialer model:** Progressive (1:1) for v1.
- **Storage:** PostgreSQL (OLTP, RLS multi-tenant), ClickHouse (OLAP), Redis 7 (FSM/queues), Yandex Object Storage (recordings + reports), Yandex KMS (per-tenant KEK).
- **Event bus:** NATS JetStream.
- **Frontend:** React 18 + TypeScript + Vite, sourced from prototype `SocioPulse.html` and `social-pulse-maket/project/`.
- **HTTP router:** gin-gonic/gin v1.10+ (ADR-0014).
- **Logger:** zap v1.27+ (ADR-0012).

## ADRs

Numbered ADRs (001–015) live inside the system design doc, §22. Standalone
files under `docs/adr/` will be added by Plan 00a Task 2.
