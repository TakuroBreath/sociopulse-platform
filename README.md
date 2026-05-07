# sociopulse-platform

Backend platform for **СоциоПульс** — multi-tenant SaaS for telephone sociological surveys (call-centres running political/social polls).

## Services in this repo

| Binary | Purpose |
|---|---|
| `cmd/api` | HTTP/WS/gRPC API; modular monolith hosting auth, crm, surveys, dialer, realtime, recording, analytics, billing, reports, tenancy modules |
| `cmd/telephony-bridge` | ESL ↔ NATS bridge between dialer and FreeSWITCH cluster |
| `cmd/recording-uploader` | systemd-deployed uploader on FS-VMs: fsnotify → ffmpeg → KMS envelope encrypt → S3 → gRPC commit |
| `cmd/migrator` | golang-migrate CLI for schema migrations |
| `cmd/worker` | asynq workers (retention, integrity, retry orchestration, etc.) |
| `cmd/synthetic` | canary monitor running production-like user journeys |
| `cmd/status-page` | minimal in-house status page |

## Stack

Go 1.22+, PostgreSQL 16 (RLS + PgBouncer transaction-mode), Redis 7, ClickHouse, NATS JetStream, gRPC mTLS, OpenTelemetry, Prometheus, Yandex Cloud (Object Storage, KMS, Managed services).

## Documentation

Specification, architecture decisions, and 22 implementation plans live in [`docs/superpowers/`](docs/superpowers/):

- `specs/` — system design spec
- `plans/` — implementation plans (Phase 1: 00, 00a, 02-14, 20 Task 1; Phase 2: 01, 08, 20 Tasks 2-7)
- `reviews/` — architecture & plans review with Phase 1 / Phase 2 split

This repo is the **master location** for all project documentation; sibling repos reference these via GitHub URLs.

## Repos in the project

- **sociopulse-platform** — this repo (backend Go monorepo)
- **sociopulse-web** — React 18 + TypeScript frontend
- **sociopulse-infra** — Terraform (Yandex Cloud) + Packer/Ansible (FS-cluster) + Helm/ArgoCD

## Status

Pre-implementation. Specification approved (157 KB), 21 plans written and reviewed (1.85 MB total), no code yet.
