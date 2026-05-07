# CONTEXT — СоциоПульс domain glossary

СоциоПульс is a multi-tenant SaaS platform for telephone sociological surveys. Russian
call-centres ("tenants") run political, social and consumer-research polls: load or
auto-generate phone lists, dial respondents through a progressive auto-dialer, conduct
the survey via an operator in a browser, record the conversation, and produce
reports. The platform is hosted in Yandex Cloud, keeps personal data inside Russia
(152-ФЗ), and ships as a Go modular monolith with two telephony sidecars.

This file is the canonical short definition of every domain term used in code, docs
and ADRs. If a word in this glossary is used somewhere with a different meaning,
that's a bug — fix the usage, or update this glossary and write an ADR.

## Glossary

- **152-ФЗ** — Russian federal law on personal data. Drives RLS, encryption-at-rest, deletion right, audit retention, RF-only data residency.
- **38-ФЗ** — Russian federal law on advertising. Sociological surveys are explicitly **out of scope**; this distinguishes the platform from cold-calling/marketing dialers.
- **Abandonment** — call answered by a respondent but not bridged to an operator within timeout. Tracked because predictive dialers (out of v1) cause it; progressive dialer aims for ~0 abandonment.
- **AHT** — Average Handling Time. Includes dial + talk + wrap. Capacity model uses AHT ≈ 4–5 minutes per call.
- **ASR** — Automatic Speech Recognition. Out of v1; reserved namespace for v2.
- **Audit log** — append-only `audit_log` table; every cross-tenant or PII-touching operation writes here. The `audit` module imports nothing else.
- **Auto-dialer (Progressive 1:1)** — dial one number per ready operator. The only mode in v1 (ADR-0003). Predictive (2:1+) is v2.
- **Call attempt** — a single dialing event (`calls` row). One respondent has many call attempts via retry rules (no-answer, busy, tech-failure).
- **ClickHouse** — OLAP store for analytics events and rolled-up metrics. Fed by NATS → ingester (ADR-0010).
- **Consent prompt** — IVR audio played before recording starts (152-ФЗ requirement). Per-tenant URL; default text declares the call is being recorded for quality control.
- **DEK** — Data Encryption Key. Per-recording AES-256-GCM key, encrypted by the tenant KEK and stored as `<call_id>.dek.enc` next to the audio object.
- **DNC** — Do Not Call list. Numbers excluded from dialing. Per-project and tenant-wide; populated via import or `wrong-person`/`dnc-hit` outcomes.
- **DSL (survey)** — domain-specific expression language for conditional branching in survey schemas. Evaluated by `expr-lang/expr` server-side and (preferably) WASM-compiled in the browser (ADR-0008).
- **Envelope encryption** — encrypt data with a per-object DEK, encrypt the DEK with a per-tenant KEK. Industry-standard pattern; lets KEK rotation skip re-encrypting bulk objects.
- **ESL** — Event Socket Library. FreeSWITCH's RPC protocol. Only `cmd/telephony-bridge` speaks ESL; everything else routes through NATS.
- **FreeSWITCH** — open-source softswitch handling SIP, RTP, recording, IVR. Runs on dedicated VMs outside Kubernetes (ADR-0007). Version 1.10.
- **FSM (operator)** — Finite State Machine of the operator: `offline → ready → dialing → call → status → verify → ready`, plus `pause` from any state. `verify` is reachable only from `success`-class outcomes.
- **JetStream** — NATS persistence layer with at-least-once delivery. Used for durable subjects (e.g. `dialer.call.finalized`, `recording.uploaded`).
- **KEK** — Key Encryption Key. Per-tenant master key in Yandex KMS, used to wrap DEKs. Rotated yearly; old DEKs decrypt under previous KEK versions.
- **KMS** — Yandex Key Management Service. Holds tenant KEKs and performs `Encrypt`/`Decrypt`/`GenerateDataKey`. Only the `tenancy` module talks to KMS directly.
- **Listen-in** — admin/supervisor silently listens to a live operator–respondent call (FreeSWITCH `mixmonitor` in `silent` mode). v2 adds `whisper` and `barge-in`.
- **Lockbox** — Yandex secret manager. Source of secrets injected into pods via CSI volume-mounts or k8s Secrets backed by Lockbox.
- **mod_verto** — FreeSWITCH module providing the Verto WebRTC signaling protocol. The operator's browser registers via `mod_verto` to get a media path to a FS-VM.
- **OLTP / OLAP split** — Postgres handles transactional state; ClickHouse handles analytical queries. NATS bridges them (ADR-0010).
- **Operator (оператор)** — call-centre worker who runs the survey with a respondent. Identified by a user account in a tenant; has an FSM session per shift.
- **Opus** — audio codec used for stored recordings (32 kbit/s, 16 kHz). Re-encoded from FreeSWITCH `.wav` by `cmd/recording-uploader`.
- **Outbox pattern** — durable transactional queue: write to `event_outbox` inside the business transaction; a separate relay publishes to NATS. Guarantees at-least-once cross-process delivery.
- **PgBouncer** — connection pooler in transaction mode (ADR-0006). Each API call is one transaction; `SET LOCAL app.tenant_id` binds RLS scope per transaction.
- **PII** — Personally Identifiable Information. In this domain: phone numbers, full names, recording audio. Encrypted at rest, hashed (HMAC-SHA256+pepper) for indexing.
- **Predictive dialer** — dialer that calls multiple numbers per operator (e.g. 2:1) anticipating answer-rate. **Out of v1** (ADR-0003); causes abandonment by design.
- **Project (проект)** — a survey campaign owned by a tenant: one survey schema, target sample, quotas, optional respondent base, working-hours window, retry rules, optional DNC.
- **Quota** — required count of respondents matching specific dimensions (region × gender × age, etc.). Drives RDD region selection and operator routing.
- **RBAC** — Role-Based Access Control. Roles: Service-Owner, tenant-Admin, Project-Manager, Supervisor, Operator. Enforced in gateway middleware.
- **RDD** — Random Digit Dialing. Generate respondent phone numbers algorithmically by region prefix (DEF/АВС-codes); used when quotas underfilled or base exhausted.
- **Recording** — encrypted Opus audio of the operator–respondent conversation, stored in S3 with envelope encryption. Metadata in `call_recordings`. Integrity target: 99.5% uploaded (ADR-0005).
- **Respondent (респондент)** — person being surveyed. Identified internally by tenant-scoped UUID; phone is encrypted+hashed.
- **RLS** — Row-Level Security in PostgreSQL. Every domain table has a policy `using (tenant_id = current_setting('app.tenant_id')::uuid)`. Bypassed only by the `tenancy_admin` role.
- **S3** — Yandex Object Storage (S3-compatible). Per-tenant buckets `sociopulse-recordings-<tenant>`. Only `cmd/recording-uploader` writes recordings; `cmd/api` only reads.
- **Service-Owner** — platform-level admin (cross-tenant). Distinct from a tenant's own admin. Reaches data via the `tenancy_admin` BYPASSRLS role and through the audit feed.
- **SIP-trunk** — connection from a telephony provider for outbound calls. Trunks are owned by the platform operator in v1; routed by `least_cost`, `round_robin`, etc.
- **Sociology** — the project's business domain. Surveys conducted by accredited research firms; explicitly **not** marketing or cold-calling (38-ФЗ does not apply).
- **Soft phone** — software SIP client running outside the browser. **Not** the path used here; the operator runs WebRTC inside the browser via `mod_verto` (ADR-0001).
- **Survey schema** — versioned JSON describing questions, branching rules (DSL), validation. Once a project starts, the schema version is immutable; new versions create a new project run.
- **Tenant (арендатор)** — call-centre customer of the platform. The unit of isolation: RLS, KEK, S3 bucket, NATS subject prefix all key on `tenant_id`.
- **TinyGo** — Go compiler producing small WASM. Used to compile the survey runtime for the browser (ADR-0008, conditional accept pending PoC).
- **TLS termination** — done at the ingress (k8s Ingress / nginx) for HTTPS/WSS; FS-VMs terminate their own TLS for SIP-WSS and ESL.
- **TURN** — relay server for WebRTC NAT traversal. Required when an enterprise firewall blocks UDP between operator browser and FS-VM.
- **Verto** — FreeSWITCH WebRTC signaling protocol used by `mod_verto`. The browser opens `wss://fs-node-N:8082/` and registers a per-shift SIP account.
- **WAS** — WebAssembly Survey runtime. Go code compiled with TinyGo to WebAssembly, evaluating the survey DSL inside the operator's browser for instant UX.
- **WebRTC** — browser-native real-time media stack (DTLS-SRTP). Carries the operator-to-FS audio path peer-to-peer relative to the browser and the FS-VM.

