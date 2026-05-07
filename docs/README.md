# СоциоПульс — Documentation

## Top-level

- [README](../README.md) — project intro, quickstart
- [ARCHITECTURE](../ARCHITECTURE.md) — short architecture overview
- [CONTRIBUTING](../CONTRIBUTING.md) — dev workflow, commit style, module boundaries
- [LICENSE](../LICENSE) — proprietary

## Specifications

- [docs/superpowers/specs/2026-05-06-sociopulse-system-design.md](superpowers/specs/2026-05-06-sociopulse-system-design.md)
  — full system design doc (FR/NFR, architecture, ADRs).

## Implementation plans

Sequenced plans for agentic execution. Each plan produces working, committable software.

- [00 — Foundation](superpowers/plans/2026-05-06-00-foundation.md) — repo bootstrap, lint, CI, hello-world cmd/api **(this plan)**
- [00a — Architecture Foundation](superpowers/plans/2026-05-06-00a-architecture-foundation.md) — module API contracts, pkg/ scaffolds, depguard, `docs/architecture/00–07-*.md`
- [00b — Tech Baseline & TDD Discipline](superpowers/plans/2026-05-06-00b-tech-baseline-tdd.md) — gin over chi, zap reaffirmed, `docs/architecture/08-tdd-discipline.md`
- 01 — Infrastructure (Terraform + k8s, **deferred to Phase 2**)
- 02 — cmd/api skeleton (config, observability, gateway middleware)
- 03 — Database & migrations
- 04 — Tenancy module
- 05 — Auth module
- 06 — CRM module
- 07 — Surveys module
- 08 — FreeSWITCH cluster + recording-uploader
- 09 — telephony-bridge
- 10 — Dialer module
- 11 — Realtime module
- 12 — Recording module
- 13 — Analytics + Reports
- 14 — Billing module
- 15 — Frontend foundation
- 16 — Frontend operator pages
- 17 — Frontend admin pages 1
- 18 — Frontend admin survey builder
- 19 — Frontend admin pages 2
- 20 — Observability foundation (**Phase 2**)

## Architecture

- [docs/architecture/](architecture/) — design docs (00-overview through 08-tdd-discipline)
- [docs/adr/](adr/) — ADRs (created by Plan 00a Task 2 — promotes spec §22 to standalone files)

## Runbooks

- [docs/runbooks/](runbooks/) — ops procedures (deploy, incident, DR; populated by Plan 20)

## API documentation

- [docs/api/](api/) — OpenAPI specs, gRPC `.proto` files, generated reference
