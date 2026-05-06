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

Specification, architecture decisions, and 21 implementation plans live in [`/Users/user/call-center/social-pulse/docs/superpowers/`](../social-pulse/docs/superpowers/) (will be migrated into `docs/` here when execution starts).

## Repos in the project

- **sociopulse-platform** — this repo (backend Go monorepo)
- **sociopulse-web** — React 18 + TypeScript frontend
- **sociopulse-infra** — Terraform (Yandex Cloud) + Packer/Ansible (FS-cluster) + Helm/ArgoCD

## Status

Pre-implementation. Specification approved (157 KB), 21 plans written and reviewed (1.85 MB total), no code yet.