## Concepts NOT in this domain

To prevent scope creep — these are explicitly **not** what СоциоПульс does:

- **Email marketing** — no SMTP send-side, no campaign builder, no email lists.
- **SMS campaigns** — out of v1; no SMS gateway integration.
- **Cold-calling regulated by 38-ФЗ** — sociological surveys are not advertising; the platform does not target or claim to support 38-ФЗ-regulated outreach.
- **CRM-style ticket management** — no leads, deals, ticket pipelines, or sales-force-style features. Respondents are sample units, not customers.
- **Video conferencing** — calls are audio-only; no WebRTC video, no screen-share, no Zoom/Meet substitute.
- **Predictive dialing in v1** — see Predictive dialer above (ADR-0003).
- **ASR / sentiment analysis in v1** — see ASR above. Recordings are stored encrypted; transcription is a v2 backlog item.
- **Customer-supplied SIP-trunks in v1** — tenants use platform-provided trunks; bring-your-own-trunk is v2.
- **Live recording replication** — at-most-99.5% integrity target accepts loss of in-progress recordings on FS-VM crash (ADR-0005); no live RTP-mirror in v1.

## Cross-references

- ADR registry: [docs/adr/](docs/adr/) — fifteen accepted decisions; each glossary term that derives from a decision links its ADR by number.
- System-design spec: [docs/superpowers/specs/2026-05-06-sociopulse-system-design.md](docs/superpowers/specs/2026-05-06-sociopulse-system-design.md) — the single source of truth for behaviour.
- Architecture overview: [docs/architecture/00-overview.md](docs/architecture/00-overview.md) — module list, dependency graph, external dependencies matrix.
- Module contracts: [docs/architecture/02-module-contracts.md](docs/architecture/02-module-contracts.md) — public interfaces of each domain module.
- Top-level architecture digest: [ARCHITECTURE.md](ARCHITECTURE.md).

When this glossary and another document disagree, the glossary loses for **behaviour**
(spec wins) and **code organisation** (architecture docs win); it wins for
**terminology** — fix the other document to match a term defined here.
