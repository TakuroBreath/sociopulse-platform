# CLAUDE.md

Guide for Claude when working in this repository.

## Project context

This is one of three repositories in the **СоциоПульс** project — a multi-tenant SaaS for telephone sociological surveys (call-centres running political/social polls). Target scale: 30 tenants, 500 concurrent operators, 50k calls/day.

| Repo | Role | URL |
|---|---|---|
| `sociopulse-platform` | Backend Go monorepo (cmd/api + sidecars + workers) | https://github.com/TakuroBreath/sociopulse-platform |
| `sociopulse-web` | React + TypeScript frontend | https://github.com/TakuroBreath/sociopulse-web |
| `sociopulse-infra` | Terraform / Packer / Ansible / Helm IaC | https://github.com/TakuroBreath/sociopulse-infra |

This is the **`sociopulse-platform`** repo — backend Go monorepo.

Implementation specification, architecture decisions, and 21 implementation plans live in [`/Users/user/call-center/social-pulse/docs/superpowers/`](../social-pulse/docs/superpowers/) until they're migrated into this repo's `docs/`. Plans relevant to this repo: 00 (foundation), 02-14, 20.

## Identity

Local git config in this repo: `TakuroBreath / maxsmurffy@gmail.com` (set via `git config --local`). The global git config on this machine is a different user — never rely on it for commits here.

`gh` CLI is authenticated as `TakuroBreath`.

## Agent skills

### Issue tracker

Issues live in GitHub Issues for this repo (`TakuroBreath/sociopulse-platform`). See `docs/agents/issue-tracker.md`.

### Triage labels

Five canonical labels (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`) — defaults, already created on GitHub. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context layout — one `CONTEXT.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.
